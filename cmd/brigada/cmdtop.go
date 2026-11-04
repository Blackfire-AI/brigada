package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/caps"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
	"github.com/Blackfire-AI/brigada/internal/projecturl"
	"github.com/Blackfire-AI/brigada/internal/term"
	"golang.org/x/sys/unix"
)

// topClient is what top asks of the API client: the status, the
// capacity snapshot, the projects (to name a repo's project and the
// view's scope) and the newest history page — all endpoints that exist — and a copy of itself
// whose reads name one project. Tests stub it.
type topClient interface {
	Status(ctx context.Context) (api.Status, error)
	Capacity(ctx context.Context, projectID int64) (engine.CapacitySnapshot, error)
	Projects(ctx context.Context) ([]engine.ProjectInfo, error)
	History(ctx context.Context, q api.HistoryQuery) (history.Page, error)
	forProject(projectID int64) topClient
}

// topAPI is the real topClient: the API client, whose ProjectReader
// answers with its own concrete type.
type topAPI struct{ *api.Client }

func (c topAPI) forProject(projectID int64) topClient {
	return topAPI{c.ProjectReader(projectID)}
}

// topEventsLimit is the history page top fetches: the newest items, as
// many as the engine's history bound (500) leaves room for beside the
// job_cap_held, job_leased and job_created items that dominate the
// timeline and that topFetch drops, so the kept ones still fill any
// terminal's events section.
const topEventsLimit = 200

// topEventsKeep is how many kept items the loop holds across ticks: the
// newest, so a busy run that pushes every kept item out of one page
// never empties the events section.
const topEventsKeep = 50

// runTop is `brigada top`: the engine as a full-screen view that
// redraws every --interval from the status, capacity and history the
// API already serves — no endpoint of its own. When stdout is not a
// terminal it prints one plain snapshot and exits, so pipes and tests
// read it. Engine errors show as a line in the view while the loop
// keeps retrying; with --no-start the retry never starts an engine.
func runTop(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("top",
		"brigada top [--interval 2s] [--project REPO-URL] [--no-start]",
		"Watch the engine full-screen; one plain snapshot when the output is not a terminal.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		interval time.Duration
		project  string
		noStart  bool
	)
	fs.DurationVar(&interval, "interval", 2*time.Second, "how often to redraw, as in 500ms or 2s")
	fs.StringVar(&project, "project", "", "the repository URL whose project to show (default: every project)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; show the error and keep retrying")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "top takes no positional arguments, got %q", extra)
	}
	if interval <= 0 {
		return usagef(stderr, "--interval must be positive, got %s", interval)
	}
	repo := ""
	if project != "" {
		canonical, err := projecturl.Canonical(project)
		if err != nil {
			return usagef(stderr, "--project: %v", err)
		}
		repo = canonical
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	if !detectCaps(stdout).TTY {
		client, code := connect(ctx, rt, "client", noStart, stderr)
		if code != 0 {
			return code
		}
		// The hint sits under the header, so it is fetched first and goes
		// into the model the snapshot renders.
		return printTopSnapshot(ctx, topAPI{client}, repo, agentsHint(ctx, rt, true), time.Now(), stdout, stderr)
	}
	return topLoop(ctx, rt, repo, interval, noStart, stdout)
}

// printTopSnapshot fetches once and prints the plain rendering; the
// loop never runs here. Any fetch error is a failure, not a line. now is
// the clock the rendering reads, so a test can fix it. hint is the
// unregistered-models line, which the rendering puts under the header.
func printTopSnapshot(ctx context.Context, client topClient, repo, hint string, now time.Time, stdout, stderr io.Writer) int {
	model, err := topFetch(ctx, client, repo)
	if err != nil {
		return fail(stderr, err)
	}
	model.hint = hint
	style := term.New(detectCaps(stdout))
	fmt.Fprintln(stdout, strings.Join(renderTop(model, style.Width(), 0, now, style), "\n"))
	return 0
}

// topModel is one tick of top's data, all of it fetched before any
// rendering: the status answer, the capacity snapshot and the newest
// history page, plus the fetch's own error and the pause the keys set.
// The rendering is a pure function of the model and the size.
type topModel struct {
	status   api.Status
	capacity engine.CapacitySnapshot
	events   []history.Item
	// scope is what the header says the view shows: a repo's short name
	// or `N projects`, "" when the projects could not be read.
	scope string
	// tagPlans says more than one project shows, so each plan line names
	// its repo.
	tagPlans bool
	// soleProject says the view is unscoped and the engine has exactly one
	// project, so `brigada push approve` needs no --repo.
	soleProject bool
	// statusOK says the status fetched, so an empty plans list is the
	// engine's answer and not the absence of one.
	statusOK bool
	// err is the fetch's first error: the view shows it as a line and
	// the loop keeps retrying. Parts that did fetch still render.
	err error
	// paused says the loop froze the refresh (the p key).
	paused bool
	// hint is the unregistered-models line of the cached listing, or "".
	hint string
}

// topFetch gathers one tick's model: the scope, the status, the capacity
// snapshot and the newest history page, of the project the repo names, or
// of the whole engine when the repo is empty. A project's status lists no
// broken agent, since agents belong to no project, so a repo adds one
// unscoped Status call and takes only that answer's agent entries, after
// the project's own. A part that fails leaves its zero value and notes
// the error; the first error comes back too, so the snapshot path can
// fail while the loop shows the line and retries.
func topFetch(ctx context.Context, client topClient, repo string) (topModel, error) {
	var model topModel
	reader := client
	var projectID int64
	if repo != "" {
		model.scope = topRepoName(repo)
		id, err := lookupProjectID(ctx, client, repo)
		if err != nil {
			model.err = err
			return model, err
		}
		projectID = id
		reader = client.forProject(id)
	} else if projects, err := client.Projects(ctx); err != nil {
		model.err = err
	} else if len(projects) == 1 {
		model.scope = topRepoName(projects[0].RepoURL)
		model.soleProject = true
	} else {
		model.scope = fmt.Sprintf("%d projects", len(projects))
		model.tagPlans = len(projects) > 1
	}
	if st, err := reader.Status(ctx); err != nil {
		model.err = err
	} else {
		model.status = st
		model.statusOK = true
		if repo != "" {
			if all, err := client.Status(ctx); err != nil {
				model.err = err
			} else {
				model.status.NeedsYou = topWithAgents(st.NeedsYou, all.NeedsYou)
			}
		}
	}
	if snap, err := reader.Capacity(ctx, projectID); err == nil {
		model.capacity = snap
	} else if model.err == nil {
		model.err = err
	}
	if page, err := client.History(ctx, api.HistoryQuery{ProjectID: projectID, Limit: topEventsLimit, Last: true}); err == nil {
		for _, it := range page.Items {
			if topShows(it) {
				model.events = append(model.events, it)
			}
		}
	} else if model.err == nil {
		model.err = err
	}
	return model, model.err
}

// topRepoName is a repo's short name: the last path segment of its URL,
// without .git.
func topRepoName(url string) string {
	name := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	return name[strings.LastIndexAny(name, "/:")+1:]
}

// topWithAgents is a project's needs-you entries followed by the agent
// entries of the unscoped queue, in the order they came.
func topWithAgents(scoped, unscoped []engine.NeedsYouItem) []engine.NeedsYouItem {
	merged := append([]engine.NeedsYouItem{}, scoped...)
	for _, n := range unscoped {
		if n.Kind == "agent" {
			merged = append(merged, n)
		}
	}
	return merged
}

// keepPrevious fills the parts a failed fetch left empty from the last
// tick's model, so an error line sits over the last data instead of a
// blank screen.
func (m topModel) keepPrevious(prev topModel) topModel {
	if m.status.Pid == 0 && m.status.StartedAt == "" {
		m.status, m.statusOK = prev.status, prev.statusOK
	}
	if len(m.capacity.Agents) == 0 && len(m.capacity.Layers) == 0 {
		m.capacity = prev.capacity
	}
	if m.events == nil {
		m.events = prev.events
	}
	if m.scope == "" {
		m.scope, m.tagPlans, m.soleProject = prev.scope, prev.tagPlans, prev.soleProject
	}
	return m
}

// mergeTopEvents is the kept items of the previous model and of a tick,
// deduplicated by (Type, ID) with the tick's copy winning, oldest first,
// the newest topEventsKeep of them.
func mergeTopEvents(prev, tick []history.Item) []history.Item {
	type key struct {
		typ string
		id  int64
	}
	seen := make(map[key]bool, len(prev)+len(tick))
	var merged []history.Item
	for _, group := range [][]history.Item{tick, prev} {
		for _, it := range group {
			k := key{it.Type, it.ID}
			if !seen[k] {
				seen[k] = true
				merged = append(merged, it)
			}
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].CreatedAt != merged[j].CreatedAt {
			return merged[i].CreatedAt < merged[j].CreatedAt
		}
		return merged[i].ID < merged[j].ID
	})
	if len(merged) > topEventsKeep {
		merged = merged[len(merged)-topEventsKeep:]
	}
	return merged
}

// dialEngine is connect for a loop that shows its errors instead of
// printing them: the client, or why there is none.
func dialEngine(ctx context.Context, rt *runtime, role string, noStart bool) (*api.Client, error) {
	var msg bytes.Buffer
	client, code := connect(ctx, rt, role, noStart, &msg)
	if code != 0 {
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg.String()), "brigada: "))
		if text == "" {
			text = "cannot reach the engine"
		}
		return nil, errors.New(text)
	}
	return client, nil
}

// renderTop lays the view out, top to bottom: the header, the meters
// strip, NEEDS YOU, the FOREMAN block (a foreman turn no plan claims) and
// the JOBS block (any other job no plan claims), each only when it has
// lines, the PLANS tree with every job under its plan, the AGENTS
// section and the EVENTS section at the bottom (the report line, the
// EVENTS head and up to 10 rows, newest first), every line cut to width.
// The height is a limit, 0 meaning none: the frame never has more lines
// than it. Every line carries a yield class (topYield) and, while the
// frame is taller than the height, lines drop class by class, each class
// from the bottom of the view upward, one line at a time, until it fits:
// a0 event rows 3 and up; a event row 2, then the foreman's report line
// above the EVENTS head; b finished plans, folded into the dim
// +N finished line; c the blank lines between sections; d queued task
// lines (waiting on, review included); e idle agents, counted in the dim
// +N idle · M off (brigada capacity) line; f held and retry lines, the
// FOREMAN and JOBS notes and parked task lines; g running lines; h every other line outside the header and NEEDS
// YOU — event row 1, the other agent lines, the global
// line, the fold lines, each plan's own line, the placeholders, the PLANS,
// AGENTS and EVENTS heads — with the meters strip dropping last of the class,
// though it sits above NEEDS YOU. A section head goes with its section's
// last line, the FOREMAN and JOBS heads too: it drops next, unless the
// frame already fits. i the hard clip: only the header (with its error
// and hint lines) and NEEDS YOU remain, and the frame is cut from the
// bottom. A fold line not yet shown costs the line it adds. With a plain
// style the text is the same and has no escapes.
func renderTop(model topModel, width, height int, now time.Time, style term.Style) []string {
	var lines []topLine
	add := func(section []topLine) {
		if len(section) == 0 {
			return
		}
		if len(lines) > 0 {
			lines = append(lines, topLine{class: yieldBlank})
		}
		lines = append(lines, section...)
	}
	add(topPinned(topHeader(model, now, style, width)))
	add([]topLine{{text: topMeters(model, style, width)[0], class: yieldRest, late: true}})
	add(topPinned(topNeedsYou(model.status, model.soleProject, style, width)))
	foreman, jobs := topPlanless(model.status, now, style, width)
	add(foreman)
	add(jobs)
	add(newTopPlans(model.status, model.tagPlans, model.statusOK, now, style, width))
	add(topAgentLines(model, now, style, width))
	// The report line, then the EVENTS head, with no blank line between.
	add(append(topReportLine(model.status, style, width), topEventLines(model.events, now, style, width)...))

	frame := &topFrame{lines: lines, hidden: model.status.HiddenFinishedPlans, style: style}
	frame.fit(height)
	out := frame.texts()
	if height > 0 && len(out) > height {
		out = out[:height]
	}
	for i, line := range out {
		out[i] = topFit(line, width)
	}
	return out
}

// topYield is a line's yield class: the order in which lines drop while
// the frame is taller than the height. To make a new kind of line yield,
// give it a class when its section builds it; the drop loop needs no
// other change.
type topYield int

const (
	yieldPinned       topYield = iota // header and NEEDS YOU: only the hard clip cuts them
	yieldEventsExtra                  // a0: event rows 3 and up
	yieldEventsSecond                 // a: event row 2, then the report line above the EVENTS head
	yieldFinished                     // b: a finished plan's line, folded into +N finished
	yieldBlank                        // c: the blank lines between sections
	yieldQueued                       // d: queued, waiting-on and review task lines
	yieldIdle                         // e: idle agents, counted in +N idle · M off (brigada capacity)
	yieldHeld                         // f: held and retry lines, parked task lines
	yieldRunning                      // g: running lines
	yieldRest                         // h: every other line outside the header and NEEDS YOU
)

// The sections whose head goes with their last line.
const (
	topSecNone = iota
	topSecForeman
	topSecJobs
	topSecPlans
	topSecAgents
	topSecEvents
)

// topFold marks a generated line: the dim count of what dropped.
type topFold int

const (
	foldNone     topFold = iota
	foldFinished         // +N finished
	foldIdle             // +N idle · M off (brigada capacity)
)

// topLine is one line of the frame with what the drop loop needs.
type topLine struct {
	text    string
	class   topYield
	section int  // the section whose head goes with its last line, or topSecNone
	head    bool // the section's head line
	fold    topFold
	count   int  // what a fold line counts besides what drops: the off agents
	late    bool // drops after the other lines of its class
	dropped bool
}

// topPinned wraps lines no class drops.
func topPinned(texts []string) []topLine {
	lines := make([]topLine, len(texts))
	for i, text := range texts {
		lines[i] = topLine{text: text}
	}
	return lines
}

// topFrame is the view's lines with the drops so far. hidden is the
// finished plans the status already hid; folded counts what the drops
// added to the two fold lines.
type topFrame struct {
	lines          []topLine
	hidden         int
	foldedFinished int
	foldedIdle     int
	style          term.Style
}

// visible says whether line i renders: not dropped, and a fold line only
// once it counts something.
func (f *topFrame) visible(i int) bool {
	l := f.lines[i]
	if l.dropped {
		return false
	}
	switch l.fold {
	case foldFinished:
		return f.hidden+f.foldedFinished > 0
	case foldIdle:
		return f.foldedIdle+l.count > 0
	}
	return true
}

func (f *topFrame) size() int {
	n := 0
	for i := range f.lines {
		if f.visible(i) {
			n++
		}
	}
	return n
}

// texts is the visible lines' text, the fold lines worded from their counts.
func (f *topFrame) texts() []string {
	var out []string
	for i, l := range f.lines {
		if !f.visible(i) {
			continue
		}
		switch l.fold {
		case foldFinished:
			out = append(out, f.style.Dim(fmt.Sprintf("+%d finished", f.hidden+f.foldedFinished)))
		case foldIdle:
			var parts []string
			if f.foldedIdle > 0 {
				parts = append(parts, fmt.Sprintf("+%d idle", f.foldedIdle))
			}
			if l.count > 0 {
				parts = append(parts, fmt.Sprintf("%d off", l.count))
			}
			out = append(out, f.style.Dim(strings.Join(parts, topEventSep(f.style))+" (brigada capacity)"))
		default:
			out = append(out, l.text)
		}
	}
	return out
}

// drop removes line i and counts it into its fold line.
func (f *topFrame) drop(i int) {
	f.lines[i].dropped = true
	switch f.lines[i].class {
	case yieldFinished:
		f.foldedFinished++
	case yieldIdle:
		f.foldedIdle++
	}
}

// dropEmptyHead drops a section's head once no other line of the section
// is left.
func (f *topFrame) dropEmptyHead(section int) {
	if section == topSecNone {
		return
	}
	head := -1
	for j, l := range f.lines {
		if l.section != section {
			continue
		}
		if l.head {
			head = j
		} else if f.visible(j) {
			return
		}
	}
	if head >= 0 {
		f.lines[head].dropped = true
	}
}

// fit drops lines until the frame has at most height lines, class by
// class and from the bottom upward in each; height 0 is no limit. Lines
// that are late in their class go after the rest of it. A section's head
// goes right after its last line, when the frame is still too tall. What
// the classes leave over is the hard clip, which the caller applies.
func (f *topFrame) fit(height int) {
	if height <= 0 {
		return
	}
	for class := yieldEventsExtra; class <= yieldRest; class++ {
		for _, late := range []bool{false, true} {
			for i := len(f.lines) - 1; i >= 0 && f.size() > height; i-- {
				l := f.lines[i]
				if l.class == class && l.late == late && f.visible(i) {
					f.drop(i)
					if f.size() > height {
						f.dropEmptyHead(l.section)
					}
				}
			}
		}
	}
}

// topFit cuts one styled line to width: a line that fits keeps its
// colour; one that overflows is cut on its plain text, so no line wraps.
func topFit(line string, width int) string {
	if term.VisibleWidth(line) <= width {
		return line
	}
	return cutLine(term.StripEscapes(line), width)
}

// topHeader is the first line of the view: the scope, when the fetch
// found one, the engine's uptime and the clock, then a dim rule filling
// the width that remains and the keys at the right end. The keys show
// only when they fit whole — left,
// two spaces and the keys within the width; a key is never cut
// mid-word — and otherwise the rule fills what remains of the width. A
// failed fetch adds its error as a second line, so the view keeps its
// shape while the loop retries.
func topHeader(model topModel, now time.Time, style term.Style, width int) []string {
	st := model.status
	sep := topEventSep(style)
	uptime := "0s"
	if started := parseStartedAt(st.StartedAt); !started.IsZero() {
		uptime = topUptime(now.Sub(started))
	}
	keys := "q quit" + sep + "p pause"
	if model.paused {
		keys = "paused; p resumes, q quits"
	}
	left := "brigada"
	if model.scope != "" {
		left += sep + model.scope
	}
	left += fmt.Sprintf("%sup %s%s%s", sep, uptime, sep, now.Format("15:04:05"))
	var line string
	if rule := width - term.VisibleWidth(left) - term.VisibleWidth(keys) - 2; rule >= 1 {
		line = left + " " + style.Dim(strings.Repeat(style.Dash(), rule)) + " " + keys
	} else {
		line = left + " " + style.Dim(strings.Repeat(style.Dash(), max(width-term.VisibleWidth(left)-1, 1)))
	}
	lines := []string{topFit(line, width)}
	if model.err != nil {
		lines = append(lines, style.Bad(cutLine("engine: "+model.err.Error(), width)))
	}
	if model.hint != "" {
		lines = append(lines, style.Dim(cutLine(model.hint, width)))
	}
	return lines
}

// topUptime is the engine's uptime as the header shows it: whole seconds
// under a minute (`45s`), whole minutes under an hour (`33m`), else hours
// and minutes (`11h33m`, `2h0m`), each truncated. The seconds stay in the
// header's clock.
func topUptime(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(max(d, 0)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// topAgents is the AGENTS section: a rule-filled head, one line per
// agent — `harness/model` padded to the widest name, the running count
// against the effective cap as `brigada capacity` folds it, then the
// quota cell — and a dim `all ≤ global N` line when the snapshot carries
// a global cap. A broken agent's reason comes from its agent needs-you
// entry, which topFetch keeps in a project's status too, "broken" when
// the status has none.
// The text comes from topAgents; topAgentLines carries each line's
// yield class, the idle agents' (no running job, quota neither
// cooling nor broken) being the ones the height drops first. Off agents
// (an effective cap of 0, nothing running, quota ready) never list. The
// rest go broken and cooling first, then running, then idle. A dim
// `+N idle · M off (brigada capacity)` line, either part left out when
// zero, counts what dropped and what is off, above the global line.
func topAgents(model topModel, now time.Time, style term.Style, width int) []string {
	frame := &topFrame{lines: topAgentLines(model, now, style, width), style: style}
	return frame.texts()
}

func topAgentLines(model topModel, now time.Time, style term.Style, width int) []topLine {
	line := func(text string, class topYield) topLine {
		return topLine{text: text, class: class, section: topSecAgents}
	}
	head := line(topSectionHead("AGENTS", style, width), yieldRest)
	head.head = true
	snap := model.capacity
	g := api.GlobalCap(snap)
	var global []topLine
	if g != nil {
		global = []topLine{line(style.Dim(fmt.Sprintf("all %s global %d", style.AtMost(), *g)), yieldRest)}
	}
	if len(snap.Agents) == 0 {
		return append([]topLine{head, line(style.Dim("no agents advertised"), yieldRest)}, global...)
	}
	reasons := make(map[string]string)
	for _, n := range model.status.NeedsYou {
		if n.Kind == "agent" {
			reasons[n.Title] = n.Reason
		}
	}
	// An agent whose effective cap is 0 with nothing running and a ready
	// quota is off: it reads as ready but takes no job, so it folds out of
	// the list for good and only the fold line counts it. The rest go
	// broken and cooling first, then running, then idle, stable inside a
	// group.
	var listed []engine.CapacityAgent
	off := 0
	for _, a := range snap.Agents {
		if topAgentOff(a, g) {
			off++
			continue
		}
		listed = append(listed, a)
	}
	group := func(a engine.CapacityAgent) int {
		switch {
		case a.Quota == "broken" || a.Quota == "cooling":
			return 0
		case a.Running > 0:
			return 1
		}
		return 2
	}
	sort.SliceStable(listed, func(i, j int) bool { return group(listed[i]) < group(listed[j]) })
	names := make([]string, len(listed))
	capCells := make([]string, len(listed))
	nameW, capW := 0, 0
	for i, a := range listed {
		names[i] = a.Harness + "/" + a.Model
		capCells[i] = api.RunningCap(a, g)
		nameW = max(nameW, utf8.RuneCountInString(names[i]))
		capW = max(capW, utf8.RuneCountInString(capCells[i]))
	}
	nameW = min(nameW, width)
	agents := make([]topLine, len(listed))
	for i, a := range listed {
		name := cutLine(names[i], nameW)
		prefix := name + strings.Repeat(" ", nameW-utf8.RuneCountInString(name)+2) +
			capCells[i] + strings.Repeat(" ", capW-utf8.RuneCountInString(capCells[i])+2)
		var quota string
		switch a.Quota {
		case "broken":
			reason := reasons[names[i]]
			if reason == "" {
				reason = "broken"
			}
			if room := width - utf8.RuneCountInString(prefix) - 2; room >= 10 {
				reason = cutLine(reason, room)
			}
			quota = style.Bad(style.Cross() + " " + reason)
		case "cooling":
			quota = style.Warn(style.Again() + " " + api.LocalClock(a.CoolingUntil, now))
		default:
			quota = style.OK(style.Check())
		}
		class := yieldRest
		if a.Running == 0 && a.Quota != "broken" && a.Quota != "cooling" {
			class = yieldIdle
		}
		agents[i] = line(prefix+quota, class)
	}
	lines := append([]topLine{head}, agents...)
	lines = append(lines, topLine{class: yieldRest, section: topSecAgents, fold: foldIdle, count: off})
	return append(lines, global...)
}

// topAgentOff says an agent is off: an effective cap of 0 (its own, or the
// global cap's), no running job and a ready quota.
func topAgentOff(a engine.CapacityAgent, global *int) bool {
	limit := a.Cap
	if global != nil && (limit == nil || *global < *limit) {
		limit = global
	}
	return limit != nil && *limit == 0 && a.Running == 0 && a.Quota == "ready"
}

// topSlots reads the global cap layer's use and cap from the capacity
// snapshot: the meters strip's slots cell. ok is false when no global
// layer came back, as with a failed capacity fetch; limit is nil when
// the layer is uncapped.
func topSlots(snap engine.CapacitySnapshot) (use int, limit *int, ok bool) {
	for _, l := range snap.Layers {
		if l.Layer == caps.LayerGlobal {
			return l.Use, l.Cap, true
		}
	}
	return 0, nil, false
}

// topMeters is the strip under the header: the global layer's slots
// against their cap, then the run, queue, held and needs counters, the
// parts three spaces apart. The slots cell has no bar when the layer is
// uncapped or capped at zero — no all-empty bar — and reads `?/?` when
// the capacity fetch failed.
func topMeters(model topModel, style term.Style, width int) []string {
	st := model.status
	cell := "SLOTS ?/?"
	if use, limit, ok := topSlots(model.capacity); ok {
		useLimit := fmt.Sprintf("%d/none", use)
		bar := ""
		if limit != nil {
			useLimit = fmt.Sprintf("%d/%d", use, *limit)
			if *limit >= 1 {
				bar = topSlotBar(use, *limit, style) + " "
			}
		}
		cell = "SLOTS " + bar + useLimit
	}
	// A counter is coloured only above zero: nothing held or needed reads plain.
	counter := func(n int, label string, colour func(string) string) string {
		text := fmt.Sprintf("%d %s", n, label)
		if n == 0 {
			return text
		}
		return colour(text)
	}
	parts := []string{
		cell,
		style.Bold(fmt.Sprintf("%d run", len(st.Running))),
		fmt.Sprintf("%d queue", st.Jobs["queued"]),
		counter(len(st.Held), "held", style.Warn),
		counter(len(st.NeedsYou), "needs", style.Bad),
	}
	return []string{topFit(strings.Join(parts, "   "), width)}
}

// topSlotBar is the meters' slots bar: the global use against its cap
// as at most ten cells — ▓ filled and ░ empty in proportion, at least
// one ▓ whenever the use is over zero, so a sliver of use always shows.
func topSlotBar(use, limit int, style term.Style) string {
	cells := min(limit, 10)
	filled := use * cells / limit
	if use > 0 && filled == 0 {
		filled = 1
	}
	filled = min(filled, cells)
	return strings.Repeat(style.Filled(), filled) + strings.Repeat(style.Empty(), cells-filled)
}

// topDescribeJob is status's own words for a job, its ` · ` joints in the
// style's separator so the plain ASCII style has no Unicode of its own.
func topDescribeJob(j engine.StatusJob, now time.Time, style term.Style) string {
	text := api.DescribeJob(j, now)
	if sep := topEventSep(style); sep != " · " {
		text = strings.ReplaceAll(text, " · ", sep)
	}
	return text
}

// topPlanFinished says a plan's State has ended it: done or cancelled.
func topPlanFinished(p engine.StatusPlan) bool {
	return p.State == "done" || p.State == "cancelled"
}

// topTaskSettled says a task's state leaves it no live work: done,
// cancelled or retired. Its plan's done/total already counts it, so the
// tree gives it no line of its own — except a done task that has not
// landed, which gets its ⇡ line (topLandingLine).
func topTaskSettled(state string) bool {
	switch state {
	case "done", "cancelled", "retired":
		return true
	}
	return false
}

// topClaims maps a job to the live plan whose tree line it renders
// under: a job of a live plan's task, or a live plan's review job
// (StatusPlan.ReviewJob). A taskless job no live plan names as its
// review is plan-less; a finished plan never carries a live job.
type topClaims struct {
	taskPlan   map[int64]int64 // task id → live plan id
	reviewPlan map[int64]int64 // review job id → live plan id
}

func newTopClaims(st api.Status) topClaims {
	c := topClaims{taskPlan: map[int64]int64{}, reviewPlan: map[int64]int64{}}
	for _, p := range st.Plans {
		if topPlanFinished(p) {
			continue
		}
		for _, t := range p.Tasks {
			c.taskPlan[t.ID] = p.ID
		}
		if p.ReviewJob != 0 {
			c.reviewPlan[p.ReviewJob] = p.ID
		}
	}
	return c
}

// plan is the live plan that claims a job, by its task or as the plan's
// review job; ok is false for a plan-less job.
func (c topClaims) plan(jobID, taskID int64) (int64, bool) {
	if taskID != 0 {
		id, ok := c.taskPlan[taskID]
		return id, ok
	}
	id, ok := c.reviewPlan[jobID]
	return id, ok
}

// topRunningLine is one running job under its task, its plan or in the
// FOREMAN or JOBS block: the Accent pointer and status's own words for the job.
func topRunningLine(j engine.StatusJob, now time.Time, style term.Style, width int) string {
	return "  " + style.Accent(style.Pointer()) + " " + cutLine(topDescribeJob(j, now, style), width-4)
}

// topHeldLine is one held job, in Warn: the task it is for, or the job
// when it has none, its kind and status's own words for the wait.
func topHeldLine(h engine.HeldJob, style term.Style, width int) string {
	what := fmt.Sprintf("job #%d", h.ID)
	if h.TaskID != 0 {
		what = fmt.Sprintf("task #%d", h.TaskID)
	}
	return "  " + style.Warn(cutLine(fmt.Sprintf("? %s %s held: %s", what, h.Kind, api.HeldWait(h)), width-2))
}

// topRetryLine is one job waiting on an infra retry, Dim, in status's
// own words.
func topRetryLine(r engine.InfraRetry, now time.Time, style term.Style, width int) string {
	return "  " + style.Dim(cutLine(style.Again()+" "+api.RetryLine(r, now), width-2))
}

// topPlanless is the two blocks under NEEDS YOU: the running, held and
// retrying jobs no live plan claims, so no job goes unseen. A foreman
// turn goes under a FOREMAN head; any other job — the review of a plan
// the tree does not list — under a JOBS head. Inside a block the order
// is running, held, retrying. A block is nil, and renderTop leaves it
// out, when it has no job. The running lines yield in class g, the held
// and retry lines in class f, and a block's head goes with its last line.
func topPlanless(st api.Status, now time.Time, style term.Style, width int) (foreman, jobs []topLine) {
	claims := newTopClaims(st)
	var fore, other []topLine
	add := func(kind, text string, class topYield) {
		if kind == string(engine.KindForeman) {
			fore = append(fore, topLine{text: text, class: class, section: topSecForeman})
		} else {
			other = append(other, topLine{text: text, class: class, section: topSecJobs})
		}
	}
	for _, j := range st.Running {
		if _, ok := claims.plan(j.ID, j.TaskID); !ok {
			add(j.Kind, topRunningLine(j, now, style, width), yieldRunning)
		}
	}
	for _, h := range st.Held {
		if _, ok := claims.plan(h.ID, h.TaskID); !ok {
			add(h.Kind, topHeldLine(h, style, width), yieldHeld)
		}
	}
	for _, r := range st.Retries {
		if _, ok := claims.plan(r.ID, r.TaskID); !ok {
			add(r.Kind, topRetryLine(r, now, style, width), yieldHeld)
		}
	}
	// Dim notes the status carries that no job line speaks for: the user's
	// messages and the slot holders in the FOREMAN block, a pending runner
	// restart in the JOBS block. They yield with the held and retry lines.
	note := func(text string, section int) topLine {
		return topLine{text: "  " + style.Dim(cutLine(text, width-2)), class: yieldHeld, section: section}
	}
	switch n := st.ForemanMessages; {
	case n == 1:
		fore = append(fore, note("1 message for the foreman", topSecForeman))
	case n > 1:
		fore = append(fore, note(fmt.Sprintf("%d messages for the foreman", n), topSecForeman))
	}
	running := make(map[int64]bool, len(st.Running))
	for _, j := range st.Running {
		running[j.ID] = true
	}
	for _, sl := range st.Slots {
		// A headless turn's own job line already speaks for its slot.
		if sl.State == engine.SlotFree || (sl.State == engine.SlotTurn && running[sl.HolderID]) {
			continue
		}
		text := "foreman slot: " + string(sl.State)
		if sl.Label != "" {
			text += " (" + sl.Label + ")"
		}
		if sl.Since != "" {
			text += " since " + api.LocalClock(sl.Since, now)
		}
		fore = append(fore, note(text, topSecForeman))
	}
	if st.RunnerDraining && len(st.Running) > 0 {
		noun := "jobs"
		if len(st.Running) == 1 {
			noun = "job"
		}
		other = append(other, note(fmt.Sprintf("runner restarting after %d running %s", len(st.Running), noun), topSecJobs))
	}
	if len(fore) > 0 {
		foreman = append([]topLine{{text: topSectionHead("FOREMAN", style, width), class: yieldRest, section: topSecForeman, head: true}}, fore...)
	}
	if len(other) > 0 {
		jobs = append([]topLine{{text: topSectionHead("JOBS", style, width), class: yieldRest, section: topSecJobs, head: true}}, other...)
	}
	return foreman, jobs
}

// topPlansTree is the PLANS section: every plan the status lists, each
// live one with its live work beneath it — the running jobs first, then
// a line per task still waiting, held, retrying, parked or reviewing —
// and each finished one a single line. A Dim "+N finished" line counts
// the finished plans the status hid.
func topPlansTree(st api.Status, now time.Time, style term.Style, width int) []string {
	frame := &topFrame{lines: newTopPlans(st, false, false, now, style, width), hidden: st.HiddenFinishedPlans, style: style}
	return frame.texts()
}

// newTopPlans renders the PLANS section with each line's yield class: a
// finished plan's line folds into the +N finished line (b), a queued,
// waiting-on or review task line is class d, a held, retry or parked
// line f (a done task not landed too, as its ⇡ line), a running line g, a
// reviewing plan's `plan review queued` line d, and a live plan's own line, the none
// placeholder, the +N finished line and the head are class h. With tagged
// each plan's own line names its repo.
func newTopPlans(st api.Status, tagged, fetched bool, now time.Time, style term.Style, width int) []topLine {
	line := func(text string, class topYield) topLine {
		return topLine{text: text, class: class, section: topSecPlans}
	}
	head := line(topSectionHead("PLANS", style, width), yieldRest)
	head.head = true
	lines := []topLine{head}
	if len(st.Plans) == 0 {
		// A status that came back with no plans, hidden or listed, is a
		// first run; one that did not come back has said nothing.
		text := "none"
		if fetched && st.HiddenFinishedPlans == 0 {
			text = `no plans yet: brigada ask "fix #12"`
		}
		lines = append(lines, line(style.Dim(text), yieldRest))
	}
	claims := newTopClaims(st)
	reasons := make(map[int64]string)
	for _, n := range append(append([]engine.NeedsYouItem{}, st.NeedsYou...), st.ForemanHolds...) {
		if n.Kind == "task" {
			reasons[n.ID] = n.Reason
		}
	}
	layout := newTopPlanLayout(st.Plans, tagged, style, width)
	for _, p := range st.Plans {
		tag := ""
		if tagged {
			tag = topRepoName(p.RepoURL)
		}
		if topPlanFinished(p) {
			lines = append(lines, line(topPlanLine(p, tag, layout, style, width), yieldFinished))
			continue
		}
		lines = append(lines, line(topPlanLine(p, tag, layout, style, width), yieldRest))
		task := func(text string, class topYield) {
			lines = append(lines, line(text, class))
		}
		titles := make(map[int64]string, len(p.Tasks))
		for _, t := range p.Tasks {
			titles[t.ID] = t.Title
		}
		// A plan that is not active shows its state word on its own line,
		// so its waiting tasks get no queued line of theirs.
		quiet := p.State != "active" && p.State != ""
		// Which tasks a job already speaks for: a task with a running,
		// held or retrying job shows as that job's line, not its state's.
		busy := make(map[int64]bool)
		reviewJob := false // a job claimed for the plan's own review shows
		for _, j := range st.Running {
			if id, ok := claims.plan(j.ID, j.TaskID); ok && id == p.ID {
				text := topRunningLine(j, now, style, width)
				if title := titles[j.TaskID]; j.TaskID != 0 && title != "" {
					text = topPlanRunningLine(j, title, now, style, width)
				}
				task(text, yieldRunning)
				busy[j.TaskID] = true
				reviewJob = reviewJob || j.TaskID == 0
			}
		}
		for _, h := range st.Held {
			if id, ok := claims.plan(h.ID, h.TaskID); ok && id == p.ID && h.TaskID == 0 {
				task(topHeldLine(h, style, width), yieldHeld)
				reviewJob = true
			}
		}
		for _, r := range st.Retries {
			if id, ok := claims.plan(r.ID, r.TaskID); ok && id == p.ID && r.TaskID == 0 {
				task(topRetryLine(r, now, style, width), yieldHeld)
				reviewJob = true
			}
		}
		if p.State == "reviewing" && !reviewJob {
			task("  "+style.Dim(style.Dot()+" plan review queued"), yieldQueued)
		}
		for _, t := range p.Tasks {
			for _, h := range st.Held {
				if h.TaskID == t.ID {
					text := topHeldLine(h, style, width)
					if t.Title != "" {
						text = topPlanHeldLine(h, t.Title, style, width)
					}
					task(text, yieldHeld)
					busy[t.ID] = true
				}
			}
			for _, r := range st.Retries {
				if r.TaskID == t.ID {
					task(topRetryLine(r, now, style, width), yieldHeld)
					busy[t.ID] = true
				}
			}
			if busy[t.ID] {
				continue
			}
			if t.LandedLabel() == "not landed" {
				task(topLandingLine(t, now, style, width), yieldHeld)
				continue
			}
			if topTaskSettled(t.State) {
				continue
			}
			if quiet && topTaskWaits(t.State) {
				continue
			}
			if text := topTaskLine(t, reasons[t.ID], style, width); text != "" {
				class := yieldQueued
				switch t.State {
				case "needs_foreman", "needs_user", "failed":
					class = yieldHeld // a parked task
				}
				task(text, class)
			}
		}
	}
	// The fold line counts the finished plans the status hid and, once
	// the height drops any, those too; it shows only when it counts one.
	return append(lines, topLine{class: yieldRest, section: topSecPlans, fold: foldFinished})
}

// topPlanCounts is a plan's done and total task counts. The total leaves
// out cancelled and retired tasks. A live plan counts a done task only
// once it is landed or is of a class that never lands (LandedLabel empty),
// so its bar does not read complete before the landing is; a finished plan
// counts every done task.
func topPlanCounts(p engine.StatusPlan) (done, total int) {
	live := !topPlanFinished(p)
	for _, t := range p.Tasks {
		switch t.State {
		case "cancelled", "retired":
			continue
		case "done":
			if !live || t.Landed || t.LandedLabel() == "" {
				done++
			}
		}
		total++
	}
	return done, total
}

// topPlanTail is what ends a plan's line: done/total and the bar for an
// active plan, `done/total ✓` for a done one, `✗` for a cancelled one,
// and a state word for any other — `awaiting approval`, `reviewing round
// N` (the plan review's round, as status words it) or `needs foreman`.
func topPlanTail(p engine.StatusPlan, style term.Style) string {
	done, total := topPlanCounts(p)
	count := fmt.Sprintf("%d/%d", done, total)
	switch p.State {
	case "done":
		return count + " " + style.OK(style.Check())
	case "cancelled":
		return style.Bad(style.Cross())
	case "", "active":
		return count + " " + topPlanBar(done, total, style)
	case "pending_approval":
		return style.Warn("awaiting approval")
	case "reviewing":
		return style.Dim(fmt.Sprintf("reviewing round %d", p.ReviewRound))
	case "needs_foreman":
		return style.Warn("needs foreman")
	}
	return style.Dim(strings.ReplaceAll(p.State, "_", " "))
}

// topPlanLayout is the columns every plan line of the list shares, so the
// tails start in one column: the widest id, the widest tag (0 when no line
// names its repo) and the widest title, the title capped so the widest
// tail still fits.
type topPlanLayout struct {
	id, tag, title, tail int
}

func newTopPlanLayout(plans []engine.StatusPlan, tagged bool, style term.Style, width int) topPlanLayout {
	var l topPlanLayout
	for _, p := range plans {
		l.id = max(l.id, len(fmt.Sprintf("#%d", p.ID)))
		if tagged {
			l.tag = max(l.tag, utf8.RuneCountInString(topRepoName(p.RepoURL)))
		}
		l.title = max(l.title, utf8.RuneCountInString(p.Title))
		l.tail = max(l.tail, term.VisibleWidth(topPlanTail(p, style)))
	}
	fixed := l.id + 1 + l.tail + 1
	if l.tag > 0 {
		fixed += l.tag + 2
	}
	l.title = min(l.title, max(width-fixed, 4))
	return l
}

// topPlanLine is a plan's own line: the id, the tag when the layout has
// tags, the title and the tail (topPlanTail), each padded to the layout's
// column. A finished plan's mark comes from its State; a live plan's id is
// Bold, and it never carries a mark, even with every task done. The tag
// sits Dim after the id and the title gives way to it.
func topPlanLine(p engine.StatusPlan, tag string, l topPlanLayout, style term.Style, width int) string {
	id := fmt.Sprintf("#%d", p.ID)
	pad := strings.Repeat(" ", l.id-len(id))
	if !topPlanFinished(p) {
		id = style.Bold(id)
	}
	out := id + pad + " "
	if l.tag > 0 {
		out += style.Dim(tag) + strings.Repeat(" ", l.tag-utf8.RuneCountInString(tag)) + "  "
	}
	title := cutLine(p.Title, l.title)
	out += title + strings.Repeat(" ", max(l.title-utf8.RuneCountInString(title), 0)) + " "
	return out + topPlanTail(p, style)
}

// topTaskWaits says a live task's state is plain waiting: not parked and
// not reviewing, so only its queued or waiting-on line speaks for it.
func topTaskWaits(state string) bool {
	switch state {
	case "needs_foreman", "needs_user", "failed", "reviewing":
		return false
	}
	return true
}

// topLandingLine is a done task that has not landed and that no job speaks
// for, Dim: `⇡ #330 title  landing: ...`, with what the landing waits on —
// `queued` for a land job in the queue, the land_ready gate's own words
// with the stored retry stamp as a local clock, or `not landed` when the
// task carries no word.
func topLandingLine(t engine.StatusTask, now time.Time, style term.Style, width int) string {
	what := "not landed"
	switch t.LandReady {
	case "":
	case "ready":
		what = "queued"
	default:
		what = topLandReadyWords(t.LandReady, now)
	}
	subject := fmt.Sprintf("#%d", t.ID)
	if t.Title != "" {
		subject += " " + t.Title
	}
	return "  " + style.Dim(cutLine(style.Up()+" "+subject+"  landing: "+what, width-2))
}

// topLandReadyWords shows the stamp in `waiting (retry at <stamp>, last
// exit N)` as a local clock; any other words stay as they are.
func topLandReadyWords(ready string, now time.Time) string {
	const marker = "retry at "
	i := strings.Index(ready, marker)
	if i < 0 {
		return ready
	}
	rest := ready[i+len(marker):]
	j := strings.IndexAny(rest, ",)")
	if j < 0 {
		return ready
	}
	return ready[:i+len(marker)] + api.LocalClock(rest[:j], now) + rest[j:]
}

// topTitled is `ref title rest` cut to width, the title giving way and
// nothing else: the title is whole when fewer than 4 columns remain for
// it, and only then does the ordinary width cut apply.
func topTitled(ref, title, rest string, width int) string {
	room := width - utf8.RuneCountInString(ref) - 1 - utf8.RuneCountInString(rest)
	switch {
	case room >= utf8.RuneCountInString(title):
	case room >= 4:
		title = cutLine(title, room)
	default:
		return cutLine(ref+" "+title+rest, width)
	}
	return ref + " " + title + rest
}

// topPlanRunningLine is a running job under its plan, with its task's
// title: `▸ #331 title · work · claude/sonnet@high · 3m12s · Edit`, the
// tail status's own words for the job minus the leading `task #N `.
func topPlanRunningLine(j engine.StatusJob, title string, now time.Time, style term.Style, width int) string {
	tail := strings.TrimPrefix(topDescribeJob(j, now, style), fmt.Sprintf("task #%d ", j.TaskID))
	return "  " + style.Accent(style.Pointer()) + " " + topTitled(fmt.Sprintf("#%d", j.TaskID), title, topEventSep(style)+tail, width-4)
}

// topPlanHeldLine is a held job under its plan, with its task's title:
// `? #333 title  held: waiting for a free account slot`.
func topPlanHeldLine(h engine.HeldJob, title string, style term.Style, width int) string {
	return "  " + style.Warn("? "+topTitled(fmt.Sprintf("#%d", h.TaskID), title, "  held: "+api.HeldWait(h), width-4))
}

// topPlanBar is a live plan's progress: eleven cells, ▓ filled in OK
// and ░ empty in Dim, in proportion to the done tasks, at least one ▓
// once any is done.
func topPlanBar(done, total int, style term.Style) string {
	const cells = 11
	filled := 0
	if total > 0 {
		filled = done * cells / total
	}
	if done > 0 && filled == 0 {
		filled = 1
	}
	filled = min(filled, cells)
	var bar string
	if filled > 0 {
		bar = style.OK(strings.Repeat(style.Filled(), filled))
	}
	if filled < cells {
		bar += style.Dim(strings.Repeat(style.Empty(), cells-filled))
	}
	return bar
}

// topTaskLine is a live task's line when no job speaks for it: Dim
// queued — or waiting on the dependencies it names — for a task not
// yet leased, Bad with the needs-you reason for a parked one, Dim
// "(review)" for a task reviewing with no job running.
func topTaskLine(t engine.StatusTask, reason string, style term.Style, width int) string {
	subject := fmt.Sprintf("#%d %s", t.ID, t.Title)
	switch t.State {
	case "needs_foreman", "needs_user", "failed":
		if reason == "" {
			reason = t.State
		}
		return "  " + style.Bad(cutLine("! "+subject+" "+reason, width-2))
	case "reviewing":
		return "  " + style.Dim(cutLine(style.Dot()+" "+subject+" (review)", width-2))
	}
	wait := "(queued)"
	if len(t.WaitingOn) > 0 {
		wait = "(waiting on " + strings.Join(t.WaitingOn, ", ") + ")"
	}
	return "  " + style.Dim(cutLine(style.Dot()+" "+subject+" "+wait, width-2))
}

// topSectionHead is a section's head line: the Bold title and a Dim
// rule filling the width. Below 60 columns the rule is dropped and the
// title stands alone — the width is too narrow to spend on fills —
// while the section's lines beneath keep their indentation.
func topSectionHead(title string, style term.Style, width int) string {
	if width < 60 {
		return style.Bold(title)
	}
	rule := width - term.VisibleWidth(title) - 1
	if rule < 1 {
		rule = 1
	}
	return style.Bold(title) + " " + style.Dim(strings.Repeat(style.Dash(), rule))
}

// topNeedsYou is the needs-you queue at the top of the view, under the
// meters and before every other section: the header line carries a rule
// fill to the width, then one Bad line per entry, cut to width and
// capped at three entry lines with one dim "+N more" line under them. The
// cap never hides a broken agent while a line remains (topNeedsYouShown).
// The section is always visible: an empty queue keeps the header and
// renders one dim line under it.
func topNeedsYou(st api.Status, sole bool, style term.Style, width int) []string {
	lines := []string{topSectionHead("NEEDS YOU", style, width)}
	if len(st.NeedsYou) == 0 {
		return append(lines, style.Dim("nothing needs you"))
	}
	shown := topNeedsYouShown(st.NeedsYou)
	// An entry with an action keeps it whole at the end of its line and the
	// reason is cut instead; the tails start in one column, after the widest
	// reason that leaves the widest tail room.
	texts := make([]string, len(shown))
	tails := make([]string, len(shown))
	col, tailW := 0, 0
	for i, n := range shown {
		texts[i], tails[i] = topNeedsYouLine(n), topNeedsYouTail(n, st, sole, style)
		if tails[i] != "" {
			col = max(col, utf8.RuneCountInString(texts[i]))
			tailW = max(tailW, utf8.RuneCountInString(tails[i]))
		}
	}
	col = max(min(col, width-tailW-2), 4)
	for i := range shown {
		text := cutLine(texts[i], width)
		if tails[i] != "" {
			left := cutLine(texts[i], col)
			text = left + strings.Repeat(" ", max(col-utf8.RuneCountInString(left), 0)) + "  " + tails[i]
		}
		lines = append(lines, style.Bad(text))
	}
	if more := len(st.NeedsYou) - len(shown); more > 0 {
		lines = append(lines, style.Dim(fmt.Sprintf("+%d more (brigada status)", more)))
	}
	return lines
}

// topNeedsYouTail is the command that answers an entry, `→ brigada ...`,
// or "" when the entry has none. NeedsYouItem.Kind is coarse — land
// holds, push waits, local landings and stranded tasks are all `task` —
// so a task entry is told apart by its State and its Reason's prefix, as
// the engine words them. A plan waiting for approval is approved; one
// parked at needs_foreman is the foreman's. `brigada push approve` takes
// the working directory's origin, which is the view's project only when it
// is unscoped with one project; otherwise the tail names the entry's plan's
// repository, and an entry whose plan is not listed has no tail.
func topNeedsYouTail(n engine.NeedsYouItem, st api.Status, sole bool, style term.Style) string {
	arrow := style.Arrow() + " "
	switch n.Kind {
	case "plan":
		if n.State == "pending_approval" {
			return fmt.Sprintf("%sbrigada plan approve %d", arrow, n.ID)
		}
	case "agent":
		if n.State == "broken" && n.Title != "" {
			return arrow + "brigada agent reset " + n.Title
		}
	case "task":
		switch {
		case n.State == "done" && strings.HasPrefix(n.Reason, "held by rule"):
			return fmt.Sprintf("%sbrigada land approve %d", arrow, n.ID)
		case strings.HasPrefix(n.Reason, "waits to push:"):
			if sole {
				return arrow + "brigada push approve"
			}
			for _, p := range st.Plans {
				if n.PlanID != 0 && p.ID == n.PlanID && p.RepoURL != "" {
					return arrow + "brigada push approve --repo " + p.RepoURL
				}
			}
		}
	}
	return ""
}

// topNeedsYouShown chooses the three entries a longer queue shows: the
// agent entries first, in queue order, the others filling the slots left.
// The chosen entries keep their queue order.
func topNeedsYouShown(queue []engine.NeedsYouItem) []engine.NeedsYouItem {
	const limit = 3
	if len(queue) <= limit {
		return queue
	}
	agents := 0
	for _, n := range queue {
		if n.Kind == "agent" {
			agents++
		}
	}
	others := max(limit-agents, 0)
	shown := make([]engine.NeedsYouItem, 0, limit)
	for _, n := range queue {
		switch {
		case len(shown) == limit:
		case n.Kind == "agent":
			shown = append(shown, n)
		case others > 0:
			shown = append(shown, n)
			others--
		}
	}
	return shown
}

// topNeedsYouLine is one needs-you entry as its compact line: the
// entry's own fields decide the subject — a task or plan its id, an
// agent its harness/model name (the name `brigada agent reset` takes),
// a foreman or project entry its Title — then the reason, or the state
// when the entry carries none.
func topNeedsYouLine(n engine.NeedsYouItem) string {
	subject := fmt.Sprintf("#%d", n.ID)
	switch n.Kind {
	case "agent", "foreman", "project":
		subject = n.Title
	}
	reason := n.Reason
	if n.Kind == "task" && n.State == "done" && strings.HasPrefix(reason, "held by rule") {
		// The reason embeds the commands that answer the hold; the tail
		// shows the one, so the line cuts the reason before them.
		reason, _, _ = strings.Cut(reason, "; release:")
	}
	if reason == "" && n.Kind == "plan" {
		// A plan entry with no reason says what the plan waits for, not
		// the raw state word.
		switch n.State {
		case "pending_approval":
			reason = "awaiting approval"
		case "needs_foreman":
			reason = "needs foreman"
		}
		if reason != "" && n.Title != "" {
			subject += " " + n.Title
		}
	}
	if reason == "" {
		reason = n.State
	}
	return "! " + subject + " " + reason
}

// topEventRows is how many event rows the EVENTS section shows at most.
const topEventRows = 10

// topShows says whether top shows an item: an event the allowlist words,
// except the foreman's report, which the report line above EVENTS says.
func topShows(it history.Item) bool {
	if it.Kind == "foreman_report" {
		return false
	}
	_, ok := topEventWords(it, time.Time{})
	return ok
}

// topReportLine is the foreman's last report above the EVENTS head, Dim:
// `report #<job>: <first line of the text>`, cut to width. Only a status
// with a report has it; it yields in the second event row's class, right
// after that row, since the report can say what NEEDS YOU does not.
func topReportLine(st api.Status, style term.Style, width int) []topLine {
	if st.Report == nil {
		return nil
	}
	first, _, _ := strings.Cut(strings.TrimSpace(st.Report.Text), "\n")
	text := fmt.Sprintf("report #%d: %s", st.Report.JobID, strings.TrimSpace(first))
	return []topLine{{text: style.Dim(cutLine(text, width)), class: yieldEventsSecond}}
}

// topEventLines is the EVENTS section: its head, then the newest kept
// items, newest first, one per row, at most topEventRows. A row is the
// time, two spaces and the event's words, the time column padded to the
// widest time of the rows built so the words start in one column; each
// row is cut to width. With no items the section is the head and a dim
// `no events`. Row 1 and the head yield last (yieldRest), row 2 in
// yieldEventsSecond and rows 3 and up in yieldEventsExtra, the first
// class to go; the head goes with the section's last row.
func topEventLines(items []history.Item, now time.Time, style term.Style, width int) []topLine {
	head := topLine{text: topSectionHead("EVENTS", style, width), class: yieldRest, section: topSecEvents, head: true}
	if len(items) == 0 {
		return []topLine{head, {text: style.Dim("no events"), class: yieldRest, section: topSecEvents}}
	}
	if len(items) > topEventRows {
		items = items[len(items)-topEventRows:]
	}
	clocks := make([]string, len(items))
	pad := 0
	for i := range items {
		clocks[i] = topClock(items[len(items)-1-i].CreatedAt, now)
		if n := term.VisibleWidth(clocks[i]); n > pad {
			pad = n
		}
	}
	lines := []topLine{head}
	for i, clock := range clocks {
		words, _ := topEventWords(items[len(items)-1-i], now)
		text := clock + strings.Repeat(" ", pad-term.VisibleWidth(clock)) + "  " + topColourStates(words, style)
		class := yieldRest
		switch {
		case i == 1:
			class = yieldEventsSecond
		case i > 1:
			class = yieldEventsExtra
		}
		lines = append(lines, topLine{text: topFit(text, width), class: class, section: topSecEvents})
	}
	return lines
}

// topEventSep joins the events section's items and the header's parts: the style's
// dot between spaces, three columns wide in either alphabet.
func topEventSep(style term.Style) string { return " " + style.Dot() + " " }

// topEventWords is an event in plain words, or false when top does not
// show its kind: history.EventWords holds the allowlist and the wording
// that `brigada history` shares. now reads the clock of an agent_cooling's
// deadline.
func topEventWords(it history.Item, now time.Time) (string, bool) {
	return history.EventWords(it, func(stamp string) string { return api.LocalClock(stamp, now) })
}

// topColourStates colours a line's state words: done and recovered in
// the OK style, failed and broken in the Bad style. A word stands
// alone or ends a field after its "=" or "_", as a status value or an
// event kind's last part does.
func topColourStates(line string, style term.Style) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		head, word := "", f
		if sep := strings.LastIndexAny(f, "=_"); sep >= 0 {
			head, word = f[:sep+1], f[sep+1:]
		}
		switch word {
		case "done", "recovered":
			fields[i] = head + style.OK(word)
		case "failed", "broken":
			fields[i] = head + style.Bad(word)
		}
	}
	return strings.Join(fields, " ")
}

// topClock is a stored stamp as a clock time in now's zone: the hour
// and minute on now's day, the date too on any other day. A stamp that
// does not parse shows as it is.
func topClock(stamp string, now time.Time) string {
	t, err := time.Parse(engine.TimeLayout, stamp)
	if err != nil {
		return stamp
	}
	t = t.In(now.Location())
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// cutLine cuts plain text to width runes, ending "..." when it had to
// cut and width leaves room for the mark. Styled text is never passed
// here: colour wraps the cut, never the other way.
func cutLine(text string, width int) string {
	if n := utf8.RuneCountInString(text); n > width && width > 3 {
		return string([]rune(text)[:width-3]) + "..."
	}
	return text
}

// The plain ANSI of the full-screen view: the alternate screen and the
// hidden cursor, restored on close — including on SIGINT and SIGTERM,
// which end the loop's context. Each frame redraws in place: home, then
// every line erased to its end, then erase below.
const (
	topEnterAlt   = "\x1b[?1049h"
	topLeaveAlt   = "\x1b[?1049l"
	topHideCursor = "\x1b[?25l"
	topShowCursor = "\x1b[?25h"
	topHome       = "\x1b[H"
	topEraseLine  = "\x1b[K"
	topEraseBelow = "\x1b[J"
)

// topScreen owns stdout for the loop: it enters the alternate screen
// and hides the cursor, redraws the given lines, and close restores
// everything.
type topScreen struct {
	w io.Writer
}

func newTopScreen(w io.Writer) *topScreen {
	fmt.Fprint(w, topEnterAlt, topHideCursor)
	return &topScreen{w: w}
}

func (s *topScreen) close() {
	fmt.Fprint(s.w, topShowCursor, topLeaveAlt)
}

func (s *topScreen) draw(lines []string) {
	var b strings.Builder
	b.WriteString(topHome)
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(line)
		b.WriteString(topEraseLine)
	}
	b.WriteString(topEraseBelow)
	fmt.Fprint(s.w, b.String())
}

// terminalSize is the output's size: the width logo.Detect reads and the
// height from the same ioctl; 80x24 when the output is not a terminal or
// the ioctl fails.
func terminalSize(w io.Writer) (width, height int) {
	detected := detectCaps(w)
	width = detected.Width
	if f, ok := w.(*os.File); ok && detected.TTY {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil {
			height = int(ws.Row)
			if width <= 0 {
				width = int(ws.Col)
			}
		}
	}
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	return width, height
}

// topLoop is the full-screen view: fetch and draw every interval, q
// quits, p freezes and unfreezes the refresh, the context's end
// (SIGINT, SIGTERM) restores the terminal and exits. A fetch that
// fails draws its error as a line over the last data and the next tick
// tries again; the dial never starts an engine under --no-start.
func topLoop(ctx context.Context, rt *runtime, repo string, interval time.Duration, noStart bool, stdout io.Writer) int {
	screen := newTopScreen(stdout)
	defer screen.close()
	keys := make(chan byte, 8)
	stopKeys := make(chan struct{})
	defer close(stopKeys)
	if restore, ok := enterRaw(os.Stdin); ok {
		defer restore()
		go readKeys(stopKeys, keys)
	}
	var model topModel
	refresh := func() {
		client, err := dialEngine(ctx, rt, "client", noStart)
		if err != nil {
			model.err = err
			return
		}
		fetched, err := topFetch(ctx, topAPI{client}, repo)
		fetched.err = err
		fetched.paused = model.paused
		fetched.events = mergeTopEvents(model.events, fetched.events)
		if err != nil {
			fetched = fetched.keepPrevious(model)
		}
		model = fetched
		// Each tick reads the cached listing; no CLI runs on a tick.
		model.hint = agentsHint(ctx, rt, false)
	}
	// A stale listing is refreshed once, off the draw path.
	go refreshAgentsCacheIfStale(ctx, rt)
	draw := func() {
		width, height := terminalSize(stdout)
		screen.draw(renderTop(model, width, height, time.Now(), term.New(detectCaps(stdout))))
	}
	refresh()
	draw()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case key := <-keys:
			switch key {
			case 'q', 'Q':
				return 0
			case 'p', 'P':
				model.paused = !model.paused
				if !model.paused {
					refresh()
				}
				draw()
			}
		case <-ticker.C:
			if !model.paused {
				refresh()
			}
			draw()
		}
	}
}

// readKeys forwards single keypresses from stdin until a read fails or
// stop closes; raw mode makes each key one byte.
func readKeys(stop <-chan struct{}, keys chan<- byte) {
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return
		}
		select {
		case keys <- buf[0]:
		case <-stop:
			return
		}
	}
}

// enterRaw puts the terminal behind f into the minimum raw mode the
// keys need: no echo, no line buffering, one byte per read. The signal
// and output settings stay, so Ctrl-C still ends the loop's context.
// ok is false — and nothing changes — when f is not a terminal or the
// ioctl fails; then the interval and the signals alone drive the view.
func enterRaw(f *os.File) (restore func(), ok bool) {
	saved, err := unix.IoctlGetTermios(int(f.Fd()), tcgets)
	if err != nil {
		return nil, false
	}
	raw := *saved
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(int(f.Fd()), tcsets, &raw); err != nil {
		return nil, false
	}
	return func() { unix.IoctlSetTermios(int(f.Fd()), tcsets, saved) }, true
}
