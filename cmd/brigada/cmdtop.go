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

// renderTop lays the view out, top to bottom: the two-row header (the
// mark, scope, uptime and clock; the slots meter and the counters), the
// engine error and the unregistered-models hint when there are any, then
// one rounded box per section — NEEDS YOU, the FOREMAN box (a foreman turn
// no plan claims) and the JOBS box (any other job no plan claims), each
// only when it has rows, the PLANS tree with every job under its plan, the
// AGENTS box and the EVENTS box at the bottom (up to 10 rows, newest first,
// the foreman's last report in the top border). Each section's text is
// built at the inner width, width-4, and boxed as `│ text │`; every line is
// cut to width.
// The height is a limit, 0 meaning none: the frame never has more lines
// than it. Every row carries a yield class (topYield) and, while the
// frame is taller than the height, rows drop class by class, each class
// from the bottom of the view upward, one row at a time, until it fits:
// a0 event rows 3 and up; a event row 2; b finished plans; c blank rows
// (none today); d queued task rows (waiting on, review included); e agent
// rows whose agents are all idle; f held
// and retry rows, the FOREMAN and JOBS notes and parked task rows; g
// running rows; h every other row outside the header and NEEDS YOU —
// event row 1, the other agent rows, each
// plan's own row, the placeholders — with the meters row dropping last of
// the class, though it sits above NEEDS YOU. A box's borders are rows of
// its section: both go right after the section's last row, and a box with
// no row never shows, so a drop may undershoot the height by a row or two.
// i the hard clip: only the header (with its error and hint rows) and
// NEEDS YOU, whose borders are pinned, remain, and the frame is cut from
// the bottom. With a plain
// style the text is the same and has no escapes.
func renderTop(model topModel, width, height int, now time.Time, style term.Style) []string {
	inner := max(width-4, 1)
	lines := topPinned([]string{topHeader(model, now, style, width)})
	if model.err != nil {
		lines = append(lines, topPinned([]string{style.Bad(cutLine("engine: "+model.err.Error(), width))})...)
	}
	lines = append(lines, topLine{text: topMeters(model, style, width), class: yieldRest, late: true})
	if model.hint != "" {
		lines = append(lines, topPinned([]string{style.Dim(cutLine(model.hint, width))})...)
	}
	section := func(id int, rows []topLine) {
		for _, l := range rows {
			l.section = id
			lines = append(lines, l)
		}
	}
	// A needs-you entry sits on a tint where the style paints one, with its
	// text bare, so the tint's own foreground reads; the style falls back to
	// Bad where it does not.
	needs := topPinned(topNeedsYou(model.status, model.soleProject, style, inner))
	for i := range needs {
		if plain := term.StripEscapes(needs[i].text); strings.HasPrefix(plain, "! ") {
			needs[i].text, needs[i].tint = plain, tintNeeds
		}
	}
	section(topSecNeeds, needs)
	foreman, jobs := topPlanless(model.status, now, style, inner)
	section(topSecForeman, foreman)
	section(topSecJobs, jobs)
	section(topSecPlans, newTopPlans(model.status, model.tagPlans, model.statusOK, now, style, inner))
	section(topSecAgents, topAgentLines(model, now, style, inner))
	section(topSecEvents, topEventLines(model.events, now, style, inner))

	frame := &topFrame{lines: lines, style: style, width: width, heads: topHeads(model, style)}
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
	yieldEventsSecond                 // a: event row 2
	yieldFinished                     // b: a finished plan's line
	yieldBlank                        // c: blank rows; no row uses it yet
	yieldQueued                       // d: queued, waiting-on and review task lines
	yieldIdle                         // e: agent rows whose agents are all idle
	yieldHeld                         // f: held and retry lines, parked task lines
	yieldRunning                      // g: running lines
	yieldRest                         // h: every other line outside the header and NEEDS YOU
)

// The boxed sections; topSecNone is the header's rows, which no box holds.
const (
	topSecNone = iota
	topSecNeeds
	topSecForeman
	topSecJobs
	topSecPlans
	topSecAgents
	topSecEvents
)

// topLine is one row of the frame with what the drop loop needs.
type topLine struct {
	text    string
	class   topYield
	section int  // the box the row sits in, or topSecNone for the header
	late    bool // drops after the other lines of its class
	dropped bool
	job     *topJobRow // a job row, whose text topRenderJobs lays out in columns
	tint    topTint    // the row's background, where the style paints one
}

// topTint names the background a row sits on.
type topTint int

const (
	tintNone  topTint = iota
	tintNeeds         // a needs-you entry: dark red
	tintRun           // a running job: dark blue
)

// topPinned wraps lines no class drops.
func topPinned(texts []string) []topLine {
	lines := make([]topLine, len(texts))
	for i, text := range texts {
		lines[i] = topLine{text: text}
	}
	return lines
}

// topHead is what a box's top border carries: the title, a note after it
// and a note at the right end, both Dim, and the styles of the border and
// the title.
type topHead struct {
	title, note, rnote string
	// rlegend is a right-hand note already coloured, left out whole when it
	// does not fit; rnote, when set, takes its place.
	rlegend            string
	border, titleStyle func(string) string
}

// topHeads is the head of each boxed section.
func topHeads(model topModel, style term.Style) map[int]topHead {
	head := func(title string) topHead {
		return topHead{title: title, border: style.Dim, titleStyle: style.Bold}
	}
	st := model.status
	needs := topHead{title: "NEEDS YOU", border: style.Dim, titleStyle: style.Dim}
	if n := len(st.NeedsYou); n > 0 {
		needs = topHead{title: fmt.Sprintf("NEEDS YOU %d", n), border: style.Bad, titleStyle: style.Bad}
	}
	plans := head("PLANS")
	plans.rlegend = style.OK(style.Filled()) + style.Dim(" landed  ") + style.Teal(style.Filled()) + style.Dim(" done  ") +
		style.Accent(style.Filled()) + style.Dim(" running  ") + style.Dim(style.Empty()+" queued")
	if model.statusOK {
		finished := st.HiddenFinishedPlans
		for _, p := range st.Plans {
			if topPlanFinished(p) {
				finished++
			}
		}
		live := len(st.Plans) - (finished - st.HiddenFinishedPlans)
		plans.note = fmt.Sprintf("%d live%s%d finished", live, topEventSep(style), finished)
	}
	events := head("EVENTS")
	if st.Report != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(st.Report.Text), "\n")
		events.rnote = strings.TrimSpace(first)
	}
	agents := head("AGENTS")
	agents.note = topAgentsNote(model.capacity, style)
	return map[int]topHead{
		topSecNeeds:   needs,
		topSecForeman: head("FOREMAN"),
		topSecJobs:    head("JOBS"),
		topSecPlans:   plans,
		topSecAgents:  agents,
		topSecEvents:  events,
	}
}

// topAgentsNote is the AGENTS border note: `all ≤ global N` when the global
// cap sits below some agent's own cap or an agent has none, and `M off`
// when M agents are off, joined by the separator, either part left out
// alone. With no agent broken or cooling, none off and no layer capping
// anything it reads `all ready`. Without agents it is empty.
func topAgentsNote(snap engine.CapacitySnapshot, style term.Style) string {
	if len(snap.Agents) == 0 {
		return ""
	}
	g := api.GlobalCap(snap)
	var parts []string
	binds, off, unwell := false, 0, false
	for _, a := range snap.Agents {
		if g != nil && (a.Cap == nil || *g < *a.Cap) {
			binds = true
		}
		if topAgentOff(a, g) {
			off++
		}
		if a.Quota == "broken" || a.Quota == "cooling" {
			unwell = true
		}
	}
	if binds {
		parts = append(parts, fmt.Sprintf("all %s global %d", style.AtMost(), *g))
	}
	if off > 0 {
		parts = append(parts, fmt.Sprintf("%d off", off))
	}
	if len(parts) > 0 {
		return strings.Join(parts, topEventSep(style))
	}
	capped := false
	for _, l := range snap.Layers {
		capped = capped || l.Cap != nil
	}
	if !unwell && !capped {
		return "all ready"
	}
	return ""
}

// topFrame is the view's rows with the drops so far.
type topFrame struct {
	lines []topLine
	style term.Style
	width int
	heads map[int]topHead
}

// visible says whether row i renders: not dropped.
func (f *topFrame) visible(i int) bool {
	return !f.lines[i].dropped
}

// size counts the visible rows, with two border rows for each box that
// has one.
func (f *topFrame) size() int {
	n, open := 0, topSecNone
	for i, l := range f.lines {
		if !f.visible(i) {
			continue
		}
		n++
		if l.section != open && l.section != topSecNone {
			n += 2
		}
		open = l.section
	}
	return n
}

// body is the text of row i.
func (f *topFrame) body(i int) string { return f.lines[i].text }

// rows is the visible rows' text with no borders.
func (f *topFrame) rows() []string {
	var out []string
	for i := range f.lines {
		if f.visible(i) {
			out = append(out, f.body(i))
		}
	}
	return out
}

// texts is the frame's lines: the header's rows as they are, every other
// row boxed, a box's borders around its rows.
func (f *topFrame) texts() []string {
	var out []string
	open := topSecNone
	closeBox := func() {
		if open != topSecNone {
			out = append(out, topBoxBottom(f.heads[open], f.style, f.width))
		}
	}
	for i, l := range f.lines {
		if !f.visible(i) {
			continue
		}
		if l.section != open {
			closeBox()
			if l.section != topSecNone {
				out = append(out, topBoxTop(f.heads[l.section], f.style, f.width))
			}
			open = l.section
		}
		if l.section == topSecNone {
			out = append(out, f.body(i))
		} else {
			out = append(out, topBoxRow(f.body(i), l.tint, f.heads[l.section], f.style, f.width))
		}
	}
	closeBox()
	return out
}

// drop removes row i.
func (f *topFrame) drop(i int) { f.lines[i].dropped = true }

// fit drops rows until the frame has at most height lines, class by
// class and from the bottom upward in each; height 0 is no limit. Rows
// that are late in their class go after the rest of it. A box's borders go
// with its last row. What the classes leave over is the hard clip, which
// the caller applies.
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
				}
			}
		}
	}
}

// topBoxTop is a box's top border: `╭─ TITLE  note ──── rnote ─╮` in the
// width. The right note gives way first, cut or left out, then the note,
// the title last.
func topBoxTop(h topHead, style term.Style, width int) string {
	dash := style.Dash()
	room := max(width-6, 1) // what the corners, the spaces and one dash leave
	title := cutLine(h.title, room)
	room -= utf8.RuneCountInString(title)
	note := ""
	if h.note != "" && room >= 2+1 {
		note = h.note
		if n := utf8.RuneCountInString(note); n > room-2 {
			note = cutLine(note, room-2)
			if utf8.RuneCountInString(note) > room-2 {
				note = ""
			}
		}
		if note != "" {
			room -= 2 + utf8.RuneCountInString(note)
		}
	}
	rnote := ""
	if h.rnote != "" && room >= 3+8 { // a right note cut under 8 cells says nothing
		rnote = cutLine(h.rnote, room-3)
		if utf8.RuneCountInString(rnote) > room-3 {
			rnote = ""
		} else {
			room -= 3 + utf8.RuneCountInString(rnote)
		}
	}
	legend := ""
	if rnote == "" && h.rlegend != "" {
		if w := term.VisibleWidth(h.rlegend); room >= 3+w {
			legend = h.rlegend
			room -= 3 + w
		}
	}
	left := h.border(style.BoxTL()+dash+" ") + h.titleStyle(title)
	if note != "" {
		left += "  " + style.Dim(note)
	}
	if legend != "" {
		return left + " " + h.border(strings.Repeat(dash, room+1)) + " " + legend + h.border(" "+dash+style.BoxTR())
	}
	if rnote != "" {
		// room is what the fill takes besides its one cell.
		return left + " " + h.border(strings.Repeat(dash, room+1)) + " " + style.Dim(rnote) + h.border(" "+dash+style.BoxTR())
	}
	return left + " " + h.border(strings.Repeat(dash, room+1)+style.BoxTR())
}

// topBoxBottom is a box's bottom border.
func topBoxBottom(h topHead, style term.Style, width int) string {
	return h.border(style.BoxBL() + strings.Repeat(style.Dash(), max(width-2, 0)) + style.BoxBR())
}

// topBoxRow is one row of a box, `│ text │`, the text cut to the inner
// width and padded to it. A tinted row is painted after the cut and the
// padding, so the tint covers both.
func topBoxRow(text string, tint topTint, h topHead, style term.Style, width int) string {
	inner := max(width-4, 1)
	text = topFit(text, inner)
	text += strings.Repeat(" ", max(inner-term.VisibleWidth(text), 0))
	switch tint {
	case tintNeeds:
		text = style.TintNeeds(text)
	case tintRun:
		text = style.TintRun(text)
	}
	side := h.border(style.BoxSide())
	return side + " " + text + " " + side
}

// topFit cuts one styled line to width: a line that fits keeps its
// colour; one that overflows is cut on its plain text, so no line wraps.
func topFit(line string, width int) string {
	if term.VisibleWidth(line) <= width {
		return line
	}
	return cutLine(term.StripEscapes(line), width)
}

// topHeader is the first row of the view, one leading space: the brand's
// one-line mark (the bare word without UTF-8), the scope, when the fetch
// found one, and the engine's uptime on the left; the clock and the keys,
// three spaces apart, at the right edge. The keys show only when they fit
// whole — left, two spaces and the clock and keys within the width; a key
// is never cut mid-word — and otherwise the clock sits against the right
// edge alone.
func topHeader(model topModel, now time.Time, style term.Style, width int) string {
	st := model.status
	sep := topEventSep(style)
	uptime := "0s"
	if started := parseStartedAt(st.StartedAt); !started.IsZero() {
		uptime = topUptime(now.Sub(started))
	}
	keys := "q quit  p pause"
	if model.paused {
		keys = "paused; p resumes, q quits"
	}
	mark := style.OneLine()
	if mark == "" {
		mark = "brigada"
	}
	left := " " + mark + "   "
	if model.scope != "" {
		left += model.scope + sep
	}
	left += "up " + uptime
	clock := now.Format("15:04:05")
	right := clock
	if full := clock + "   " + keys; term.VisibleWidth(left)+2+term.VisibleWidth(full) <= width {
		right = full
	}
	gap := max(width-term.VisibleWidth(left)-term.VisibleWidth(right), 1)
	return topFit(left+strings.Repeat(" ", gap)+right, width)
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

// topFamily is the colour of a running agent's name by family: claude
// violet, dsh mint, and glm (a last segment starting with `glm`) blue; any
// other agent is Dim. name is the agent as a row shows it.
func topFamily(harness, name string, style term.Style) func(string) string {
	switch last := name[strings.LastIndex(name, "/")+1:]; {
	case harness == "claude":
		return style.Violet
	case harness == "dsh":
		return style.Mint
	case strings.HasPrefix(last, "glm"):
		return style.Blue
	}
	return style.Dim
}

// topAgentLimit is an agent's effective cap: its own, or the global cap
// when that is smaller; nil when nothing caps it.
func topAgentLimit(a engine.CapacityAgent, global *int) *int {
	limit := a.Cap
	if global != nil && (limit == nil || *global < *limit) {
		limit = global
	}
	return limit
}

// topAgentCells is an agent's running jobs against its effective cap: one
// cell per slot, filled (Accent) up to the running count and empty (Dim)
// after, as `■□□`. An uncapped agent, one capped at 0 or one over 8 slots
// shows api.RunningCap's text instead. The width is the visible width.
func topAgentCells(a engine.CapacityAgent, global *int, style term.Style) (string, int) {
	limit := topAgentLimit(a, global)
	if limit == nil || *limit < 1 || *limit > 8 {
		text := api.RunningCap(a, global)
		return text, utf8.RuneCountInString(text)
	}
	filled := min(a.Running, *limit)
	var b strings.Builder
	if filled > 0 {
		b.WriteString(style.Accent(strings.Repeat(style.Filled(), filled)))
	}
	if filled < *limit {
		b.WriteString(style.Dim(strings.Repeat(style.Empty(), *limit-filled)))
	}
	return b.String(), *limit
}

// topAgents is the AGENTS section's rows in plain text, for tests.
func topAgents(model topModel, now time.Time, style term.Style, width int) []string {
	frame := &topFrame{lines: topAgentLines(model, now, style, width), style: style}
	return frame.rows()
}

// topAgentLines is the AGENTS rows: the listed agents as cells, two
// columns of equal width when the inner width is 72 or more and the widest
// mark, name and cells fit half of it, one column otherwise, filled row by
// row. A cell is the mark, the name padded to the widest, the running
// jobs against the effective cap (topAgentCells) and the quota text:
// `●` OK, `↺` Warn with `until HH:MM`, `✗` Bad with the reason from the
// agent's needs-you entry ("broken" when it has none), cut to the cell.
// The order is broken and cooling first, then running, then idle, stable
// inside a group; a running agent's name takes its family colour and an
// idle one is Dim. Off agents (an effective cap of 0, nothing running,
// quota ready) never list. A row yields in the idle class when every agent
// in it is idle. With no agent the row says so.
func topAgentLines(model topModel, now time.Time, style term.Style, width int) []topLine {
	snap := model.capacity
	g := api.GlobalCap(snap)
	if len(snap.Agents) == 0 {
		return []topLine{{text: style.Dim("no agents advertised"), class: yieldRest, section: topSecAgents}}
	}
	reasons := make(map[string]string)
	for _, n := range model.status.NeedsYou {
		if n.Kind == "agent" {
			reasons[n.Title] = n.Reason
		}
	}
	var listed []engine.CapacityAgent
	for _, a := range snap.Agents {
		if !topAgentOff(a, g) {
			listed = append(listed, a)
		}
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
	cells := make([]string, len(listed))
	nameW, cellW := 0, 0
	for i, a := range listed {
		names[i] = topAgentName(a.Harness, a.Model, "")
		var w int
		cells[i], w = topAgentCells(a, g, style)
		nameW = max(nameW, utf8.RuneCountInString(names[i]))
		cellW = max(cellW, w)
	}
	markW := 1 // the widest mark; ASCII marks differ in width
	for _, m := range []string{style.Bullet(), style.Again(), style.Cross()} {
		markW = max(markW, utf8.RuneCountInString(m))
	}
	fixed := markW + 1 + nameW + 2 + cellW
	// The note that is never cut — `until HH:MM` — and room for a reason
	// to show something must fit beside the cells, or the list is one column.
	noteW := 0
	for _, a := range listed {
		switch a.Quota {
		case "cooling":
			noteW = max(noteW, 2+len("until 15:04"))
		case "broken":
			noteW = max(noteW, 2+6)
		}
	}
	cols, colW := 1, width
	if half := (width - 2) / 2; width >= 72 && fixed+noteW <= half {
		cols, colW = 2, half
	}
	cell := func(i int) (string, bool) {
		a := listed[i]
		idle := a.Running == 0 && a.Quota != "broken" && a.Quota != "cooling"
		var mark, note string
		var markFn func(string) string
		switch a.Quota {
		case "broken":
			reason := reasons[a.Harness+"/"+a.Model]
			if reason == "" {
				reason = "broken"
			}
			if room := colW - fixed - 2; room > 0 {
				reason = cutLine(reason, room)
			}
			mark, markFn, note = style.Cross(), style.Bad, reason
		case "cooling":
			mark, markFn, note = style.Again(), style.Warn, "until "+api.LocalClock(a.CoolingUntil, now)
		default:
			mark, markFn = style.Bullet(), style.OK
		}
		nameFn := func(s string) string { return s }
		switch {
		case a.Running > 0:
			nameFn = topFamily(a.Harness, names[i], style)
		case idle:
			nameFn = style.Dim
		}
		pad := func(s string, w int) string { return strings.Repeat(" ", max(w-term.VisibleWidth(s), 0)) }
		out := markFn(mark) + pad(mark, markW) + " " + nameFn(names[i]) + pad(names[i], nameW) + "  " +
			cells[i]
		if note != "" {
			out += pad(cells[i], cellW) + "  " + markFn(note)
		}
		// A cell that cannot hold its text is cut, so it never runs into
		// the next column.
		if term.VisibleWidth(out) > colW {
			out = cutLine(term.StripEscapes(out), colW)
		}
		return out, idle
	}
	var lines []topLine
	for i := 0; i < len(listed); i += cols {
		text, idle := cell(i)
		for j := i + 1; j < min(i+cols, len(listed)); j++ {
			next, nextIdle := cell(j)
			text += strings.Repeat(" ", max(colW+2-term.VisibleWidth(text), 0)) + next
			idle = idle && nextIdle
		}
		class := yieldRest
		if idle {
			class = yieldIdle
		}
		lines = append(lines, topLine{text: text, class: class, section: topSecAgents})
	}
	return lines
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

// topMeters is the header's second row, one leading space: the slots
// meter and the global layer's `use/cap slots` on the left, the run,
// queue, held and needs counters, two spaces apart, at the right edge.
// The row has no meter when the layer is uncapped or capped at zero — no
// all-empty meter — and reads `?/? slots` when the capacity fetch failed;
// when the row is narrow the meter shrinks, and under 4 cells it goes.
func topMeters(model topModel, style term.Style, width int) string {
	st := model.status
	cell := "?/? slots"
	use, capped := 0, 0 // capped is the cap when the meter shows
	if u, limit, ok := topSlots(model.capacity); ok {
		use = u
		useLimit := fmt.Sprintf("%d/none", u)
		if limit != nil {
			useLimit = fmt.Sprintf("%d/%d", u, *limit)
			capped = *limit
		}
		cell = useLimit + " slots"
	}
	// A counter is coloured only above zero: nothing held or needed reads plain.
	counter := func(n int, label string, colour func(string) string) string {
		text := fmt.Sprintf("%d %s", n, label)
		if n == 0 {
			return text
		}
		return colour(text)
	}
	right := strings.Join([]string{
		style.Bold(fmt.Sprintf("%d run", len(st.Running))),
		fmt.Sprintf("%d queue", st.Jobs["queued"]),
		counter(len(st.Held), "held", style.Warn),
		counter(len(st.NeedsYou), "needs", style.Bad),
	}, "  ")
	// The meter takes what the row leaves, up to its 18 cells, and goes
	// when fewer than 4 are left.
	left := " " + cell
	if capped >= 1 {
		cells := min(topMeterCells, width-term.VisibleWidth(left)-2-term.VisibleWidth(right)-2)
		if cells >= topMeterMin {
			left = " " + topSlotMeter(use, capped, cells, style) + "  " + cell
		}
	}
	gap := max(width-term.VisibleWidth(left)-term.VisibleWidth(right), 1)
	return topFit(left+strings.Repeat(" ", gap)+right, width)
}

// The slots meter's size: 18 cells, left out under 4.
const (
	topMeterCells = 18
	topMeterMin   = 4
)

// topSlotMeter is the header's slots meter: the global use against its cap
// as cells of ━, filled in proportion, at least one whenever the use is
// over zero, so a sliver of use always shows. The filled cells are Accent,
// Warn once the use reaches the cap; the rest are Dim in the same glyph.
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

// topJobRow is a job row before its columns are laid out: the mark, the
// task id, the title and then either a running job's cells (kind, agent,
// age, last event) or the wait text of a row with no running job, which
// takes the kind column and runs to the end of the line.
type topJobRow struct {
	mark        string
	markFn      func(string) string
	id, title   string
	dim         bool // the id, title and mark are Dim
	running     bool
	kind, agent string
	agentFn     func(string) string // the agent's family colour (topFamily)
	age, event  string
	wait        string
	waitFn      func(string) string
}

// topAgentName is the agent of a job as its row shows it:
// harness/model@effort, a name of more than two segments (a model that
// carries a path of its own) cut to its last segment.
func topAgentName(harness, model, effort string) string {
	if harness == "" {
		return ""
	}
	name := harness
	if model != "" {
		name += "/" + model
	}
	if strings.Count(name, "/") > 1 {
		name = name[strings.LastIndex(name, "/")+1:]
	}
	if model != "" && effort != "" {
		name += "@" + effort
	}
	return name
}

// topAge is how long a job has run, as `45s`, `9m04s` or `1h02m03s`, or ""
// when the job has no lease time.
func topAge(leasedAt string, now time.Time) string {
	t, err := time.Parse(engine.TimeLayout, leasedAt)
	if err != nil || now.Before(t) {
		return ""
	}
	s := int(now.Sub(t) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm%02ds", s/3600, s%3600/60, s%60)
}

// topRunningRow is a running job: with a task its id and the task's title,
// without one `job #N` in the title column.
func topRunningRow(j engine.StatusJob, title string, now time.Time, style term.Style) *topJobRow {
	r := &topJobRow{
		mark: style.Pointer(), markFn: style.Accent, running: true, kind: j.Kind,
		agent: topAgentName(j.Harness, j.Model, j.Effort), age: topAge(j.LeasedAt, now),
		agentFn: topFamily(j.Harness, topAgentName(j.Harness, j.Model, ""), style),
		event:   strings.TrimPrefix(j.LastEvent, "tool_start "),
	}
	if j.TaskID != 0 {
		r.id, r.title = fmt.Sprintf("#%d", j.TaskID), title
	} else {
		r.title = fmt.Sprintf("job #%d", j.ID)
	}
	return r
}

// topHeldRow is a held job, in Warn: the task it is for (its title when it
// has one), or the job when it has none, and status's own words for the
// wait; the kind goes before them where no title says what the job is.
func topHeldRow(h engine.HeldJob, title string, style term.Style) *topJobRow {
	r := &topJobRow{mark: style.Question(), markFn: style.Warn, waitFn: style.Warn, title: title, wait: "held: " + api.HeldWait(h)}
	if h.TaskID != 0 {
		r.id = fmt.Sprintf("#%d", h.TaskID)
	} else {
		r.title = fmt.Sprintf("job #%d", h.ID)
	}
	if r.title == "" || h.TaskID == 0 {
		r.wait = h.Kind + " " + r.wait
	}
	return r
}

// topJobLine wraps a job row as a frame row of the class.
func topJobLine(r *topJobRow, class topYield, section int) topLine {
	l := topLine{class: class, section: section, job: r}
	if r.running {
		l.tint = tintRun
	}
	return l
}

// topRenderJobs lays out the job rows of one box in fixed columns that line
// up across its rows: mark, id, title, kind, agent, age and last event, two
// spaces apart. The title gives way first and is never under 8 columns
// while it has them; the kind, agent and age are never cut. The last event
// is left out below 80 terminal columns and cut at the end of the line when
// the title is at 8 columns and it still does not fit. A row with no
// running job puts its wait text in the kind column, cut at the line's end.
func topRenderJobs(lines []topLine, style term.Style, width int) {
	var idW, titleW, kindW, agentW, ageW, eventW int
	found, running := false, false
	for _, l := range lines {
		r := l.job
		if r == nil {
			continue
		}
		found = true
		idW = max(idW, utf8.RuneCountInString(r.id))
		titleW = max(titleW, utf8.RuneCountInString(r.title))
		if r.running {
			running = true
			kindW = max(kindW, utf8.RuneCountInString(r.kind))
			agentW = max(agentW, utf8.RuneCountInString(r.agent))
			ageW = max(ageW, utf8.RuneCountInString(r.age))
			eventW = max(eventW, utf8.RuneCountInString(r.event))
		}
	}
	if !found {
		return
	}
	if width < 76 { // below 80 terminal columns
		eventW = 0
	}
	const minTitle = 8
	prefix := 5 // indent, mark and two spaces
	if idW > 0 {
		prefix += idW + 2
	}
	cols := 0
	for _, w := range []int{kindW, agentW, ageW} {
		if w > 0 {
			cols += 2 + w
		}
	}
	avail := width - prefix - cols
	title, event := titleW, 0
	switch {
	case !running:
		title = min(titleW, max(avail-20, minTitle))
	case eventW > 0 && avail-titleW-2 >= eventW:
		event = eventW
	case eventW > 0 && avail-2-eventW >= min(titleW, minTitle):
		title, event = avail-2-eventW, eventW
	case eventW > 0:
		title = min(titleW, minTitle)
		event = max(avail-title-2, 0)
	default:
		title = min(titleW, max(avail, minTitle))
	}
	pad := func(s string, w int) string { return s + strings.Repeat(" ", max(w-utf8.RuneCountInString(s), 0)) }
	for i := range lines {
		r := lines[i].job
		if r == nil {
			continue
		}
		d := func(s string) string {
			if r.dim {
				return style.Dim(s)
			}
			return s
		}
		var b strings.Builder
		b.WriteString("  " + r.markFn(r.mark) + "  ")
		if idW > 0 {
			b.WriteString(d(pad(r.id, idW)) + "  ")
		}
		cell := cutLine(r.title, title)
		b.WriteString(d(pad(cell, title)))
		if r.running {
			for _, c := range []struct {
				s  string
				w  int
				fn func(string) string
			}{{r.kind, kindW, nil}, {r.agent, agentW, r.agentFn}, {r.age, ageW, nil}} {
				if c.w > 0 {
					cellText := c.s
					if c.fn != nil && c.s != "" {
						cellText = c.fn(c.s)
					}
					b.WriteString("  " + cellText + strings.Repeat(" ", max(c.w-utf8.RuneCountInString(c.s), 0)))
				}
			}
			if event > 0 && r.event != "" {
				b.WriteString("  " + cutLine(r.event, event))
			}
		} else {
			used := 5 + title
			if idW > 0 {
				used += idW + 2
			}
			if room := width - used - 2; room > 0 && r.wait != "" {
				b.WriteString("  " + r.waitFn(cutLine(r.wait, room)))
			}
		}
		lines[i].text = strings.TrimRight(b.String(), " ")
	}
}

// topRetryLine is one job waiting on an infra retry, Dim, in status's
// own words.
func topRetryLine(r engine.InfraRetry, now time.Time, style term.Style, width int) string {
	return "  " + style.Dim(cutLine(style.Again()+" "+api.RetryLine(r, now), width-2))
}

// topPlanless is the rows of the two boxes under NEEDS YOU: the running,
// held and retrying jobs no live plan claims, so no job goes unseen. A
// foreman turn goes in the FOREMAN box; any other job — the review of a
// plan the tree does not list — in the JOBS box. Inside a box the order
// is running, held, retrying. A box is nil, and renderTop leaves it out,
// when it has no row. The running rows yield in class g, the held and
// retry rows in class f.
func topPlanless(st api.Status, now time.Time, style term.Style, width int) (foreman, jobs []topLine) {
	claims := newTopClaims(st)
	var fore, other []topLine
	add := func(kind string, l topLine) {
		if kind == string(engine.KindForeman) {
			l.section = topSecForeman
			fore = append(fore, l)
		} else {
			l.section = topSecJobs
			other = append(other, l)
		}
	}
	for _, j := range st.Running {
		if _, ok := claims.plan(j.ID, j.TaskID); !ok {
			add(j.Kind, topJobLine(topRunningRow(j, "", now, style), yieldRunning, 0))
		}
	}
	for _, h := range st.Held {
		if _, ok := claims.plan(h.ID, h.TaskID); !ok {
			add(h.Kind, topJobLine(topHeldRow(h, "", style), yieldHeld, 0))
		}
	}
	for _, r := range st.Retries {
		if _, ok := claims.plan(r.ID, r.TaskID); !ok {
			add(r.Kind, topLine{text: topRetryLine(r, now, style, width), class: yieldHeld})
		}
	}
	topRenderJobs(fore, style, width)
	topRenderJobs(other, style, width)
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
	return fore, other
}

// topPlansTree is the PLANS section: every plan the status lists, each
// live one with its live work beneath it — the running jobs first, then
// a line per task still waiting, held, retrying, parked or reviewing —
// and each finished one a single line. The PLANS note counts the finished
// plans, those the status hid too.
func topPlansTree(st api.Status, now time.Time, style term.Style, width int) []string {
	frame := &topFrame{lines: newTopPlans(st, false, false, now, style, width), style: style}
	return frame.rows()
}

// newTopPlans renders the PLANS section's rows with each row's yield
// class: a finished plan's row is class b, a queued,
// waiting-on or review task row is class d, a held, retry or parked
// row f (a done task not landed too, as its ⇡ row), a running row g, a
// reviewing plan's `plan review queued` row d, and a live plan's own row
// and the none placeholder are class h. With tagged
// each plan's own row names its repo.
func newTopPlans(st api.Status, tagged, fetched bool, now time.Time, style term.Style, width int) []topLine {
	line := func(text string, class topYield) topLine {
		return topLine{text: text, class: class, section: topSecPlans}
	}
	var lines []topLine
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
	running := make(map[int64]bool, len(st.Running))
	for _, j := range st.Running {
		if j.TaskID != 0 {
			running[j.TaskID] = true
		}
	}
	layout := newTopPlanLayout(st.Plans, tagged, running, style, width)
	for _, p := range st.Plans {
		tag := ""
		if tagged {
			tag = topRepoName(p.RepoURL)
		}
		if topPlanFinished(p) {
			lines = append(lines, line(topPlanLine(p, tag, layout, running, style, width), yieldFinished))
			continue
		}
		lines = append(lines, line(topPlanLine(p, tag, layout, running, style, width), yieldRest))
		task := func(text string, class topYield) {
			lines = append(lines, line(text, class))
		}
		jobRow := func(r *topJobRow, class topYield) {
			lines = append(lines, topJobLine(r, class, topSecPlans))
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
				jobRow(topRunningRow(j, titles[j.TaskID], now, style), yieldRunning)
				busy[j.TaskID] = true
				reviewJob = reviewJob || j.TaskID == 0
			}
		}
		for _, h := range st.Held {
			if id, ok := claims.plan(h.ID, h.TaskID); ok && id == p.ID && h.TaskID == 0 {
				jobRow(topHeldRow(h, "", style), yieldHeld)
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
					jobRow(topHeldRow(h, t.Title, style), yieldHeld)
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
				jobRow(topLandingRow(t, now, style), yieldHeld)
				continue
			}
			if topTaskSettled(t.State) {
				continue
			}
			if quiet && topTaskWaits(t.State) {
				continue
			}
			switch t.State {
			case "needs_foreman", "needs_user", "failed":
				task(topParkedLine(t, reasons[t.ID], style, width), yieldHeld)
			default:
				jobRow(topWaitRow(t, style), yieldQueued)
			}
		}
	}
	topRenderJobs(lines, style, width)
	return lines
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

// topPlanCells is a live plan's tasks, one cell each in task order, the
// cancelled and retired left out: ■ landed in OK (or done, of a class that
// never lands), ■ done and not landed in Teal, ■ running in Accent (the
// task has a job in running), ■ parked in Bad, and an empty □ in Dim for
// the rest.
func topPlanCells(p engine.StatusPlan, running map[int64]bool, style term.Style) string {
	var b strings.Builder
	for _, t := range p.Tasks {
		switch {
		case t.State == "cancelled" || t.State == "retired":
		case t.State == "done" && t.LandedLabel() == "not landed":
			b.WriteString(style.Teal(style.Filled()))
		case t.State == "done":
			b.WriteString(style.OK(style.Filled()))
		case running[t.ID]:
			b.WriteString(style.Accent(style.Filled()))
		case t.State == "needs_foreman" || t.State == "needs_user" || t.State == "failed":
			b.WriteString(style.Bad(style.Filled()))
		default:
			b.WriteString(style.Dim(style.Empty()))
		}
	}
	return b.String()
}

// topPlanTail is what ends a plan's line, at the right edge: the task
// cells and done/total for an active plan (just done/total when the cells
// take more than a third of the width), `done` for a done one,
// `cancelled` for a cancelled one, and a state word for any other —
// `awaiting approval`, `reviewing round N` (the plan review's round, as
// status words it) or `needs foreman`.
func topPlanTail(p engine.StatusPlan, running map[int64]bool, width int, style term.Style) string {
	done, total := topPlanCounts(p)
	count := fmt.Sprintf("%d/%d", done, total)
	switch p.State {
	case "done":
		return style.OK("done")
	case "cancelled":
		return style.Bad("cancelled")
	case "", "active":
		if total == 0 || total*3 > width {
			return count
		}
		return topPlanCells(p, running, style) + "  " + count
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
// title, the title capped so the widest tail still fits at the right edge.
type topPlanLayout struct {
	id, tag, title, tail int
}

func newTopPlanLayout(plans []engine.StatusPlan, tagged bool, running map[int64]bool, style term.Style, width int) topPlanLayout {
	var l topPlanLayout
	for _, p := range plans {
		l.id = max(l.id, len(fmt.Sprintf("#%d", p.ID)))
		if tagged {
			l.tag = max(l.tag, utf8.RuneCountInString(topRepoName(p.RepoURL)))
		}
		l.title = max(l.title, utf8.RuneCountInString(p.Title))
		l.tail = max(l.tail, term.VisibleWidth(topPlanTail(p, running, width, style)))
	}
	fixed := l.id + 1 + l.tail + 1
	if l.tag > 0 {
		fixed += l.tag + 2
	}
	l.title = min(l.title, max(width-fixed, 4))
	return l
}

// topPlanLine is a plan's own line: the id, the tag when the layout has
// tags and the title on the left, the tail (topPlanTail) right-aligned to
// the edge. A live plan's id is Bold. The tag sits Dim after the id and
// the title gives way to it.
func topPlanLine(p engine.StatusPlan, tag string, l topPlanLayout, running map[int64]bool, style term.Style, width int) string {
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
	tail := topPlanTail(p, running, width, style)
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
// for: a teal ⇡, the task's id and title, and `landing: ...` with what the
// landing waits on — `queued` for a land job in the queue, the land_ready
// gate's own words with the stored retry stamp as a local clock, or `not
// landed` when the task carries no word.
func topLandingRow(t engine.StatusTask, now time.Time, style term.Style) *topJobRow {
	what := "not landed"
	switch t.LandReady {
	case "":
	case "ready":
		what = "queued"
	default:
		what = topLandReadyWords(t.LandReady, now)
	}
	return &topJobRow{mark: style.Up(), markFn: style.Teal, waitFn: style.Teal,
		id: fmt.Sprintf("#%d", t.ID), title: t.Title, wait: "landing: " + what}
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

// topParkedLine is a live task parked for you: Bad, with the needs-you
// reason, or the task's state when it has none.
func topParkedLine(t engine.StatusTask, reason string, style term.Style, width int) string {
	if reason == "" {
		reason = t.State
	}
	return "  " + style.Bad(cutLine(fmt.Sprintf("! #%d %s %s", t.ID, t.Title, reason), width-2))
}

// topWaitRow is a live task no job speaks for, all Dim: `(queued)`, or
// `(waiting on ...)` for the dependencies it names, or `(review)` for a
// task reviewing with no job running.
func topWaitRow(t engine.StatusTask, style term.Style) *topJobRow {
	wait := "(queued)"
	switch {
	case t.State == "reviewing":
		wait = "(review)"
	case len(t.WaitingOn) > 0:
		wait = "(waiting on " + strings.Join(t.WaitingOn, ", ") + ")"
	}
	return &topJobRow{mark: style.Dot(), markFn: style.Dim, waitFn: style.Dim, dim: true,
		id: fmt.Sprintf("#%d", t.ID), title: t.Title, wait: wait}
}

// topNeedsYou is the rows of the NEEDS YOU box, the first section under
// the header: one Bad row per entry, cut to width and capped at three
// entry rows with one dim "+N more" row under them. The cap never hides a
// broken agent while a row remains (topNeedsYouShown). The box is always
// visible: an empty queue renders one dim row, `✓ nothing needs you`.
func topNeedsYou(st api.Status, sole bool, style term.Style, width int) []string {
	if len(st.NeedsYou) == 0 {
		return []string{style.Dim(style.Check() + " nothing needs you")}
	}
	var lines []string
	shown := topNeedsYouShown(st.NeedsYou)
	// An entry with an action keeps it whole at the end of its row and the
	// reason is cut instead; the tails start in one column, placed so the
	// widest tail ends at the right edge.
	texts := make([]string, len(shown))
	tails := make([]string, len(shown))
	tailW := 0
	for i, n := range shown {
		texts[i], tails[i] = topNeedsYouLine(n), topNeedsYouTail(n, st, sole, style)
		tailW = max(tailW, utf8.RuneCountInString(tails[i]))
	}
	col := max(width-tailW-2, 4)
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
// except the foreman's report, which the EVENTS border says.
func topShows(it history.Item) bool {
	if it.Kind == "foreman_report" {
		return false
	}
	_, ok := topEventWords(it, time.Time{})
	return ok
}

// topEventLines is the EVENTS section's rows: the newest kept
// items, newest first, one per row, at most topEventRows. A row is the
// time, two spaces and the event's words, the time column padded to the
// widest time of the rows built so the words start in one column; each
// row is cut to width. With no items the section is a dim
// `no events`. Row 1 yields last (yieldRest), row 2 in
// yieldEventsSecond and rows 3 and up in yieldEventsExtra, the first
// class to go.
func topEventLines(items []history.Item, now time.Time, style term.Style, width int) []topLine {
	if len(items) == 0 {
		return []topLine{{text: style.Dim("no events"), class: yieldRest, section: topSecEvents}}
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
