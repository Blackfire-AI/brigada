package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
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

// renderTop lays the view out, top to bottom: the header, then the
// AGENTS, RUNNING, QUEUE, PLANS, NEEDS YOU and EVENTS sections, every
// line cut to width and the whole trimmed to height — height 0 means no
// height limit — so a later section yields its lines before an earlier
// one. With a plain style the text is the same and has no escapes.
func renderTop(model topModel, width, height int, now time.Time, style term.Style) []string {
	sections := [][]string{
		topHeader(model, now, style, width),
		api.AgentsTable(model.capacity, now, style, width),
		topRunning(model.status, now, style, width),
		topQueue(model.status, style, width),
		topPlans(model.status, style, width),
		topNeedsYou(model.status, style, width),
		topEvents(model.events, now, style, width),
	}
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

// topHeader is the first line of the view: the engine's pid and
// uptime, the runners, the global slot use against its cap, the time,
// and the keys. A failed fetch adds its error as a second line, so the
// view keeps its shape while the loop retries.
func topHeader(model topModel, now time.Time, style term.Style, width int) []string {
	st := model.status
	uptime := "0s"
	if started := parseStartedAt(st.StartedAt); !started.IsZero() {
		uptime = formatUptime(now.Sub(started))
	}
	runners, used, limit := topSlots(model.capacity)
	keys := "q quits, p pauses"
	if model.paused {
		keys = "paused; p resumes, q quits"
	}
	line := fmt.Sprintf("brigada top · pid %d · up %s · runners %d · slots %s/%s · %s · %s",
		st.Pid, uptime, runners, used, limit, now.Format("15:04:05"), keys)
	lines := []string{cutLine(line, width)}
	if model.err != nil {
		lines = append(lines, style.Bad(cutLine("engine: "+model.err.Error(), width)))
	}
	if model.hint != "" {
		lines = append(lines, cutLine(model.hint, width))
	}
	return lines
}

// topSlots reads the header's runner count and the global cap layer's
// use and cap from the capacity snapshot: "?" when no global layer came
// back, as with a failed capacity fetch.
func topSlots(snap engine.CapacitySnapshot) (runners int, used, limit string) {
	for _, l := range snap.Layers {
		switch l.Layer {
		case caps.LayerRunner:
			runners++
		case caps.LayerGlobal:
			used = strconv.Itoa(l.Use)
			limit = "none"
			if l.Cap != nil {
				limit = strconv.Itoa(*l.Cap)
			}
		}
	}
	if used == "" {
		used, limit = "?", "?"
	}
	return runners, used, limit
}

// topRunning lists the jobs a runner holds, one line each: job, task,
// kind, agent, age and the worker's latest move.
func topRunning(st api.Status, now time.Time, style term.Style, width int) []string {
	lines := []string{style.Bold("RUNNING")}
	if len(st.Running) == 0 {
		return append(lines, "  none")
	}
	for _, j := range st.Running {
		line := fmt.Sprintf("job #%d", j.ID)
		if j.TaskID != 0 {
			line += fmt.Sprintf(" task #%d", j.TaskID)
		}
		line += " " + j.Kind
		if j.Harness != "" {
			agent := j.Harness + "/" + j.Model
			if j.Effort != "" {
				agent += "@" + j.Effort
			}
			line += " " + agent
		}
		if leased, err := time.Parse(engine.TimeLayout, j.LeasedAt); err == nil {
			line += " " + formatUptime(now.Sub(leased))
		}
		if j.LastEvent != "" {
			line += " " + j.LastEvent
		}
		lines = append(lines, "  "+cutLine(line, width-2))
	}
	return lines
}

// topQueue counts the queued jobs and lists the held ones with the
// layer that holds each.
func topQueue(st api.Status, style term.Style, width int) []string {
	lines := []string{style.Bold("QUEUE")}
	queued := st.Jobs["queued"]
	if queued == 0 && len(st.Held) == 0 {
		return append(lines, "  empty")
	}
	if queued > 0 {
		lines = append(lines, fmt.Sprintf("  %d queued", queued))
	}
	for _, h := range st.Held {
		line := fmt.Sprintf("held: job #%d %s by %s", h.ID, h.Kind, h.Layer)
		if h.Agent != "" {
			line += " (" + h.Agent + ")"
		}
		lines = append(lines, "  "+cutLine(line, width-2))
	}
	return lines
}

// topPlans lists the active plans with done/total tasks, and under
// each the tasks parked at needs_foreman or needs_user with the reason
// the needs-you queue carries for them.
func topPlans(st api.Status, style term.Style, width int) []string {
	lines := []string{style.Bold("PLANS")}
	var active []engine.StatusPlan
	for _, p := range st.Plans {
		if p.State == "active" {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		return append(lines, "  none")
	}
	reasons := make(map[int64]string)
	for _, n := range st.NeedsYou {
		if n.Kind == "task" {
			reasons[n.ID] = n.Reason
		}
	}
	for _, p := range active {
		done := 0
		for _, t := range p.Tasks {
			if t.State == "done" {
				done++
			}
		}
		lines = append(lines, cutLine(fmt.Sprintf("  #%d %s %s %d/%d", p.ID, p.State, p.Title, done, len(p.Tasks)), width))
		for _, t := range p.Tasks {
			if t.State != "needs_foreman" && t.State != "needs_user" {
				continue
			}
			line := fmt.Sprintf("    task #%d %s %s", t.ID, t.State, t.Title)
			if reason := reasons[t.ID]; reason != "" {
				line += ": " + reason
			}
			lines = append(lines, cutLine(line, width))
		}
	}
	return lines
}

// topNeedsYou lists the needs-you queue, one line per entry.
func topNeedsYou(st api.Status, style term.Style, width int) []string {
	lines := []string{style.Bold("NEEDS YOU")}
	if len(st.NeedsYou) == 0 {
		return append(lines, "  nothing")
	}
	for _, n := range st.NeedsYou {
		line := fmt.Sprintf("%s #%d %s", n.Kind, n.ID, n.State)
		if n.Kind == "agent" {
			line = fmt.Sprintf("agent %s %s", n.Title, n.State) // named as `brigada agent reset` takes it
		}
		if n.Reason != "" {
			line += ": " + n.Reason
		}
		lines = append(lines, "  "+cutLine(line, width-2))
	}
	return lines
}

// topEvents lists the newest history items, newest first, each as its
// time and its one-line gist; the layout leaves as many lines as fit.
func topEvents(items []history.Item, now time.Time, style term.Style, width int) []string {
	lines := []string{style.Bold("EVENTS")}
	if len(items) == 0 {
		return append(lines, "  none")
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		lines = append(lines, "  "+cutLine(topClock(it.CreatedAt, now)+" "+history.Line(it), width-2))
	}
	return lines
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
