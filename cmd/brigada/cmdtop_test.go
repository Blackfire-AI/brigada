package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// fakeTop stubs the top client: what it serves, what fails and what the
// calls asked. status is the unscoped answer; scopedStatus is a project
// reader's, and when nil the reader serves status without its agent
// entries, as the engine does.
type fakeTop struct {
	status          api.Status
	scopedStatus    *api.Status
	capacity        engine.CapacitySnapshot
	projects        []engine.ProjectInfo
	page            history.Page
	statusErr       error
	scopedStatusErr error
	capacityErr     error
	projectsErr     error
	historyErr      error

	statusCalls       int
	scopedStatusCalls int
	projectsCalls     int
	readerProject     int64
	capacityProject   int64
	historyProject    int64
}

func (f *fakeTop) Status(context.Context) (api.Status, error) {
	f.statusCalls++
	return f.status, f.statusErr
}

func (f *fakeTop) Capacity(_ context.Context, projectID int64) (engine.CapacitySnapshot, error) {
	f.capacityProject = projectID
	return f.capacity, f.capacityErr
}

func (f *fakeTop) Projects(context.Context) ([]engine.ProjectInfo, error) {
	f.projectsCalls++
	return f.projects, f.projectsErr
}

func (f *fakeTop) History(_ context.Context, q api.HistoryQuery) (history.Page, error) {
	f.historyProject = q.ProjectID
	return f.page, f.historyErr
}

func (f *fakeTop) forProject(projectID int64) topClient {
	f.readerProject = projectID
	return fakeTopReader{fakeTop: f, project: projectID}
}

// fakeTopReader is fakeTop's project reader, the seam ProjectReader is on
// the real client: a read with no project of its own falls back to the
// reader's.
type fakeTopReader struct {
	*fakeTop
	project int64
}

func (r fakeTopReader) Status(context.Context) (api.Status, error) {
	r.scopedStatusCalls++
	if r.scopedStatus != nil {
		return *r.scopedStatus, r.scopedStatusErr
	}
	st := r.status
	st.NeedsYou = nil
	for _, n := range r.status.NeedsYou {
		if n.Kind != "agent" {
			st.NeedsYou = append(st.NeedsYou, n)
		}
	}
	return st, r.scopedStatusErr
}

func (r fakeTopReader) Capacity(ctx context.Context, projectID int64) (engine.CapacitySnapshot, error) {
	if projectID == 0 {
		projectID = r.project
	}
	return r.fakeTop.Capacity(ctx, projectID)
}

func (r fakeTopReader) History(ctx context.Context, q api.HistoryQuery) (history.Page, error) {
	if q.ProjectID == 0 {
		q.ProjectID = r.project
	}
	return r.fakeTop.History(ctx, q)
}

// topNow is the tests' clock: a fixed instant.
var topNow = time.Date(2026, 10, 20, 15, 4, 9, 0, time.UTC)

// busyTopModel is one full view's data: an agent, an active plan with a
// done task, a parked one, one running and one held, the needs-you
// queue and events.
func busyTopModel() topModel {
	eight := 8
	leased := topNow.Add(-100 * time.Second).UTC().Format(engine.TimeLayout)
	return topModel{
		scope: "r",
		status: api.Status{
			Pid:       4242,
			StartedAt: topNow.Add(-2 * time.Hour).UTC().Format(engine.TimeLayout),
			Jobs:      map[string]int{"queued": 2},
			Running: []engine.StatusJob{
				{ID: 5, TaskID: 3, Kind: "work", Status: "running", Harness: "claude", Model: "sonnet",
					Effort: "high", LeasedAt: leased, LastEvent: "tool_start Edit"},
			},
			Held: []engine.HeldJob{{ID: 6, TaskID: 4, Kind: "work", Layer: "runner"}},
			Plans: []engine.StatusPlan{{
				ID: 1, Title: "fix the frobnicator", State: "active",
				Tasks: []engine.StatusTask{
					{ID: 1, Title: "the frobnicator", State: "done"},
					{ID: 2, Title: "wire the sprocket", State: "needs_foreman"},
					{ID: 3, Title: "oil the gears", State: "running"},
					{ID: 4, Title: "polish the knobs", State: "ready"},
				},
			}},
			NeedsYou: []engine.NeedsYouItem{
				{Kind: "task", ID: 2, State: "needs_foreman", Title: "wire the sprocket", Reason: "should the sprocket be blue?"},
			},
		},
		capacity: engine.CapacitySnapshot{
			Agents: []engine.CapacityAgent{
				{Harness: "claude", Model: "opus", Tier: "heavy", Effort: "high", Cost: 3, CapLayer: "none", Running: 1, Quota: "ready"},
			},
			Layers: []engine.CapacityLayer{{Layer: "global", Cap: &eight, Use: 3}},
		},
		events: []history.Item{
			{Type: history.TypeEvent, ID: 11, Kind: "task_landed", ProjectID: 1, TaskID: 3,
				Payload: []byte(`{"branch":"main","pushed":"pushed"}`), CreatedAt: "2026-10-20T15:02:41.000000000Z"},
			{Type: history.TypeEvent, ID: 12, Kind: "task_done", ProjectID: 1, TaskID: 3,
				Payload: []byte(`{"status":"done"}`), CreatedAt: "2026-10-20T15:02:45.000000000Z"},
		},
	}
}

func plainTop(model topModel, width, height int) string {
	return strings.Join(renderTop(model, width, height, topNow, term.New(logo.Caps{UTF8: true})), "\n")
}

// readTop is plainTop with its boxes read down (topBoxed): the text a test
// looks for without the borders.
func readTop(model topModel, width, height int) string {
	return strings.Join(topBoxed(strings.Split(plainTop(model, width, height), "\n")), "\n")
}

// topBoxed is a plain frame with its boxes read down: a top border as
// `╭ TITLE` (its title alone, no count, note or fill), a bottom border as
// `╰`, a row as its text with the sides and the padding taken off. Lines
// outside a box stay as they are.
func topBoxed(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "╭─ "):
			title, _, _ := strings.Cut(strings.TrimPrefix(l, "╭─ "), "  ")
			title, _, _ = strings.Cut(title, " ─")
			out[i] = "╭ " + title
		case strings.HasPrefix(l, "╰"):
			out[i] = "╰"
		case strings.HasPrefix(l, "│ "):
			out[i] = strings.TrimRight(strings.TrimSuffix(strings.TrimPrefix(l, "│ "), " │"), " ")
		default:
			out[i] = l
		}
	}
	return out
}

// TestRenderTopSections: the view's order — header, meters, NEEDS YOU,
// the FOREMAN block, the PLANS tree, AGENTS, then the events section — with
// every section's text in its place.
func TestRenderTopSections(t *testing.T) {
	model := busyTopModel()
	model.statusOK = true
	model.status.Running = append(model.status.Running,
		engine.StatusJob{ID: 30, Kind: "foreman", Status: "running"})
	got := plainTop(model, 100, 0)
	want := []string{
		" ▙ brigada   r · up 2h0m",
		"15:04:09   q quit  p pause",
		"━━━━━━━━━━━━━━━━━━  3/8 slots",
		"2 run  2 queue  1 held  1 needs",
		"╭─ NEEDS YOU 1 ─",
		"│ ! #2 should the sprocket be blue?",
		"╭─ FOREMAN ─",
		"│   ▸  job #30  foreman",
		"╭─ PLANS  1 live · 0 finished ─",
		"■ landed  ■ done  ■ running  □ queued ─╮",
		"│ #1 fix the frobnicator",
		"■■■□  1/4 │",
		"│   ▸  #3  oil the gears     work  claude/sonnet@high  1m40s  Edit",
		"│   ! #2 wire the sprocket should the sprocket be blue?",
		"│   ?  #4  polish the knobs  held: waiting for a runner slot",
		"╭─ AGENTS  all ≤ global 8 ─",
		"│ ● claude/opus  ■□□□□□□□",
		"╭─ EVENTS ─",
		"│ 15:02  #3 done",
		"│ 15:02  #3 landed",
	}
	last := -1
	for _, w := range want {
		i := strings.Index(got, w)
		if i < 0 {
			t.Errorf("view lacks %q:\n%s", w, got)
			continue
		}
		if i < last {
			t.Errorf("%q renders out of order:\n%s", w, got)
		}
		last = i
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("plain view has escapes:\n%q", got)
	}
}

// TestTopHeaderRowOne: the header's first row is one leading space, the
// mark, the scope and uptime on the left, the clock and the keys at the
// right edge, filling the width exactly.
func TestTopHeaderRowOne(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	left := " ▙ brigada   r · up 2h0m"
	right := "15:04:09   q quit  p pause"
	want := left + strings.Repeat(" ", 80-term.VisibleWidth(left)-term.VisibleWidth(right)) + right
	if got := topHeader(busyTopModel(), topNow, style, 80); got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
	paused := busyTopModel()
	paused.paused = true
	keys := "15:04:09   paused; p resumes, q quits"
	if got := topHeader(paused, topNow, style, 80); got != left+strings.Repeat(" ", 80-term.VisibleWidth(left)-term.VisibleWidth(keys))+keys {
		t.Errorf("paused header = %q", got)
	}
}

// TestTopHeaderMark: the mark is the brand's one-line form, only its ▙
// coloured, exactly as logo prints it at each depth; plain and NO_COLOR
// print it bare; without UTF-8 the header reads the word alone.
func TestTopHeaderMark(t *testing.T) {
	for _, tt := range []struct {
		name string
		caps logo.Caps
		want string
	}{
		{"true colour", logo.Caps{Color: logo.TrueColor, UTF8: true}, "\x1b[38;2;255;90;31m▙\x1b[0m brigada"},
		{"256 colours", logo.Caps{Color: logo.Color256, UTF8: true}, "\x1b[38;5;202m▙\x1b[0m brigada"},
		{"16 colours", logo.Caps{Color: logo.Color16, UTF8: true}, "\x1b[31m▙\x1b[0m brigada"},
		{"plain", logo.Caps{UTF8: true}, "▙ brigada"},
		{"no color", logo.Caps{Color: logo.NoColor, UTF8: true}, "▙ brigada"},
		{"ascii", logo.Caps{}, "brigada"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := topHeader(busyTopModel(), topNow, term.New(tt.caps), 80)
			if !strings.HasPrefix(got, " "+tt.want+"   r") {
				t.Errorf("header = %q, want it to start with the mark %q", got, tt.want)
			}
			if tt.caps.Color == logo.NoColor && strings.Contains(got, "\x1b") {
				t.Errorf("header has escapes: %q", got)
			}
			if !tt.caps.UTF8 && (strings.Contains(got, "▙") || strings.Contains(got, "▲")) {
				t.Errorf("ascii header has a mark: %q", got)
			}
		})
	}
}

// TestTopHeaderHiVisOnlyOnTheMark: in a whole true-colour or 256-colour
// frame the hi-vis escape appears once, on the ▙.
func TestTopHeaderHiVisOnlyOnTheMark(t *testing.T) {
	for _, tt := range []struct {
		caps logo.Caps
		hi   string
	}{
		{logo.Caps{Color: logo.TrueColor, UTF8: true}, "38;2;255;90;31"},
		{logo.Caps{Color: logo.Color256, UTF8: true}, "38;5;202"},
	} {
		frame := strings.Join(renderTop(busyTopModel(), 100, 0, topNow, term.New(tt.caps)), "\n")
		if n := strings.Count(frame, tt.hi); n != 1 {
			t.Errorf("%s appears %d times, want once:\n%q", tt.hi, n, frame)
		}
		if !strings.Contains(frame, "\x1b["+tt.hi+"m▙\x1b[0m") {
			t.Errorf("the hi-vis escape is not on the ▙: %q", frame)
		}
	}
}

// TestTopMetersSlotsCell: the meter is 18 cells of ━ in proportion to the
// cap, at least one filled for a sliver of use, and there is none for a
// cap of zero, an uncapped layer or a failed fetch; the number carries the
// use either way.
func TestTopMetersSlotsCell(t *testing.T) {
	two, three, twelve, zero := 2, 3, 12, 0
	cells := func(filled int) string { return strings.Repeat("━", filled) + strings.Repeat("━", 18-filled) }
	tests := []struct {
		name   string
		layers []engine.CapacityLayer
		want   string
	}{
		{"cap 3", []engine.CapacityLayer{{Layer: "global", Cap: &three, Use: 2}}, cells(12) + "  2/3 slots"},
		{"cap 2", []engine.CapacityLayer{{Layer: "global", Cap: &two, Use: 1}}, cells(9) + "  1/2 slots"},
		{"at least one filled", []engine.CapacityLayer{{Layer: "global", Cap: &twelve, Use: 1}}, cells(1) + "  1/12 slots"},
		{"cap 0 has no meter", []engine.CapacityLayer{{Layer: "global", Cap: &zero}}, "0/0 slots"},
		{"uncapped has no meter", []engine.CapacityLayer{{Layer: "global", Use: 2}}, "2/none slots"},
		{"failed fetch", nil, "?/? slots"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := busyTopModel()
			model.capacity.Layers = tt.layers
			got := topMeters(model, term.New(logo.Caps{UTF8: true}), 100)
			if !strings.HasPrefix(got, " "+tt.want+" ") {
				t.Errorf("meters = %q, want the slots cell %q", got, tt.want)
			}
		})
	}
}

// TestTopMetersMeterColours: filled cells are Accent and Warn once the use
// reaches the cap, the rest Dim in the same glyph.
func TestTopMetersMeterColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	three, model := 3, busyTopModel()
	model.capacity.Layers = []engine.CapacityLayer{{Layer: "global", Cap: &three, Use: 1}}
	if got, want := topMeters(model, style, 100), style.Accent(strings.Repeat("━", 6))+style.Dim(strings.Repeat("━", 12)); !strings.Contains(got, want) {
		t.Errorf("meters = %q, want %q", got, want)
	}
	model.capacity.Layers = []engine.CapacityLayer{{Layer: "global", Cap: &three, Use: 3}}
	if got, want := topMeters(model, style, 100), style.Warn(strings.Repeat("━", 18)); !strings.Contains(got, want) {
		t.Errorf("meters at the cap = %q, want %q", got, want)
	}
}

// TestTopMetersShrinks: a narrow row shrinks the meter and, under 4 cells,
// leaves it out; the row always fills the width.
func TestTopMetersShrinks(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	for width, cells := range map[int]int{100: 18, 63: 18, 55: 10, 49: 4, 48: 0, 40: 0} {
		got := topMeters(busyTopModel(), style, width)
		if n := strings.Count(got, "━"); n != cells {
			t.Errorf("width %d: %d meter cells in %q, want %d", width, n, got, cells)
		}
		if term.VisibleWidth(got) > width {
			t.Errorf("width %d: row is %d wide", width, term.VisibleWidth(got))
		}
	}
}

// TestTopMetersCountersAndColours: the counters sit at the right edge —
// run bold, queue plain, held warn, needs bad — two spaces apart.
func TestTopMetersCountersAndColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	got := topMeters(busyTopModel(), style, 100)
	want := style.Bold("1 run") + "  2 queue  " + style.Warn("1 held") + "  " + style.Bad("1 needs")
	if !strings.HasSuffix(got, want) {
		t.Errorf("meters = %q, want it to end with %q", got, want)
	}
}

// topCollapse is a row with its runs of spaces made one, so a test of the
// words need not know how far the tail column sits.
func topCollapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestTopNeedsYouCapNeverHidesAgents: a queue over three lines chooses
// its agent entries first, shows the choice in queue order and counts the
// rest; agents alone over the cap fold like any other entry.
func TestTopNeedsYouCapNeverHidesAgents(t *testing.T) {
	var mixed, agents []engine.NeedsYouItem
	for i := 1; i <= 5; i++ {
		mixed = append(mixed, engine.NeedsYouItem{Kind: "task", ID: int64(i), Reason: "why"})
	}
	mixed = append(mixed, engine.NeedsYouItem{Kind: "agent", ID: 6, State: "broken", Title: "dsh/large", Reason: "auth"})
	for i := 1; i <= 4; i++ {
		agents = append(agents, engine.NeedsYouItem{Kind: "agent", ID: int64(i), State: "broken", Title: fmt.Sprintf("dsh/m%d", i), Reason: "auth"})
	}
	for _, tt := range []struct {
		name  string
		queue []engine.NeedsYouItem
		want  []string
	}{
		{"one agent behind tasks", mixed, []string{"! #1 why", "! #2 why", "! dsh/large auth  → brigada agent reset dsh/large", "+3 more (brigada status)"}},
		{"four agents", agents, []string{
			"! dsh/m1 auth  → brigada agent reset dsh/m1",
			"! dsh/m2 auth  → brigada agent reset dsh/m2",
			"! dsh/m3 auth  → brigada agent reset dsh/m3",
			"+1 more (brigada status)"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := topNeedsYou(api.Status{NeedsYou: tt.queue}, false, term.New(logo.Caps{UTF8: true}), 96)
			if len(got) != len(tt.want) {
				t.Fatalf("lines = %q, want %q", got, tt.want)
			}
			for i := range got {
				if topCollapse(got[i]) != topCollapse(tt.want[i]) {
					t.Errorf("line %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestTopNeedsYouCompactLines: every entry kind renders its compact
// line, the entry's own fields deciding the subject; an entry with no
// reason shows its state.
func TestTopNeedsYouCompactLines(t *testing.T) {
	tests := []struct {
		name string
		item engine.NeedsYouItem
		want string
	}{
		{"task", engine.NeedsYouItem{Kind: "task", ID: 298, State: "needs_foreman", Reason: "budget out"}, "! #298 budget out"},
		{"plan", engine.NeedsYouItem{Kind: "plan", ID: 9, State: "pending_approval", Reason: "over the approval limit"}, "! #9 over the approval limit  → brigada plan approve 9"},
		{"agent", engine.NeedsYouItem{Kind: "agent", ID: 4, State: "broken", Title: "dsh/large", Reason: "auth: no key"}, "! dsh/large auth: no key  → brigada agent reset dsh/large"},
		{"foreman", engine.NeedsYouItem{Kind: "foreman", ID: 1, State: "outage", Title: "https://example.invalid/r.git", Reason: "turns failed on the infrastructure"}, "! https://example.invalid/r.git turns failed on the infrastructure"},
		{"project", engine.NeedsYouItem{Kind: "project", ID: 2, State: "duplicate", Title: "https://example.invalid/r.git", Reason: "projects 1 and 2 are the same repository"}, "! https://example.invalid/r.git projects 1 and 2 are the same repository"},
		{"no reason shows the state", engine.NeedsYouItem{Kind: "task", ID: 7, State: "needs_user"}, "! #7 needs_user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := topNeedsYou(api.Status{NeedsYou: []engine.NeedsYouItem{tt.item}}, false, term.New(logo.Caps{UTF8: true}), 120)
			if len(lines) != 1 || topCollapse(lines[0]) != topCollapse(tt.want) {
				t.Errorf("lines = %q, want the entry %q", lines, tt.want)
			}
		})
	}
}

// TestTopNeedsYouCapsAtThreeLines: three entry lines render, the rest
// folds into one "+N more" line.
func TestTopNeedsYouCapsAtThreeLines(t *testing.T) {
	var items []engine.NeedsYouItem
	for i := 1; i <= 5; i++ {
		items = append(items, engine.NeedsYouItem{Kind: "task", ID: int64(i), State: "needs_foreman", Reason: fmt.Sprintf("why %d", i)})
	}
	got := strings.Join(topNeedsYou(api.Status{NeedsYou: items}, false, term.New(logo.Caps{UTF8: true}), 96), "\n")
	want := strings.Join([]string{
		"! #1 why 1", "! #2 why 2", "! #3 why 3",
		"+2 more (brigada status)",
	}, "\n")
	if got != want {
		t.Errorf("lines =\n%s\nwant\n%s", got, want)
	}
}

// TestTopNeedsYouColours: the entries are Bad, the "+N more" and the
// empty queue's row dim; the box is Bad with entries, with the count in
// the title, and grey without.
func TestTopNeedsYouColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	st := api.Status{NeedsYou: []engine.NeedsYouItem{
		{Kind: "task", ID: 2, State: "needs_foreman", Reason: "should the sprocket be blue?"},
		{Kind: "agent", ID: 4, State: "broken", Title: "dsh/large", Reason: "auth: no key"},
	}}
	lines := topNeedsYou(st, false, style, 96)
	tail := "→ brigada agent reset dsh/large"
	agent := "! dsh/large auth: no key"
	agent += strings.Repeat(" ", 96-utf8.RuneCountInString(tail)-2-utf8.RuneCountInString(agent)) + "  " + tail
	if lines[0] != style.Bad("! #2 should the sprocket be blue?") || lines[1] != style.Bad(agent) {
		t.Errorf("entries = %q", lines)
	}
	head := topHeads(topModel{status: st}, style)[topSecNeeds]
	if want := head.border("╭─ ") + style.Bad("NEEDS YOU 2"); !strings.HasPrefix(topBoxTop(head, style, 100), want) {
		t.Errorf("top border = %q, want a Bad border and title %q", topBoxTop(head, style, 100), want)
	}
	if head := topHeads(topModel{}, style)[topSecNeeds]; head.title != "NEEDS YOU" ||
		!strings.HasPrefix(topBoxTop(head, style, 100), style.Dim("╭─ ")+style.Dim("NEEDS YOU")) {
		t.Errorf("empty box head = %+v, want a grey border and title with no count", head)
	}
	var four []engine.NeedsYouItem
	for i := 0; i < 4; i++ {
		four = append(four, engine.NeedsYouItem{Kind: "task", ID: int64(i + 1), Reason: "why"})
	}
	if got := topNeedsYou(api.Status{NeedsYou: four}, false, style, 96); got[3] != style.Dim("+1 more (brigada status)") {
		t.Errorf("cap line = %q, want dim +1 more (brigada status)", got[3])
	}
	if got := topNeedsYou(api.Status{}, false, style, 96); got[0] != style.Dim("✓ nothing needs you") {
		t.Errorf("empty queue = %q, want dim ✓ nothing needs you", got)
	}
}

// TestTopNeedsYouEmptyQueue: the section stays visible when nothing
// needs the user.
func TestTopNeedsYouEmptyQueue(t *testing.T) {
	lines := topNeedsYou(api.Status{}, false, term.New(logo.Caps{UTF8: true}), 76)
	want := "✓ nothing needs you"
	if got := strings.Join(lines, "\n"); got != want {
		t.Errorf("empty queue =\n%s\nwant\n%s", got, want)
	}
}

// topEventTexts is the texts of an events section's lines.
func topEventTexts(items []history.Item, now time.Time, style term.Style, width int) []string {
	var out []string
	for _, l := range topEventLines(items, now, style, width) {
		out = append(out, l.text)
	}
	return out
}

// topEventsFixture is n task_done events 1..n, oldest first, a minute
// apart on topNow's day, ending at 15:02.
func topEventsFixture(n int) []history.Item {
	var items []history.Item
	for i := 1; i <= n; i++ {
		at := time.Date(2026, 10, 20, 15, 2, 0, 0, time.UTC).Add(-time.Duration(n-i) * time.Minute)
		items = append(items, history.Item{Type: history.TypeEvent, ID: int64(i), Kind: "task_done", TaskID: int64(i),
			Payload: []byte(`{"status":"done"}`), CreatedAt: at.Format(engine.TimeLayout)})
	}
	return items
}

// TestTopEventsList: the EVENTS section is one row per event, newest
// first, the time then two spaces then the words; at most 10 rows; a dim
// `no events` when nothing happened.
func TestTopEventsList(t *testing.T) {
	items := busyTopModel().events
	style := term.New(logo.Caps{UTF8: true})
	want := []string{"15:02  #3 done", "15:02  #3 landed"}
	if got := topEventTexts(items, topNow, style, 80); !reflect.DeepEqual(got, want) {
		t.Errorf("list = %q, want %q", got, want)
	}
	if got := topEventTexts(nil, topNow, style, 80); !reflect.DeepEqual(got, []string{"no events"}) {
		t.Errorf("empty = %q", got)
	}
	got := topEventTexts(topEventsFixture(14), topNow, style, 80)
	if len(got) != 10 || got[0] != "15:02  #14 done" || got[9] != "14:53  #5 done" {
		t.Errorf("14 events = %q, want the newest 10, #14 first", got)
	}
}

// TestTopEventsTimeColumn: the words start in one column, the time
// column as wide as the widest time of the rows built, a mixed-day list
// included, and a dropped older row does not widen it.
func TestTopEventsTimeColumn(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	items := []history.Item{
		{Type: history.TypeEvent, ID: 1, Kind: "task_done", TaskID: 1, Payload: []byte(`{"status":"done"}`), CreatedAt: "2026-10-18T09:30:00.000000000Z"},
		{Type: history.TypeEvent, ID: 2, Kind: "task_done", TaskID: 2, Payload: []byte(`{"status":"done"}`), CreatedAt: "2026-10-20T15:02:00.000000000Z"},
	}
	// The newest row's time is padded to the widest time, "10-18 09:30".
	want := []string{"15:02        #2 done", "10-18 09:30  #1 done"}
	if got := topEventTexts(items, topNow, style, 80); !reflect.DeepEqual(got, want) {
		t.Errorf("mixed days = %q, want %q", got, want)
	}
	// With ten newer rows the other-day event is not built and the column is 5 wide.
	many := append([]history.Item{items[0]}, topEventsFixture(10)...)
	if got := topEventTexts(many, topNow, style, 80); got[0] != "15:02  #10 done" || got[9] != "14:53  #1 done" {
		t.Errorf("dropped row shifted the column: %q", got)
	}
}

// TestTopEventsCut: a row of exactly the width stays whole and one
// column less cuts it with an ellipsis.
func TestTopEventsCut(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	items := busyTopModel().events
	if got := topEventTexts(items, topNow, style, 14)[0]; got != "15:02  #3 done" {
		t.Errorf("row of exactly the width = %q", got)
	}
	if got := topEventTexts(items, topNow, style, 13)[0]; got != "15:02  #3 ..." {
		t.Errorf("row one column too wide = %q", got)
	}
}

// TestTopEventWords: every allowlisted kind in its plain words, and the
// kinds and types the events section leaves out.
func TestTopEventWords(t *testing.T) {
	stamp := "2026-10-20T15:02:45.000000000Z"
	until := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	ev := func(kind string, task, job int64, payload string) history.Item {
		return history.Item{Type: history.TypeEvent, ID: 1, Kind: kind, ProjectID: 1, TaskID: task, JobID: job,
			Payload: []byte(payload), CreatedAt: stamp}
	}
	style := term.New(logo.Caps{UTF8: true})
	tests := []struct {
		name string
		item history.Item
		want string
	}{
		{"plan_done", ev("plan_done", 0, 0, `{"plan":99,"status":"done"}`), "15:02  plan #99 done"},
		{"plan_done without a plan", ev("plan_done", 0, 0, `{}`), "15:02  plan done"},
		{"plan_approved", ev("plan_approved", 0, 0, `{"plan":7,"status":"active"}`), "15:02  plan #7 approved"},
		{"plan_awaiting_approval", ev("plan_awaiting_approval", 0, 0, `{"plan":8}`), "15:02  plan #8 awaiting approval"},
		{"task_done", ev("task_done", 3, 0, `{"status":"done"}`), "15:02  #3 done"},
		{"task_landed", ev("task_landed", 328, 0, `{"pushed":"pushed"}`), "15:02  #328 landed"},
		{"task_landed no push state", ev("task_landed", 328, 0, ``), "15:02  #328 landed"},
		{"task_landed waiting", ev("task_landed", 328, 0, `{"pushed":"waiting"}`), "15:02  #328 landed, push waits"},
		{"task_landed local", ev("task_landed", 328, 0, `{"pushed":"local"}`), "15:02  #328 landed, push waits"},
		{"task_pushed", ev("task_pushed", 328, 0, `{}`), "15:02  #328 pushed"},
		{"task_failed", ev("task_failed", 4, 0, `{"status":"failed"}`), "15:02  #4 failed"},
		{"task_land_failed", ev("task_land_failed", 4, 0, `{"reason":"conflict"}`), "15:02  #4 land failed"},
		{"review approve", ev("review_verdict", 0, 9, `{"status":"approve","branch":"fix/x"}`), "15:02  review approve fix/x"},
		{"review changes", ev("review_verdict", 0, 9, `{"status":"changes","branch":"fix/x"}`), "15:02  review changes fix/x"},
		{"review no verdict", ev("review_verdict", 0, 9, `{"status":"no_verdict"}`), "15:02  review no verdict"},
		{"agent_broken", ev("agent_broken", 0, 0, `{"harness":"codex","model":"gpt-5","reason":"auth"}`), "15:02  codex/gpt-5 broken"},
		{"agent_recovered", ev("agent_recovered", 0, 0, `{"harness":"codex","model":"gpt-5"}`), "15:02  codex/gpt-5 recovered"},
		{"agent_cooling", ev("agent_cooling", 3, 5, `{"account":"work","until":"`+until+`"}`), "15:02  work cooling until 16:04"},
		{"agent_cooling without a deadline", ev("agent_cooling", 3, 5, `{"account":"work"}`), "15:02  work cooling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topEventTexts([]history.Item{tt.item}, topNow, style, 200)[0]; got != tt.want {
				t.Errorf("row = %q, want %q", got, tt.want)
			}
			if _, ok := topEventWords(tt.item, topNow); !ok {
				t.Errorf("kind %s is not kept", tt.item.Kind)
			}
		})
	}
	// The report wording stays in topEventWords; top shows the item nowhere.
	report := ev("foreman_report", 0, 41, `{"report":"all landed"}`)
	if words, ok := topEventWords(report, topNow); !ok || words != "report #41" {
		t.Errorf("foreman_report words = %q, %v", words, ok)
	}
	if topShows(report) {
		t.Error("top shows a foreman_report")
	}
	for _, out := range []history.Item{
		{Type: history.TypeMessage, ID: 1, Kind: "worker_result", Author: "worker",
			Content: `{"status":"done","summary":"Plan 99 landed"}`, CreatedAt: stamp},
		{Type: history.TypeMessage, ID: 2, Kind: "task_done", Author: "worker", CreatedAt: stamp},
		ev("job_created", 3, 5, `{"branch":"task/3-1"}`),
		ev("job_leased", 3, 5, `{}`),
		ev("job_cap_held", 3, 5, `{}`),
		ev("task_running", 3, 0, `{}`),
		ev("review_changes", 3, 0, `{}`),
	} {
		if words, ok := topEventWords(out, topNow); ok {
			t.Errorf("%s %s is kept as %q", out.Type, out.Kind, words)
		}
	}
}

// TestTopAndHistoryShareWords: top and history word one event the same,
// the words being history.EventWords' alone.
func TestTopAndHistoryShareWords(t *testing.T) {
	until := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	it := history.Item{Type: history.TypeEvent, ID: 1, Kind: "agent_cooling", ProjectID: 1,
		Payload: []byte(`{"account":"work","until":"` + until + `"}`), CreatedAt: "2026-10-20T15:02:45.000000000Z"}
	topWords, ok := topEventWords(it, topNow)
	want, wantOK := history.EventWords(it, func(s string) string { return api.LocalClock(s, topNow) })
	if !ok || !wantOK || topWords != want || want != "work cooling until 16:04" {
		t.Errorf("top words %q (%v), shared words %q (%v)", topWords, ok, want, wantOK)
	}
	var b bytes.Buffer
	if err := history.RenderReadable(&b, []history.Item{it}, time.UTC, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "10-20 15:02 work cooling until ") {
		t.Errorf("history line = %q", b.String())
	}
}

// TestTopFetchFiltersEvents: topFetch keeps only the allowlisted events,
// oldest first as the page came, and asks for the wider page.
func TestTopFetchFiltersEvents(t *testing.T) {
	stamp := "2026-10-20T15:02:45.000000000Z"
	fake := &fakeTop{
		projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}},
		page: history.Page{Items: []history.Item{
			{Type: history.TypeEvent, ID: 1, Kind: "job_created", TaskID: 3, JobID: 5, CreatedAt: stamp},
			{Type: history.TypeEvent, ID: 2, Kind: "task_running", TaskID: 3, CreatedAt: stamp},
			{Type: history.TypeMessage, ID: 3, Kind: "worker_result", Author: "worker", Content: `{"status":"done"}`, CreatedAt: stamp},
			{Type: history.TypeEvent, ID: 4, Kind: "task_done", TaskID: 3, CreatedAt: stamp},
			{Type: history.TypeEvent, ID: 5, Kind: "job_leased", TaskID: 3, JobID: 5, CreatedAt: stamp},
			{Type: history.TypeEvent, ID: 6, Kind: "foreman_report", JobID: 41, CreatedAt: stamp},
		}},
	}
	model, err := topFetch(context.Background(), fake, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(model.events) != 1 || model.events[0].ID != 4 {
		t.Errorf("events = %+v, want only the task_done, no foreman_report", model.events)
	}
	for _, text := range topEventTexts(model.events, topNow, term.New(logo.Caps{UTF8: true}), 80) {
		if strings.Contains(text, "report") {
			t.Errorf("row %q names a report", text)
		}
	}
	if topEventsLimit != 200 {
		t.Errorf("topEventsLimit = %d, want 200", topEventsLimit)
	}
}

// TestTopEventsKeptAcrossTicks: a page of 200 items with no kept one
// leaves the previous tick's events as they were, and two ticks that share
// an item show it once.
func TestTopEventsKeptAcrossTicks(t *testing.T) {
	stamp := "2026-10-20T15:02:45.000000000Z"
	var noise []history.Item
	for i := 0; i < topEventsLimit; i++ {
		noise = append(noise, history.Item{Type: history.TypeEvent, ID: int64(1000 + i), Kind: "job_leased", JobID: 5, CreatedAt: stamp})
	}
	fake := &fakeTop{
		projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}},
		page:     history.Page{Items: noise},
	}
	prev := busyTopModel()
	tick, err := topFetch(context.Background(), fake, "")
	if err != nil || len(tick.events) != 0 {
		t.Fatalf("topFetch: %v, events %+v, want none kept", err, tick.events)
	}
	merged := mergeTopEvents(prev.events, tick.events)
	if len(merged) != 2 || merged[0].ID != 11 || merged[1].ID != 12 {
		t.Errorf("merged = %+v, want the previous strip", merged)
	}
	style := term.New(logo.Caps{UTF8: true})
	if got := topEventTexts(merged, topNow, style, 200); len(got) != 2 || got[0] != "15:02  #3 done" || got[1] != "15:02  #3 landed" {
		t.Errorf("list = %q, want the previous tick's", got)
	}

	// Dedupe: the next tick repeats item 12 and adds item 13.
	next := []history.Item{
		prev.events[1],
		{Type: history.TypeEvent, ID: 13, Kind: "task_pushed", TaskID: 3, CreatedAt: "2026-10-20T15:03:00.000000000Z"},
	}
	merged = mergeTopEvents(merged, next)
	if len(merged) != 3 || merged[0].ID != 11 || merged[1].ID != 12 || merged[2].ID != 13 {
		t.Errorf("merged = %+v, want 11, 12, 13 once each, oldest first", merged)
	}
	// A message with the same ID is another item.
	merged = mergeTopEvents(merged, []history.Item{{Type: history.TypeMessage, ID: 12}})
	if len(merged) != 4 {
		t.Errorf("a message of the same id merged away: %+v", merged)
	}

	// Only the newest topEventsKeep stay.
	var many []history.Item
	for i := 1; i <= topEventsKeep+10; i++ {
		many = append(many, history.Item{Type: history.TypeEvent, ID: int64(i), Kind: "task_done", TaskID: 1,
			CreatedAt: time.Date(2026, 10, 20, 10, 0, i, 0, time.UTC).Format(engine.TimeLayout)})
	}
	kept := mergeTopEvents(nil, many)
	if len(kept) != topEventsKeep || kept[0].ID != 11 || kept[len(kept)-1].ID != int64(topEventsKeep+10) {
		t.Errorf("kept %d items from id %d to %d, want the newest %d", len(kept), kept[0].ID, kept[len(kept)-1].ID, topEventsKeep)
	}
}

// TestTopClockMinutes: the events section's pairs show HH:MM — the date
// too on another day — while the header's own clock keeps its seconds.
func TestTopClockMinutes(t *testing.T) {
	if got := topClock("2026-10-20T15:02:45.000000000Z", topNow); got != "15:02" {
		t.Errorf("same-day stamp = %q, want 15:02", got)
	}
	if got := topClock("2026-10-19T15:02:45.000000000Z", topNow); got != "10-19 15:02" {
		t.Errorf("other-day stamp = %q, want 10-19 15:02", got)
	}
	style := term.New(logo.Caps{UTF8: true})
	model := busyTopModel()
	strip := strings.Join(topEventTexts(model.events, topNow, style, 200), "\n")
	if !strings.Contains(strip, "15:02  #3 done") ||
		strings.Contains(strip, "15:02:45") || strings.Contains(strip, "15:02:41") {
		t.Errorf("strip = %q, want HH:MM pairs", strip)
	}
	header := topHeader(model, topNow, style, 100)
	if !strings.Contains(header, "15:04:09") {
		t.Errorf("header = %q, want its seconds", header)
	}
}

// TestTopEventItemStates: the state words are coloured — done and
// recovered in the OK style, failed and broken in the Bad style — as a
// whole word, a status value or an event kind's last part.
func TestTopEventItemStates(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	tests := []struct {
		name string
		item history.Item
		want string
	}{
		{"done", history.Item{Type: history.TypeEvent, ID: 1, Kind: "task_done", TaskID: 3,
			Payload: []byte(`{"status":"done"}`), CreatedAt: "2026-10-20T15:02:45.000000000Z"},
			"15:02  #3 " + style.OK("done")},
		{"recovered", history.Item{Type: history.TypeEvent, ID: 2, Kind: "agent_recovered", ProjectID: 1,
			Payload: []byte(`{"harness":"codex","model":"gpt-5"}`), CreatedAt: "2026-10-20T15:02:46.000000000Z"},
			"15:02  codex/gpt-5 " + style.OK("recovered")},
		{"failed", history.Item{Type: history.TypeEvent, ID: 3, Kind: "task_failed", TaskID: 4,
			Payload: []byte(`{"status":"failed"}`), CreatedAt: "2026-10-20T15:02:47.000000000Z"},
			"15:02  #4 " + style.Bad("failed")},
		{"broken", history.Item{Type: history.TypeEvent, ID: 4, Kind: "agent_broken", ProjectID: 1,
			Payload: []byte(`{"harness":"codex","model":"gpt-5","reason":"auth: no key"}`), CreatedAt: "2026-10-20T15:02:48.000000000Z"},
			"15:02  codex/gpt-5 " + style.Bad("broken")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topEventTexts([]history.Item{tt.item}, topNow, style, 200)[0]; got != tt.want {
				t.Errorf("row = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRenderTopGlobalCapLine: the AGENTS section names the global cap
// under its table, and the line is omitted when the snapshot carries no
// global cap.
func TestRenderTopGlobalCapLine(t *testing.T) {
	eight := 8
	tests := []struct {
		name   string
		layers []engine.CapacityLayer
		want   string
	}{
		{"a capped global", []engine.CapacityLayer{{Layer: "global", Cap: &eight}}, "all ≤ global 8"},
		{"no global layer", nil, ""},
		{"an uncapped global", []engine.CapacityLayer{{Layer: "global"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := busyTopModel()
			model.capacity.Layers = tt.layers
			got := plainTop(model, 100, 0)
			if tt.want == "" {
				if strings.Contains(got, "all ≤ global") {
					t.Errorf("view has a global cap line without a cap:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("view lacks %q:\n%s", tt.want, got)
			}
		})
	}
}

// TestRenderTopShowsGateHoldsAndLandJobs: a land job a runner holds
// renders under its task inside its plan, though the task is done, and
// the land-gate and push-gate holds sit in NEEDS YOU with the rule that
// tripped and the task id.
func TestRenderTopShowsGateHoldsAndLandJobs(t *testing.T) {
	model := busyTopModel()
	leased := topNow.Add(-100 * time.Second).UTC().Format(engine.TimeLayout)
	model.status.Plans[0].Tasks = append(model.status.Plans[0].Tasks,
		engine.StatusTask{ID: 8, Title: "ship the gizmo", State: "done"},
		engine.StatusTask{ID: 9, Title: "push the widget", State: "done"})
	model.status.Running = append(model.status.Running,
		engine.StatusJob{ID: 10, TaskID: 9, Kind: "land", Status: "running", LeasedAt: leased})
	model.status.NeedsYou = append(model.status.NeedsYou,
		engine.NeedsYouItem{Kind: "task", ID: 8, State: "done", Title: "ship the gizmo", Reason: "held by rule gate_land: gate_land is ask; release: brigada land approve 8; change the rule: brigada land approve --and-stop-asking 8"},
		engine.NeedsYouItem{Kind: "task", ID: 9, State: "done", Title: "push the widget", Reason: "waits to push: task/9-1@abc123"})
	got := readTop(model, 120, 0)
	hold, tail := "! #8 held by rule gate_land: gate_land is ask", "→ brigada land approve 8"
	want := "╭ NEEDS YOU 3" +
		"\n! #2 should the sprocket be blue?" +
		"\n" + hold + strings.Repeat(" ", 116-utf8.RuneCountInString(hold)-utf8.RuneCountInString(tail)) + tail +
		"\n! #9 waits to push: task/9-1@abc123"
	tree := "  ▸  #3  oil the gears     work  claude/sonnet@high  1m40s  Edit" +
		"\n  ▸  #9  push the widget   land                      1m40s" +
		"\n  ! #2 wire the sprocket should the sprocket be blue?"
	for _, w := range []string{"#1 fix the frobnicator", "■■■□■■  3/6", tree, want} {
		if !strings.Contains(got, w) {
			t.Errorf("view lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "#8 ship the gizmo") {
		t.Errorf("a done task with no job renders a line:\n%s", got)
	}
}

// TestRenderTopKeepsAHeldTaskOutOfNeedsYou: a task the foreman holds
// arrives in foreman_holds, not needs_you (§10): it leaves the NEEDS YOU
// queue and the count, and the plan tree keeps the worker's question
// beside it.
func TestRenderTopKeepsAHeldTaskOutOfNeedsYou(t *testing.T) {
	model := busyTopModel()
	model.status.ForemanHolds = model.status.NeedsYou
	model.status.NeedsYou = nil
	got := readTop(model, 200, 0)
	if !strings.Contains(got, "nothing needs you") || strings.Contains(got, "! #2 should the sprocket be blue?") {
		t.Errorf("a held task is listed under NEEDS YOU:\n%s", got)
	}
	if want := "! #2 wire the sprocket should the sprocket be blue?"; !strings.Contains(got, want) {
		t.Errorf("the plan tree lacks %q:\n%s", want, got)
	}
}

// TestRenderTopShowsAnInfraRetry: a waiting retry renders as its ↺ line
// under the plan its task belongs to, in place of the task's queued line.
func TestRenderTopShowsAnInfraRetry(t *testing.T) {
	model := busyTopModel()
	model.status.Held = nil
	nb := topNow.Add(5 * time.Minute).UTC().Format(engine.TimeLayout)
	model.status.Retries = []engine.InfraRetry{{ID: 9, TaskID: 4, Kind: "work", NotBefore: nb, Reason: "could not resolve github.com"}}
	got := readTop(model, 200, 0)
	want := "  ! #2 wire the sprocket should the sprocket be blue?" +
		"\n  ↺ task #4 work: retry in 5m: could not resolve github.com" +
		"\n╰\n╭ AGENTS"
	if !strings.Contains(got, want) {
		t.Errorf("view lacks %q:\n%s", want, got)
	}
	if strings.Contains(got, "#4 polish the knobs") {
		t.Errorf("the retrying task also renders its queued line:\n%s", got)
	}
}

// TestTopPlansTree: every plan the status lists renders — a live one
// with its running jobs first, then a line per task still live work, a
// finished one as one line.
func TestTopPlansTree(t *testing.T) {
	leased := topNow.Add(-14 * time.Minute).UTC().Format(engine.TimeLayout)
	nb := topNow.Add(5 * time.Minute).UTC().Format(engine.TimeLayout)
	// right puts a plan line's tail at the right edge of 80 columns.
	right := func(left, tail string) string {
		return left + strings.Repeat(" ", 80-utf8.RuneCountInString(left)-utf8.RuneCountInString(tail)) + tail
	}
	tests := []struct {
		name string
		st   api.Status
		want []string
	}{
		{"no plans", api.Status{}, []string{"none"}},
		{"every task line form", api.Status{
			Plans: []engine.StatusPlan{{ID: 96, Title: "Rework brigada top", State: "active", Tasks: []engine.StatusTask{
				{ID: 310, Title: "helpers", State: "done"},
				{ID: 311, Title: "dropped", State: "cancelled"},
				{ID: 312, Title: "old", State: "retired"},
				{ID: 313, Title: "landing", State: "done"},
				{ID: 314, Title: "finish", State: "needs_foreman"},
				{ID: 315, Title: "ask", State: "needs_user"},
				{ID: 316, Title: "audit", State: "reviewing"},
				{ID: 317, Title: "work", State: "running"},
				{ID: 318, Title: "tree", State: "ready"},
				{ID: 319, Title: "docs", State: "pending", WaitingOn: []string{"tree", "#317"}},
				{ID: 320, Title: "fix", State: "ready"},
				{ID: 321, Title: "net", State: "ready"},
			}}},
			Running: []engine.StatusJob{
				{ID: 40, TaskID: 317, Kind: "work", Harness: "opencode", Model: "glm-5.3", LeasedAt: leased, LastEvent: "editing cmdtop.go"},
				{ID: 43, TaskID: 313, Kind: "land", LeasedAt: leased},
			},
			Held:     []engine.HeldJob{{ID: 41, TaskID: 320, Kind: "fix", Layer: "runner"}},
			Retries:  []engine.InfraRetry{{ID: 42, TaskID: 321, Kind: "work", NotBefore: nb, Reason: "could not resolve github.com"}},
			NeedsYou: []engine.NeedsYouItem{{Kind: "task", ID: 314, State: "needs_foreman", Reason: "rounds spent"}},
		}, []string{
			right("#96 Rework brigada top", "■■■■□■□□□□  2/10"),
			"  ▸  #317  work     work  opencode/glm-5.3  14m00s  editing cmdtop.go",
			"  ▸  #313  landing  land                    14m00s",
			"  ! #314 finish rounds spent",
			"  ! #315 ask needs_user",
			"  ·  #316  audit    (review)",
			"  ·  #318  tree     (queued)",
			"  ·  #319  docs     (waiting on tree, #317)",
			"  ?  #320  fix      held: waiting for a runner slot",
			"  ↺ task #321 work: retry in 5m: could not resolve github.com",
		}},
		{"finished plans one line", api.Status{
			Plans: []engine.StatusPlan{
				{ID: 97, Title: "all done, not closed", State: "active", Tasks: []engine.StatusTask{{ID: 1, State: "done"}, {ID: 2, State: "done"}}},
				{ID: 83, Title: "Role instructions", State: "done", Tasks: []engine.StatusTask{{ID: 3, State: "done"}, {ID: 4, State: "done"}, {ID: 5, State: "cancelled"}}},
				{ID: 80, Title: "Abandoned", State: "cancelled", Tasks: []engine.StatusTask{{ID: 6, Title: "never", State: "cancelled"}, {ID: 7, Title: "left", State: "ready"}}},
			},
			HiddenFinishedPlans: 41,
		}, []string{
			right("#97 all done, not closed", "■■  2/2"),
			right("#83 Role instructions", "done"),
			right("#80 Abandoned", "cancelled"),
		}},
		{"a live plan's review job under its plan", api.Status{
			Plans: []engine.StatusPlan{{ID: 12, Title: "Review me", State: "reviewing", ReviewRound: 2, ReviewJob: 50,
				Tasks: []engine.StatusTask{{ID: 1, Title: "first", State: "pending"}}}},
			Running: []engine.StatusJob{{ID: 50, Kind: "plan_review", Harness: "claude", Model: "opus", LeasedAt: leased}},
		}, []string{
			right("#12 Review me", "reviewing round 2"),
			"  ▸  job #50  plan_review  claude/opus  14m00s",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(topPlansTree(tt.st, topNow, term.New(logo.Caps{UTF8: true}), 80), "\n")
			if want := strings.Join(tt.want, "\n"); got != want {
				t.Errorf("tree =\n%s\nwant\n%s", got, want)
			}
			if foreman, jobs := topPlanless(tt.st, topNow, term.New(logo.Caps{UTF8: true}), 80); len(foreman)+len(jobs) != 0 {
				t.Errorf("a plan's job is plan-less: %v %v", foreman, jobs)
			}
		})
	}
}

// TestTopPlansTreeColours: the live plan's id Bold, its task cells each in
// the colour of the task's state (landed OK, running Accent, parked Bad,
// queued Dim), the finished words OK and Bad, the row forms in their
// colours: the Accent ▸, the Warn ? and held words, the Dim · row.
func TestTopPlansTreeColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	st := api.Status{
		Plans: []engine.StatusPlan{
			{ID: 1, Title: "live", State: "active", Tasks: []engine.StatusTask{
				{ID: 1, State: "done"}, {ID: 2, Title: "run", State: "running"}, {ID: 3, Title: "q", State: "ready"},
				{ID: 4, Title: "held", State: "ready"}, {ID: 5, Title: "stuck", State: "needs_foreman"},
			}},
			{ID: 2, Title: "done", State: "done", Tasks: []engine.StatusTask{{ID: 6, State: "done"}}},
			{ID: 3, Title: "gone", State: "cancelled"},
		},
		Running: []engine.StatusJob{{ID: 9, TaskID: 2, Kind: "work"}},
		Held:    []engine.HeldJob{{ID: 10, TaskID: 4, Kind: "work", Layer: "runner"}},
	}
	got := topPlansTree(st, topNow, style, 80)
	// right puts a tail of the given visible width at the edge of 80 columns.
	right := func(left string, tail string, tailW int) string {
		return left + strings.Repeat(" ", 80-term.VisibleWidth(left)-tailW) + tail
	}
	want := []string{
		right(style.Bold("#1")+" live", style.OK("■")+style.Accent("■")+style.Dim("□")+style.Dim("□")+style.Bad("■")+"  1/5", 10),
		"  " + style.Accent("▸") + "  #2  run   work",
		"  " + style.Dim("·") + "  " + style.Dim("#3") + "  " + style.Dim("q   ") + "  " + style.Dim("(queued)"),
		"  " + style.Warn("?") + "  #4  held  " + style.Warn("held: waiting for a runner slot"),
		"  " + style.Bad("! #5 stuck needs_foreman"),
		right("#2 done", style.OK("done"), 4),
		right("#3 gone", style.Bad("cancelled"), 9),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree =\n%q\nwant\n%q", got, want)
	}
}

// topPlanLines is a status's PLANS rows in plain text.
func topPlanLines(st api.Status, width int) []string {
	return topPlansTree(st, topNow, term.New(logo.Caps{UTF8: true}), width)
}

// topLineClass is the yield class of the PLANS line with the text.
func topLineClass(t *testing.T, st api.Status, width int, text string) topYield {
	t.Helper()
	for _, l := range newTopPlans(st, false, false, topNow, term.New(logo.Caps{UTF8: true}), width) {
		if l.text == text {
			return l.class
		}
	}
	t.Fatalf("no PLANS line %q", text)
	return 0
}

// TestTopPlanStateWords: a plan that is neither active nor finished shows
// a state word in place of the count and bar, and its waiting tasks get no
// queued line; running, parked and the plan review's own lines stay, and a
// reviewing plan with no review job claimed shows `plan review queued`.
func TestTopPlanStateWords(t *testing.T) {
	st := api.Status{Plans: []engine.StatusPlan{
		{ID: 1, Title: "approve me", State: "pending_approval", Tasks: []engine.StatusTask{
			{ID: 1, Title: "a", State: "pending"}, {ID: 5, Title: "e", State: "ready", WaitingOn: []string{"#1"}}}},
		{ID: 2, Title: "review me", State: "reviewing", ReviewRound: 3, Tasks: []engine.StatusTask{{ID: 2, Title: "b", State: "pending"}}},
		{ID: 3, Title: "foreman", State: "needs_foreman", ReviewRound: 1, Tasks: []engine.StatusTask{
			{ID: 3, Title: "c", State: "ready"}, {ID: 4, Title: "d", State: "needs_user"}}},
	}}
	right := func(left, tail string) string {
		return left + strings.Repeat(" ", 80-len(left)-len(tail)) + tail
	}
	want := []string{
		right("#1 approve me", "awaiting approval"),
		right("#2 review me", "reviewing round 3"),
		"  · plan review queued",
		right("#3 foreman", "needs foreman"),
		"  ! #4 d needs_user",
	}
	if got := topPlanLines(st, 80); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := topLineClass(t, st, 80, "  · plan review queued"); c != yieldQueued {
		t.Errorf("plan review queued is class %d, want the queued class", c)
	}

	// A claimed review job speaks for the review: running, held or retrying.
	nb := topNow.Add(time.Minute).UTC().Format(engine.TimeLayout)
	reviewing := engine.StatusPlan{ID: 2, Title: "review me", State: "reviewing", ReviewRound: 1, ReviewJob: 50}
	for name, with := range map[string]func(*api.Status){
		"running": func(s *api.Status) { s.Running = []engine.StatusJob{{ID: 50, Kind: "plan_review"}} },
		"held":    func(s *api.Status) { s.Held = []engine.HeldJob{{ID: 50, Kind: "plan_review", Layer: "runner"}} },
		"retry":   func(s *api.Status) { s.Retries = []engine.InfraRetry{{ID: 50, Kind: "plan_review", NotBefore: nb}} },
	} {
		s := api.Status{Plans: []engine.StatusPlan{reviewing}}
		with(&s)
		if got := strings.Join(topPlanLines(s, 80), "\n"); strings.Contains(got, "plan review queued") {
			t.Errorf("%s review job: the queued line shows:\n%s", name, got)
		}
	}
}

// TestTopPlanTailsAlign: the tails of every plan line end at the right
// edge, the title cut first when the widest tail would not fit.
func TestTopPlanTailsAlign(t *testing.T) {
	const repo = "https://example.invalid/acme/brigada.git"
	st := api.Status{Plans: []engine.StatusPlan{
		{ID: 101, RepoURL: repo, Title: "Fix events strip", State: "reviewing", ReviewRound: 2, ReviewJob: 50},
		{ID: 102, RepoURL: repo, Title: "Top polish", State: "active", Tasks: []engine.StatusTask{
			{ID: 1, State: "done"}, {ID: 2, State: "done"}, {ID: 3, State: "retired"},
			{ID: 4, State: "cancelled"}, {ID: 5, State: "running"}, {ID: 6, State: "running"}, {ID: 7, State: "running"}}},
		{ID: 9, RepoURL: repo, Title: "Shipped", State: "done", Tasks: []engine.StatusTask{{ID: 8, State: "done"}}},
	}}
	style := term.New(logo.Caps{UTF8: true})
	plain := func(width int) []string {
		var out []string
		for _, l := range newTopPlans(st, true, false, topNow, style, width) {
			if strings.HasPrefix(l.text, "#") {
				out = append(out, l.text)
			}
		}
		return out
	}
	right := func(left, tail string, width int) string {
		return left + strings.Repeat(" ", width-utf8.RuneCountInString(left)-utf8.RuneCountInString(tail)) + tail
	}
	want := []string{
		right("#101 brigada  Fix events strip", "reviewing round 2", 80),
		right("#102 brigada  Top polish", "■■□□□  2/5", 80),
		right("#9   brigada  Shipped", "done", 80),
	}
	if got := plain(80); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// At 40 columns the titles are cut to keep the widest tail whole, and
	// the tails still end at the edge; five cells are over a third of 40,
	// so that plan's tail is just its count.
	narrow := plain(40)
	for _, l := range narrow {
		if n := term.VisibleWidth(l); n > 40 {
			t.Errorf("line %q is %d wide", l, n)
		}
	}
	for i, tail := range []string{"reviewing round 2", "2/5", "done"} {
		if !strings.HasSuffix(narrow[i], tail) {
			t.Errorf("line %d = %q, want the tail %q at the edge", i, narrow[i], tail)
		}
	}
	if !strings.Contains(narrow[0], "Fix e...") {
		t.Errorf("narrow line = %q, want the title cut", narrow[0])
	}
}

// TestTopPlanDoneNotLanded: a done implement task that is not landed and
// that no job speaks for gets a ⇡ line with what the landing waits on, and
// the plan's bar does not count it until it is landed.
func TestTopPlanDoneNotLanded(t *testing.T) {
	stamp := time.Date(2026, 10, 20, 11, 10, 0, 0, time.UTC).Format(engine.TimeLayout)
	tasks := []engine.StatusTask{
		{ID: 330, Title: "events wording", State: "done", Class: "implement", LandReady: "waiting (retry at " + stamp + ", last exit 1)"},
		{ID: 331, Title: "queued land", State: "done", Class: "implement", LandReady: "ready"},
		{ID: 332, Title: "bare", State: "done", Class: "implement"},
		{ID: 333, Title: "landed", State: "done", Class: "implement", Landed: true, LandedCommit: "abc123"},
		{ID: 334, Title: "notes", State: "done", Class: "research"},
	}
	st := api.Status{Plans: []engine.StatusPlan{{ID: 7, Title: "land me", State: "active", Tasks: tasks}}}
	want := []string{
		"#7 land me" + strings.Repeat(" ", 80-len("#7 land me")-utf8.RuneCountInString("■■■■■  2/5")) + "■■■■■  2/5",
		"  ⇡  #330  events wording  landing: waiting (retry at 11:10, last exit 1)",
		"  ⇡  #331  queued land     landing: queued",
		"  ⇡  #332  bare            landing: not landed",
	}
	if got := topPlansTree(st, topNow, term.New(logo.Caps{UTF8: true}), 80); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := topLineClass(t, st, 80, want[1]); c != yieldHeld {
		t.Errorf("the ⇡ line is class %d, want the held class", c)
	}
	teal := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	if r := topLandingRow(tasks[2], topNow, teal); r.markFn("⇡") != teal.Teal("⇡") || r.waitFn("x") != teal.Teal("x") || r.wait != "landing: not landed" || r.dim {
		t.Errorf("the ⇡ row is not teal with uncoloured id and title: %+v", r)
	}

	// A land job that speaks for the task shows as its own row, not the ⇡ one.
	st.Running = []engine.StatusJob{{ID: 9, TaskID: 332, Kind: "land"}}
	got := strings.Join(topPlanLines(st, 80), "\n")
	if !strings.Contains(got, "▸  #332  bare") || !strings.Contains(got, "  land") || strings.Contains(got, "#332  bare            landing") {
		t.Errorf("a running land job and the ⇡ row both speak:\n%s", got)
	}

	// The cells read complete only when the landing is; a class that never
	// lands and a finished plan count state done.
	last := func(state string, landed bool) string {
		p := engine.StatusPlan{ID: 8, Title: "p", State: state, Tasks: []engine.StatusTask{
			{ID: 1, State: "done", Class: "implement", Landed: true},
			{ID: 2, Title: "last", State: "done", Class: "implement", Landed: landed}}}
		plain := term.New(logo.Caps{UTF8: true})
		return topPlanLine(p, "", newTopPlanLayout([]engine.StatusPlan{p}, false, nil, plain, 80), nil, plain, 80)
	}
	if got := last("active", false); topCollapse(got) != "#8 p ■■ 1/2" || utf8.RuneCountInString(got) != 80 {
		t.Errorf("the count includes a task not landed: %q", got)
	}
	if got := last("active", true); topCollapse(got) != "#8 p ■■ 2/2" {
		t.Errorf("the count misses a landed task: %q", got)
	}
	if got := last("done", false); topCollapse(got) != "#8 p done" || !strings.HasSuffix(got, "done") {
		t.Errorf("a finished plan = %q, want its state word at the edge", got)
	}
}

// TestTopPlanJobLinesNameTheirTask: under a plan the running and held
// lines name the task by id and title, the title the only part cut.
func TestTopPlanJobLinesNameTheirTask(t *testing.T) {
	leased := topNow.Add(-192 * time.Second).UTC().Format(engine.TimeLayout)
	job := engine.StatusJob{ID: 40, TaskID: 331, Kind: "work", Harness: "claude", Model: "sonnet", Effort: "high", LeasedAt: leased, LastEvent: "tool_start Edit"}
	st := api.Status{
		Plans: []engine.StatusPlan{{ID: 7, Title: "p", State: "active", Tasks: []engine.StatusTask{
			{ID: 331, Title: "clip frame to height", State: "running"},
			{ID: 333, Title: "agent order", State: "ready"},
			{ID: 334, State: "running"}}}},
		Running: []engine.StatusJob{job},
		Held:    []engine.HeldJob{{ID: 41, TaskID: 333, Kind: "work", Layer: "account"}},
	}
	// Wide: the whole row, the cells two spaces apart.
	got := topPlanLines(st, 100)
	if want := "  ▸  #331  clip frame to height  work  claude/sonnet@high  3m12s  Edit"; got[1] != want {
		t.Errorf("running row = %q, want %q", got[1], want)
	}
	if want := "  ?  #333  agent order           held: waiting for a free account slot"; got[2] != want {
		t.Errorf("held row = %q, want %q", got[2], want)
	}
	// Narrow (below 80 terminal columns): the last event goes and the title
	// is the only cell cut; the kind, agent and age stay whole.
	got = topPlanLines(st, 56)
	if !strings.Contains(got[1], "  work  claude/sonnet@high  3m12s") || strings.Contains(got[1], "Edit") ||
		!strings.HasPrefix(got[1], "  ▸  #331  clip fram...") || utf8.RuneCountInString(got[1]) > 56 {
		t.Errorf("narrow running row = %q", got[1])
	}
	if !strings.HasPrefix(topCollapse(got[2]), "? #333 agent order held: waiting") {
		t.Errorf("narrow held row = %q", got[2])
	}
	// A task with no title has an empty title cell.
	st.Running = []engine.StatusJob{{ID: 42, TaskID: 334, Kind: "work", LeasedAt: leased}}
	st.Held = nil
	if got := topPlanLines(st, 100); !strings.HasPrefix(got[1], "  ▸  #334  ") || !strings.HasSuffix(got[1], "work  3m12s") {
		t.Errorf("untitled task row = %q", got[1])
	}
}

// TestTopPlanCellColours: one cell per task in task order, cancelled and
// retired left out: landed and done-of-a-class-that-never-lands OK, done
// and not landed Teal, a task with a job Accent, parked Bad, the rest an
// empty Dim cell.
func TestTopPlanCellColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	p := engine.StatusPlan{ID: 1, State: "active", Tasks: []engine.StatusTask{
		{ID: 1, State: "done", Class: "implement", Landed: true},
		{ID: 2, State: "done", Class: "research"},
		{ID: 3, State: "done", Class: "implement"},
		{ID: 4, State: "cancelled"},
		{ID: 5, State: "running"},
		{ID: 6, State: "running"},
		{ID: 7, State: "needs_foreman"},
		{ID: 8, State: "needs_user"},
		{ID: 9, State: "failed"},
		{ID: 10, State: "retired"},
		{ID: 11, State: "ready"},
		{ID: 12, State: "reviewing"},
	}}
	got := topPlanCells(p, map[int64]bool{5: true}, style)
	want := style.OK("■") + style.OK("■") + style.Teal("■") + style.Accent("■") + style.Dim("□") +
		style.Bad("■") + style.Bad("■") + style.Bad("■") + style.Dim("□") + style.Dim("□")
	if got != want {
		t.Errorf("cells = %q, want %q", got, want)
	}
	// The cells over a third of the width leave just the count.
	if tail := topPlanTail(p, map[int64]bool{5: true}, 29, style); tail != "2/10" {
		t.Errorf("tail at 29 columns = %q, want the count alone", tail)
	}
}

// TestTopPlansLegend: the PLANS border carries the legend, each cell in
// its colour, and leaves it out when it does not fit.
func TestTopPlansLegend(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	model := busyTopModel()
	model.statusOK = true
	head := topHeads(model, style)[topSecPlans]
	wide := topBoxTop(head, style, 100)
	legend := style.OK("■") + style.Dim(" landed  ") + style.Teal("■") + style.Dim(" done  ") +
		style.Accent("■") + style.Dim(" running  ") + style.Dim("□ queued")
	if !strings.Contains(wide, legend) || term.VisibleWidth(wide) != 100 {
		t.Errorf("wide border = %q, want the legend and 100 columns", wide)
	}
	narrow := topBoxTop(head, style, 60)
	if strings.Contains(narrow, "landed") || !strings.Contains(term.StripEscapes(narrow), "PLANS  1 live · 0 finished") || term.VisibleWidth(narrow) != 60 {
		t.Errorf("narrow border = %q, want the note and no legend", narrow)
	}
}

// TestRenderTopTints: a needs-you entry sits on a dark red and a running
// job row on a dark blue, in every box, at 256 colours and true colour,
// the background opened again after each inner span and covering the
// padding; there is no tint at 16 colours or without colour, and the text
// reads the same.
func TestRenderTopTints(t *testing.T) {
	model := busyTopModel()
	model.statusOK = true
	model.status.Running = append(model.status.Running, engine.StatusJob{ID: 30, Kind: "foreman", Status: "running"})
	plain := renderTop(model, 100, 0, topNow, term.New(logo.Caps{UTF8: true}))
	for _, tt := range []struct {
		depth      logo.Depth
		needs, run string
	}{
		{logo.Color256, "\x1b[48;5;52m\x1b[38;5;224m", "\x1b[48;5;17m\x1b[38;5;153m"},
		{logo.TrueColor, "\x1b[48;2;59;20;24m\x1b[38;2;255;215;215m", "\x1b[48;2;18;35;63m\x1b[38;2;215;227;255m"},
	} {
		lines := renderTop(model, 100, 0, topNow, term.New(logo.Caps{Color: tt.depth, UTF8: true}))
		if len(lines) != len(plain) {
			t.Fatalf("depth %d: %d lines, want %d", tt.depth, len(lines), len(plain))
		}
		tinted := map[string]int{}
		for i, line := range lines {
			if got := term.StripEscapes(line); got != plain[i] {
				t.Errorf("depth %d line %d reads %q, want %q", tt.depth, i, got, plain[i])
			}
			switch {
			case strings.Contains(plain[i], "│ ! #2"):
				tinted["needs"]++
				if !strings.Contains(line, tt.needs) {
					t.Errorf("depth %d: the needs-you entry is not tinted: %q", tt.depth, line)
				}
			case strings.Contains(plain[i], "▸"):
				tinted["run"]++
				if !strings.Contains(line, tt.run) {
					t.Errorf("depth %d: the running row is not tinted: %q", tt.depth, line)
				}
				// The Accent ▸ ends in a reset; the tint opens again after it.
				if !strings.Contains(line, "\x1b[0m"+tt.run) {
					t.Errorf("depth %d: the background does not survive the ▸: %q", tt.depth, line)
				}
				// The padding is inside the tint.
				if !strings.Contains(line, "    \x1b[0m ") {
					t.Errorf("depth %d: the tint stops short of the padding: %q", tt.depth, line)
				}
			case strings.Contains(plain[i], "wire the sprocket"):
				if strings.Contains(line, "\x1b[48;") {
					t.Errorf("depth %d: a parked row is tinted: %q", tt.depth, line)
				}
			}
		}
		if tinted["needs"] != 1 || tinted["run"] != 2 {
			t.Errorf("depth %d: tinted rows %v, want 1 needs-you entry and 2 running rows (FOREMAN and PLANS)", tt.depth, tinted)
		}
	}
	for _, depth := range []logo.Depth{logo.Color16, logo.NoColor} {
		frame := strings.Join(renderTop(model, 100, 0, topNow, term.New(logo.Caps{Color: depth, UTF8: true})), "\n")
		if strings.Contains(frame, "\x1b[48;") || strings.Contains(frame, "\x1b[4") && depth == logo.NoColor {
			t.Errorf("depth %d: the frame has a background", depth)
		}
	}
}

// TestTopJobColumns: the running rows of a box line up whatever their
// agents and ages, a job with no task has `job #N` in the title column, an
// agent path of more than two segments reads as its last segment, the age
// is padded, and a last event that does not fit is cut at the line's end.
func TestTopJobColumns(t *testing.T) {
	at := func(d time.Duration) string { return topNow.Add(-d).UTC().Format(engine.TimeLayout) }
	st := api.Status{
		Plans: []engine.StatusPlan{{ID: 7, Title: "p", State: "active", ReviewJob: 50, Tasks: []engine.StatusTask{
			{ID: 342, Title: "clip frame", State: "running"},
			{ID: 343, Title: "show plan state word", State: "running"},
			{ID: 345, Title: "docs", State: "running"}}}},
		Running: []engine.StatusJob{
			{ID: 1, TaskID: 342, Kind: "work", Harness: "opencode", Model: "zai-coding-plan/glm-5.3", LeasedAt: at(192 * time.Second), LastEvent: "tool_start Edit cmdtop.go"},
			{ID: 2, TaskID: 343, Kind: "fix", Harness: "claude", Model: "sonnet", Effort: "high", LeasedAt: at(544 * time.Second), LastEvent: "tool_start Bash go test"},
			{ID: 3, TaskID: 345, Kind: "work", Harness: "codex", Model: "gpt-5", LeasedAt: at(3723 * time.Second)},
			{ID: 50, Kind: "plan_review", Harness: "claude", Model: "opus", LeasedAt: at(45 * time.Second)},
		},
	}
	got := topPlanLines(st, 100)
	want := []string{
		"  ▸  #342  clip frame            work         glm-5.3             3m12s     Edit cmdtop.go",
		"  ▸  #343  show plan state word  fix          claude/sonnet@high  9m04s     Bash go test",
		"  ▸  #345  docs                  work         codex/gpt-5         1h02m03s",
		"  ▸        job #50               plan_review  claude/opus         45s",
	}
	if len(got) != 5 {
		t.Fatalf("rows = %q", got)
	}
	for i, w := range want {
		if got[1+i] != w {
			t.Errorf("row %d = %q\nwant      %q", i, got[1+i], w)
		}
	}
	// A last event too long for the line is cut at its end, the title at 8.
	st.Running[1].LastEvent = "tool_start " + strings.Repeat("e", 60)
	if row := topPlanLines(st, 100)[2]; utf8.RuneCountInString(row) > 100 || !strings.HasSuffix(row, "...") ||
		!strings.Contains(row, "show ...") {
		t.Errorf("long event row = %q", row)
	}
	// Below 80 terminal columns the last event is left out.
	if row := topPlanLines(st, 70)[1]; strings.Contains(row, "Edit") {
		t.Errorf("narrow row keeps the event: %q", row)
	}
}

// TestTopAgentName: harness/model@effort, a name of more than two
// segments cut to its last one, the effort kept.
func TestTopAgentName(t *testing.T) {
	for _, tt := range []struct{ harness, model, effort, want string }{
		{"claude", "sonnet", "high", "claude/sonnet@high"},
		{"opencode", "zai-coding-plan/glm-5.3", "", "glm-5.3"},
		{"opencode", "zai-coding-plan/glm-5.3", "low", "glm-5.3@low"},
		{"claude", "", "high", "claude"},
		{"", "", "", ""},
	} {
		if got := topAgentName(tt.harness, tt.model, tt.effort); got != tt.want {
			t.Errorf("topAgentName(%q, %q, %q) = %q, want %q", tt.harness, tt.model, tt.effort, got, tt.want)
		}
	}
}

// TestTopAge: whole seconds under a minute, minutes and seconds under an
// hour, hours with minutes and seconds padded to two digits.
func TestTopAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45s", 9*time.Minute + 4*time.Second: "9m04s", time.Hour + 2*time.Minute + 3*time.Second: "1h02m03s",
	} {
		if got := topAge(topNow.Add(-d).UTC().Format(engine.TimeLayout), topNow); got != want {
			t.Errorf("topAge(%s) = %q, want %q", d, got, want)
		}
	}
	if got := topAge("", topNow); got != "" {
		t.Errorf("no lease = %q, want empty", got)
	}
}

// TestTopPlanTotalsLeaveOutCancelledAndRetired: cancelled and retired
// tasks are not part of the total, so a plan that finished its work reads
// complete.
func TestTopPlanTotalsLeaveOutCancelledAndRetired(t *testing.T) {
	var tasks []engine.StatusTask
	for i := int64(1); i <= 8; i++ {
		tasks = append(tasks, engine.StatusTask{ID: i, State: "done"})
	}
	for i := int64(9); i <= 13; i++ {
		tasks = append(tasks, engine.StatusTask{ID: i, State: "cancelled"})
	}
	tasks = append(tasks, engine.StatusTask{ID: 14, State: "retired"})
	st := api.Status{Plans: []engine.StatusPlan{
		{ID: 96, Title: "Rework", State: "done", Tasks: tasks},
		{ID: 97, Title: "Live", State: "active", Tasks: []engine.StatusTask{
			{ID: 1, State: "done"}, {ID: 2, State: "done"}, {ID: 3, State: "done"},
			{ID: 4, State: "cancelled"}, {ID: 5, State: "retired"}, {ID: 6, Title: "go", State: "running"}}},
	}}
	right := func(left, tail string) string {
		return left + strings.Repeat(" ", 80-utf8.RuneCountInString(left)-utf8.RuneCountInString(tail)) + tail
	}
	want := []string{right("#96 Rework", "done"), right("#97 Live", "■■■□  3/4"), "  ·  #6  go  (queued)"}
	if got := topPlanLines(st, 80); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTopNeedsYouPlanWithoutReason: a plan entry with no reason shows its
// title and what the plan waits for, not the raw state word.
func TestTopNeedsYouPlanWithoutReason(t *testing.T) {
	tests := []struct {
		item engine.NeedsYouItem
		want string
	}{
		{engine.NeedsYouItem{Kind: "plan", ID: 2, State: "pending_approval", Title: "Top polish"}, "! #2 Top polish awaiting approval"},
		{engine.NeedsYouItem{Kind: "plan", ID: 3, State: "needs_foreman", Title: "Top polish"}, "! #3 Top polish needs foreman"},
		{engine.NeedsYouItem{Kind: "plan", ID: 4, State: "pending_approval"}, "! #4 awaiting approval"},
		{engine.NeedsYouItem{Kind: "plan", ID: 5, State: "pending_approval", Title: "Top polish", Reason: "over the limit"}, "! #5 over the limit"},
	}
	for _, tt := range tests {
		if got := topNeedsYouLine(tt.item); got != tt.want {
			t.Errorf("line = %q, want %q", got, tt.want)
		}
	}
}

// TestTopPlanlessJobsHaveNoHead: a running job no live plan claims used
// to render as a bare block under NEEDS YOU, reading as part of it. Now a
// foreman turn sits under a FOREMAN head and any other such job under a
// JOBS head, each its own section between NEEDS YOU and PLANS.
func TestTopPlanlessJobsHaveNoHead(t *testing.T) {
	foremanJob := engine.StatusJob{ID: 2619, Kind: "foreman", Harness: "claude", Model: "opus"}
	otherJob := engine.StatusJob{ID: 63, Kind: "plan_review"}
	nb := topNow.Add(2 * time.Minute).UTC().Format(engine.TimeLayout)
	calm := func() topModel {
		m := busyTopModel()
		m.status.NeedsYou, m.status.Running, m.status.Held, m.status.Plans = nil, nil, nil, nil
		return m
	}
	// between returns the view's lines from NEEDS YOU's top border through
	// PLANS's, each border as `╭ TITLE` or `╰` and each row as its text.
	between := func(width int, m topModel) []string {
		lines := topBoxed(renderTop(m, width, 0, topNow, term.New(logo.Caps{UTF8: true})))
		from, to := -1, -1
		for i, l := range lines {
			if strings.HasPrefix(l, "╭ NEEDS YOU") {
				from = i
			}
			if strings.HasPrefix(l, "╭ PLANS") {
				to = i
			}
		}
		if from < 0 || to < from {
			t.Fatalf("view lacks NEEDS YOU or PLANS:\n%s", strings.Join(lines, "\n"))
		}
		return lines[from : to+1]
	}
	head := func(title string) string { return "╭ " + title }
	fline := "  ▸  job #2619  foreman  claude/opus"
	oline := "  ▸  job #63  plan_review"
	needsLine := "! #2 should the sprocket be blue?"
	nothing := "✓ nothing needs you"

	t.Run("reported case", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{foremanJob}
		want := []string{head("NEEDS YOU"), nothing, "╰", head("FOREMAN"), fline, "╰", head("PLANS")}
		if got := between(80, m); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("foreman only with needs-you entries", func(t *testing.T) {
		m := calm()
		m.status.NeedsYou = busyTopModel().status.NeedsYou
		m.status.Running = []engine.StatusJob{foremanJob}
		got := strings.Join(between(80, m), "\n")
		want := strings.Join([]string{head("NEEDS YOU 1"), needsLine, "╰", head("FOREMAN"), fline, "╰", head("PLANS")}, "\n")
		if got != want || strings.Contains(got, "JOBS") {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("non-foreman job only", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{otherJob}
		got := strings.Join(between(80, m), "\n")
		want := strings.Join([]string{head("NEEDS YOU"), nothing, "╰", head("JOBS"), oline, "╰", head("PLANS")}, "\n")
		if got != want || strings.Contains(got, "FOREMAN") {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("both blocks, foreman first", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{otherJob, foremanJob}
		got := strings.Join(between(80, m), "\n")
		want := strings.Join([]string{head("NEEDS YOU"), nothing, "╰", head("FOREMAN"), fline, "╰",
			head("JOBS"), oline, "╰", head("PLANS")}, "\n")
		if got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("held and retrying jobs are classified", func(t *testing.T) {
		m := calm()
		m.status.Held = []engine.HeldJob{
			{ID: 61, Kind: "foreman", Layer: "no_pair", Agent: "claude/opus"},
			{ID: 64, Kind: "plan_review", Layer: "runner"},
		}
		m.status.Retries = []engine.InfraRetry{
			{ID: 62, Kind: "foreman", NotBefore: nb, Reason: "could not resolve github.com"},
			{ID: 65, Kind: "plan_review", NotBefore: nb, Reason: "dns"},
		}
		style := term.New(logo.Caps{UTF8: true})
		trim := func(s string) string { return strings.TrimRight(s, " ") }
		want := []string{head("NEEDS YOU"), nothing, "╰",
			head("FOREMAN"),
			"  ?  job #61  foreman held: foreman turn waiting: no runner offers claude...",
			trim(topRetryLine(m.status.Retries[0], topNow, style, 76)), "╰",
			head("JOBS"),
			"  ?  job #64  plan_review held: waiting for a runner slot",
			trim(topRetryLine(m.status.Retries[1], topNow, style, 76)), "╰",
			head("PLANS")}
		if got := between(80, m); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("no plan-less job", func(t *testing.T) {
		got := plainTop(busyTopModel(), 100, 0)
		if strings.Contains(got, "FOREMAN") || strings.Contains(got, "JOBS") {
			t.Errorf("a box renders with no plan-less job:\n%s", got)
		}
		if !strings.Contains(strings.Join(topBoxed(strings.Split(got, "\n")), "\n"), "! #2 should the sprocket be blue?\n╰\n╭ PLANS") {
			t.Errorf("NEEDS YOU is not followed by PLANS:\n%s", got)
		}
	})
	t.Run("narrow boxes stay", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{otherJob, foremanJob}
		got := between(59, m)
		for _, title := range []string{"╭ FOREMAN", "╭ JOBS"} {
			found := false
			for _, l := range got {
				found = found || l == title
			}
			if !found {
				t.Errorf("no %s box in\n%s", title, strings.Join(got, "\n"))
			}
		}
	})
	t.Run("height budget counts the blocks", func(t *testing.T) {
		m := busyTopModel()
		m.status.Running = append(m.status.Running, otherJob, foremanJob)
		style := term.New(logo.Caps{UTF8: true})
		full := renderTop(m, 60, 0, topNow, style)
		if got := renderTop(m, 60, len(full), topNow, style); strings.Join(got, "\n") != strings.Join(full, "\n") {
			t.Errorf("at the exact height lines yield:\n%s", strings.Join(got, "\n"))
		}
		short := renderTop(m, 60, len(full)-1, topNow, style)
		if len(short) != len(full)-1 {
			t.Errorf("one line less: %d lines, want %d", len(short), len(full)-1)
		}
		if len(short) == len(full) || strings.Join(short[:len(short)-1], "\n") != strings.Join(full[:len(short)-1], "\n") {
			t.Errorf("one line less did not yield only the events strip's second line:\n%s", strings.Join(short, "\n"))
		}
	})
}

// TestTopPlanless: the jobs no listed live plan claims — a taskless
// running turn, a taskless held one, a taskless retry, the review of a
// plan the tree does not list — render in the FOREMAN and JOBS blocks,
// which are absent from the view when every job has its plan.
func TestTopPlanless(t *testing.T) {
	nb := topNow.Add(2 * time.Minute).UTC().Format(engine.TimeLayout)
	model := busyTopModel()
	model.status.Running = append(model.status.Running,
		engine.StatusJob{ID: 60, Kind: "foreman", Harness: "claude", Model: "opus"},
		engine.StatusJob{ID: 63, Kind: "plan_review"})
	model.status.Held = append(model.status.Held,
		engine.HeldJob{ID: 61, Kind: "foreman", Layer: "no_pair", Agent: "claude/opus"})
	model.status.Retries = []engine.InfraRetry{{ID: 62, Kind: "foreman", NotBefore: nb, Reason: "could not resolve github.com"}}
	// Job 63 reviews a plan hidden behind the finished ones the tree lists.
	model.status.HiddenFinishedPlans = 1
	got := readTop(model, 100, 0)
	want := "! #2 should the sprocket be blue?" +
		"\n╰\n╭ FOREMAN" +
		"\n  ▸  job #60  foreman  claude/opus" +
		"\n  ?  job #61  foreman held: foreman turn waiting: no runner offers claude/opus" +
		"\n  ↺ job #62 foreman: retry in 2m: could not resolve github.com" +
		"\n╰\n╭ JOBS" +
		"\n  ▸  job #63  plan_review" +
		"\n╰\n╭ PLANS"
	if !strings.Contains(got, want) {
		t.Errorf("view lacks the FOREMAN and JOBS boxes after NEEDS YOU:\n%s", got)
	}
}

// crowdedTopModel is a busy engine on a short screen: twelve live plans
// each with a running job and a queued task, six finished plans listed
// and four more hidden, a full NEEDS YOU queue, a foreman turn and a held
// job no plan claims, and four agents of which three are idle.
func crowdedTopModel() topModel {
	m := busyTopModel()
	m.status.Running, m.status.Held, m.status.Plans = nil, nil, nil
	finished := func(id int64) engine.StatusPlan {
		return engine.StatusPlan{ID: id, Title: fmt.Sprintf("done plan %d", id), State: "done",
			Tasks: []engine.StatusTask{{ID: id * 10, Title: "t", State: "done"}}}
	}
	for id := int64(30); id < 32; id++ {
		m.status.Plans = append(m.status.Plans, finished(id))
	}
	for i := int64(1); i <= 12; i++ {
		m.status.Plans = append(m.status.Plans, engine.StatusPlan{
			ID: i, Title: fmt.Sprintf("live plan %d", i), State: "active",
			Tasks: []engine.StatusTask{
				{ID: 100 + i, Title: "running work", State: "running"},
				{ID: 200 + i, Title: "queued work", State: "ready"},
			}})
		m.status.Running = append(m.status.Running,
			engine.StatusJob{ID: 1000 + i, TaskID: 100 + i, Kind: "work", Status: "running", Harness: "claude", Model: "sonnet"})
	}
	for id := int64(32); id < 36; id++ {
		m.status.Plans = append(m.status.Plans, finished(id))
	}
	m.status.HiddenFinishedPlans = 4
	m.status.Running = append(m.status.Running, engine.StatusJob{ID: 2000, Kind: "foreman", Status: "running"})
	m.status.Held = []engine.HeldJob{{ID: 2001, Kind: "plan_review", Layer: "runner"}}
	m.status.NeedsYou = nil
	for i := int64(1); i <= 5; i++ {
		m.status.NeedsYou = append(m.status.NeedsYou,
			engine.NeedsYouItem{Kind: "task", ID: i, State: "needs_foreman", Reason: fmt.Sprintf("question %d", i)})
	}
	m.capacity.Agents = append(m.capacity.Agents,
		engine.CapacityAgent{Harness: "codex", Model: "a", CapLayer: "none", Quota: "ready"},
		engine.CapacityAgent{Harness: "codex", Model: "b", CapLayer: "none", Quota: "ready"},
		engine.CapacityAgent{Harness: "codex", Model: "c", CapLayer: "none", Quota: "ready"})
	return m
}

// TestRenderTopOverflowsAShortTerminal: on a 12-row terminal top drew 19
// lines, so the alternate screen scrolled and the header, the error line
// and NEEDS YOU went first, while running job lines yielded and finished
// plans never did. The frame never has more lines than the height, every
// finished plan drops before any running line does, and the header and
// NEEDS YOU survive at each height.
func TestRenderTopOverflowsAShortTerminal(t *testing.T) {
	m := crowdedTopModel()
	m.err = errors.New("connection refused")
	m.hint = "2 models offered; run brigada agents"
	const running = 13
	for _, height := range []int{12, 24, 40} {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			lines := renderTop(m, 80, height, topNow, term.New(logo.Caps{UTF8: true}))
			got := strings.Join(lines, "\n")
			if len(lines) > height {
				t.Fatalf("got %d lines, want at most %d:\n%s", len(lines), height, got)
			}
			if !strings.HasPrefix(lines[0], " ▙ brigada   r · up") || !strings.Contains(lines[1], "engine: connection refused") {
				t.Errorf("the header and its error line are not first:\n%s", got)
			}
			for _, w := range []string{"2 models offered", "NEEDS YOU 5", "! #1 question 1", "! #2 question 2", "! #3 question 3", "+2 more"} {
				if !strings.Contains(got, w) {
					t.Errorf("the frame lacks %q:\n%s", w, got)
				}
			}
			shown := strings.Count(got, "▸")
			finished := strings.Count(got, "done plan")
			if shown < running && finished > 0 {
				t.Errorf("a running line dropped (%d of %d) while %d finished plans stay:\n%s", shown, running, finished, got)
			}
			if height == 12 && shown >= running {
				t.Errorf("no running line dropped at 12 rows, the case is vacuous:\n%s", got)
			}
		})
	}
}

// TestRenderTopBelowTheFloor: below the lines the header and NEEDS YOU
// need, the height wins. The meters row is the last row outside the two
// to go, and the hard clip cuts into NEEDS YOU from its bottom.
func TestRenderTopBelowTheFloor(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	header := " ▙ brigada   r · up 2h0m"
	needsHead := "╭─ NEEDS YOU 1 ─"
	entry := "│ ! #2 should the sprocket be blue?"
	queue := busyTopModel()
	queue.status.NeedsYou = nil
	for i := int64(1); i <= 5; i++ {
		queue.status.NeedsYou = append(queue.status.NeedsYou,
			engine.NeedsYouItem{Kind: "task", ID: i, State: "needs_foreman", Reason: "q"})
	}
	tests := []struct {
		name   string
		model  topModel
		height int
		want   []string // line prefixes
	}{
		{"height 3 is clipped", busyTopModel(), 3, []string{header, needsHead, entry}},
		{"height 4 drops the meters", busyTopModel(), 4, []string{header, needsHead, entry, "╰"}},
		{"height 5 keeps the meters", busyTopModel(), 5, []string{header, " ━", needsHead, entry, "╰"}},
		{"hard clip", queue, 2, []string{header, "╭─ NEEDS YOU 5 ─"}},
		{"hard clip into the entries", queue, 4, []string{header, "╭─ NEEDS YOU 5 ─", "│ ! #1 q", "│ ! #2 q"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := renderTop(tt.model, 80, tt.height, topNow, style)
			if len(lines) != len(tt.want) {
				t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(tt.want), strings.Join(lines, "\n"))
			}
			for i, w := range tt.want {
				if !strings.HasPrefix(lines[i], w) {
					t.Errorf("line %d = %q, want prefix %q", i, lines[i], w)
				}
			}
		})
	}
}

// TestRenderTopFitsSize: the busy fixture at 40x10 has at most 10 lines
// (a drop may undershoot by a row or two) with the header first and NEEDS
// YOU kept, and no line is wider than 40.
func TestRenderTopFitsSize(t *testing.T) {
	lines := renderTop(busyTopModel(), 40, 10, topNow, term.New(logo.Caps{UTF8: true}))
	got := strings.Join(lines, "\n")
	if len(lines) > 10 || len(lines) < 8 {
		t.Errorf("got %d lines, want 8 to 10:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], " ▙ brigada") || !strings.Contains(got, "╭─ NEEDS YOU 1 ") || !strings.Contains(got, "! #2 should the sprocket be blue?") {
		t.Errorf("the header is not first or NEEDS YOU is gone:\n%s", got)
	}
	for _, l := range lines {
		if n := term.VisibleWidth(l); n > 40 {
			t.Errorf("line %q is %d wide", l, n)
		}
	}
}

// TestRenderTopHeightYieldOrder: rows beyond the height yield class by
// class, each from the bottom of the view upward, one row at a time:
// event row 2, the held and parked rows, the running row, then the class
// of every other row — event row 1, the global row, the agent rows
// bottom-up, the plan's own row, the meters row last — and last the hard
// clip into NEEDS YOU. A box's borders go with its section's last row, so
// a height may be undershot by a row or two. (Queued rows and idle agents
// are in TestRenderTopQueuedYield and TestTopAgentsBudget.)
func TestRenderTopHeightYieldOrder(t *testing.T) {
	model := agentsModel()
	// A standalone review's branch is long enough that the two items
	// wrap at width 76 and each fits a line whole.
	model.events = []history.Item{
		{Type: history.TypeEvent, ID: 11, Kind: "review_verdict", ProjectID: 1, JobID: 5,
			Payload:   []byte(`{"status":"approve","branch":"task/3-1-oil-the-gears-after-the-frobnicator-fix"}`),
			CreatedAt: "2026-10-20T15:02:41.000000000Z"},
		model.events[1],
	}
	// The marks, in the order their rows drop: event row 2, the held row,
	// the parked row, the running row, then the class of every other row —
	// event row 1, the global row, the agent rows bottom-up, the plan's own
	// row — and the meters row last.
	marks := []string{
		"review approve", "?  #4  polish the knobs", "! #2 wire the sprocket", "▸  #3  oil the gears",
		"15:02  #3 done", "● claude/opus", "↺ codex/gpt-5",
		"#1 fix the frobnicator", "3/8 slots",
	}
	style := term.New(logo.Caps{UTF8: true})
	full := renderTop(model, 76, 0, topNow, style)
	for _, m := range marks {
		if !strings.Contains(strings.Join(full, "\n"), m) {
			t.Fatalf("the unbounded view lacks %q:\n%s", m, strings.Join(full, "\n"))
		}
	}
	for height := len(full); height >= 2; height-- {
		lines := renderTop(model, 76, height, topNow, style)
		got := strings.Join(lines, "\n")
		if len(lines) > height || (height >= 6 && len(lines) < height-2) {
			t.Errorf("height %d: got %d lines (a drop undershoots by at most 2):\n%s", height, len(lines), got)
		}
		if !strings.HasPrefix(lines[0], " ▙ brigada   r · up") {
			t.Errorf("height %d: first line is %q, want the header", height, lines[0])
		}
		for i := range marks {
			if strings.Contains(got, marks[i]) {
				continue
			}
			// A mark gone means every earlier mark is gone.
			for _, earlier := range marks[:i] {
				if strings.Contains(got, earlier) {
					t.Errorf("height %d: %q dropped while %q stays:\n%s", height, marks[i], earlier, got)
				}
			}
		}
		for _, l := range lines {
			if n := term.VisibleWidth(l); n > 76 {
				t.Errorf("height %d: line %q is %d wide", height, l, n)
			}
		}
		if height >= 5 && !strings.Contains(got, "╭─ NEEDS YOU 2 ") {
			t.Errorf("height %d: NEEDS YOU is gone:\n%s", height, got)
		}
	}
}

func TestRenderTopShowsError(t *testing.T) {
	model := busyTopModel()
	model.err = errors.New("connection refused")
	got := plainTop(model, 100, 0)
	if !strings.Contains(got, "engine: connection refused") || !strings.Contains(got, "claude/opus") {
		t.Errorf("error line or kept data missing:\n%s", got)
	}
}

func TestTopFetch(t *testing.T) {
	fake := &fakeTop{
		status:   busyTopModel().status,
		capacity: busyTopModel().capacity,
		projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}},
		page:     history.Page{Items: busyTopModel().events},
	}
	model, err := topFetch(context.Background(), fake, "https://example.invalid/r.git")
	if err != nil || model.err != nil {
		t.Fatalf("topFetch: %v, model.err %v", err, model.err)
	}
	if model.status.Pid != 4242 || len(model.events) != 2 || len(model.capacity.Agents) != 1 {
		t.Errorf("model incomplete: %+v", model)
	}
}

func TestTopFetchPartialError(t *testing.T) {
	fake := &fakeTop{
		status:      busyTopModel().status,
		projects:    []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}},
		capacityErr: errors.New("capacity down"),
	}
	model, err := topFetch(context.Background(), fake, "https://example.invalid/r.git")
	if err == nil || model.err == nil || model.status.Pid != 4242 {
		t.Errorf("want the error with the status kept, got %v / %+v", err, model)
	}
}

func TestTopFetchUnknownProject(t *testing.T) {
	_, err := topFetch(context.Background(), &fakeTop{}, "https://example.invalid/nope.git")
	if err == nil || !strings.Contains(err.Error(), "no active project") {
		t.Errorf("got %v, want no active project", err)
	}
}

func TestTopFetchProjectsErrorSetsModelErr(t *testing.T) {
	boom := errors.New("engine gone")
	model, err := topFetch(context.Background(), &fakeTop{projectsErr: boom}, "https://example.invalid/r.git")
	if !errors.Is(err, boom) {
		t.Errorf("returned error %v, want %v", err, boom)
	}
	if !errors.Is(model.err, boom) {
		t.Errorf("model.err = %v, want %v", model.err, boom)
	}
}

// TestTopFetchWithProjectScopesEveryCall: --project puts its id on the
// status, the capacity and the history.
func TestTopFetchWithProjectScopesEveryCall(t *testing.T) {
	fake := &fakeTop{projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}}}
	if _, err := topFetch(context.Background(), fake, "https://example.invalid/r.git"); err != nil {
		t.Fatal(err)
	}
	if fake.readerProject != 7 || fake.capacityProject != 7 || fake.historyProject != 7 {
		t.Errorf("reader %d, capacity %d, history %d; want 7 on all three", fake.readerProject, fake.capacityProject, fake.historyProject)
	}
	if fake.scopedStatusCalls != 1 || fake.statusCalls != 1 {
		t.Errorf("scoped Status calls %d, unscoped %d; want one of each", fake.scopedStatusCalls, fake.statusCalls)
	}
}

// TestTopFetchWithoutProjectIgnoresTheWorkingDirectory: no --project
// scopes nothing, even in a checkout of a project the engine has, and a
// tick makes exactly one Status call. Projects is read once, for the
// header's scope.
func TestTopFetchWithoutProjectIgnoresTheWorkingDirectory(t *testing.T) {
	const origin = "https://example.invalid/r.git"
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "remote", "add", "origin", origin)
	t.Chdir(repo)
	fake := &fakeTop{projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: origin}}}
	if _, err := topFetch(context.Background(), fake, ""); err != nil {
		t.Fatal(err)
	}
	if fake.readerProject != 0 || fake.capacityProject != 0 || fake.historyProject != 0 {
		t.Errorf("reader %d, capacity %d, history %d; want none scoped", fake.readerProject, fake.capacityProject, fake.historyProject)
	}
	if fake.statusCalls != 1 || fake.scopedStatusCalls != 0 {
		t.Errorf("unscoped Status calls %d, scoped %d; want exactly one unscoped", fake.statusCalls, fake.scopedStatusCalls)
	}
	if fake.projectsCalls != 1 {
		t.Errorf("Projects called %d times, want once", fake.projectsCalls)
	}
}

// TestTopHeaderNamesTheScope: the header names the repo with --project or
// with one project, counts two or more, and drops the scope when the
// projects cannot be read, the error on its line and the rest rendered.
func TestTopHeaderNamesTheScope(t *testing.T) {
	const repo = "https://example.invalid/acme/brigada.git"
	one := []engine.ProjectInfo{{ProjectID: 7, RepoURL: repo}}
	two := append(one, engine.ProjectInfo{ProjectID: 8, RepoURL: "https://example.invalid/acme/other.git"})
	up := "up 1h0m"
	for _, tt := range []struct {
		name        string
		projects    []engine.ProjectInfo
		projectsErr error
		repo        string
		want        string
	}{
		{"project flag", two, nil, repo, " ▙ brigada   brigada · " + up + " "},
		{"one project", one, nil, "", " ▙ brigada   brigada · " + up + " "},
		{"two projects", two, nil, "", " ▙ brigada   2 projects · " + up + " "},
		{"failed projects", nil, errors.New("projects down"), "", " ▙ brigada   " + up + " "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeTop{status: ownProjectStatus(), projects: tt.projects, projectsErr: tt.projectsErr}
			model, err := topFetch(context.Background(), fake, tt.repo)
			if !errors.Is(err, tt.projectsErr) {
				t.Fatalf("err = %v, want %v", err, tt.projectsErr)
			}
			got := plainTop(model, 100, 0)
			if !strings.HasPrefix(got, tt.want) {
				t.Errorf("frame opens %q, want %q", strings.SplitN(got, "\n", 2)[0], tt.want)
			}
			if !strings.Contains(got, "own project plan") {
				t.Errorf("the plans did not render:\n%s", got)
			}
			if tt.projectsErr != nil && !strings.Contains(got, "engine: projects down") {
				t.Errorf("no error line:\n%s", got)
			}
		})
	}
}

// TestTopPlanLineNamesItsRepo: a plan line carries its repo when more
// than one project shows, not with one or under --project.
func TestTopPlanLineNamesItsRepo(t *testing.T) {
	const repo = "https://example.invalid/acme/brigada.git"
	one := []engine.ProjectInfo{{ProjectID: 7, RepoURL: repo}}
	two := append(one, engine.ProjectInfo{ProjectID: 8, RepoURL: "https://example.invalid/acme/other.git"})
	st := api.Status{Plans: []engine.StatusPlan{{ID: 101, RepoURL: repo, Title: "Fix events strip", State: "active"}}}
	for _, tt := range []struct {
		name     string
		projects []engine.ProjectInfo
		repo     string
		want     string
	}{
		{"two projects", two, "", "#101 brigada Fix events strip 0/0"},
		{"one project", one, "", "#101 Fix events strip 0/0"},
		{"project flag", two, repo, "#101 Fix events strip 0/0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model, err := topFetch(context.Background(), &fakeTop{status: st, projects: tt.projects}, tt.repo)
			if err != nil {
				t.Fatal(err)
			}
			got := plainTop(model, 100, 0)
			found := false
			for _, l := range strings.Split(got, "\n") {
				found = found || strings.Contains(topCollapse(l), tt.want)
			}
			if !found {
				t.Errorf("frame lacks %q:\n%s", tt.want, got)
			}
		})
	}
}

// TestTopPlanTagKeepsItsCells: on a narrow line the title gives way to
// the tag, and the line still fits; a finished plan carries the tag too.
func TestTopPlanTagKeepsItsCells(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	const repo = "https://example.invalid/acme/brigada.git"
	live := engine.StatusPlan{ID: 101, RepoURL: repo, Title: strings.Repeat("long title ", 8), State: "active"}
	got := topPlanLine(live, "brigada", newTopPlanLayout([]engine.StatusPlan{live}, true, nil, plain, 50), nil, plain, 50)
	if !strings.HasPrefix(got, "#101 brigada  long") || !strings.HasSuffix(got, "... 0/0") {
		t.Errorf("line = %q, want the tag whole and the title cut", got)
	}
	if n := utf8.RuneCountInString(got); n != 50 {
		t.Errorf("line is %d cells, want 50: %q", n, got)
	}
	done := engine.StatusPlan{ID: 102, RepoURL: repo, Title: "shipped", State: "done"}
	if got := topPlanLine(done, "brigada", newTopPlanLayout([]engine.StatusPlan{done}, true, nil, plain, 50), nil, plain, 50); topCollapse(got) != "#102 brigada shipped done" || !strings.HasSuffix(got, "done") || utf8.RuneCountInString(got) != 50 {
		t.Errorf("finished line = %q", got)
	}
}

func TestTopRepoName(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/acme/brigada.git": "brigada",
		"https://github.com/acme/brigada":     "brigada",
		"https://github.com/acme/brigada/":    "brigada",
		"git@github.com:brigada.git":          "brigada",
		"brigada":                             "brigada",
	} {
		if got := topRepoName(url); got != want {
			t.Errorf("topRepoName(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestTopHelpSaysEveryProject: --project's help names the default.
func TestTopHelpSaysEveryProject(t *testing.T) {
	var out, errOut bytes.Buffer
	runTop(context.Background(), []string{"--help"}, &out, &errOut)
	if got := out.String() + errOut.String(); !strings.Contains(got, "default: every project") {
		t.Errorf("help lacks the default:\n%s", got)
	}
}

// otherProjectsFake serves the engine as --project sees it: the given
// status scoped; on the unscoped one a broken agent, another project's
// task and plan entries and its plan; and the capacity listing the agent
// as broken.
func otherProjectsFake(scoped api.Status) *fakeTop {
	unscoped := scoped
	unscoped.NeedsYou = []engine.NeedsYouItem{
		{Kind: "task", ID: 901, State: "needs_user", Reason: "other task entry"},
		{Kind: "plan", ID: 902, State: "pending_approval", Reason: "other plan entry"},
		{Kind: "agent", ID: 7, State: "broken", Title: "claude/opus", Reason: "login expired"},
	}
	unscoped.Plans = []engine.StatusPlan{{ID: 903, Title: "other project plan", State: "active"}}
	return &fakeTop{
		status:       unscoped,
		scopedStatus: &scoped,
		capacity: engine.CapacitySnapshot{Agents: []engine.CapacityAgent{
			{Harness: "claude", Model: "opus", CapLayer: "none", Quota: "broken"},
		}},
		projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}},
	}
}

// ownProjectStatus is a scoped status holding this project's one plan.
func ownProjectStatus() api.Status {
	return api.Status{
		Pid:       4242,
		StartedAt: topNow.Add(-time.Hour).UTC().Format(engine.TimeLayout),
		Plans:     []engine.StatusPlan{{ID: 1, Title: "own project plan", State: "active"}},
	}
}

// TestTopProjectKeepsBrokenAgentsAndNothingElse: the unscoped call adds
// the agent entry, so NEEDS YOU shows it and AGENTS its reason, and no
// other entry or plan of another project reaches the frame.
func TestTopProjectKeepsBrokenAgentsAndNothingElse(t *testing.T) {
	scoped := ownProjectStatus()
	scoped.NeedsYou = []engine.NeedsYouItem{{Kind: "task", ID: 3, State: "needs_user", Reason: "own task entry"}}
	model, err := topFetch(context.Background(), otherProjectsFake(scoped), "https://example.invalid/r.git")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(model.status.NeedsYou); n != 2 || model.status.NeedsYou[0].ID != 3 || model.status.NeedsYou[1].Kind != "agent" {
		t.Fatalf("NeedsYou = %+v, want the task, then the agent", model.status.NeedsYou)
	}
	got := plainTop(model, 100, 0)
	for _, want := range []string{"! #3 own task entry", "! claude/opus login expired", "own project plan", "✗ claude/opus  0/none  login expired", "2 needs"} {
		if !strings.Contains(got, want) {
			t.Errorf("frame lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"#901", "#902", "#903", "other task entry", "other plan entry", "other project plan", "✗ broken"} {
		if strings.Contains(got, not) {
			t.Errorf("frame shows %q:\n%s", not, got)
		}
	}
}

// TestTopProjectAgentSurvivesTheCap: with a full scoped queue the merged
// agent still takes a NEEDS YOU line, +N more counts the rest, and no
// other entry of another project shows.
func TestTopProjectAgentSurvivesTheCap(t *testing.T) {
	scoped := ownProjectStatus()
	for i := int64(1); i <= 4; i++ {
		scoped.NeedsYou = append(scoped.NeedsYou, engine.NeedsYouItem{Kind: "task", ID: i, State: "needs_user", Reason: "own task entry"})
	}
	model, err := topFetch(context.Background(), otherProjectsFake(scoped), "https://example.invalid/r.git")
	if err != nil {
		t.Fatal(err)
	}
	got := plainTop(model, 100, 0)
	for _, want := range []string{"! #1 own task entry", "! #2 own task entry", "! claude/opus login expired", "+2 more", "✗ claude/opus  0/none  login expired"} {
		if !strings.Contains(got, want) {
			t.Errorf("frame lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"#3 own", "#901", "#902", "#903", "other task entry", "other plan entry", "other project plan", "✗ broken"} {
		if strings.Contains(got, not) {
			t.Errorf("frame shows %q:\n%s", not, got)
		}
	}
}

// TestTopProjectFailedAgentCallKeepsTheScopedStatus: the unscoped call's
// failure is the tick's error, the project's plans still render and the
// snapshot fails as on any fetch error.
func TestTopProjectFailedAgentCallKeepsTheScopedStatus(t *testing.T) {
	boom := errors.New("unscoped down")
	fake := otherProjectsFake(ownProjectStatus())
	fake.statusErr = boom
	model, err := topFetch(context.Background(), fake, "https://example.invalid/r.git")
	if !errors.Is(err, boom) || !errors.Is(model.err, boom) {
		t.Fatalf("err %v, model.err %v; want %v", err, model.err, boom)
	}
	if got := plainTop(model, 100, 0); !strings.Contains(got, "own project plan") || strings.Contains(got, "other project plan") {
		t.Errorf("the scoped plans did not survive:\n%s", got)
	}
	var out, errOut bytes.Buffer
	if code := printTopSnapshot(context.Background(), fake, "https://example.invalid/r.git", "", topNow, &out, &errOut); code != 1 {
		t.Errorf("snapshot exit %d, want 1", code)
	}
}

// TestTopProjectFailedStatusSkipsTheAgentCall: with no scoped status
// there is nothing to merge into.
func TestTopProjectFailedStatusSkipsTheAgentCall(t *testing.T) {
	boom := errors.New("scoped down")
	fake := otherProjectsFake(ownProjectStatus())
	fake.scopedStatusErr = boom
	if _, err := topFetch(context.Background(), fake, "https://example.invalid/r.git"); !errors.Is(err, boom) {
		t.Fatalf("err %v, want %v", err, boom)
	}
	if fake.statusCalls != 0 {
		t.Errorf("unscoped Status calls = %d, want none", fake.statusCalls)
	}
}

// topRequests records the API requests and answers each with an empty
// body of its shape.
type topRequests struct{ seen []string }

func (r *topRequests) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = append(r.seen, req.URL.Path+"?"+req.URL.RawQuery)
	body, err := json.Marshal(struct{}{})
	if req.URL.Path == "/v1/projects" {
		body, err = json.Marshal([]engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}})
	}
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}

// TestTopAPIScopesTheRequests: the real client's requests carry the
// project with --project, but for the extra unscoped status, and carry
// none without it.
func TestTopAPIScopesTheRequests(t *testing.T) {
	count := func(seen []string, path string) (scoped, unscoped int) {
		for _, req := range seen {
			if !strings.HasPrefix(req, path+"?") {
				continue
			}
			if strings.Contains(req, "project=7") {
				scoped++
			} else {
				unscoped++
			}
		}
		return scoped, unscoped
	}
	requests := &topRequests{}
	client := topAPI{api.NewClientWithHTTPClient(&http.Client{Transport: requests}, "token")}
	if _, err := topFetch(context.Background(), client, "https://example.invalid/r.git"); err != nil {
		t.Fatal(err)
	}
	if s, u := count(requests.seen, "/v1/status"); s != 1 || u != 1 {
		t.Errorf("status scoped %d, unscoped %d, want 1 and 1: %q", s, u, requests.seen)
	}
	for _, path := range []string{"/v1/capacity", "/v1/history"} {
		if s, u := count(requests.seen, path); s != 1 || u != 0 {
			t.Errorf("%s scoped %d, unscoped %d, want 1 and 0: %q", path, s, u, requests.seen)
		}
	}
	requests.seen = nil
	if _, err := topFetch(context.Background(), client, ""); err != nil {
		t.Fatal(err)
	}
	if len(requests.seen) != 4 {
		t.Errorf("requests = %q, want projects, status, capacity and history", requests.seen)
	}
	for _, req := range requests.seen {
		if strings.Contains(req, "project=") {
			t.Errorf("request %q is scoped, want the whole engine", req)
		}
	}
}

func TestTopKeepPrevious(t *testing.T) {
	prev := busyTopModel()
	prev.scope, prev.tagPlans = "2 projects", true
	failed := topModel{err: errors.New("engine gone")}.keepPrevious(prev)
	if failed.scope != "2 projects" || !failed.tagPlans {
		t.Errorf("scope %q, tag %v: the failed fetch lost them", failed.scope, failed.tagPlans)
	}
	got := readTop(failed, 100, 0)
	if !strings.Contains(got, "engine: engine gone") || !strings.Contains(got, "claude/opus") || !strings.Contains(got, "15:02  #3 done\n15:02  #3 landed") {
		t.Errorf("failed fetch blanked the screen:\n%s", got)
	}
}

func TestPrintTopSnapshot(t *testing.T) {
	fake := &fakeTop{
		status:   busyTopModel().status,
		capacity: busyTopModel().capacity,
		projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}},
	}
	var out, errOut bytes.Buffer
	if code := printTopSnapshot(context.Background(), fake, "https://example.invalid/r.git", "", topNow, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), " ▙ brigada   r · up ") {
		t.Errorf("snapshot lacks the header:\n%s", out.String())
	}
	fake.projectsErr = errors.New("down")
	out.Reset()
	if code := printTopSnapshot(context.Background(), fake, "https://example.invalid/r.git", "", topNow, &out, &errOut); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

// topEventRowCount is how many event rows a frame shows.
func topEventRowCount(lines []string) int {
	n := 0
	for _, l := range topBoxed(lines) {
		if len(l) > 7 && l[2] == ':' && l[5] == ' ' && l[6] == ' ' {
			n++
		}
	}
	return n
}

// TestTopSnapshotShowsTenEvents: the piped snapshot renders with height 0,
// so 12 events show as the newest 10 rows, #12 first.
func TestTopSnapshotShowsTenEvents(t *testing.T) {
	fake := &fakeTop{
		status:   busyTopModel().status,
		capacity: busyTopModel().capacity,
		projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}},
		page:     history.Page{Items: topEventsFixture(12)},
	}
	var out, errOut bytes.Buffer
	if code := printTopSnapshot(context.Background(), fake, "https://example.invalid/r.git", "", topNow, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if n := topEventRowCount(lines); n != 10 {
		t.Errorf("snapshot has %d event rows, want 10:\n%s", n, out.String())
	}
	read := strings.Join(topBoxed(lines), "\n")
	if !strings.Contains(out.String(), "╭─ EVENTS ─") || !strings.Contains(read, "15:02  #12 done\n15:01  #11 done\n") ||
		strings.Contains(out.String(), "#2 done") {
		t.Errorf("snapshot events are not the newest 10, newest first:\n%s", out.String())
	}
}

// TestRenderTopEventRowYield: a tall screen uses its spare rows for
// events, up to 10 and no more; rows 3 and up drop first, from the
// bottom, before row 2, and the frame never exceeds the height; the
// report in the border stays as long as a row does.
func TestRenderTopEventRowYield(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	model := agentsModel()
	model.events = topEventsFixture(14)
	model.status.Report = &engine.StatusReport{ProjectID: 1, JobID: 41, Text: "All landed."}
	full := renderTop(model, 76, 0, topNow, style)
	if n := topEventRowCount(full); n != 10 {
		t.Fatalf("unbounded frame has %d event rows, want 10:\n%s", n, strings.Join(full, "\n"))
	}
	n := len(full)
	tall := renderTop(model, 76, n+30, topNow, style)
	if strings.Join(tall, "\n") != strings.Join(full, "\n") {
		t.Errorf("a tall screen shows more or fewer lines than 10 rows:\n%s", strings.Join(tall, "\n"))
	}
	for k := 0; k <= 8; k++ {
		lines := renderTop(model, 76, n-k, topNow, style)
		got := strings.Join(lines, "\n")
		if len(lines) != n-k || topEventRowCount(lines) != 10-k || !strings.Contains(got, "All landed. ─╮") ||
			!strings.Contains(strings.Join(topBoxed(lines), "\n"), "15:02  #14 done\n15:01  #13 done") {
			t.Errorf("at %d (rows 3+ drop one by one) got %d lines, %d rows:\n%s", n-k, len(lines), topEventRowCount(lines), got)
		}
		if k > 0 && strings.Contains(got, fmt.Sprintf("#%d done", 14-(10-k))) {
			t.Errorf("at %d the row #%d, the lowest, stays:\n%s", n-k, 14-(10-k), got)
		}
	}
	// Exactly room for 2 rows keeps both.
	two := renderTop(model, 76, n-8, topNow, style)
	if topEventRowCount(two) != 2 || !strings.Contains(strings.Join(two, "\n"), "15:01  #13 done") {
		t.Errorf("room for 2 rows:\n%s", strings.Join(two, "\n"))
	}
	// One shorter drops row 2; the report stays in the border with row 1.
	one := renderTop(model, 76, n-9, topNow, style)
	got := strings.Join(one, "\n")
	if len(one) != n-9 || topEventRowCount(one) != 1 || strings.Contains(got, "#13 done") || !strings.Contains(got, "All landed.") {
		t.Errorf("one line shorter should drop row 2 only:\n%s", got)
	}
	// No height ever gets a frame taller than it.
	for h := 1; h <= n; h++ {
		if lines := renderTop(model, 76, h, topNow, style); len(lines) > h {
			t.Errorf("height %d got %d lines", h, len(lines))
		}
	}
}

// topGoldenDir is testdata resolved at package init: other tests change
// the working directory, which would break a relative path.
var topGoldenDir, _ = filepath.Abs("testdata")

var updateTopGolden = flag.Bool("update", false, "rewrite the top snapshot golden files")

// quietTopModel is a fetched model with nothing to show: no events, no
// plans, nothing that needs the user.
func quietTopModel() topModel {
	return topModel{status: api.Status{
		Pid:       4242,
		StartedAt: topNow.Add(-2 * time.Hour).UTC().Format(engine.TimeLayout),
	}}
}

// TestTopSnapshotGolden: the piped one-shot snapshot, through a stubbed
// client and a non-terminal stdout, with the environment pinned so the
// caps are UTF-8, no colour, 80 columns on every host. The output holds
// no escape and equals the golden file; -update rewrites the files.
func TestTopSnapshotGolden(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("COLUMNS", "80")
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "")
	const repo = "https://example.invalid/r.git"
	for _, tc := range []struct {
		name  string
		model topModel
		want  []string
	}{
		{"busy", goldenBusyModel(), []string{"q quit  p pause", "Plan 1 is half done; task 2 waits for your answer. ─╮", "╭─ NEEDS YOU 2 "}},
		{"quiet", quietTopModel(), []string{"no events", "✓ nothing needs you", "?/? slots"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeTop{
				status:   tc.model.status,
				capacity: tc.model.capacity,
				projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: repo}},
				page:     history.Page{Items: tc.model.events},
			}
			var out, errOut bytes.Buffer
			if code := printTopSnapshot(context.Background(), fake, repo, "", topNow, &out, &errOut); code != 0 {
				t.Fatalf("exit %d: %s", code, errOut.String())
			}
			got := out.String()
			if strings.Contains(got, "\x1b") {
				t.Errorf("snapshot holds an escape:\n%q", got)
			}
			for i, line := range strings.Split(got, "\n") {
				if n := utf8.RuneCountInString(line); n > 80 {
					t.Errorf("line %d is %d columns, want at most 80: %q", i+1, n, line)
				}
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("snapshot lacks %q:\n%s", w, got)
				}
			}
			path := filepath.Join(topGoldenDir, "top-snapshot-"+tc.name+".golden")
			if *updateTopGolden {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test ./cmd/brigada -run TestTopSnapshotGolden -update)", err)
			}
			if got != string(want) {
				gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
				for i := 0; i < max(len(gl), len(wl)); i++ {
					var g, w string
					if i < len(gl) {
						g = gl[i]
					}
					if i < len(wl) {
						w = wl[i]
					}
					if g != w {
						t.Errorf("%s line %d:\n got  %q\n want %q", path, i+1, g, w)
					}
				}
			}
		})
	}
}

func TestTopTakesNoArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runTop(context.Background(), []string{"extra"}, &out, &errOut); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if code := runTop(context.Background(), []string{"--interval", "0s"}, &out, &errOut); code != 2 {
		t.Errorf("exit %d, want 2 for a zero interval", code)
	}
}

func TestTopScreenDrawRedrawsInPlace(t *testing.T) {
	var out bytes.Buffer
	screen := newTopScreen(&out)
	out.Reset()
	screen.draw([]string{"one", "two", "three"})
	got := out.String()
	if n := strings.Count(got, "\r\n"); n != 2 {
		t.Errorf("frame has %d line breaks, want 2:\n%q", n, got)
	}
	if !strings.HasPrefix(got, "\x1b[H") {
		t.Errorf("frame does not start with home: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b[J") {
		t.Errorf("frame does not end with erase below: %q", got)
	}
	if strings.Contains(got, "\x1b[2J") {
		t.Errorf("frame clears by scrolling: %q", got)
	}
}

// TestRenderTopScreenHeights: a screen shorter than the content returns
// exactly the height — the header first, no line cut, the lines that
// still fit — and a screen taller than the content changes nothing.
func TestRenderTopScreenHeights(t *testing.T) {
	lines := renderTop(busyTopModel(), 60, 5, topNow, term.New(logo.Caps{UTF8: true}))
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want exactly 5:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "▙ brigada   r · up") {
		t.Errorf("first line is %q, want the header", lines[0])
	}
	got := strings.Join(lines, "\n")
	for _, w := range []string{
		"3/8 slots  1 run  2 queue  1 held  1 needs",
		"╭─ NEEDS YOU 1 ",
		"! #2 should the sprocket be blue?",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("frame lacks %q:\n%s", w, got)
		}
	}
	for _, w := range []string{"PLANS", "#1 fix the frobnicator", "all ≤ global 8", "#3 done"} {
		if strings.Contains(got, w) {
			t.Errorf("frame keeps %q past the height:\n%s", w, got)
		}
	}
	if strings.Contains(got, "...") {
		t.Errorf("a floor line is cut:\n%s", got)
	}
	for _, l := range lines {
		if n := term.VisibleWidth(l); n > 60 {
			t.Errorf("line %q is %d wide", l, n)
		}
	}
	tall := renderTop(busyTopModel(), 60, 40, topNow, term.New(logo.Caps{UTF8: true}))
	unbounded := renderTop(busyTopModel(), 60, 0, topNow, term.New(logo.Caps{UTF8: true}))
	if strings.Join(tall, "\n") != strings.Join(unbounded, "\n") {
		t.Errorf("tall screen trims:\n%s\nwant\n%s", strings.Join(tall, "\n"), strings.Join(unbounded, "\n"))
	}
}

// shortTopModel is busyTopModel with a header left short enough — 22
// columns — that the keys fit from width 50 up.
func shortTopModel() topModel {
	model := busyTopModel()
	model.status.StartedAt = topNow.UTC().Format(engine.TimeLayout)
	return model
}

// TestTopHeaderKeysFit: the keys show only when the left part, two
// spaces, the clock and the keys fit the width whole — a key is never cut
// mid-word — and otherwise the clock sits against the right edge alone.
// busyTopModel's header left is 24 columns, so its keys fit only from
// width 52 up.
func TestTopHeaderKeysFit(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	tests := []struct {
		name  string
		model topModel
		width int
		keys  bool
	}{
		{"busy at 40 drops the keys", busyTopModel(), 40, false},
		{"busy at 51 drops the keys", busyTopModel(), 51, false},
		{"busy at 52 shows the keys", busyTopModel(), 52, true},
		{"busy at 60 shows the keys", busyTopModel(), 60, true},
		{"short left at 49 drops the keys", shortTopModel(), 49, false},
		{"short left at 50 shows the keys", shortTopModel(), 50, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := topHeader(tt.model, topNow, style, tt.width)
			if n := term.VisibleWidth(line); n > tt.width || (tt.width >= 52 || tt.keys) && n != tt.width {
				t.Errorf("header is %d wide, want %d: %q", n, tt.width, line)
			}
			if tt.keys && !strings.HasSuffix(line, "15:04:09   q quit  p pause") {
				t.Errorf("header lacks the keys: %q", line)
			}
			if !tt.keys && (strings.Contains(line, "quit") || !strings.HasSuffix(line, "15:04:09")) {
				t.Errorf("header keeps the keys or loses the clock: %q", line)
			}
		})
	}
	want := " ▙ brigada   r · up 0s" + strings.Repeat(" ", 49-22-8) + "15:04:09"
	if got := topHeader(shortTopModel(), topNow, style, 49); got != want {
		t.Errorf("keys-dropped header = %q, want %q", got, want)
	}
}

// TestRenderTopNarrowSectionHeads: below 60 columns the section heads
// drop their rule fills and keep their titles, the lines beneath keep
// their indentation, and at 60 the fills return.
func TestRenderTopNarrowSectionHeads(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	for _, width := range []int{20, 40, 59, 60} {
		out := renderTop(busyTopModel(), width, 0, topNow, style)
		got := strings.Join(out, "\n")
		for _, head := range []string{"╭─ NEEDS YOU 1 ", "╭─ PLANS", "╭─ AGENTS", "╭─ EVENTS"} {
			if !strings.Contains(got, head) {
				t.Errorf("width %d: view lacks the box head %q:\n%s", width, head, got)
			}
		}
		for _, line := range out {
			if n := term.VisibleWidth(line); n > width {
				t.Errorf("width %d: line is %d wide: %q", width, n, line)
			}
		}
	}
	// At 59 columns the last event goes and the kind, agent and age stay whole.
	narrow := strings.Join(renderTop(busyTopModel(), 59, 0, topNow, style), "\n")
	if !strings.Contains(narrow, "│   ▸  #3  oil the gears  work  claude/sonnet@high  1m40s │") {
		t.Errorf("narrow view loses the indentation inside a box:\n%s", narrow)
	}
}

// agentsModel is busyTopModel with three agents side by side: a short
// ready name, a cooling one and a long broken one.
func agentsModel() topModel {
	model := busyTopModel()
	four := 4
	cool := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	model.capacity.Agents = []engine.CapacityAgent{
		{Harness: "claude", Model: "opus", Cap: &four, CapLayer: "runner", Running: 1, Quota: "ready"},
		{Harness: "codex", Model: "gpt-5", CapLayer: "none", Quota: "cooling", CoolingUntil: cool},
		{Harness: "claude", Model: "sonnet-long-name", CapLayer: "none", Running: 2, Quota: "broken"},
	}
	model.status.NeedsYou = append(model.status.NeedsYou,
		engine.NeedsYouItem{Kind: "agent", ID: 7, State: "broken", Title: "claude/sonnet-long-name", Reason: "login expired"})
	return model
}

// TestTopAgentsOneLinePerAgent: every state is one line; cells align;
// the cap agrees with the folded cell; the quota cells read per state.
func TestTopAgentsOneLinePerAgent(t *testing.T) {
	model := agentsModel()
	plain := term.New(logo.Caps{UTF8: true})
	got := topAgents(model, topNow, plain, 100)
	if len(got) != 2 {
		t.Fatalf("section = %d rows, want 3 agents in two columns:\n%s", len(got), strings.Join(got, "\n"))
	}
	cool := topNow.Add(time.Hour).In(topNow.Location()).Format("15:04")
	// Broken and cooling agents first, then the running one, row by row.
	wants := []string{
		"↺ codex/gpt-5              □□□□□□□□  until " + cool,
		"● claude/opus              ■□□□",
	}
	if !strings.HasPrefix(got[0], wants[0]) || !strings.HasSuffix(got[0], "✗ claude/sonnet-long-name  ■■□□□□□□  login exp...") || got[1] != wants[1] {
		t.Errorf("rows = %q, want %q", got, wants)
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	styled := topAgents(model, topNow, colour, 100)
	if s := term.StripEscapes(strings.Join(styled, "\n")); s != strings.Join(got, "\n") {
		t.Errorf("styled text differs from plain:\n%s\nvs\n%s", s, strings.Join(got, "\n"))
	}
	if !strings.Contains(styled[1], colour.OK("●")) || !strings.Contains(styled[0], colour.Warn("↺")) ||
		!strings.Contains(styled[0], colour.Warn("until "+cool)) || !strings.Contains(styled[0], colour.Bad("✗")) ||
		!strings.Contains(styled[0], colour.Bad("login exp...")) {
		t.Errorf("quota styles wrong: %q", styled)
	}
}

// TestTopAgentsBrokenFallbackAndCut: a broken agent with no needs-you
// entry reads "broken"; a name longer than the width is cut with "...".
func TestTopAgentsBrokenFallbackAndCut(t *testing.T) {
	model := agentsModel()
	model.status.NeedsYou = model.status.NeedsYou[:1]
	plain := term.New(logo.Caps{UTF8: true})
	got := topAgents(model, topNow, plain, 100)
	if !strings.HasSuffix(got[0], "✗ claude/sonnet-long-name  ■■□□□□□□  broken") {
		t.Errorf("fallback row = %q", got[0])
	}
	cut := topAgents(model, topNow, plain, 12)
	if len(cut) != 3 || !strings.HasPrefix(cut[1], "✗ claude/") || !strings.HasSuffix(cut[1], "...") || utf8.RuneCountInString(cut[1]) > 12 {
		t.Errorf("long cell not cut: %q", cut)
	}
}

// TestTopAgentsNoGlobalNote: no global cap, no `all ≤` in the border.
func TestTopAgentsNoGlobalNote(t *testing.T) {
	model := agentsModel()
	model.capacity.Layers = nil
	style := term.New(logo.Caps{UTF8: true})
	if note := topHeads(model, style)[topSecAgents].note; strings.Contains(note, "all ≤") {
		t.Errorf("note = %q", note)
	}
	if got := topAgents(model, topNow, style, 100); len(got) != 2 || !strings.Contains(got[0], "0/none") && !strings.Contains(got[0], "□") {
		t.Errorf("rows = %q", got)
	}
}

// TestTopAgentsBudget: class e of the drop order — a row of idle agents
// leaves the AGENTS section first, with nothing counting it; a row with a
// busy, cooling or broken agent stays.
func TestTopAgentsBudget(t *testing.T) {
	model := agentsModel()
	model.capacity.Agents = append(model.capacity.Agents,
		engine.CapacityAgent{Harness: "codex", Model: "a", CapLayer: "none", Quota: "ready"},
		engine.CapacityAgent{Harness: "codex", Model: "b", CapLayer: "none", Quota: "ready"})
	model.events = nil
	model.status.Running, model.status.Held, model.status.Plans = nil, nil, nil
	style := term.New(logo.Caps{UTF8: true})
	full := renderTop(model, 76, 0, topNow, style)
	// One line less, the last idle agent is the next to go.
	base := len(full)
	agentLines := func(lines []string) string {
		boxed := topBoxed(lines)
		from := -1
		for i, l := range boxed {
			if l == "╭ AGENTS" {
				from = i
			}
		}
		if from < 0 {
			return ""
		}
		rest := boxed[from+1:]
		for i, l := range rest {
			if l == "╰" {
				return strings.Join(rest[:i], "\n")
			}
		}
		return strings.Join(rest, "\n")
	}
	lines := renderTop(model, 76, base, topNow, style)
	if len(lines) != base || !strings.Contains(agentLines(lines), "codex/b") {
		t.Errorf("at %d the idle agents are not all kept:\n%s", base, strings.Join(lines, "\n"))
	}
	// The row of the one idle agent left alone goes first, with nothing
	// counting it; the row it shares with the running agent stays.
	lines = renderTop(model, 76, base-1, topNow, style)
	if got := agentLines(lines); len(lines) != base-1 || strings.Contains(got, "codex/b") || !strings.Contains(got, "codex/a") ||
		strings.Contains(strings.Join(lines, "\n"), "idle") {
		t.Errorf("at %d the AGENTS section is\n%s\nin\n%s", base-1, got, strings.Join(lines, "\n"))
	}
	// The cooling, broken and running agents outlast the idle ones: the
	// events row, the lowest of the rest, goes next with its box.
	lines = renderTop(model, 76, base-2, topNow, style)
	if len(lines) > base-2 || len(lines) < base-4 || !strings.Contains(agentLines(lines), "✗ claude/sonnet-long-name") ||
		!strings.Contains(agentLines(lines), "● claude/opus") || strings.Contains(agentLines(lines), "codex/b") {
		t.Errorf("at %d:\n%s", base-2, strings.Join(lines, "\n"))
	}
}

// TestRenderTopFinishedFold: class b — finished plan rows drop, bottom of
// the view first, one row each; the PLANS note keeps counting them.
func TestRenderTopFinishedFold(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	model := busyTopModel()
	model.statusOK = true
	model.events = model.events[1:] // one event row, so class a has nothing to drop first
	for id := int64(7); id <= 8; id++ {
		model.status.Plans = append(model.status.Plans, engine.StatusPlan{
			ID: id, Title: fmt.Sprintf("old plan %d", id), State: "done",
			Tasks: []engine.StatusTask{{ID: id * 10, Title: "t", State: "done"}}})
	}
	full := renderTop(model, 100, 0, topNow, style)
	n := len(full)
	if lines := renderTop(model, 100, n, topNow, style); len(lines) != n {
		t.Fatalf("at %d lines yield:\n%s", n, strings.Join(lines, "\n"))
	}
	// One line less: the last finished plan drops, with no fold row, and
	// the PLANS note still counts it.
	got := strings.Join(renderTop(model, 100, n-1, topNow, style), "\n")
	if !strings.Contains(got, "#7 old plan 7") || strings.Contains(got, "old plan 8") || strings.Contains(got, "+1 finished") ||
		len(strings.Split(got, "\n")) != n-1 || !strings.Contains(got, "╭─ PLANS  1 live · 2 finished ─") {
		t.Errorf("at %d:\n%s", n-1, got)
	}
	got = strings.Join(renderTop(model, 100, n-2, topNow, style), "\n")
	if strings.Contains(got, "old plan") || strings.Contains(got, "finished\n") || len(strings.Split(got, "\n")) != n-2 ||
		!strings.Contains(got, "1 live · 2 finished") {
		t.Errorf("at %d:\n%s", n-2, got)
	}
	// Hidden finished plans count in the note too.
	model.status.HiddenFinishedPlans = 3
	if got := plainTop(model, 100, 0); !strings.Contains(got, "╭─ PLANS  1 live · 5 finished ─") {
		t.Errorf("the note leaves out the hidden plans:\n%s", got)
	}
}

// TestRenderTopQueuedYield: class d — the queued, waiting-on and review
// task lines go from the last plan upward after the blank lines and
// before the idle agents, then held and parked lines.
func TestRenderTopQueuedYield(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	model := busyTopModel()
	model.events = model.events[1:] // one event row, so class a has nothing to drop first
	model.status.Plans = append(model.status.Plans, engine.StatusPlan{
		ID: 2, Title: "second", State: "active",
		Tasks: []engine.StatusTask{
			{ID: 5, Title: "five", State: "ready"},
			{ID: 6, Title: "six", State: "ready", WaitingOn: []string{"#5"}},
			{ID: 7, Title: "seven", State: "reviewing"},
		}})
	model.capacity.Agents = append(model.capacity.Agents,
		engine.CapacityAgent{Harness: "codex", Model: "a", CapLayer: "none", Quota: "ready"})
	full := strings.Join(renderTop(model, 100, 0, topNow, style), "\n")
	for _, w := range []string{"·  #5  five", "(queued)", "(waiting on #5)", "(review)", "codex/a"} {
		if !strings.Contains(full, w) {
			t.Fatalf("the full view lacks %q:\n%s", w, full)
		}
	}
	base := len(strings.Split(full, "\n"))
	tests := []struct {
		height int
		lacks  []string
		keeps  []string
	}{
		{base, nil, []string{"(review)", "(waiting on #5)", "(queued)", "codex/a", "?  #4  polish the knobs"}},
		{base - 1, []string{"(review)"}, []string{"(waiting on #5)", "(queued)", "codex/a"}},
		{base - 2, []string{"(review)", "(waiting on #5)"}, []string{"·  #5  five", "codex/a"}},
		{base - 3, []string{"(review)", "(waiting on #5)", "(queued)"}, []string{"codex/a", "?  #4  polish the knobs"}},
		// The idle agent shares a row with the running one, so it stays
		// until the whole row goes; the held row goes with the next step.
		{base - 4, []string{"(queued)", "?  #4  polish the knobs"}, []string{"codex/a", "▸  #3  oil the gears"}},
	}
	for _, tt := range tests {
		lines := renderTop(model, 100, tt.height, topNow, style)
		got := strings.Join(lines, "\n")
		if len(lines) != tt.height {
			t.Errorf("at %d got %d lines:\n%s", tt.height, len(lines), got)
		}
		for _, w := range tt.lacks {
			if strings.Contains(got, w) {
				t.Errorf("at %d the frame keeps %q:\n%s", tt.height, w, got)
			}
		}
		for _, w := range tt.keeps {
			if !strings.Contains(got, w) {
				t.Errorf("at %d the frame lacks %q:\n%s", tt.height, w, got)
			}
		}
	}
}

// goldenBusyModel is agentsModel with the foreman's last report, so the
// busy golden shows the report line.
func goldenBusyModel() topModel {
	model := agentsModel()
	model.status.Report = &engine.StatusReport{ProjectID: 1, JobID: 41, CreatedAt: "2026-10-20T15:00:00.000000000Z",
		Text: "Plan 1 is half done; task 2 waits for your answer.\nThe rest follows."}
	return model
}

// TestTopNeedsYouTails: an entry that has an action keeps it whole at the
// end of its line, told apart by Kind, State and the Reason's prefix; the
// rest have none.
func TestTopNeedsYouTails(t *testing.T) {
	const repo = "https://example.invalid/acme/brigada.git"
	st := api.Status{Plans: []engine.StatusPlan{{ID: 5, RepoURL: repo}}}
	hold := "held by rule approval_tiers: heavy; release: brigada land approve 312; change the rule: brigada land approve --and-stop-asking 312"
	tests := []struct {
		name string
		item engine.NeedsYouItem
		sole bool
		want string
	}{
		{"plan pending approval", engine.NeedsYouItem{Kind: "plan", ID: 9, State: "pending_approval", Title: "Top polish"},
			false, "! #9 Top polish awaiting approval  → brigada plan approve 9"},
		{"plan with the foreman", engine.NeedsYouItem{Kind: "plan", ID: 9, State: "needs_foreman", Title: "Top polish"},
			false, "! #9 Top polish needs foreman"},
		{"land hold, the command shows once", engine.NeedsYouItem{Kind: "task", ID: 312, State: "done", Reason: hold},
			false, "! #312 held by rule approval_tiers: heavy  → brigada land approve 312"},
		{"held by rule on a task not done", engine.NeedsYouItem{Kind: "task", ID: 4, State: "needs_foreman", Reason: "held by rule x"},
			false, "! #4 held by rule x"},
		{"push wait, one project", engine.NeedsYouItem{Kind: "task", ID: 7, State: "done", PlanID: 5, Reason: "waits to push: task/7-1@abc123"},
			true, "! #7 waits to push: task/7-1@abc123  → brigada push approve"},
		{"push wait, named repo", engine.NeedsYouItem{Kind: "task", ID: 7, State: "done", PlanID: 5, Reason: "waits to push: task/7-1@abc123"},
			false, "! #7 waits to push: task/7-1@abc123  → brigada push approve --repo " + repo},
		{"push wait, plan not listed", engine.NeedsYouItem{Kind: "task", ID: 7, State: "done", PlanID: 6, Reason: "waits to push: task/7-1@abc123"},
			false, "! #7 waits to push: task/7-1@abc123"},
		{"push by hand", engine.NeedsYouItem{Kind: "task", ID: 8, State: "done", PlanID: 5, Reason: "push by hand: git push origin main"},
			true, "! #8 push by hand: git push origin main"},
		{"stranded task", engine.NeedsYouItem{Kind: "task", ID: 10, State: "needs_foreman", Reason: "stranded: no runner"},
			true, "! #10 stranded: no runner"},
		{"environment cause", engine.NeedsYouItem{Kind: "task", ID: 11, State: "needs_user", Reason: "environment: git is missing"},
			true, "! #11 environment: git is missing"},
		{"broken agent", engine.NeedsYouItem{Kind: "agent", ID: 4, State: "broken", Title: "codex/gpt-5", Reason: "auth"},
			false, "! codex/gpt-5 auth  → brigada agent reset codex/gpt-5"},
		{"agent not broken", engine.NeedsYouItem{Kind: "agent", ID: 4, State: "cooling", Title: "codex/gpt-5", Reason: "slow"},
			false, "! codex/gpt-5 slow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := st
			s.NeedsYou = []engine.NeedsYouItem{tt.item}
			lines := topNeedsYou(s, tt.sole, term.New(logo.Caps{UTF8: true}), 116)
			if len(lines) != 1 || topCollapse(lines[0]) != topCollapse(tt.want) {
				t.Errorf("lines = %q, want the entry %q", lines, tt.want)
			}
		})
	}
}

// TestTopNeedsYouTailsAlign: the tails start in one column, placed so the
// widest tail ends at the inner right edge; a narrow width cuts the reason
// and keeps the tail whole.
func TestTopNeedsYouTailsAlign(t *testing.T) {
	hold := engine.NeedsYouItem{Kind: "task", ID: 312, State: "done",
		Reason: "held by rule approval_tiers: heavy; release: brigada land approve 312; change the rule: brigada land approve --and-stop-asking 312"}
	plan := engine.NeedsYouItem{Kind: "plan", ID: 9, State: "pending_approval", Title: "Top polish"}
	plain := term.New(logo.Caps{UTF8: true})
	st := api.Status{NeedsYou: []engine.NeedsYouItem{hold, plan}}
	got := topNeedsYou(st, false, plain, 96)
	tails := []string{"→ brigada land approve 312", "→ brigada plan approve 9"}
	lefts := []string{"! #312 held by rule approval_tiers: heavy", "! #9 Top polish awaiting approval"}
	for i := range got {
		want := lefts[i] + strings.Repeat(" ", 96-26-2-utf8.RuneCountInString(lefts[i])) + "  " + tails[i]
		if got[i] != want {
			t.Errorf("line %d = %q, want %q", i, got[i], want)
		}
	}
	if !strings.HasSuffix(got[0], tails[0]) {
		t.Errorf("the widest tail does not end at the edge: %q", got[0])
	}
	if a, b := strings.Index(got[0], "→"), strings.Index(got[1], "→"); a != b {
		t.Errorf("tails start at byte %d and %d, want one column", a, b)
	}

	narrow := topNeedsYou(api.Status{NeedsYou: []engine.NeedsYouItem{hold}}, false, plain, 60)[0]
	if want := "! #312 held by rule approval_...  → brigada land approve 312"; narrow != want {
		t.Errorf("narrow line = %q, want %q", narrow, want)
	}
	if n := utf8.RuneCountInString(narrow); n != 60 {
		t.Errorf("narrow line is %d cells, want 60", n)
	}
	if strings.Count(narrow, "brigada land approve") != 1 {
		t.Errorf("the command shows more than once: %q", narrow)
	}
}

// TestTopReportInEventsBorder: the first line of the foreman's last report
// sits Dim at the right of the EVENTS top border, with no prefix, only
// when there is one, cut to fit; it is not a row and costs none.
func TestTopReportInEventsBorder(t *testing.T) {
	model := agentsModel()
	model.events = []history.Item{
		{Type: history.TypeEvent, ID: 11, Kind: "review_verdict", ProjectID: 1, JobID: 5,
			Payload:   []byte(`{"status":"approve","branch":"task/3-1-oil-the-gears-after-the-frobnicator-fix"}`),
			CreatedAt: "2026-10-20T15:02:41.000000000Z"},
		model.events[1],
	}
	style := term.New(logo.Caps{UTF8: true})
	if got := strings.Join(renderTop(model, 76, 0, topNow, style), "\n"); strings.Contains(got, "report") || strings.Contains(got, "All landed") {
		t.Errorf("a status with no report shows one:\n%s", got)
	}
	model.status.Report = &engine.StatusReport{ProjectID: 1, JobID: 41, Text: "All landed, but #5 needs a look.\nSecond line."}
	full := renderTop(model, 76, 0, topNow, style)
	tail := full[len(full)-4:]
	row := func(s string) string { return "│ " + s + strings.Repeat(" ", 72-utf8.RuneCountInString(s)) + " │" }
	want := []string{
		"╭─ EVENTS " + strings.Repeat("─", 30) + " All landed, but #5 needs a look. ─╮",
		row("15:02  #3 done"),
		row("15:02  review approve task/3-1-oil-the-gears-after-the-frobnicator-fix"),
		"╰" + strings.Repeat("─", 74) + "╯",
	}
	if strings.Join(tail, "\n") != strings.Join(want, "\n") {
		t.Errorf("tail =\n%s\nwant\n%s", strings.Join(tail, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(strings.Join(full, "\n"), "report #41") || strings.Contains(strings.Join(full, "\n"), "Second line") {
		t.Errorf("the report shows more than its first line, or with a prefix:\n%s", strings.Join(full, "\n"))
	}
	// Dim, and cut to fit with the title and the corners kept.
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	head := topHeads(model, colour)[topSecEvents]
	if got := topBoxTop(head, colour, 40); !strings.Contains(got, colour.Dim("All landed, but #5 nee...")) || term.VisibleWidth(got) != 40 {
		t.Errorf("cut border = %q, want a Dim note cut to fit in 40 columns", got)
	}
	if got := term.StripEscapes(topBoxTop(head, colour, 20)); got != "╭─ EVENTS ─────────╮" {
		t.Errorf("narrow border = %q, want the note left out", got)
	}
	// Event row 2 is the only row of class a.
	one := renderTop(model, 76, len(full)-1, topNow, style)
	if got := strings.Join(one, "\n"); strings.Contains(got, "review approve") || !strings.Contains(got, "All landed") {
		t.Errorf("one line less should drop only event row 2:\n%s", got)
	}
}

// TestTopForemanAndJobsNotes: the user's messages and the slot holders
// show in the FOREMAN block, a pending runner restart in the JOBS block,
// each in a dim line of the held class; a headless turn whose job runs is
// not repeated, and a block shows when it has any line.
func TestTopForemanAndJobsNotes(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	stamp := time.Date(2026, 10, 20, 11, 10, 0, 0, time.UTC).Format(engine.TimeLayout)
	texts := func(lines []topLine) []string {
		var out []string
		for _, l := range lines {
			out = append(out, l.text)
		}
		return out
	}
	foreman, jobs := topPlanless(api.Status{
		ForemanMessages: 2,
		Running:         []engine.StatusJob{{ID: 30, Kind: "foreman"}},
		Slots: []engine.Slot{
			{ProjectID: 1, State: engine.SlotFree},
			{ProjectID: 1, State: engine.SlotAttach, HolderID: 3, Label: "attached chat", Since: stamp},
			{ProjectID: 2, State: engine.SlotTurn, HolderID: 30, Label: "foreman turn", Since: stamp},
			{ProjectID: 3, State: engine.SlotTurn, HolderID: 31, Label: "foreman turn", Since: stamp},
		},
	}, topNow, plain, 80)
	want := []string{
		"  ▸  job #30  foreman",
		"  2 messages for the foreman",
		"  foreman slot: attach (attached chat) since 11:10",
		"  foreman slot: headless turn (foreman turn) since 11:10",
	}
	if got := texts(foreman); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("FOREMAN block =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(jobs) != 0 {
		t.Errorf("JOBS block = %v, want none", texts(jobs))
	}
	for _, l := range foreman[1:] {
		if l.class != yieldHeld || l.section != topSecForeman {
			t.Errorf("note %q is class %d section %d, want held / FOREMAN", l.text, l.class, l.section)
		}
	}
	// The headless turn whose job runs is skipped; one message reads singular,
	// and a block shows with only a note.
	foreman, _ = topPlanless(api.Status{ForemanMessages: 1,
		Slots: []engine.Slot{{State: engine.SlotTurn, HolderID: 30, Label: "foreman turn", Since: stamp}}}, topNow, plain, 80)
	if got := texts(foreman); len(got) != 2 || got[0] != "  1 message for the foreman" {
		t.Errorf("FOREMAN block = %q", got)
	}
	foreman, _ = topPlanless(api.Status{Running: []engine.StatusJob{{ID: 30, Kind: "foreman"}},
		Slots: []engine.Slot{{State: engine.SlotTurn, HolderID: 30, Label: "foreman turn", Since: stamp}}}, topNow, plain, 80)
	if got := texts(foreman); len(got) != 1 {
		t.Errorf("the running turn's slot repeats its job line: %q", got)
	}
	// A pending runner restart sits in the JOBS block, only while something runs.
	_, jobs = topPlanless(api.Status{RunnerDraining: true,
		Running: []engine.StatusJob{{ID: 63, Kind: "plan_review"}}}, topNow, plain, 80)
	if got := texts(jobs); len(got) != 2 || got[1] != "  runner restarting after 1 running job" || jobs[1].class != yieldHeld || jobs[1].section != topSecJobs {
		t.Errorf("JOBS block = %q", got)
	}
	_, jobs = topPlanless(api.Status{RunnerDraining: true,
		Running: []engine.StatusJob{{ID: 63, Kind: "plan_review"}, {ID: 64, Kind: "plan_review"}}}, topNow, plain, 80)
	if got := texts(jobs); got[len(got)-1] != "  runner restarting after 2 running jobs" {
		t.Errorf("JOBS block = %q", got)
	}
	foreman, jobs = topPlanless(api.Status{RunnerDraining: true}, topNow, plain, 80)
	if len(foreman)+len(jobs) != 0 {
		t.Errorf("a drain with nothing running shows blocks: %v %v", texts(foreman), texts(jobs))
	}
	// Dim, and no escape in the plain style.
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	foreman, _ = topPlanless(api.Status{ForemanMessages: 1}, topNow, colour, 80)
	if foreman[0].text != "  "+colour.Dim("1 message for the foreman") {
		t.Errorf("note = %q, want Dim", foreman[0].text)
	}
}

// TestTopMetersZeroCountersPlain: held and needs colour only above zero.
func TestTopMetersZeroCountersPlain(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	model := busyTopModel()
	model.status.Held, model.status.NeedsYou = nil, nil
	got := topMeters(model, style, 100)
	want := style.Bold("1 run") + "  2 queue  0 held  0 needs"
	if !strings.HasSuffix(got, want) {
		t.Errorf("meters = %q, want it to end with %q", got, want)
	}
	if plain := topMeters(model, term.New(logo.Caps{UTF8: true}), 100); !strings.HasSuffix(plain, "1 run  2 queue  0 held  0 needs") {
		t.Errorf("plain meters = %q", plain)
	}
}

// TestTopAgentsOrderAndOff: off agents — an effective cap of 0, nothing
// running, a ready quota — never list; the rest go broken and cooling
// first, then running, then idle, in snapshot order within a group.
func TestTopAgentsOrderAndOff(t *testing.T) {
	zero := 0
	cool := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	model := busyTopModel()
	model.capacity.Agents = []engine.CapacityAgent{
		{Harness: "x", Model: "idle1", CapLayer: "none", Quota: "ready"},
		{Harness: "x", Model: "off", Cap: &zero, CapLayer: "model", Quota: "ready"},
		{Harness: "x", Model: "run", CapLayer: "none", Running: 1, Quota: "ready"},
		{Harness: "x", Model: "cool", CapLayer: "none", Quota: "cooling", CoolingUntil: cool},
		{Harness: "x", Model: "broke", CapLayer: "none", Quota: "broken"},
		{Harness: "x", Model: "idle2", CapLayer: "none", Quota: "ready"},
		{Harness: "x", Model: "offbusy", Cap: &zero, CapLayer: "model", Running: 1, Quota: "ready"},
	}
	got := topAgents(model, topNow, term.New(logo.Caps{UTF8: true}), 100)
	var names []string
	for _, l := range got {
		for _, f := range strings.Fields(l) {
			if strings.HasPrefix(f, "x/") {
				names = append(names, f)
			}
		}
	}
	if want := "x/cool x/broke x/run x/offbusy x/idle1 x/idle2"; strings.Join(names, " ") != want {
		t.Errorf("order = %q, want %q", strings.Join(names, " "), want)
	}
	if note := topAgentsNote(model.capacity, term.New(logo.Caps{UTF8: true})); note != "all ≤ global 8 · 1 off" {
		t.Errorf("note = %q, want the off count", note)
	}
	// The global cap makes every ready, idle agent off.
	model.capacity.Layers = []engine.CapacityLayer{{Layer: "global", Cap: &zero}}
	got = topAgents(model, topNow, term.New(logo.Caps{UTF8: true}), 100)
	if joined := strings.Join(got, "\n"); strings.Contains(joined, "x/idle1") || !strings.Contains(joined, "x/run") {
		t.Errorf("a global cap of 0 should fold the idle agents out:\n%s", joined)
	}
}

// TestTopAgentsNote: the AGENTS border note is `all ≤ global N` when the
// global cap sits below some agent's cap or an agent has none, ` · M off`
// when agents are off, either part alone, `all ready` when nothing is
// broken, cooling, off or capped, and nothing otherwise; it is Dim and
// cut or left out as the frame's notes are.
func TestTopAgentsNote(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	three, two, zero := 3, 2, 0
	ready := engine.CapacityAgent{Harness: "a", Model: "ready", Quota: "ready"}
	capped := engine.CapacityAgent{Harness: "a", Model: "capped", Cap: &two, Quota: "ready"}
	off := engine.CapacityAgent{Harness: "a", Model: "off", Cap: &zero, Quota: "ready"}
	cooling := engine.CapacityAgent{Harness: "a", Model: "cool", Quota: "cooling"}
	global := []engine.CapacityLayer{{Layer: "global", Cap: &three}}
	for _, tt := range []struct {
		name   string
		agents []engine.CapacityAgent
		layers []engine.CapacityLayer
		want   string
	}{
		{"global binds an uncapped agent", []engine.CapacityAgent{ready, off}, global, "all ≤ global 3 · 1 off"},
		{"global and off", []engine.CapacityAgent{ready, off, off}, global, "all ≤ global 3 · 2 off"},
		{"global alone", []engine.CapacityAgent{ready}, global, "all ≤ global 3"},
		{"off alone", []engine.CapacityAgent{capped, off}, nil, "1 off"},
		{"global above every own cap", []engine.CapacityAgent{capped}, []engine.CapacityLayer{{Layer: "global", Cap: &three}}, ""},
		{"all ready", []engine.CapacityAgent{ready, ready}, nil, "all ready"},
		{"all ready with an uncapped layer", []engine.CapacityAgent{ready}, []engine.CapacityLayer{{Layer: "global"}}, "all ready"},
		{"cooling", []engine.CapacityAgent{ready, cooling}, nil, ""},
		{"a layer caps something", []engine.CapacityAgent{capped}, []engine.CapacityLayer{{Layer: "runner", Cap: &two}}, ""},
		{"no agents", nil, nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := topAgentsNote(engine.CapacitySnapshot{Agents: tt.agents, Layers: tt.layers}, plain); got != tt.want {
				t.Errorf("note = %q, want %q", got, tt.want)
			}
		})
	}
	// ASCII: no Unicode of its own.
	ascii := term.New(logo.Caps{})
	if got := topAgentsNote(engine.CapacitySnapshot{Agents: []engine.CapacityAgent{ready, off}, Layers: global}, ascii); got != "all <= global 3 | 1 off" {
		t.Errorf("ASCII note = %q", got)
	}
	// Dim in the border, and left out when the box is too narrow.
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	model := topModel{capacity: engine.CapacitySnapshot{Agents: []engine.CapacityAgent{ready}}}
	head := topHeads(model, colour)[topSecAgents]
	if border := topBoxTop(head, colour, 40); !strings.Contains(border, colour.Dim("all ready")) {
		t.Errorf("border = %q, want the note Dim", border)
	}
	if border := term.StripEscapes(topBoxTop(head, colour, 14)); strings.Contains(border, "all") || term.VisibleWidth(border) != 14 {
		t.Errorf("narrow border = %q, want the note left out", border)
	}
}

// TestTopAgentCells: one cell per slot of the effective cap, filled up to
// the running count; a cap over 8, no cap or a cap of 0 keep the text.
func TestTopAgentCells(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	three, nine, zero, two := 3, 9, 0, 2
	for _, tt := range []struct {
		name   string
		a      engine.CapacityAgent
		global *int
		want   string
		w      int
	}{
		{"idle", engine.CapacityAgent{Cap: &three, CapLayer: "runner"}, nil, style.Dim("□□□"), 3},
		{"some", engine.CapacityAgent{Cap: &three, CapLayer: "runner", Running: 1}, nil, style.Accent("■") + style.Dim("□□"), 3},
		{"full", engine.CapacityAgent{Cap: &three, CapLayer: "runner", Running: 3}, nil, style.Accent("■■■"), 3},
		{"over full", engine.CapacityAgent{Cap: &three, CapLayer: "runner", Running: 5}, nil, style.Accent("■■■"), 3},
		{"global is smaller", engine.CapacityAgent{Cap: &three, CapLayer: "runner", Running: 1}, &two, style.Accent("■") + style.Dim("□"), 2},
		{"cap over 8", engine.CapacityAgent{Cap: &nine, CapLayer: "runner", Running: 1}, nil, "1/9 (runner)", 12},
		{"uncapped", engine.CapacityAgent{Running: 2}, nil, "2/none", 6},
		{"cap 0", engine.CapacityAgent{Cap: &zero, CapLayer: "runner", Running: 1}, nil, "1/0 (runner)", 12},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, w := topAgentCells(tt.a, tt.global, style)
			if got != tt.want || w != tt.w {
				t.Errorf("cells = %q (%d), want %q (%d)", got, w, tt.want, tt.w)
			}
		})
	}
}

// TestTopAgentsColumnsAndMarks: two columns of equal width, filled row by
// row, when the inner width is 72 or more; one column below it or when the
// cells do not fit half; each mark reads as spec'd, a reason is cut to its
// cell, and a row is idle only when both its agents are.
func TestTopAgentsColumnsAndMarks(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	four := 4
	cool := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	model := topModel{
		capacity: engine.CapacitySnapshot{Agents: []engine.CapacityAgent{
			{Harness: "claude", Model: "sonnet", Cap: &four, CapLayer: "runner", Running: 1, Quota: "ready"},
			{Harness: "claude", Model: "opus", Cap: &four, CapLayer: "runner", Quota: "ready"},
			{Harness: "opencode", Model: "zai-coding-plan/glm-5.3", Cap: &four, CapLayer: "runner", Quota: "cooling", CoolingUntil: cool},
			{Harness: "dsh", Model: "deepseek-flash", Cap: &four, CapLayer: "runner", Quota: "broken"},
			{Harness: "claude", Model: "haiku", Cap: &four, CapLayer: "runner", Quota: "ready"},
		}},
		status: api.Status{NeedsYou: []engine.NeedsYouItem{
			{Kind: "agent", State: "broken", Title: "dsh/deepseek-flash", Reason: "login expired"}}},
	}
	lines := topAgentLines(model, topNow, plain, 92)
	var rows []string
	for _, l := range lines {
		rows = append(rows, l.text)
	}
	cooled := topNow.Add(time.Hour).In(topNow.Location()).Format("15:04")
	// Broken and cooling first, then the running one, then the idle ones.
	two := func(a, b string) string { return a + strings.Repeat(" ", 47-utf8.RuneCountInString(a)) + b }
	want := []string{
		two("↺ glm-5.3             □□□□  until "+cooled, "✗ dsh/deepseek-flash  □□□□  login expired"),
		two("● claude/sonnet       ■□□□", "● claude/opus         □□□□"),
		"● claude/haiku        □□□□",
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %q", rows)
	}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("row %d = %q\nwant     %q", i, rows[i], w)
		}
	}
	if lines[0].class != yieldRest || lines[1].class != yieldRest || lines[2].class != yieldIdle {
		t.Errorf("classes = %d %d %d, want a row idle only when both agents are", lines[0].class, lines[1].class, lines[2].class)
	}
	// Both agents idle: the row is in the idle class.
	idle := topModel{capacity: engine.CapacitySnapshot{Agents: []engine.CapacityAgent{model.capacity.Agents[1], model.capacity.Agents[4]}}}
	if l := topAgentLines(idle, topNow, plain, 92); len(l) != 1 || l[0].class != yieldIdle {
		t.Errorf("idle pair = %+v", l)
	}
	// One column below 72 columns; the reason is cut to the cell.
	one := topAgentLines(model, topNow, plain, 60)
	if len(one) != 5 || !strings.HasPrefix(one[1].text, "✗ dsh/deepseek-flash  □□□□  login expired") || utf8.RuneCountInString(one[1].text) > 60 {
		t.Errorf("one column = %+v", one)
	}
	// Narrow cells that cannot hold half the width fall to one column.
	narrow := topAgentLines(model, topNow, plain, 72)
	if len(narrow) != 5 && len(narrow) != 3 {
		t.Errorf("72 columns = %+v", narrow)
	}
	cut := topAgentLines(model, topNow, plain, 92)[0].text
	if !strings.Contains(cut, "login expired") || strings.Contains(cut, "...") {
		t.Errorf("a reason that fits was cut: %q", cut)
	}
	model.status.NeedsYou[0].Reason = strings.Repeat("long reason ", 6)
	if cut := topAgentLines(model, topNow, plain, 92)[0].text; !strings.Contains(cut, "...") || utf8.RuneCountInString(cut) > 92 {
		t.Errorf("a long reason is not cut: %q", cut)
	}
	// No reason: broken.
	model.status.NeedsYou = nil
	if row := topAgentLines(model, topNow, plain, 92)[0].text; !strings.HasSuffix(row, "  broken") {
		t.Errorf("fallback row = %q", row)
	}
	// ASCII marks.
	ascii := topAgentLines(model, topNow, term.New(logo.Caps{}), 92)
	for _, l := range ascii {
		for _, r := range l.text {
			if r >= 0x80 {
				t.Errorf("%q in the ASCII row %q", r, l.text)
			}
		}
	}
	if !strings.HasPrefix(ascii[0].text, "-> glm-5.3") || !strings.Contains(ascii[0].text, "x  dsh/deepseek-flash") || !strings.Contains(ascii[1].text, "*  claude/sonnet") {
		t.Errorf("ASCII rows = %+v", ascii)
	}
}

// TestTopFamilyColours: a running agent's name takes its family colour on
// its AGENTS row and on its job line; an idle agent stays Dim; plain style
// and NO_COLOR carry no colour.
func TestTopFamilyColours(t *testing.T) {
	four := 4
	agents := []engine.CapacityAgent{
		{Harness: "claude", Model: "sonnet", Cap: &four, CapLayer: "runner", Running: 1, Quota: "ready"},
		{Harness: "opencode", Model: "zai-coding-plan/glm-5.3", Cap: &four, CapLayer: "runner", Running: 1, Quota: "ready"},
		{Harness: "dsh", Model: "deepseek", Cap: &four, CapLayer: "runner", Running: 1, Quota: "ready"},
		{Harness: "codex", Model: "gpt-5", Cap: &four, CapLayer: "runner", Running: 1, Quota: "ready"},
		{Harness: "claude", Model: "haiku", Cap: &four, CapLayer: "runner", Quota: "ready"},
	}
	model := topModel{capacity: engine.CapacitySnapshot{Agents: agents}}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	var text string
	for _, l := range topAgentLines(model, topNow, colour, 92) {
		text += l.text + "\n"
	}
	for _, w := range []string{colour.Violet("claude/sonnet"), colour.Blue("glm-5.3"), colour.Mint("dsh/deepseek"), colour.Dim("codex/gpt-5"), colour.Dim("claude/haiku")} {
		if !strings.Contains(text, w) {
			t.Errorf("AGENTS rows lack %q:\n%s", w, text)
		}
	}
	// The same colour on the job line's agent column, through one helper.
	at := topNow.Add(-time.Minute).UTC().Format(engine.TimeLayout)
	for harness, model := range map[string]string{"claude": "sonnet", "opencode": "zai-coding-plan/glm-5.3", "dsh": "deepseek", "codex": "gpt-5"} {
		j := engine.StatusJob{ID: 1, Kind: "work", Harness: harness, Model: model, LeasedAt: at}
		lines := []topLine{topJobLine(topRunningRow(j, "", topNow, colour), yieldRunning, topSecJobs)}
		topRenderJobs(lines, colour, 100)
		name := topAgentName(harness, model, "")
		if want := topFamily(harness, name, colour)(name); !strings.Contains(lines[0].text, want) {
			t.Errorf("%s job line = %q, want the agent as %q", harness, lines[0].text, want)
		}
	}
	for _, style := range []term.Style{term.New(logo.Caps{UTF8: true}), term.New(logo.Caps{Color: logo.NoColor, UTF8: true})} {
		for _, l := range topAgentLines(model, topNow, style, 92) {
			if strings.Contains(l.text, "\x1b") {
				t.Errorf("an uncoloured row has an escape: %q", l.text)
			}
		}
	}
	if topFamily("opencode", "glm-5.3", colour)("x") != colour.Blue("x") || topFamily("opencode", "kimi", colour)("x") != colour.Dim("x") {
		t.Errorf("family rule: glm blue, other Dim")
	}
}

// TestTopUptime: the header's uptime is whole seconds under a minute,
// whole minutes under an hour, else hours and minutes, each truncated.
func TestTopUptime(t *testing.T) {
	for _, tt := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{0, "0s"},
		{45 * time.Second, "45s"},
		{59*time.Second + 900*time.Millisecond, "59s"},
		{time.Minute, "1m"},
		{33*time.Minute + 9*time.Second, "33m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{time.Hour, "1h0m"},
		{2 * time.Hour, "2h0m"},
		{11*time.Hour + 33*time.Minute + 9*time.Second, "11h33m"},
		{49*time.Hour + 59*time.Second, "49h0m"},
	} {
		if got := topUptime(tt.d); got != tt.want {
			t.Errorf("topUptime(%s) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// TestTopFirstRunLine: a status that came back with no plans, listed or
// hidden, reads as a first run; one that did not come back keeps `none`.
func TestTopFirstRunLine(t *testing.T) {
	const first = `no plans yet: brigada ask "fix #12"`
	const repo = "https://example.invalid/r.git"
	projects := []engine.ProjectInfo{{ProjectID: 7, RepoURL: repo}}
	fetched, err := topFetch(context.Background(), &fakeTop{status: quietTopModel().status, projects: projects}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := plainTop(fetched, 80, 0); !strings.Contains(got, "│ "+first+" ") || strings.Contains(got, "│ none ") {
		t.Errorf("a fetched empty status lacks the first-run line:\n%s", got)
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	if lines := newTopPlans(fetched.status, false, true, topNow, colour, 80); lines[0].text != colour.Dim(first) {
		t.Errorf("first-run line = %q, want Dim", lines[0].text)
	}
	// Plans hidden behind the finished ones are not a first run.
	hidden := fetched
	hidden.status.HiddenFinishedPlans = 2
	if got := plainTop(hidden, 80, 0); !strings.Contains(got, "│ none ") || strings.Contains(got, "no plans yet") {
		t.Errorf("hidden finished plans read as a first run:\n%s", got)
	}
	// A failed status fetch has said nothing; the loop's kept status neither.
	failed, err := topFetch(context.Background(), &fakeTop{statusErr: errors.New("down"), projects: projects}, "")
	if err == nil || failed.statusOK {
		t.Fatalf("failed fetch: err %v, statusOK %t", err, failed.statusOK)
	}
	if got := plainTop(failed, 80, 0); !strings.Contains(got, "│ none ") || strings.Contains(got, "no plans yet") {
		t.Errorf("a failed fetch reads as a first run:\n%s", got)
	}
	if got := plainTop(topModel{}, 80, 0); !strings.Contains(got, "│ none ") {
		t.Errorf("a model never fetched lacks none:\n%s", got)
	}
	kept := failed.keepPrevious(fetched)
	if got := plainTop(kept, 80, 0); !strings.Contains(got, "│ "+first+" ") {
		t.Errorf("the kept status lost its first-run line:\n%s", got)
	}
}

// TestTopHintUnderHeader: the unregistered-models hint is a Dim line under
// the header's two rows, in the snapshot as in the loop, the same text in
// plain style.
func TestTopHintUnderHeader(t *testing.T) {
	const hint = "2 models your CLIs offer but no agent registers: brigada agents"
	model := busyTopModel()
	model.hint = hint
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	if lines := renderTop(model, 100, 0, topNow, colour); lines[2] != colour.Dim(hint) {
		t.Errorf("view opens %q, want the hint Dim under the meters", lines[:3])
	}
	lines := renderTop(model, 100, 0, topNow, term.New(logo.Caps{UTF8: true}))
	if !strings.HasPrefix(lines[1], " ━") || lines[2] != hint || !strings.HasPrefix(lines[3], "╭─ NEEDS YOU") {
		t.Errorf("view opens %q, want the header, the meters, the hint and the NEEDS YOU box", lines[:4])
	}

	t.Setenv("TERM", "xterm-256color")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("COLUMNS", "80")
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "")
	fake := &fakeTop{
		status:   busyTopModel().status,
		capacity: busyTopModel().capacity,
		projects: []engine.ProjectInfo{{ProjectID: 7, RepoURL: "https://example.invalid/r.git"}},
	}
	var out, errOut bytes.Buffer
	if code := printTopSnapshot(context.Background(), fake, "https://example.invalid/r.git", hint, topNow, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	got := strings.Split(out.String(), "\n")
	if got[2] != hint || strings.Contains(out.String(), "\x1b") || strings.HasSuffix(strings.TrimRight(out.String(), "\n"), hint) {
		t.Errorf("snapshot lines = %q, want the plain hint under the header and not at the bottom", got)
	}
}

// TestRenderTopASCIIWithoutUTF8: with UTF8 off the whole frame is ASCII,
// at 80 columns and below 60, whatever glyph the view draws.
func TestRenderTopASCIIWithoutUTF8(t *testing.T) {
	model := agentsModel()
	nb := topNow.Add(5 * time.Minute).UTC().Format(engine.TimeLayout)
	model.hint = "2 models your CLIs offer but no agent registers: brigada agents"
	model.status.Plans[0].Tasks = append(model.status.Plans[0].Tasks,
		engine.StatusTask{ID: 5, Title: "ship it", State: "done", Class: "implement", LandReady: "ready"},
		engine.StatusTask{ID: 6, Title: "net", State: "ready"})
	model.status.Retries = []engine.InfraRetry{{ID: 9, TaskID: 6, Kind: "work", NotBefore: nb, Reason: "dns"}}
	model.status.NeedsYou = append(model.status.NeedsYou,
		engine.NeedsYouItem{Kind: "plan", ID: 2, State: "pending_approval", Title: "Plan two"})
	model.status.Report = &engine.StatusReport{JobID: 41, Text: "All landed."}
	ascii := term.New(logo.Caps{})
	for _, width := range []int{50, 60, 80, 100, 120} {
		for _, line := range renderTop(model, width, 0, topNow, ascii) {
			for _, r := range line {
				if r >= 0x80 {
					t.Errorf("width %d: %q in the ASCII line %q", width, r, line)
					break
				}
			}
		}
	}
	got := strings.Join(renderTop(model, 80, 0, topNow, ascii), "\n")
	for _, w := range []string{
		" brigada   r | up 2h0m",
		"15:04:09   q quit  p pause",
		"==================  3/8 slots",
		"+- NEEDS YOU 3 ----",
		"| ! #2 Plan two awaiting ap...  -> brigada plan approve 2 ",
		"+- PLANS ",
		"# landed  # done  # running  . queued -+",
		"| #1 fix the frobnicator",
		"###.#.  1/6 |",
		"|   >  #3  oil the gears     work  claude/sonnet@high  1m40s  Edit",
		"|   ^  #5  ship it           landing: queued",
		"|   -> task #6 work: retry in 5m: dns",
		"+- AGENTS  all <= global 8 -",
		"| -> codex/gpt-5              ........  until 16:04",
		"x  claude/sonnet-long-name  ##......  login expired",
		"| *  claude/opus              #...",
		"+- EVENTS ",
		" All landed. -+",
		"| 15:02  #3 done",
		"| 15:02  #3 landed",
		"+-----",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("ASCII view lacks %q:\n%s", w, got)
		}
	}
}

// TestRenderTopBoxesFillTheWidth: at 60, 80, 100 and 120 columns no line
// is wider than the width, every boxed row (a line starting with a box
// side) is exactly the width, and the borders are exactly the width too.
func TestRenderTopBoxesFillTheWidth(t *testing.T) {
	model := agentsModel()
	model.statusOK = true
	model.status.Report = &engine.StatusReport{JobID: 41, Text: "All landed, but #5 needs a look."}
	for _, width := range []int{60, 80, 100, 120} {
		for _, style := range []term.Style{term.New(logo.Caps{UTF8: true}), term.New(logo.Caps{})} {
			boxed := 0
			for _, line := range renderTop(model, width, 0, topNow, style) {
				n := utf8.RuneCountInString(line)
				if n > width {
					t.Errorf("width %d: line is %d wide: %q", width, n, line)
				}
				if first := string([]rune(line)[:1]); strings.Contains("│|╭╰+", first) {
					boxed++
					if n != width {
						t.Errorf("width %d: boxed line is %d wide: %q", width, n, line)
					}
				}
			}
			if boxed == 0 {
				t.Errorf("width %d: no boxed line", width)
			}
		}
	}
}

// TestRenderTopStyles: plain text reads the same styled and unstyled at
// every colour depth; NO_COLOR (no colour at all) has no escape.
func TestRenderTopStyles(t *testing.T) {
	model := agentsModel()
	model.statusOK = true
	model.err = errors.New("connection refused")
	model.hint = "2 models offered"
	model.status.Report = &engine.StatusReport{JobID: 41, Text: "All landed."}
	for _, width := range []int{60, 100} {
		plain := renderTop(model, width, 0, topNow, term.New(logo.Caps{UTF8: true}))
		if strings.Contains(strings.Join(plain, "\n"), "\x1b") {
			t.Errorf("width %d: the plain frame has an escape", width)
		}
		if noColor := renderTop(model, width, 0, topNow, term.New(logo.Caps{Color: logo.NoColor, UTF8: true})); strings.Join(noColor, "\n") != strings.Join(plain, "\n") {
			t.Errorf("width %d: the NO_COLOR frame differs from the plain one", width)
		}
		for _, depth := range []logo.Depth{logo.Color16, logo.Color256, logo.TrueColor} {
			styled := renderTop(model, width, 0, topNow, term.New(logo.Caps{Color: depth, UTF8: true}))
			if len(styled) != len(plain) {
				t.Fatalf("width %d depth %v: %d lines, want %d", width, depth, len(styled), len(plain))
			}
			for i := range styled {
				if got := term.StripEscapes(styled[i]); got != plain[i] {
					t.Errorf("width %d depth %v line %d: %q reads %q, want %q", width, depth, i, styled[i], got, plain[i])
				}
			}
		}
	}
}

// TestRenderTopFinalOrder: header, meters, NEEDS YOU, PLANS, AGENTS and
// the events section in order, no blank line anywhere.
func TestRenderTopFinalOrder(t *testing.T) {
	got := plainTop(agentsModel(), 100, 0)
	last := -1
	for _, h := range []string{"▙ brigada", "slots", "╭─ NEEDS YOU", "╭─ PLANS", "╭─ AGENTS", "╭─ EVENTS", "15:02  #3 done"} {
		i := strings.Index(got, h)
		if i < last {
			t.Errorf("%q out of order:\n%s", h, got)
		}
		last = i
	}
	if strings.Contains(got, "\n\n") || strings.HasSuffix(got, "\n") {
		t.Errorf("stray blank line:\n%q", got)
	}
}
