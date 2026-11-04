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
// read it. Engine errors show as a band in the view while the loop
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
	// err is the fetch's first error: the view shows it as a band and
	// the loop keeps retrying. Parts that did fetch still render.
	err error
	// paused says the loop froze the refresh (the p key); pausedAt is when,
	// the clock the sections read while it holds.
	paused   bool
	pausedAt time.Time
	// fetchedAt is the last fetch that came back whole; zero before the first.
	fetchedAt time.Time
	// dialFailed says err is the engine not answering the dial, as against a
	// fetch error from an engine that is running.
	dialFailed bool
	// hint is the unregistered-models line of the cached listing, or "".
	hint string
}

// afterDial is the model once the loop has dialled the engine: a failed dial
// keeps the data, notes the error and sets the dial-failed flag; a good one
// clears the flag before the fetch runs, so a fetch that then fails reads as
// an error from a running engine.
func (m topModel) afterDial(err error) topModel {
	m.dialFailed = err != nil
	if err != nil {
		m.err = err
	}
	return m
}

// afterFetch is the model once a tick fetched, over prev: the fetch's error,
// the pause, the dial flag and the last good time carried across, the events
// merged and, on an error, the parts the fetch left empty kept from prev. A
// clean fetch sets the last good time to now; a failed one keeps prev's.
func afterFetch(prev, fetched topModel, err error, now time.Time) topModel {
	fetched.err = err
	fetched.paused, fetched.pausedAt, fetched.dialFailed = prev.paused, prev.pausedAt, prev.dialFailed
	fetched.fetchedAt = prev.fetchedAt
	fetched.events = mergeTopEvents(prev.events, fetched.events)
	if err != nil {
		return fetched.keepPrevious(prev)
	}
	fetched.fetchedAt = now
	return fetched
}

// togglePause is the model after the p key: paused with the time it began, or
// resumed with none.
func (m topModel) togglePause(now time.Time) topModel {
	m.paused = !m.paused
	m.pausedAt = time.Time{}
	if m.paused {
		m.pausedAt = now
	}
	return m
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
// tick's model, so the error band sits over the last data instead of a
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

// renderTop lays the view out, top to bottom: the header row (the brand mark,
// scope, uptime, clock and keys), the slots row (the meter and the counters)
// or, while the fetch is failing or the view is paused, a blank row, a
// full-width band (topBand) and a blank row in its place, the hint line, then
// one rounded box per section — NEEDS YOU,
// the FOREMAN block (a foreman turn no plan claims) and the JOBS block (any
// other job no plan claims), each only when it has lines, the PLANS tree
// with every job under its plan, the AGENTS section and the EVENTS section
// at the bottom (up to 10 rows, newest first, the foreman's report line in
// the right of its top border). A section's text is built at the box's
// inner width, so a cut never eats a border. Every line is cut to width.
// The height is a limit, 0 meaning none: the frame never has more lines
// than it. Every line carries a yield class (topYield) and, while the
// frame is taller than the height, lines drop class by class, each class
// from the bottom of the view upward, one line at a time, until it fits:
// a0 event rows 3 and up; a event row 2; b finished plans' lines; c the
// blank rows around a band; d queued task lines (waiting
// on, review included); e idle agent rows, uncounted; f held and retry
// lines, the FOREMAN and JOBS notes and parked task lines; g running lines;
// h every other row outside the header, the band and NEEDS YOU — event row
// 1, the other agent rows, each plan's own line, the placeholders — with the
// slots row dropping last of the class, though it
// sits above NEEDS YOU. A section's top and bottom borders are rows of the
// section: both go right after its last row, so an empty box never shows
// and a drop may undershoot the height by a row or two. NEEDS YOU keeps
// its borders. i the hard clip: only the header, the band (with its hint
// line) and NEEDS YOU remain, and the frame is cut from the bottom. While
// the fetch is failing every line but the header and the band is Dim. With
// a plain style the text is the same and has no escapes.
func renderTop(model topModel, width, height int, now time.Time, style term.Style) []string {
	inner := max(width-4, 1)
	// The sections read the pause time while paused, so job ages stop; the
	// header and the band read the real clock.
	secNow := now
	if model.paused && !model.pausedAt.IsZero() {
		secNow = model.pausedAt
	}
	var lines []topLine
	add := func(section []topLine) { lines = append(lines, section...) }
	add(topPinned(topHeader(model, now, style, width)))
	band := topBand(model, now, style, width)
	if band == "" {
		add([]topLine{{text: topMeters(model, style, width)[0], class: yieldRest, late: true}})
	} else {
		add([]topLine{{class: yieldBlank}, {text: band}, {class: yieldBlank}})
	}
	add(topPinned(topHint(model, style, width)))
	add(newTopNeedsYou(model.status, model.soleProject, style, inner))
	foreman, jobs := topPlanless(model.status, secNow, style, inner)
	add(foreman)
	add(jobs)
	add(topPlansBox(model.status, model.tagPlans, model.statusOK, model.events, secNow, style, inner))
	add(topAgentLines(model, secNow, style, inner))
	events := topEventLinesAt(model.events, model.statusOK && topFirstRun(model.status), secNow, style, inner)
	if r := model.status.Report; r != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(r.Text), "\n")
		events[0].box.right = strings.TrimSpace(first)
	}
	add(events)

	frame := &topFrame{lines: lines, style: style, width: width}
	frame.fit(height)
	out := frame.texts()
	if height > 0 && len(out) > height {
		out = out[:height]
	}
	for i, line := range out {
		out[i] = topFit(line, width)
	}
	if model.err != nil {
		// Old data must not read as live: every line below the header and the
		// band goes to the one Dim colour, tints and family colours stripped.
		bandText := topFit(band, width)
		for i, line := range out {
			if i > 0 && line != "" && line != bandText {
				out[i] = style.Dim(term.StripEscapes(line))
			}
		}
	}
	return out
}

// topYield is a line's yield class: the order in which lines drop while
// the frame is taller than the height. To make a new kind of line yield,
// give it a class when its section builds it; the drop loop needs no
// other change.
type topYield int

const (
	yieldPinned       topYield = iota // header, band, NEEDS YOU and the borders: only the hard clip cuts them
	yieldEventsExtra                  // a0: event rows 3 and up
	yieldEventsSecond                 // a: event row 2
	yieldFinished                     // b: a finished plan's line
	yieldBlank                        // c: the blank rows around a band
	yieldQueued                       // d: queued, waiting-on and review task lines
	yieldIdle                         // e: idle agent rows
	yieldHeld                         // f: held and retry lines, parked task lines
	yieldRunning                      // g: running lines
	yieldRest                         // h: every other line outside the header and NEEDS YOU
)

// The sections whose borders go with their last row.
const (
	topSecNone = iota
	topSecNeeds
	topSecForeman
	topSecJobs
	topSecPlans
	topSecAgents
	topSecEvents
)

// topLine is one line of the frame with what the drop loop needs.
type topLine struct {
	text    string
	class   topYield
	section int        // the section whose borders go with its last row, or topSecNone
	box     *topBox    // set on a section's top border
	foot    bool       // the section's bottom border
	boxed   bool       // the text sits in a box row
	bad     bool       // the box is a Bad one
	tint    int        // the row's background tint (topTint...), none by default
	job     *topJobRow // a job row whose text topLayoutJobs builds with its box's columns
	late    bool       // drops after the other lines of its class
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

// topFrame is the view's lines with the drops so far. width is the frame's
// width: with it the box lines render, and 0 leaves the rows as bare text,
// the borders out.
type topFrame struct {
	lines []topLine
	style term.Style
	width int
}

// visible says whether line i renders: it is not dropped.
func (f *topFrame) visible(i int) bool { return !f.lines[i].dropped }

func (f *topFrame) size() int {
	n := 0
	for i := range f.lines {
		if f.visible(i) {
			n++
		}
	}
	return n
}

// texts is the visible lines' text, every box line drawn at the frame's
// width.
func (f *topFrame) texts() []string {
	var out []string
	for i, l := range f.lines {
		if !f.visible(i) {
			continue
		}
		text := l.text
		switch {
		case f.width <= 0:
			if l.foot {
				continue
			}
			if l.box != nil {
				text = l.box.title
			}
		case l.box != nil:
			text = topBoxTop(*l.box, f.style, f.width)
		case l.foot:
			text = topBoxBottom(l.bad, f.style, f.width)
		case l.boxed:
			text = topBoxRow(text, l.bad, l.tint, f.style, f.width)
		}
		out = append(out, text)
	}
	return out
}

// drop removes line i.
func (f *topFrame) drop(i int) { f.lines[i].dropped = true }

// dropEmptyBorders drops a section's borders once no other line of the
// section is left.
func (f *topFrame) dropEmptyBorders(section int) {
	if section == topSecNone || section == topSecNeeds {
		return
	}
	for j, l := range f.lines {
		if l.section == section && !l.foot && l.box == nil && f.visible(j) {
			return
		}
	}
	for j, l := range f.lines {
		if l.section == section && (l.foot || l.box != nil) {
			f.lines[j].dropped = true
		}
	}
}

// fit drops lines until the frame has at most height lines, class by
// class and from the bottom upward in each; height 0 is no limit. Lines
// that are late in their class go after the rest of it. A section's
// borders go right after its last row. What the classes leave over is the
// hard clip, which the caller applies.
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
					f.dropEmptyBorders(l.section)
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

// topHeader is the first row of the view, after one leading space: the
// brand mark (`▙ brigada`, the word alone without UTF-8), three spaces, the
// scope when the fetch found one and the engine's uptime joined by the
// separator, and at the right end the clock, three spaces and the keys. The
// keys show only when they fit whole — the left part, two spaces and the
// right part within the width; a key is never cut mid-word — and otherwise
// the clock moves up against the right edge. While the fetch is failing the
// clock is Bad and the uptime is left out; the error itself is the band
// under the header (topBand).
func topHeader(model topModel, now time.Time, style term.Style, width int) []string {
	st := model.status
	sep := topEventSep(style)
	uptime := "0s"
	if started := parseStartedAt(st.StartedAt); !started.IsZero() {
		uptime = topUptime(now.Sub(started))
	}
	keys := "q quit  p pause"
	// The brand's one-line form; without UTF-8 it has none and the word stands.
	mark := style.OneLine()
	if mark == "" {
		mark = "brigada"
	}
	left := " " + mark + "   "
	failing := model.err != nil
	switch {
	case failing && model.scope != "":
		// The uptime is the old data's; the band says so.
		left += model.scope
	case failing:
		left = strings.TrimRight(left, " ")
	case model.scope != "":
		left += model.scope + sep + "up " + uptime
	default:
		left += "up " + uptime
	}
	clock := now.Format("15:04:05")
	clockOut := clock
	if failing {
		clockOut = style.Bad(clock)
	}
	// The keys show only when they fit whole, two spaces from the left
	// part; otherwise the clock moves up against the right edge.
	right := clockOut + "   " + keys
	gap := width - term.VisibleWidth(left) - term.VisibleWidth(right)
	if gap < 2 {
		right = clockOut
		gap = width - term.VisibleWidth(left) - term.VisibleWidth(right)
	}
	line := left
	if gap >= 1 {
		line += strings.Repeat(" ", gap) + right
	}
	return []string{topFit(line, width)}
}

// topBand is the full-width band under the header while the view is failing
// or paused, or "" when it is neither; an error shows over a pause, since old
// data must not read as live. The band is solid Bad or Warn with dark text:
// one leading space, the Bold first part, three spaces, the middle part, and
// at the right edge the Bold action with one trailing space, padded to
// width. The middle is left out before the first part is cut, and the action
// when even the first part and it do not fit.
//
// An error is `✗ ENGINE UNREACHABLE` with `brigada start` when the dial
// failed, else `✗ engine: <text>` with no action; the middle is `retrying`
// and the age of the last good fetch. A pause is `|| PAUSED`, `at HH:MM:SS`
// and `ages frozen`, and the action `p resumes`.
func topBand(model topModel, now time.Time, style term.Style, width int) string {
	var first, middle, action string
	paint := style.BandWarn
	switch {
	case model.err != nil:
		paint = style.BandBad
		sep := topEventSep(style)
		age := "no data yet"
		if !model.fetchedAt.IsZero() {
			age = "data is " + topUptime(now.Sub(model.fetchedAt)) + " old"
		}
		middle = "retrying" + sep + age
		if model.dialFailed {
			first, action = style.Cross()+" ENGINE UNREACHABLE", "brigada start"
		} else {
			first = style.Cross() + " engine: " + strings.Join(strings.Fields(model.err.Error()), " ")
		}
	case model.paused:
		at := model.pausedAt
		if at.IsZero() {
			at = now
		}
		first, action = "|| PAUSED", "p resumes"
		middle = "at " + at.Format("15:04:05") + topEventSep(style) + "ages frozen"
	default:
		return ""
	}
	count := utf8.RuneCountInString
	// Columns: a leading space, the first part, the gaps, a trailing space.
	room := width - 2
	withMiddle := count(first) + 3 + count(middle)
	switch {
	case action != "" && withMiddle+2+count(action) <= room:
	case action != "" && count(first)+2+count(action) <= room:
		middle = ""
	case withMiddle <= room:
		action = ""
	default:
		action, middle = "", ""
		first = cutLine(first, room)
	}
	body := " " + style.Bold(first)
	used := 1 + count(first)
	if middle != "" {
		body += "   " + middle
		used += 3 + count(middle)
	}
	if action != "" {
		body += strings.Repeat(" ", max(width-used-count(action)-1, 1)) + style.Bold(action) + " "
	} else {
		body += strings.Repeat(" ", max(width-used, 0))
	}
	return paint(body)
}

// topHint is the unregistered-models line under the header, Dim, or nothing.
func topHint(model topModel, style term.Style, width int) []string {
	if model.hint == "" {
		return nil
	}
	return []string{style.Dim(cutLine(model.hint, width))}
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

// topMeters is the header's second row, after one leading space: the slots
// meter and `use/cap slots` at the left, the run, queue, held and needs
// counters at the right, two spaces apart. There is no meter when the layer
// is uncapped or capped at zero — `2/none slots`, `0/0 slots` — and the cell
// reads `?/? slots` when the capacity fetch failed.
func topMeters(model topModel, style term.Style, width int) []string {
	st := model.status
	cell := "?/? slots"
	meterUse, meterLimit := 0, 0
	if use, limit, ok := topSlots(model.capacity); ok {
		cell = fmt.Sprintf("%d/none slots", use)
		if limit != nil {
			cell = fmt.Sprintf("%d/%d slots", use, *limit)
			meterUse, meterLimit = use, *limit
		}
	}
	// A counter is coloured only above zero: nothing held or needed reads plain.
	counter := func(n int, label string, colour func(string) string) string {
		text := fmt.Sprintf("%d %s", n, label)
		if n == 0 {
			return text
		}
		return colour(text)
	}
	counters := []string{
		style.Bold(fmt.Sprintf("%d run", len(st.Running))),
		fmt.Sprintf("%d queue", st.Jobs["queued"]),
		counter(len(st.Held), "held", style.Warn),
		counter(len(st.NeedsYou), "needs", style.Bad),
	}
	right := strings.Join(counters, "  ")
	// The meter takes what the row leaves, up to 18 cells; under 4 it is
	// left out. Only a positive cap has one.
	left := " " + cell
	if meterLimit >= 1 {
		room := width - 1 - 2 - term.VisibleWidth(cell) - 2 - term.VisibleWidth(right)
		if cells := min(room, topMeterCells); cells >= 4 {
			left = " " + topSlotMeter(meterUse, meterLimit, cells, style) + "  " + cell
		}
	}
	gap := max(width-term.VisibleWidth(left)-term.VisibleWidth(right), 2)
	return []string{topFit(left+strings.Repeat(" ", gap)+right, width)}
}

// topMeterCells is the slots meter's width in cells.
const topMeterCells = 18

// topSlotMeter is the slots meter: the global use against its cap as
// cells of one glyph — filled in proportion, at least one whenever the use
// is over zero, so a sliver of use always shows. The filled cells are
// Accent, Warn once the use reaches the cap; the rest are Dim, so the plain
// text is the same at any use and the number beside it carries the use.
func topSlotMeter(use, limit, cells int, style term.Style) string {
	filled := use * cells / limit
	if use > 0 && filled == 0 {
		filled = 1
	}
	filled = min(filled, cells)
	colour := style.Accent
	if use >= limit {
		colour = style.Warn
	}
	var meter string
	if filled > 0 {
		meter = colour(strings.Repeat(style.Meter(), filled))
	}
	if filled < cells {
		meter += style.Dim(strings.Repeat(style.Meter(), cells-filled))
	}
	return meter
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
	add := func(kind string, line topLine) {
		if kind == string(engine.KindForeman) {
			line.section = topSecForeman
			fore = append(fore, line)
		} else {
			line.section = topSecJobs
			other = append(other, line)
		}
	}
	for _, j := range st.Running {
		if _, ok := claims.plan(j.ID, j.TaskID); !ok {
			add(j.Kind, topLine{job: topRunningRow(j, "", now, style), class: yieldRunning})
		}
	}
	for _, h := range st.Held {
		if _, ok := claims.plan(h.ID, h.TaskID); !ok {
			add(h.Kind, topLine{job: topHeldRow(h, "", style), class: yieldHeld})
		}
	}
	for _, r := range st.Retries {
		if _, ok := claims.plan(r.ID, r.TaskID); !ok {
			add(r.Kind, topLine{text: topRetryLine(r, now, style, width), class: yieldHeld})
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
	topLayoutJobs(fore, style, width)
	topLayoutJobs(other, style, width)
	if len(fore) > 0 {
		foreman = topBoxed(topSecForeman, topBox{title: "FOREMAN"}, fore)
	}
	if len(other) > 0 {
		jobs = topBoxed(topSecJobs, topBox{title: "JOBS"}, other)
	}
	return foreman, jobs
}

// topPlansTree is the PLANS section: every plan the status lists, each
// live one with its live work beneath it — the running jobs first, then
// a line per task still waiting, held, retrying, parked or reviewing —
// and each finished one a single line, as bare text under the title.
func topPlansTree(st api.Status, now time.Time, style term.Style, width int) []string {
	frame := &topFrame{lines: newTopPlans(st, false, false, now, style, width), style: style}
	return frame.texts()
}

// newTopPlans renders the PLANS box with each line's yield class: a
// finished plan's line is class b, a queued, waiting-on or review task
// line is class d, a held, retry or parked line f (a done task not landed
// too, as its ⇡ line), a running line g, a reviewing plan's
// `plan review queued` line d, and a live plan's own line and the none
// placeholder are class h. The top border's note counts the live plans and
// the finished ones, listed and hidden, once the status has come back
// (fetched). With tagged each plan's own line names its repo.
func newTopPlans(st api.Status, tagged, fetched bool, now time.Time, style term.Style, width int) []topLine {
	return topPlansBox(st, tagged, fetched, nil, now, style, width)
}

// topFirstRun says the status has no plans at all, listed or hidden. It means
// a first run only once the status has come back.
func topFirstRun(st api.Status) bool {
	return len(st.Plans) == 0 && st.HiddenFinishedPlans == 0
}

// topFirstRunRows is what the PLANS box reads on a first run: how to give the
// foreman something to do.
func topFirstRunRows(style term.Style, line func(string, topYield) topLine) []topLine {
	return []topLine{
		line("no plans yet. Give the foreman something to do:", yieldRest),
		line("", yieldRest),
		line("  $ "+style.Accent(`brigada ask "fix #12"`), yieldRest),
		line("", yieldRest),
		line(style.Dim("Plans and tasks appear here. What needs you shows above."), yieldRest),
	}
}

// topPlansBox is newTopPlans with the kept events, whose newest task_landed
// the idle row of a box with no live plan reads. A status that came back with
// no plans is a first run (topFirstRunRows); one that did not come back has
// said nothing and keeps `none`. When plans exist and none is live, the note
// reads `none live · N finished` and a Dim `idle: last work landed HH:MM`
// row ends the finished lines, left out when no landing is kept.
func topPlansBox(st api.Status, tagged, fetched bool, events []history.Item, now time.Time, style term.Style, width int) []topLine {
	line := func(text string, class topYield) topLine {
		return topLine{text: text, class: class, section: topSecPlans}
	}
	box := topBox{title: "PLANS", legend: topPlanLegend(style)}
	live, finished := 0, st.HiddenFinishedPlans
	for _, p := range st.Plans {
		if topPlanFinished(p) {
			finished++
		} else {
			live++
		}
	}
	if fetched {
		box.note = fmt.Sprintf("%d live%s%d finished", live, topEventSep(style), finished)
		if live == 0 && finished > 0 {
			box.note = fmt.Sprintf("none live%s%d finished", topEventSep(style), finished)
		}
	}
	var lines []topLine
	if fetched && topFirstRun(st) {
		lines = topFirstRunRows(style, line)
	}
	claims := newTopClaims(st)
	reasons := make(map[int64]string)
	for _, n := range append(append([]engine.NeedsYouItem{}, st.NeedsYou...), st.ForemanHolds...) {
		if n.Kind == "task" {
			reasons[n.ID] = n.Reason
		}
	}
	running := make(map[int64]bool, len(st.Running))
	for _, j := range st.Running {
		if j.TaskID != 0 {
			running[j.TaskID] = true
		}
	}
	layout := newTopPlanLayout(st.Plans, running, tagged, style, width)
	for _, p := range st.Plans {
		tag := ""
		if tagged {
			tag = topRepoName(p.RepoURL)
		}
		if topPlanFinished(p) {
			lines = append(lines, line(topPlanLine(p, tag, running, layout, style, width), yieldFinished))
			continue
		}
		lines = append(lines, line(topPlanLine(p, tag, running, layout, style, width), yieldRest))
		task := func(text string, class topYield) {
			lines = append(lines, line(text, class))
		}
		row := func(r *topJobRow, class topYield) {
			lines = append(lines, topLine{job: r, class: class, section: topSecPlans})
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
				row(topRunningRow(j, titles[j.TaskID], now, style), yieldRunning)
				busy[j.TaskID] = true
				reviewJob = reviewJob || j.TaskID == 0
			}
		}
		for _, h := range st.Held {
			if id, ok := claims.plan(h.ID, h.TaskID); ok && id == p.ID && h.TaskID == 0 {
				row(topHeldRow(h, "", style), yieldHeld)
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
					row(topHeldRow(h, t.Title, style), yieldHeld)
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
				row(topLandingRow(t, now, style), yieldHeld)
				continue
			}
			if topTaskSettled(t.State) {
				continue
			}
			if quiet && topTaskWaits(t.State) {
				continue
			}
			lines = append(lines, topTaskLine(t, reasons[t.ID], style, width))
		}
	}
	if fetched && live == 0 && finished > 0 {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Kind == "task_landed" {
				lines = append(lines, line(style.Dim("idle: last work landed "+topClock(events[i].CreatedAt, now)), yieldFinished))
				break
			}
		}
	}
	if len(lines) == 0 {
		lines = append(lines, line(style.Dim("none"), yieldRest))
	}
	topLayoutJobs(lines, style, width)
	return topBoxed(topSecPlans, box, lines)
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

// topPlanTail is what ends a plan's line, right-aligned: the cells
// (topPlanCells), two spaces and done/total for an active plan, just
// done/total once the cells would take more than a third of the width,
// `done` for a done plan, `cancelled` for a cancelled one, and a state word
// for any other — `awaiting approval`, `reviewing round N` (the plan
// review's round, as status words it) or `needs foreman`.
func topPlanTail(p engine.StatusPlan, running map[int64]bool, style term.Style, width int) string {
	switch p.State {
	case "done":
		return style.OK("done")
	case "cancelled":
		return style.Bad("cancelled")
	case "", "active":
		done, total := topPlanCounts(p)
		count := fmt.Sprintf("%d/%d", done, total)
		if cells := topPlanCells(p, running, style); cells != "" && term.VisibleWidth(cells)*3 <= width {
			return cells + "  " + count
		}
		return count
	case "pending_approval":
		return style.Warn("awaiting approval")
	case "reviewing":
		return style.Dim(fmt.Sprintf("reviewing round %d", p.ReviewRound))
	case "needs_foreman":
		return style.Warn("needs foreman")
	}
	return style.Dim(strings.ReplaceAll(p.State, "_", " "))
}

// topPlanLayout is the columns every plan line of the list shares: the
// widest id, the widest tag (0 when no line names its repo) and the widest
// title, the title capped so the widest tail still fits. The tails end at
// the right edge.
type topPlanLayout struct {
	id, tag, title, tail int
}

func newTopPlanLayout(plans []engine.StatusPlan, running map[int64]bool, tagged bool, style term.Style, width int) topPlanLayout {
	var l topPlanLayout
	for _, p := range plans {
		l.id = max(l.id, len(fmt.Sprintf("#%d", p.ID)))
		if tagged {
			l.tag = max(l.tag, utf8.RuneCountInString(topRepoName(p.RepoURL)))
		}
		l.title = max(l.title, utf8.RuneCountInString(p.Title))
		l.tail = max(l.tail, term.VisibleWidth(topPlanTail(p, running, style, width)))
	}
	fixed := l.id + 1 + l.tail + 1
	if l.tag > 0 {
		fixed += l.tag + 2
	}
	l.title = min(l.title, max(width-fixed, 4))
	return l
}

// topPlanLine is a plan's own line: the id, the tag when the layout has
// tags, the title, and the tail (topPlanTail) right-aligned to the width. A
// live plan's id is Bold. The tag sits Dim after the id and the title gives
// way to the widest tail.
func topPlanLine(p engine.StatusPlan, tag string, running map[int64]bool, l topPlanLayout, style term.Style, width int) string {
	id := fmt.Sprintf("#%d", p.ID)
	pad := strings.Repeat(" ", l.id-len(id))
	if !topPlanFinished(p) {
		id = style.Bold(id)
	}
	out := id + pad + " "
	if l.tag > 0 {
		out += style.Dim(tag) + strings.Repeat(" ", l.tag-utf8.RuneCountInString(tag)) + "  "
	}
	out += cutLine(p.Title, l.title)
	tail := topPlanTail(p, running, style, width)
	return out + strings.Repeat(" ", max(width-term.VisibleWidth(out)-term.VisibleWidth(tail), 1)) + tail
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

// topLandingRow is a done task that has not landed and that no job speaks
// for, a column row with a teal `⇡` and `landing: ...` in the kind column,
// with what the landing waits on — `queued` for a land job in the queue, the
// land_ready gate's own words with the stored retry stamp as a local clock,
// or `not landed` when the task carries no word.
func topLandingRow(t engine.StatusTask, now time.Time, style term.Style) *topJobRow {
	what := "not landed"
	switch t.LandReady {
	case "":
	case "ready":
		what = "queued"
	default:
		what = topLandReadyWords(t.LandReady, now)
	}
	return topWaitRow(style.Up(), topJobID(t.ID), t.Title, "landing: "+what, style.Teal)
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

// topHeldRow is one held job, a column row with a Warn `?` and
// `held: ...` in the kind column: the task it is for, or `job #N` in the
// title column when it has none. title is the task's title, "" when unknown.
func topHeldRow(h engine.HeldJob, title string, style term.Style) *topJobRow {
	if h.TaskID == 0 {
		title = fmt.Sprintf("job #%d", h.ID)
	}
	return topWaitRow(style.Question(), topJobID(h.TaskID), title, "held: "+api.HeldWait(h), style.Warn)
}

// topPlanCells is a live plan's progress, one cell per task in task order,
// cancelled and retired tasks left out: landed OK, or done of a class that
// never lands; done and not landed teal; running (the task has a job in
// running) Accent; parked Bad; any other task an empty cell in Dim.
func topPlanCells(p engine.StatusPlan, running map[int64]bool, style term.Style) string {
	var cells strings.Builder
	for _, t := range p.Tasks {
		switch {
		case t.State == "cancelled" || t.State == "retired":
		case t.State == "done" && (t.Landed || t.LandedLabel() == ""):
			cells.WriteString(style.OK(style.Filled()))
		case t.State == "done":
			cells.WriteString(style.Teal(style.Filled()))
		case running[t.ID]:
			cells.WriteString(style.Accent(style.Filled()))
		case t.State == "needs_foreman" || t.State == "needs_user" || t.State == "failed":
			cells.WriteString(style.Bad(style.Filled()))
		default:
			cells.WriteString(style.Dim(style.Empty()))
		}
	}
	return cells.String()
}

// topPlanLegend is the PLANS border's key to the cells, each in its colour.
func topPlanLegend(style term.Style) string {
	return style.OK(style.Filled()) + style.Dim(" landed") + "  " +
		style.Teal(style.Filled()) + style.Dim(" done") + "  " +
		style.Accent(style.Filled()) + style.Dim(" running") + "  " +
		style.Dim(style.Empty()) + style.Dim(" queued")
}

// topTaskLine is a live task's line when no job speaks for it: a column
// row of Dim `·`, the id, the title and queued — or waiting on the
// dependencies it names — for a task not yet leased, or "(review)" for a
// task reviewing with no job running; a free-form Bad `!` line on the
// needs-you tint, with the needs-you reason, for a parked one. A parked
// line yields in class f, the rest in class d.
func topTaskLine(t engine.StatusTask, reason string, style term.Style, width int) topLine {
	line := topLine{section: topSecPlans}
	switch t.State {
	case "needs_foreman", "needs_user", "failed":
		if reason == "" {
			reason = t.State
		}
		subject := fmt.Sprintf("#%d %s", t.ID, t.Title)
		line = topNeedsRow(cutLine("! "+subject+" "+reason, width-2), style)
		line.text = "  " + line.text
		line.section, line.class = topSecPlans, yieldHeld
		return line
	case "reviewing":
		line.job = topDimRow(style.Dot(), topJobID(t.ID), t.Title, "(review)")
	default:
		wait := "(queued)"
		if len(t.WaitingOn) > 0 {
			wait = "(waiting on " + strings.Join(t.WaitingOn, ", ") + ")"
		}
		line.job = topDimRow(style.Dot(), topJobID(t.ID), t.Title, wait)
	}
	line.class = yieldQueued
	return line
}

// topNeedsYou is the needs-you queue as bare text, without its borders:
// the title, then the rows of newTopNeedsYou.
func topNeedsYou(st api.Status, sole bool, style term.Style, width int) []string {
	frame := &topFrame{lines: newTopNeedsYou(st, sole, style, width), style: style}
	return frame.texts()
}

// newTopNeedsYou is the needs-you box at the top of the view, under the
// header and before every other section: one Bad row per entry, cut to
// width and capped at three entry rows with one dim "+N more" row under
// them. The cap never hides a broken agent while a row remains
// (topNeedsYouShown). The box is Bad, its title carrying the queue's full
// count. The section is always visible: an empty queue has a grey border
// and title with no count, and one dim `✓ nothing needs you` row. All of it
// is pinned.
func newTopNeedsYou(st api.Status, sole bool, style term.Style, width int) []topLine {
	if len(st.NeedsYou) == 0 {
		rows := []topLine{{text: style.Dim(style.Check() + " nothing needs you")}}
		return topBoxed(topSecNeeds, topBox{title: "NEEDS YOU"}, rows)
	}
	shown := topNeedsYouShown(st.NeedsYou)
	// An entry with an action keeps it whole at the end of its line and the
	// reason is cut instead; the tails start in one column, placed so the
	// widest tail ends at the right edge.
	texts := make([]string, len(shown))
	tails := make([]string, len(shown))
	tailW := 0
	for i, n := range shown {
		texts[i], tails[i] = topNeedsYouLine(n), topNeedsYouTail(n, st, sole, style)
		if tails[i] != "" {
			tailW = max(tailW, utf8.RuneCountInString(tails[i]))
		}
	}
	col := max(width-tailW-2, 4)
	var rows []topLine
	for i := range shown {
		text := cutLine(texts[i], width)
		if tails[i] != "" {
			left := cutLine(texts[i], col)
			text = left + strings.Repeat(" ", max(col-utf8.RuneCountInString(left), 0)) + "  " + tails[i]
		}
		rows = append(rows, topNeedsRow(text, style))
	}
	if more := len(st.NeedsYou) - len(shown); more > 0 {
		rows = append(rows, topLine{text: style.Dim(fmt.Sprintf("+%d more (brigada status)", more))})
	}
	box := topBox{title: fmt.Sprintf("NEEDS YOU %d", len(st.NeedsYou)), bad: true}
	return topBoxed(topSecNeeds, box, rows)
}

// topNeedsRow is a needs-you row: on a dark red, its text the tint's own
// colour, where the style tints; Bad text where it does not.
func topNeedsRow(text string, style term.Style) topLine {
	if topTints(style) {
		return topLine{text: text, tint: topTintNeeds}
	}
	return topLine{text: style.Bad(text)}
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
// except the foreman's report, which the EVENTS border says.
func topShows(it history.Item) bool {
	if it.Kind == "foreman_report" {
		return false
	}
	_, ok := topEventWords(it, time.Time{})
	return ok
}

// topEventLines is the EVENTS box: the newest kept items, newest first,
// one per row, at most topEventRows. A row is the time, two spaces and the
// event's words, the time column padded to the widest time of the rows
// built so the words start in one column; each row is cut to width. With
// no items the box holds a dim `no events`. Row 1 yields last
// (yieldRest), row 2 in yieldEventsSecond and rows 3 and up in
// yieldEventsExtra, the first class to go; the borders go with the last
// row. The top border's right end carries the foreman's report line
// (renderTop sets it).
func topEventLines(items []history.Item, now time.Time, style term.Style, width int) []topLine {
	return topEventLinesAt(items, false, now, style, width)
}

// topEventLinesAt is topEventLines that reads `no events yet` instead of `no
// events` on a first run.
func topEventLinesAt(items []history.Item, firstRun bool, now time.Time, style term.Style, width int) []topLine {
	box := topBox{title: "EVENTS"}
	if len(items) == 0 {
		text := "no events"
		if firstRun {
			text += " yet"
		}
		return topBoxed(topSecEvents, box, []topLine{{text: style.Dim(text), class: yieldRest}})
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
	var lines []topLine
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
		lines = append(lines, topLine{text: topFit(text, width), class: class})
	}
	return topBoxed(topSecEvents, box, lines)
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
// fails draws its error as a band over the last data and the next tick
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
		model = model.afterDial(err)
		if err != nil {
			return
		}
		fetched, err := topFetch(ctx, topAPI{client}, repo)
		model = afterFetch(model, fetched, err, time.Now())
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
				model = model.togglePause(time.Now())
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
