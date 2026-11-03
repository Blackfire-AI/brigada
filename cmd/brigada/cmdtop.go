package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
// capacity snapshot, the projects (to name a repo's project) and the
// newest history page — all endpoints that exist. Tests stub it.
type topClient interface {
	Status(ctx context.Context) (api.Status, error)
	Capacity(ctx context.Context, projectID int64) (engine.CapacitySnapshot, error)
	Projects(ctx context.Context) ([]engine.ProjectInfo, error)
	History(ctx context.Context, q api.HistoryQuery) (history.Page, error)
}

// topEventsLimit is the history page top fetches: the newest items,
// enough to fill any terminal's events section.
const topEventsLimit = 50

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
	fs.StringVar(&project, "project", "", "the repository URL whose project to show (default: the working directory's repository)")
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
		if code := printTopSnapshot(ctx, client, repo, stdout, stderr); code != 0 {
			return code
		}
		if hint := agentsHint(ctx, rt, true); hint != "" {
			fmt.Fprintln(stdout, hint)
		}
		return 0
	}
	return topLoop(ctx, rt, repo, interval, noStart, stdout)
}

// printTopSnapshot fetches once and prints the plain rendering; the
// loop never runs here. Any fetch error is a failure, not a line.
func printTopSnapshot(ctx context.Context, client topClient, repo string, stdout, stderr io.Writer) int {
	model, err := topFetch(ctx, client, repo)
	if err != nil {
		return fail(stderr, err)
	}
	style := term.New(detectCaps(stdout))
	fmt.Fprintln(stdout, strings.Join(renderTop(model, style.Width(), 0, time.Now(), style), "\n"))
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
	// err is the fetch's first error: the view shows it as a line and
	// the loop keeps retrying. Parts that did fetch still render.
	err error
	// paused says the loop froze the refresh (the p key).
	paused bool
	// hint is the unregistered-models line of the cached listing, or "".
	hint string
}

// topFetch gathers one tick's model: the project the repo names, then
// the status, the capacity snapshot and the newest history page. A part
// that fails leaves its zero value and notes the error; the first error
// comes back too, so the snapshot path can fail while the loop shows the
// line and retries.
func topFetch(ctx context.Context, client topClient, repo string) (topModel, error) {
	projectID, err := lookupProjectID(ctx, client, repo)
	if err != nil {
		return topModel{err: err}, err
	}
	var model topModel
	if st, err := client.Status(ctx); err != nil {
		model.err = err
	} else {
		model.status = st
	}
	if snap, err := client.Capacity(ctx, projectID); err == nil {
		model.capacity = snap
	} else if model.err == nil {
		model.err = err
	}
	if page, err := client.History(ctx, api.HistoryQuery{Limit: topEventsLimit, Last: true}); err == nil {
		model.events = page.Items
	} else if model.err == nil {
		model.err = err
	}
	return model, model.err
}

// keepPrevious fills the parts a failed fetch left empty from the last
// tick's model, so an error line sits over the last data instead of a
// blank screen.
func (m topModel) keepPrevious(prev topModel) topModel {
	if m.status.Pid == 0 && m.status.StartedAt == "" {
		m.status = prev.status
	}
	if len(m.capacity.Agents) == 0 && len(m.capacity.Layers) == 0 {
		m.capacity = prev.capacity
	}
	if m.events == nil {
		m.events = prev.events
	}
	return m
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
// strip, NEEDS YOU, the loose jobs no plan claims (only when there are
// any), the PLANS tree with every job under its plan, the AGENTS
// section and the events strip at the bottom, every line cut to width
// and the whole trimmed to height — height 0 means no height limit — so
// a later section yields its lines before an earlier one. With a plain
// style the text is the same and has no escapes.
func renderTop(model topModel, width, height int, now time.Time, style term.Style) []string {
	agents := api.AgentsTable(model.capacity, now, style, width)
	if global := globalCap(model.capacity); global != nil {
		agents = append(agents, style.Dim(fmt.Sprintf("all \u2264 global %d", *global)))
	}
	sections := [][]string{
		topHeader(model, now, style, width),
		topMeters(model, style, width),
		topNeedsYou(model.status, style, width),
	}
	if loose := topLoose(model.status, now, style, width); len(loose) > 0 {
		sections = append(sections, loose)
	}
	sections = append(sections,
		topPlansTree(model.status, now, style, width),
		agents,
		topEvents(model.events, now, style, width),
	)
	var lines []string
	for i, section := range sections {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, section...)
	}
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, line := range lines {
		lines[i] = topFit(line, width)
	}
	return lines
}

// topFit cuts one styled line to width: a line that fits keeps its
// colour; one that overflows is cut on its plain text, so no line wraps.
func topFit(line string, width int) string {
	if term.VisibleWidth(line) <= width {
		return line
	}
	return cutLine(term.StripEscapes(line), width)
}

// topHeader is the first line of the view: the engine's pid, its uptime
// and the clock, then a dim rule filling the width that remains and the
// keys at the right end. A failed fetch adds its error as a second
// line, so the view keeps its shape while the loop retries.
func topHeader(model topModel, now time.Time, style term.Style, width int) []string {
	st := model.status
	uptime := "0s"
	if started := parseStartedAt(st.StartedAt); !started.IsZero() {
		uptime = formatUptime(now.Sub(started))
	}
	keys := "q quit · p pause"
	if model.paused {
		keys = "paused; p resumes, q quits"
	}
	left := fmt.Sprintf("brigada top · pid %d · up %s · %s", st.Pid, uptime, now.Format("15:04:05"))
	rule := width - term.VisibleWidth(left) - term.VisibleWidth(keys) - 2
	if rule < 1 {
		rule = 1
	}
	line := left + " " + style.Dim(strings.Repeat("─", rule)) + " " + keys
	lines := []string{topFit(line, width)}
	if model.err != nil {
		lines = append(lines, style.Bad(cutLine("engine: "+model.err.Error(), width)))
	}
	if model.hint != "" {
		lines = append(lines, cutLine(model.hint, width))
	}
	return lines
}

// globalCap is the snapshot's global layer cap, nil when there is no
// global layer or the layer is uncapped; it is the line under the AGENTS
// table.
func globalCap(snap engine.CapacitySnapshot) *int {
	for _, l := range snap.Layers {
		if l.Layer == caps.LayerGlobal {
			return l.Cap
		}
	}
	return nil
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
				bar = topSlotBar(use, *limit) + " "
			}
		}
		cell = "SLOTS " + bar + useLimit
	}
	parts := []string{
		cell,
		style.Bold(fmt.Sprintf("%d run", len(st.Running))),
		fmt.Sprintf("%d queue", st.Jobs["queued"]),
		style.Warn(fmt.Sprintf("%d held", len(st.Held))),
		style.Bad(fmt.Sprintf("%d needs", len(st.NeedsYou))),
	}
	return []string{topFit(strings.Join(parts, "   "), width)}
}

// topSlotBar is the meters' slots bar: the global use against its cap
// as at most ten cells — ▓ filled and ░ empty in proportion, at least
// one ▓ whenever the use is over zero, so a sliver of use always shows.
func topSlotBar(use, limit int) string {
	cells := min(limit, 10)
	filled := use * cells / limit
	if use > 0 && filled == 0 {
		filled = 1
	}
	filled = min(filled, cells)
	return strings.Repeat("▓", filled) + strings.Repeat("░", cells-filled)
}

// topPlanFinished says a plan's State has ended it: done or cancelled.
func topPlanFinished(p engine.StatusPlan) bool {
	return p.State == "done" || p.State == "cancelled"
}

// topTaskSettled says a task's state leaves it no live work: done,
// cancelled or retired. Its plan's done/total already counts it, so the
// tree gives it no line of its own.
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
// review is loose; a finished plan never carries a live job.
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
// review job; ok is false for a loose job.
func (c topClaims) plan(jobID, taskID int64) (int64, bool) {
	if taskID != 0 {
		id, ok := c.taskPlan[taskID]
		return id, ok
	}
	id, ok := c.reviewPlan[jobID]
	return id, ok
}

// topRunningLine is one running job under its task, its plan or in the
// loose block: the Accent pointer and status's own words for the job.
func topRunningLine(j engine.StatusJob, now time.Time, style term.Style, width int) string {
	return "  " + style.Accent("▸") + " " + cutLine(api.DescribeJob(j, now), width-4)
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
	return "  " + style.Dim(cutLine("↺ "+api.RetryLine(r, now), width-2))
}

// topLoose is the block under NEEDS YOU, with no head: the running,
// held and retrying jobs no live plan claims — a foreman turn, the
// review of a plan the tree does not list — so no job goes unseen. It
// is empty, and renderTop leaves it out, when every job has its plan.
func topLoose(st api.Status, now time.Time, style term.Style, width int) []string {
	claims := newTopClaims(st)
	var lines []string
	for _, j := range st.Running {
		if _, ok := claims.plan(j.ID, j.TaskID); !ok {
			lines = append(lines, topRunningLine(j, now, style, width))
		}
	}
	for _, h := range st.Held {
		if _, ok := claims.plan(h.ID, h.TaskID); !ok {
			lines = append(lines, topHeldLine(h, style, width))
		}
	}
	for _, r := range st.Retries {
		if _, ok := claims.plan(r.ID, r.TaskID); !ok {
			lines = append(lines, topRetryLine(r, now, style, width))
		}
	}
	return lines
}

// topPlansTree is the PLANS section: every plan the status lists, each
// live one with its live work beneath it — the running jobs first, then
// a line per task still waiting, held, retrying, parked or reviewing —
// and each finished one a single line. A Dim "+N finished" line counts
// the finished plans the status hid.
func topPlansTree(st api.Status, now time.Time, style term.Style, width int) []string {
	lines := []string{topSectionHead("PLANS", style, width)}
	if len(st.Plans) == 0 {
		lines = append(lines, style.Dim("none"))
	}
	claims := newTopClaims(st)
	reasons := make(map[int64]string)
	for _, n := range st.NeedsYou {
		if n.Kind == "task" {
			reasons[n.ID] = n.Reason
		}
	}
	for _, p := range st.Plans {
		lines = append(lines, topPlanLine(p, style, width))
		if topPlanFinished(p) {
			continue
		}
		// Which tasks a job already speaks for: a task with a running,
		// held or retrying job shows as that job's line, not its state's.
		busy := make(map[int64]bool)
		for _, j := range st.Running {
			if id, ok := claims.plan(j.ID, j.TaskID); ok && id == p.ID {
				lines = append(lines, topRunningLine(j, now, style, width))
				busy[j.TaskID] = true
			}
		}
		for _, h := range st.Held {
			if id, ok := claims.plan(h.ID, h.TaskID); ok && id == p.ID && h.TaskID == 0 {
				lines = append(lines, topHeldLine(h, style, width))
			}
		}
		for _, r := range st.Retries {
			if id, ok := claims.plan(r.ID, r.TaskID); ok && id == p.ID && r.TaskID == 0 {
				lines = append(lines, topRetryLine(r, now, style, width))
			}
		}
		for _, t := range p.Tasks {
			for _, h := range st.Held {
				if h.TaskID == t.ID {
					lines = append(lines, topHeldLine(h, style, width))
					busy[t.ID] = true
				}
			}
			for _, r := range st.Retries {
				if r.TaskID == t.ID {
					lines = append(lines, topRetryLine(r, now, style, width))
					busy[t.ID] = true
				}
			}
			if busy[t.ID] || topTaskSettled(t.State) {
				continue
			}
			if line := topTaskLine(t, reasons[t.ID], style, width); line != "" {
				lines = append(lines, line)
			}
		}
	}
	if st.HiddenFinishedPlans > 0 {
		lines = append(lines, style.Dim(fmt.Sprintf("+%d finished", st.HiddenFinishedPlans)))
	}
	return lines
}

// topPlanLine is a plan's own line. A finished plan's mark comes from
// its State: `#id title done/total ✓` for done, `#id title ✗` for
// cancelled. A live plan's is the Bold id, the title, done/total and an
// eleven-cell progress bar — never a mark, even with every task done.
func topPlanLine(p engine.StatusPlan, style term.Style, width int) string {
	done := 0
	for _, t := range p.Tasks {
		if t.State == "done" {
			done++
		}
	}
	count := fmt.Sprintf("%d/%d", done, len(p.Tasks))
	id := fmt.Sprintf("#%d", p.ID)
	var tail string
	switch p.State {
	case "done":
		tail = count + " " + style.OK("✓")
	case "cancelled":
		tail = style.Bad("✗")
	default:
		tail = count + " " + topPlanBar(done, len(p.Tasks), style)
	}
	room := width - term.VisibleWidth(id) - term.VisibleWidth(tail) - 2
	title := cutLine(p.Title, max(room, 4))
	if !topPlanFinished(p) {
		id = style.Bold(id)
	}
	return id + " " + title + " " + tail
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
		bar = style.OK(strings.Repeat("▓", filled))
	}
	if filled < cells {
		bar += style.Dim(strings.Repeat("░", cells-filled))
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
		return "  " + style.Dim(cutLine("· "+subject+" (review)", width-2))
	}
	wait := "(queued)"
	if len(t.WaitingOn) > 0 {
		wait = "(waiting on " + strings.Join(t.WaitingOn, ", ") + ")"
	}
	return "  " + style.Dim(cutLine("· "+subject+" "+wait, width-2))
}

// topSectionHead is a section's head line: the Bold title and a Dim
// rule filling the width.
func topSectionHead(title string, style term.Style, width int) string {
	rule := width - term.VisibleWidth(title) - 1
	if rule < 1 {
		rule = 1
	}
	return style.Bold(title) + " " + style.Dim(strings.Repeat("─", rule))
}

// topNeedsYou is the needs-you queue at the top of the view, under the
// meters and before every other section: the header line carries a rule
// fill to the width, then one Bad line per entry, cut to width and
// capped at three entry lines with one dim "+N more" line under them.
// The section is always visible: an empty queue keeps the header and
// renders one dim line under it.
func topNeedsYou(st api.Status, style term.Style, width int) []string {
	lines := []string{topSectionHead("NEEDS YOU", style, width)}
	if len(st.NeedsYou) == 0 {
		return append(lines, style.Dim("nothing needs you"))
	}
	for i, n := range st.NeedsYou {
		if i == 3 {
			lines = append(lines, style.Dim(fmt.Sprintf("+%d more", len(st.NeedsYou)-i)))
			break
		}
		lines = append(lines, style.Bad(cutLine(topNeedsYouLine(n), width)))
	}
	return lines
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
	if reason == "" {
		reason = n.State
	}
	return "! " + subject + " " + reason
}

// topEvents is the strip at the bottom: the newest history items,
// newest first, joined by " · " into one line cut to width. Wrapping
// the strip over two lines is a later task's; topEventItem renders each
// item for both.
func topEvents(items []history.Item, now time.Time, style term.Style, width int) []string {
	if len(items) == 0 {
		return []string{style.Dim("no events")}
	}
	parts := make([]string, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		parts[len(items)-1-i] = topEventItem(items[i], now, style)
	}
	return []string{topFit(strings.Join(parts, " · "), width)}
}

// topEventItem is one history item as the events strip shows it: its
// clock time and its one-line gist, the state words coloured.
func topEventItem(it history.Item, now time.Time, style term.Style) string {
	return topClock(it.CreatedAt, now) + " " + topColourStates(history.Line(it), style)
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

// topClock is a stored stamp as a clock time in now's zone: the hour,
// minute and second on now's day, the date too on any other day. A
// stamp that does not parse shows as it is.
func topClock(stamp string, now time.Time) string {
	t, err := time.Parse(engine.TimeLayout, stamp)
	if err != nil {
		return stamp
	}
	t = t.In(now.Location())
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04:05")
	}
	return t.Format("01-02 15:04:05")
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
		fetched, err := topFetch(ctx, client, repo)
		fetched.err = err
		fetched.paused = model.paused
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
