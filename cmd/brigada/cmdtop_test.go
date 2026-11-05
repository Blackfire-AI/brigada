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
	"regexp"
	"strings"
	"syscall"
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

// alignRight is a plan line: left, then tail ending at width.
func alignRight(left, tail string, width int) string {
	return left + strings.Repeat(" ", width-term.CellWidth(left)-term.CellWidth(tail)) + tail
}

func plainTop(model topModel, width, height int) string {
	return strings.Join(renderTop(model, width, height, topNow, term.New(logo.Caps{UTF8: true})), "\n")
}

// unboxTop is plainTop with every box read as the bare text it holds: a top
// border as its title (and note), a row as its text with the action tail
// narrowed to two spaces, a bottom border as a blank line.
func unboxTop(model topModel, width, height int) string {
	return unboxLines(strings.Split(plainTop(model, width, height), "\n"))
}

func unboxLines(lines []string) string {
	var out []string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "╭─ "):
			title := strings.TrimPrefix(l, "╭─ ")
			if i := strings.Index(title, " ─"); i >= 0 {
				title = title[:i]
			}
			out = append(out, title)
		case strings.HasPrefix(l, "│ "):
			out = append(out, squashTail(strings.TrimRight(strings.TrimSuffix(strings.TrimPrefix(l, "│ "), " │"), " ")))
		case strings.HasPrefix(l, "╰"):
			out = append(out, "")
		default:
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
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
		"▙ brigada   r · up 2h0m",
		"15:04:09   q quit  p pause\n",
		"━━━━━━━━━━━━━━━━━━  3/8 slots",
		"2 run  2 queue  1 held  1 needs\n",
		"╭─ NEEDS YOU 1 ─",
		"│ ! #2 should the sprocket be blue?",
		"╭─ FOREMAN ─",
		"│   ▸ job #30  foreman",
		"╭─ PLANS  1 live · 0 finished ─",
		" ■ landed  ▣ done  ◆ running  □ queued ─╮",
		"#1 fix the frobnicator",
		"■!◆□  1/4 │",
		"  ▸ #3  oil the gears     work  claude/sonnet@high  1m40s  Edit",
		"  ! #2 wire the sprocket should the sprocket be blue?",
		"  ? #4  polish the knobs  held: waiting for a runner slot",
		"╭─ AGENTS  all ≤ global 8 ─",
		"claude/opus",
		"╭─ EVENTS ─",
		"15:02  task  #3  landed  oil the gears",
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

// TestTopHeaderRuleAndKeys: the header is one line — scope, uptime and
// clock, a rule filling the width that remains, the keys at the right
// end — and fills the width exactly.
func TestTopHeaderRuleAndKeys(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	left := " ▙ brigada   r · up 2h0m"
	right := "15:04:09   q quit  p pause"
	want := left + strings.Repeat(" ", 80-term.CellWidth(left)-term.CellWidth(right)) + right
	lines := topHeader(busyTopModel(), topNow, style, 80)
	if lines[0] != want {
		t.Errorf("header = %q, want %q", lines[0], want)
	}
	if got := term.CellWidth(lines[0]); got != 80 {
		t.Errorf("header is %d wide, want 80", got)
	}
	// Pausing leaves the keys alone: the band says p resumes.
	paused := busyTopModel()
	paused.paused = true
	if got := topHeader(paused, topNow, style, 80)[0]; got != want {
		t.Errorf("paused header = %q, want %q", got, want)
	}
}

// TestTopHeaderMark: the mark is the brand's one-line form, only the ▙
// coloured with the hi-vis orange at each depth; plain with no colour; and
// the word alone, with no ▙ or ▲, without UTF-8.
func TestTopHeaderMark(t *testing.T) {
	model := busyTopModel()
	for _, tt := range []struct {
		name string
		caps logo.Caps
		want string
	}{
		{"true colour", logo.Caps{Color: logo.TrueColor, UTF8: true}, "\x1b[38;2;255;90;31m▙\x1b[0m brigada"},
		{"256 colours", logo.Caps{Color: logo.Color256, UTF8: true}, "\x1b[38;5;202m▙\x1b[0m brigada"},
		{"16 colours", logo.Caps{Color: logo.Color16, UTF8: true}, "\x1b[31m▙\x1b[0m brigada"},
		{"no colour", logo.Caps{Color: logo.NoColor, UTF8: true}, "▙ brigada"},
		{"plain", logo.Caps{UTF8: true}, "▙ brigada"},
		{"ASCII", logo.Caps{}, "brigada"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			row := topHeader(model, topNow, term.New(tt.caps), 80)[0]
			sep := " · "
			if !tt.caps.UTF8 {
				sep = " | "
			}
			if !strings.HasPrefix(row, " "+tt.want+"   r"+sep+"up 2h0m") {
				t.Errorf("header = %q, want the mark %q", row, tt.want)
			}
			if tt.caps.Color == logo.NoColor && strings.Contains(row, "\x1b") {
				t.Errorf("header has escapes: %q", row)
			}
			if !tt.caps.UTF8 && (strings.Contains(row, "▙") || strings.Contains(row, "▲")) {
				t.Errorf("ASCII header has a mark: %q", row)
			}
		})
	}
}

var (
	planCellsField  = regexp.MustCompile(`(?m)^(│ #\d+ .* )([#=>.!■▣◆□]+)(  \d+/\d+ │)$`)
	planLegendField = regexp.MustCompile(`[#=>.■▣◆□] landed  [#=>.■▣◆□] done  [#=>.■▣◆□] running  [#=>.■▣◆□] queued`)
)

// normPlanGlyphs rewrites the two fields of a plain frame that differ with
// the style's glyph set, the plan cells and the PLANS legend, to the glyphs
// of a coloured Unicode frame, and nothing else: a styled frame without its
// escapes then reads as the no-colour one. Every state but queued becomes
// `■`, queued `□`.
func normPlanGlyphs(frame string) string {
	frame = planCellsField.ReplaceAllStringFunc(frame, func(row string) string {
		m := planCellsField.FindStringSubmatch(row)
		cells := strings.Map(func(r rune) rune {
			if r == '□' || r == '.' {
				return '□'
			}
			return '■'
		}, m[2])
		return m[1] + cells + m[3]
	})
	return planLegendField.ReplaceAllString(frame, "■ landed  ■ done  ■ running  □ queued")
}

// TestRenderTopBoxesAtEveryWidth: at 60, 80, 100 and 120 columns no line is
// over the width and every boxed row is exactly the width; the plain style
// reads the same as the styled text; NO_COLOR has no escape; the ASCII frame
// has no Unicode of its own.
func TestRenderTopBoxesAtEveryWidth(t *testing.T) {
	model := agentsModel()
	model.statusOK = true
	model.status.Report = &engine.StatusReport{JobID: 41, Text: "All landed, but #5 needs a look.\nSecond line."}
	plain := term.New(logo.Caps{UTF8: true})
	for _, width := range []int{60, 80, 100, 120} {
		lines := renderTop(model, width, 0, topNow, plain)
		for i, l := range lines {
			boxed := strings.HasPrefix(l, "│") || strings.HasPrefix(l, "╭") || strings.HasPrefix(l, "╰")
			if n := term.CellWidth(l); n > width || (boxed && n != width) {
				t.Errorf("width %d line %d %q is %d wide", width, i, l, n)
			}
		}
		for _, caps := range []logo.Caps{
			{Color: logo.Color16, UTF8: true},
			{Color: logo.Color256, UTF8: true},
			{Color: logo.TrueColor, UTF8: true},
		} {
			styled := renderTop(model, width, 0, topNow, term.New(caps))
			if got, want := normPlanGlyphs(term.StripEscapes(strings.Join(styled, "\n"))), normPlanGlyphs(strings.Join(lines, "\n")); got != want {
				t.Errorf("width %d: styled text differs from plain:\n%s\nvs\n%s", width, got, want)
			}
		}
		if got := strings.Join(renderTop(model, width, 0, topNow, term.New(logo.Caps{Color: logo.NoColor, UTF8: true})), "\n"); strings.Contains(got, "\x1b") {
			t.Errorf("width %d: NO_COLOR frame has an escape: %q", width, got)
		}
		for _, l := range renderTop(model, width, 0, topNow, term.New(logo.Caps{})) {
			for _, r := range l {
				if r >= 0x80 {
					t.Errorf("width %d: %q in the ASCII line %q", width, r, l)
					break
				}
			}
		}
	}
}

// TestTopHiVisOnlyOnTheMark: in a whole frame at true colour and at 256
// colours the hi-vis escape appears once, on the ▙.
func TestTopHiVisOnlyOnTheMark(t *testing.T) {
	for _, tt := range []struct {
		caps logo.Caps
		hi   string
	}{
		{logo.Caps{Color: logo.TrueColor, UTF8: true}, "38;2;255;90;31"},
		{logo.Caps{Color: logo.Color256, UTF8: true}, "38;5;202"},
	} {
		frame := strings.Join(renderTop(busyTopModel(), 100, 0, topNow, term.New(tt.caps)), "\n")
		if n := strings.Count(frame, tt.hi); n != 1 {
			t.Errorf("%s appears %d times, want once", tt.hi, n)
		}
		if !strings.Contains(frame, "\x1b["+tt.hi+"m▙") {
			t.Errorf("%s is not on the mark", tt.hi)
		}
	}
}

// TestTopMetersSlotsCell: the slots cell scales its bar to the cap —
// at most ten cells — never renders an all-empty bar for a sliver of
// use, and has no bar at all for a cap of zero, an uncapped layer or a
// failed fetch.
func TestTopMetersSlotsCell(t *testing.T) {
	two, five, twelve, zero := 2, 5, 12, 0
	bar := func(n int) string { return strings.Repeat("━", n) }
	tests := []struct {
		name   string
		layers []engine.CapacityLayer
		want   string
	}{
		{"cap 2", []engine.CapacityLayer{{Layer: "global", Cap: &two, Use: 1}}, " " + bar(18) + "  1/2 slots"},
		{"cap 5", []engine.CapacityLayer{{Layer: "global", Cap: &five, Use: 2}}, " " + bar(18) + "  2/5 slots"},
		{"cap above 10", []engine.CapacityLayer{{Layer: "global", Cap: &twelve, Use: 6}}, " " + bar(18) + "  6/12 slots"},
		{"cap 0 has no meter", []engine.CapacityLayer{{Layer: "global", Cap: &zero, Use: 2}}, " 2/0 slots"},
		{"uncapped has no meter", []engine.CapacityLayer{{Layer: "global", Use: 2}}, " 2/none slots"},
		{"failed fetch", nil, " ?/? slots"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := busyTopModel()
			model.capacity.Layers = tt.layers
			got := topMeters(model, term.New(logo.Caps{UTF8: true}), 100)[0]
			if !strings.HasPrefix(got, tt.want+"  ") {
				t.Errorf("meters = %q, want the slots cell %q", got, tt.want)
			}
		})
	}
	// The meter fills in proportion, at least one cell once use is over zero,
	// Accent until the cap is reached and Warn from there; the rest is Dim.
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	for _, tt := range []struct {
		use, limit int
		want       string
	}{
		{1, 12, style.Accent("━") + style.Dim(bar(17))},
		{6, 12, style.Accent(bar(9)) + style.Dim(bar(9))},
		{0, 12, style.Dim(bar(18))},
		{12, 12, style.Warn(bar(18))},
	} {
		if got := topSlotMeter(tt.use, tt.limit, 18, style); got != tt.want {
			t.Errorf("meter %d/%d = %q, want %q", tt.use, tt.limit, got, tt.want)
		}
	}
	// The meter shrinks to fit and is left out under 4 cells.
	model := busyTopModel()
	if got := topMeters(model, term.New(logo.Caps{UTF8: true}), 50)[0]; strings.Count(got, "━") != 5 || term.CellWidth(got) != 50 {
		t.Errorf("shrunk meter = %q", got)
	}
	if got := topMeters(model, term.New(logo.Caps{UTF8: true}), 48)[0]; strings.Contains(got, "━") {
		t.Errorf("a meter under 4 cells stays: %q", got)
	}
	if got := topMeters(model, term.New(logo.Caps{}), 100)[0]; !strings.HasPrefix(got, " "+strings.Repeat("=", 18)+"  3/8 slots") {
		t.Errorf("ASCII meter = %q", got)
	}
}

// TestTopMetersCountersAndColours: the counters follow the cell — run
// bold, queue plain, held warn, needs bad — three spaces apart.
func TestTopMetersCountersAndColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	got := topMeters(busyTopModel(), style, 100)[0]
	want := " " + style.Accent(strings.Repeat("━", 6)) + style.Dim(strings.Repeat("━", 12)) + "  3/8 slots" +
		strings.Repeat(" ", 100-1-18-2-len("3/8 slots")-len("1 run  2 queue  1 held  1 needs")) +
		style.Bold("1 run") + "  2 queue  " + style.Warn("1 held") + "  " + style.Bad("1 needs")
	if got != want {
		t.Errorf("meters = %q, want %q", got, want)
	}
}

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
			got := topNeedsYou(api.Status{NeedsYou: tt.queue}, false, term.New(logo.Caps{UTF8: true}), 100)[1:]
			for i := range got {
				got[i] = squashTail(got[i])
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("lines = %q, want %q", got, tt.want)
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
			if len(lines) != 2 || squashTail(lines[1]) != tt.want {
				t.Errorf("lines = %q, want the entry %q", lines, tt.want)
			}
			if lines[0] != "NEEDS YOU 1" {
				t.Errorf("title = %q, want the queue's count", lines[0])
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
	got := strings.Join(topNeedsYou(api.Status{NeedsYou: items}, false, term.New(logo.Caps{UTF8: true}), 100), "\n")
	want := strings.Join([]string{
		"NEEDS YOU 5",
		"! #1 why 1", "! #2 why 2", "! #3 why 3",
		"+2 more (brigada status)",
	}, "\n")
	if got != want {
		t.Errorf("lines =\n%s\nwant\n%s", got, want)
	}
}

// TestTopNeedsYouColours: the entries are Bad, the "+N more" and the
// empty queue's line dim, the header's rule fill dim too.
func TestTopNeedsYouColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	st := api.Status{NeedsYou: []engine.NeedsYouItem{
		{Kind: "task", ID: 2, State: "needs_foreman", Reason: "should the sprocket be blue?"},
		{Kind: "agent", ID: 4, State: "broken", Title: "dsh/large", Reason: "auth: no key"},
	}}
	lines := topNeedsYou(st, false, style, 100)
	boxed := (&topFrame{lines: newTopNeedsYou(st, false, style, 96), style: style, width: 100}).texts()
	if want := style.Bad("╭─") + " " + style.Bad("NEEDS YOU 2") + " " + style.Bad(strings.Repeat("─", 100-3-len("NEEDS YOU 2")-2)) + style.Bad("╮"); boxed[0] != want {
		t.Errorf("top border = %q, want a Bad border and title with the count", boxed[0])
	}
	if boxed[1][:len(style.Bad("│"))] != style.Bad("│") || boxed[len(boxed)-1] != style.Bad("╰"+strings.Repeat("─", 98)+"╯") {
		t.Errorf("box = %q, want Bad sides and bottom", boxed)
	}
	tail := "→ brigada agent reset dsh/large"
	wantTail := fmt.Sprintf("%-*s  %s", 100-term.CellWidth(tail)-2, "! dsh/large auth: no key", tail)
	if lines[1] != style.Bad("! #2 should the sprocket be blue?") || lines[2] != style.Bad(wantTail) {
		t.Errorf("entries = %q", lines)
	}
	var four []engine.NeedsYouItem
	for i := 0; i < 4; i++ {
		four = append(four, engine.NeedsYouItem{Kind: "task", ID: int64(i + 1), Reason: "why"})
	}
	if got := topNeedsYou(api.Status{NeedsYou: four}, false, style, 100); got[4] != style.Dim("+1 more (brigada status)") {
		t.Errorf("cap line = %q, want dim +1 more (brigada status)", got[4])
	}
	if got := topNeedsYou(api.Status{}, false, style, 100); got[1] != style.Dim("✓ nothing needs you") {
		t.Errorf("empty queue = %q, want dim ✓ nothing needs you", got)
	}
	empty := (&topFrame{lines: newTopNeedsYou(api.Status{}, false, style, 96), style: style, width: 100}).texts()
	if empty[0] != style.Dim("╭─")+" "+style.Bold("NEEDS YOU")+" "+style.Dim(strings.Repeat("─", 100-3-len("NEEDS YOU")-2))+style.Dim("╮") {
		t.Errorf("empty top border = %q, want a grey border and title", empty[0])
	}
}

// squashTail narrows the gap before an action tail to two spaces: the tails
// sit against the right edge of the box.
func squashTail(s string) string {
	i := strings.Index(s, "  →")
	if i < 0 {
		return s
	}
	return strings.TrimRight(s[:i], " ") + "  " + strings.TrimLeft(s[i:], " ")
}

// TestTopNeedsYouEmptyQueue: the section stays visible when nothing
// needs the user.
func TestTopNeedsYouEmptyQueue(t *testing.T) {
	lines := topNeedsYou(api.Status{}, false, term.New(logo.Caps{UTF8: true}), 80)
	if got := strings.Join(lines, "\n"); got != "NEEDS YOU\n✓ nothing needs you" {
		t.Errorf("empty queue =\n%s", got)
	}
	if got := topNeedsYou(api.Status{}, false, term.New(logo.Caps{}), 80); got[1] != "ok nothing needs you" {
		t.Errorf("ASCII empty queue = %q", got)
	}
}

// topEventTexts is the texts of an events section's lines, the title for
// its top border and no bottom border.
func topEventTexts(items []history.Item, status api.Status, now time.Time, style term.Style, width int) []string {
	return (&topFrame{lines: topEventLines(items, status, now, style, width), style: style}).texts()
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

// TestTopEventsList: the EVENTS section is a head and one row per
// event, newest first, the time then two spaces then the words; at
// most 10 rows; a dim `no events` under the head when nothing happened.
func TestTopEventsList(t *testing.T) {
	model := busyTopModel()
	style := term.New(logo.Caps{UTF8: true})
	head := "EVENTS"
	// The landed task's done row is hidden.
	want := []string{head, "15:02  task  #3  landed  oil the gears"}
	if got := topEventTexts(model.events, model.status, topNow, style, 80); !reflect.DeepEqual(got, want) {
		t.Errorf("list = %q, want %q", got, want)
	}
	if got := topEventTexts(nil, api.Status{}, topNow, style, 80); !reflect.DeepEqual(got, []string{head, "no events"}) {
		t.Errorf("empty = %q", got)
	}
	got := topEventTexts(topEventsFixture(14), api.Status{}, topNow, style, 80)
	if len(got) != 11 || got[1] != "15:02  task  #14  done" || got[10] != "14:53  task  #5   done" {
		t.Errorf("14 events = %q, want the head and the newest 10, #14 first", got)
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
	want := []string{"15:02        task  #2  done", "10-18 09:30  task  #1  done"}
	if got := topEventTexts(items, api.Status{}, topNow, style, 80)[1:]; !reflect.DeepEqual(got, want) {
		t.Errorf("mixed days = %q, want %q", got, want)
	}
	// With ten newer rows the other-day event is not built and the column is 5 wide.
	many := append([]history.Item{items[0]}, topEventsFixture(10)...)
	if got := topEventTexts(many, api.Status{}, topNow, style, 80); got[1] != "15:02  task  #10  done" || got[10] != "14:53  task  #1   done" {
		t.Errorf("dropped row shifted the column: %q", got)
	}
}

// topTitleStatus is a status whose plan 1 and task 3 have the given titles.
func topTitleStatus(plan, task string) api.Status {
	return api.Status{Plans: []engine.StatusPlan{{ID: 1, Title: plan, Tasks: []engine.StatusTask{{ID: 3, Title: task}}}}}
}

// TestTopEventsCut: only the title gives way. A title of exactly its room
// stays whole and one cell more is cut with an ellipsis; under 8 cells of
// room the title is left out with its gap, and the other columns never cut.
func TestTopEventsCut(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	item := []history.Item{{Type: history.TypeEvent, ID: 1, Kind: "task_landed", TaskID: 3, CreatedAt: "2026-10-20T15:02:00.000000000Z"}}
	status := topTitleStatus("p", "0123456789")
	const fixed = len("15:02  task  #3  landed")
	// Room is width - fixed - 2.
	if got := topEventTexts(item, status, topNow, style, fixed+2+10)[1]; got != "15:02  task  #3  landed  0123456789" {
		t.Errorf("title of exactly its room = %q", got)
	}
	if got := topEventTexts(item, status, topNow, style, fixed+2+9)[1]; got != "15:02  task  #3  landed  012345..." {
		t.Errorf("title one cell too long = %q", got)
	}
	if got := topEventTexts(item, status, topNow, style, fixed+2+8)[1]; got != "15:02  task  #3  landed  01234..." {
		t.Errorf("title at the floor = %q", got)
	}
	for _, w := range []int{fixed + 2 + 7, fixed + 2, fixed} {
		if got := topEventTexts(item, status, topNow, style, w)[1]; got != "15:02  task  #3  landed" {
			t.Errorf("width %d: title not left out: %q", w, got)
		}
	}
	// Below the fixed columns topFit is the last resort.
	if got := topEventTexts(item, status, topNow, style, fixed-1)[1]; got != "15:02  task  #3  la..." {
		t.Errorf("below the fixed columns = %q", got)
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
		{"plan_done", ev("plan_done", 0, 0, `{"plan":99,"status":"done"}`), "15:02  plan  #99  done"},
		{"plan_done without a plan", ev("plan_done", 0, 0, `{}`), "15:02  plan  done"},
		{"plan_done with a plan and a task", ev("plan_done", 12, 0, `{"plan":99}`), "15:02  plan  #99  done"},
		{"plan_approved", ev("plan_approved", 0, 0, `{"plan":7,"status":"active"}`), "15:02  plan  #7  approved"},
		{"plan_awaiting_approval", ev("plan_awaiting_approval", 0, 0, `{"plan":8}`), "15:02  plan  #8  awaiting approval"},
		{"task_done", ev("task_done", 3, 0, `{"status":"done"}`), "15:02  task  #3  done"},
		{"task_landed", ev("task_landed", 328, 0, `{"pushed":"pushed"}`), "15:02  task  #328  landed"},
		{"task_landed no push state", ev("task_landed", 328, 0, ``), "15:02  task  #328  landed"},
		{"task_landed waiting", ev("task_landed", 328, 0, `{"pushed":"waiting"}`), "15:02  task  #328  landed, push waits"},
		{"task_landed local", ev("task_landed", 328, 0, `{"pushed":"local"}`), "15:02  task  #328  landed, push waits"},
		{"task_pushed", ev("task_pushed", 328, 0, `{}`), "15:02  task  #328  pushed"},
		{"task_failed", ev("task_failed", 4, 0, `{"status":"failed"}`), "15:02  task  #4  failed"},
		{"task_land_failed", ev("task_land_failed", 4, 0, `{"reason":"conflict"}`), "15:02  task  #4  land failed"},
		{"review approve", ev("review_verdict", 0, 9, `{"status":"approve","branch":"fix/x"}`), "15:02  review  approve  fix/x"},
		{"review changes", ev("review_verdict", 0, 9, `{"status":"changes","branch":"fix/x"}`), "15:02  review  changes  fix/x"},
		{"review no verdict", ev("review_verdict", 0, 9, `{"status":"no_verdict"}`), "15:02  review  no verdict"},
		{"agent_broken", ev("agent_broken", 0, 0, `{"harness":"codex","model":"gpt-5","reason":"auth"}`), "15:02  agent  broken  codex/gpt-5"},
		{"agent_recovered", ev("agent_recovered", 0, 0, `{"harness":"codex","model":"gpt-5"}`), "15:02  agent  recovered  codex/gpt-5"},
		{"agent_cooling", ev("agent_cooling", 3, 5, `{"account":"work","until":"`+until+`"}`), "15:02  agent  cooling  work until 16:04"},
		{"agent_cooling without a deadline", ev("agent_cooling", 3, 5, `{"account":"work"}`), "15:02  agent  cooling  work"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topEventTexts([]history.Item{tt.item}, api.Status{}, topNow, style, 200)[1]; got != tt.want {
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
	for _, text := range topEventTexts(model.events, api.Status{}, topNow, term.New(logo.Caps{UTF8: true}), 80) {
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
	if got := topEventTexts(merged, prev.status, topNow, style, 200); len(got) != 2 || got[1] != "15:02  task  #3  landed  oil the gears" {
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
	strip := strings.Join(topEventTexts(model.events, model.status, topNow, style, 200), "\n")
	if !strings.Contains(strip, "15:02  task  #3  landed") ||
		strings.Contains(strip, "15:02:45") || strings.Contains(strip, "15:02:41") {
		t.Errorf("strip = %q, want HH:MM pairs", strip)
	}
	header := topHeader(model, topNow, style, 100)[0]
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
			"15:02  task  #3  " + style.OK("done")},
		{"recovered", history.Item{Type: history.TypeEvent, ID: 2, Kind: "agent_recovered", ProjectID: 1,
			Payload: []byte(`{"harness":"codex","model":"gpt-5"}`), CreatedAt: "2026-10-20T15:02:46.000000000Z"},
			"15:02  agent  " + style.OK("recovered") + "  codex/gpt-5"},
		{"failed", history.Item{Type: history.TypeEvent, ID: 3, Kind: "task_failed", TaskID: 4,
			Payload: []byte(`{"status":"failed"}`), CreatedAt: "2026-10-20T15:02:47.000000000Z"},
			"15:02  task  #4  " + style.Bad("failed")},
		{"broken", history.Item{Type: history.TypeEvent, ID: 4, Kind: "agent_broken", ProjectID: 1,
			Payload: []byte(`{"harness":"codex","model":"gpt-5","reason":"auth: no key"}`), CreatedAt: "2026-10-20T15:02:48.000000000Z"},
			"15:02  agent  " + style.Bad("broken") + "  codex/gpt-5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topEventTexts([]history.Item{tt.item}, api.Status{}, topNow, style, 200)[1]; got != tt.want {
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
	got := unboxTop(model, 200, 0)
	want := "NEEDS YOU 3" +
		"\n! #2 should the sprocket be blue?" +
		"\n! #8 held by rule gate_land: gate_land is ask  → brigada land approve 8" +
		"\n! #9 waits to push: task/9-1@abc123"
	tree := alignRight("#1 fix the frobnicator", "■!◆□■■  3/6", 196) +
		"\n  ▸ #3  oil the gears     work  claude/sonnet@high  1m40s  Edit" +
		"\n  ▸ #9  push the widget   land                      1m40s" +
		"\n  ! #2 wire the sprocket should the sprocket be blue?"
	for _, w := range []string{tree, want} {
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
	got := plainTop(model, 200, 0)
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
	got := unboxTop(model, 200, 0)
	want := "  ! #2 wire the sprocket should the sprocket be blue?" +
		"\n  ↺ task #4 work: retry in 5m: could not resolve github.com" +
		"\n\nAGENTS"
	if !strings.Contains(got, want) {
		t.Errorf("view lacks %q:\n%s", want, got)
	}
	if strings.Contains(got, "#4 polish the knobs") {
		t.Errorf("the retrying task also renders its queued line:\n%s", got)
	}
}

// TestTopPlansTree: every plan the status lists renders — a live one
// with its running jobs first, then a line per task still live work, a
// finished one as one line — then the hidden finished plans counted.
func TestTopPlansTree(t *testing.T) {
	leased := topNow.Add(-14 * time.Minute).UTC().Format(engine.TimeLayout)
	nb := topNow.Add(5 * time.Minute).UTC().Format(engine.TimeLayout)
	head := "PLANS"
	tests := []struct {
		name string
		st   api.Status
		want []string
	}{
		{"no plans", api.Status{}, []string{head, "none"}},
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
			head,
			alignRight("#96 Rework brigada top", "■■!!□◆□□□□  2/10", 80),
			"  ▸ #317  work     work  opencode/glm-5.3  14m00s  editing cmdtop.go",
			"  ▸ #313  landing  land                    14m00s",
			"  ! #314 finish rounds spent",
			"  ! #315 ask needs_user",
			"  · #316  audit    (review)",
			"  · #318  tree     (queued)",
			"  · #319  docs     (waiting on tree, #317)",
			"  ? #320  fix      held: waiting for a runner slot",
			"  ↺ task #321 work: retry in 5m: could not resolve github.com",
		}},
		{"finished plans one line, hidden ones counted", api.Status{
			Plans: []engine.StatusPlan{
				{ID: 97, Title: "all done, not closed", State: "active", Tasks: []engine.StatusTask{{ID: 1, State: "done"}, {ID: 2, State: "done"}}},
				{ID: 83, Title: "Role instructions", State: "done", Tasks: []engine.StatusTask{{ID: 3, State: "done"}, {ID: 4, State: "done"}, {ID: 5, State: "cancelled"}}},
				{ID: 80, Title: "Abandoned", State: "cancelled", Tasks: []engine.StatusTask{{ID: 6, Title: "never", State: "cancelled"}, {ID: 7, Title: "left", State: "ready"}}},
			},
			HiddenFinishedPlans: 41,
		}, []string{
			head,
			alignRight("#97 all done, not closed", "■■  2/2", 80),
			alignRight("#83 Role instructions", "done", 80),
			alignRight("#80 Abandoned", "cancelled", 80),
		}},
		{"a live plan's review job under its plan", api.Status{
			Plans: []engine.StatusPlan{{ID: 12, Title: "Review me", State: "reviewing", ReviewRound: 2, ReviewJob: 50,
				Tasks: []engine.StatusTask{{ID: 1, Title: "first", State: "pending"}}}},
			Running: []engine.StatusJob{{ID: 50, Kind: "plan_review", Harness: "claude", Model: "opus", LeasedAt: leased}},
		}, []string{
			head,
			alignRight("#12 Review me", "reviewing round 2", 80),
			"  ▸ job #50  plan_review  claude/opus  14m00s",
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

// TestTopPlansTreeColours: the live plan's id Bold, the bar's fill OK and
// its empty cells Dim, the finished marks OK and Bad, the line forms in
// their colours.
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
	want := []string{
		"PLANS",
		alignRight(style.Bold("#1")+" live", style.OK("■")+style.Accent("■")+style.Dim("□")+style.Dim("□")+style.Bad("■")+"  1/5", 80),
		"  " + style.Accent("▸") + " #2  run   work",
		"  " + style.Dim("· #3  q     (queued)"),
		"  " + style.Warn("?") + " #4  held  " + style.Warn("held: waiting for a runner slot"),
		"  " + style.Bad("! #5 stuck needs_foreman"),
		alignRight("#2 done", style.OK("done"), 80),
		alignRight("#3 gone", style.Bad("cancelled"), 80),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree =\n%q\nwant\n%q", got, want)
	}
}

// topPlanLines is a status's PLANS lines in plain text, without the head.
func topPlanLines(st api.Status, width int) []string {
	return topPlansTree(st, topNow, term.New(logo.Caps{UTF8: true}), width)[1:]
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
	want := []string{
		alignRight("#1 approve me", "awaiting approval", 80),
		alignRight("#2 review me", "reviewing round 3", 80),
		"  · plan review queued",
		alignRight("#3 foreman", "needs foreman", 80),
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

// TestTopPlanTailsAlign: the tails of every plan line start in one column,
// the width of the widest id, tag and title, the title cut first when the
// tail would not fit.
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
	right := func(left, tail string) string {
		return left + strings.Repeat(" ", 80-utf8.RuneCountInString(left)-utf8.RuneCountInString(tail)) + tail
	}
	want := []string{
		right("#101 brigada  Fix events strip", "reviewing round 2"),
		right("#102 brigada  Top polish", "■■□□□  2/5"),
		right("#9   brigada  Shipped", "done"),
	}
	if got := plain(80); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// At 40 columns the titles are cut to keep the widest tail whole, and
	// the tails still end at the right edge.
	narrow := plain(40)
	for _, l := range narrow {
		if n := term.CellWidth(l); n != 40 {
			t.Errorf("line %q is %d wide, want 40", l, n)
		}
	}
	if !strings.Contains(narrow[0], "Fix e... reviewing round 2") {
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
		"#7 land me" + strings.Repeat(" ", 80-len("#7 land me")-utf8.RuneCountInString("▣▣▣■■  2/5")) + "▣▣▣■■  2/5",
		"  ⇡ #330  events wording  landing: waiting (retry at 11:10, last exit 1)",
		"  ⇡ #331  queued land     landing: queued",
		"  ⇡ #332  bare            landing: not landed",
	}
	if got := topPlansTree(st, topNow, term.New(logo.Caps{UTF8: true}), 80)[1:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := topLineClass(t, st, 80, want[1]); c != yieldHeld {
		t.Errorf("the ⇡ line is class %d, want the held class", c)
	}
	teal := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	lines := []topLine{{job: topLandingRow(tasks[2], topNow, teal)}}
	topLayoutJobs(lines, teal, 80)
	if want := "  " + teal.Teal("⇡") + " #332  bare  " + teal.Teal("landing: not landed"); lines[0].text != want {
		t.Errorf("the ⇡ line = %q, want teal mark and wait text over a plain id and title: %q", lines[0].text, want)
	}

	// A land job that speaks for the task shows as its own line, not the ⇡ one.
	st.Running = []engine.StatusJob{{ID: 9, TaskID: 332, Kind: "land"}}
	got := strings.Join(topPlanLines(st, 80), "\n")
	if !strings.Contains(got, "▸ #332  bare") || strings.Contains(got, "landing: not landed") {
		t.Errorf("a running land job and the ⇡ line both speak:\n%s", got)
	}

	// The tail reads complete only when the landing is; a class that never
	// lands and a finished plan count state done.
	plain := term.New(logo.Caps{UTF8: true})
	last := func(state string, landed bool) string {
		p := engine.StatusPlan{ID: 8, Title: "p", State: state, Tasks: []engine.StatusTask{
			{ID: 1, State: "done", Class: "implement", Landed: true},
			{ID: 2, Title: "last", State: "done", Class: "implement", Landed: landed}}}
		return strings.TrimLeft(topPlanLine(p, "", nil, newTopPlanLayout([]engine.StatusPlan{p}, nil, false, plain, 40), plain, 40), "#8 p ")
	}
	if got := last("active", false); got != "■▣  1/2" {
		t.Errorf("the tail counts a task not landed: %q", got)
	}
	if got := last("active", true); got != "■■  2/2" {
		t.Errorf("the tail misses a landed task: %q", got)
	}
	if got := last("done", false); got != "done" {
		t.Errorf("a finished plan = %q, want the word done", got)
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
	tests := []struct {
		width int
		want  []string
	}{
		{100, []string{
			"  ▸ #331  clip frame to height  work  claude/sonnet@high  3m12s  Edit",
			"  ? #333  agent order           held: waiting for a free account slot",
		}},
		// Under 76 columns the last event is left out and the title gives way:
		// the kind, agent and age stay whole; the wait text is cut at the end.
		{60, []string{
			"  ▸ #331  clip frame to ...  work  claude/sonnet@high  3m12s",
			"  ? #333  agent order        held: waiting for a free acc...",
		}},
	}
	for _, tt := range tests {
		got := topPlanLines(st, tt.width)
		if len(got) < 3 || got[1] != tt.want[0] || got[2] != tt.want[1] {
			t.Errorf("width %d lines =\n%s\nwant\n%s", tt.width, strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
		}
	}
	// A last event still too long with the title at 8 columns is cut at the
	// line's end; the kind, agent and age stay whole.
	long := job
	long.LastEvent = "tool_start " + strings.Repeat("x", 60)
	st.Running = []engine.StatusJob{long}
	got := topPlanLines(st, 80)[1]
	if !strings.HasPrefix(got, "  ▸ #331  clip ...  work  claude/sonnet@high  3m12s  xxx") || !strings.HasSuffix(got, "xxx...") || utf8.RuneCountInString(got) != 80 {
		t.Errorf("line = %q, want the title at 8 and the event cut to 80", got)
	}
	// A task with no title has an empty title cell.
	st = api.Status{
		Plans:   []engine.StatusPlan{{ID: 7, Title: "p", State: "active", Tasks: []engine.StatusTask{{ID: 334, State: "running"}}}},
		Running: []engine.StatusJob{{ID: 42, TaskID: 334, Kind: "work", LeasedAt: leased}},
	}
	if got := topPlanLines(st, 100); got[1] != "  ▸ #334  work  3m12s" {
		t.Errorf("untitled task line = %q", got[1])
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
	want := []string{right("#96 Rework", "done"), right("#97 Live", "■■■□  3/4"), "  · #6  go  (queued)"}
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
	// between returns the view's lines from NEEDS YOU's head through
	// PLANS's head.
	between := func(width int, m topModel) []string {
		lines := renderTop(m, width, 0, topNow, term.New(logo.Caps{UTF8: true}))
		from, to := -1, -1
		for i, l := range lines {
			if strings.HasPrefix(l, "╭─ NEEDS YOU") {
				from = i
			}
			if strings.HasPrefix(l, "╭─ PLANS") {
				to = i
			}
		}
		if from < 0 || to < from {
			t.Fatalf("view lacks NEEDS YOU or PLANS:\n%s", strings.Join(lines, "\n"))
		}
		// The boxes read as the bare section text they hold: a top border
		// as its title, a row as its text, a bottom border as a blank line.
		var out []string
		for _, l := range lines[from : to+1] {
			switch {
			case strings.HasPrefix(l, "╭─ "):
				title := strings.TrimPrefix(l, "╭─ ")
				if i := strings.Index(title, "  "); i >= 0 {
					title = title[:i]
				}
				if i := strings.Index(title, " ─"); i >= 0 {
					title = title[:i]
				}
				out = append(out, title)
			case strings.HasPrefix(l, "│ "):
				text := strings.TrimSuffix(strings.TrimPrefix(l, "│ "), " │")
				out = append(out, strings.TrimRight(strings.Replace(text, "✓ nothing needs you", "nothing needs you", 1), " "))
			case strings.HasPrefix(l, "╰"):
				out = append(out, "")
			}
		}
		return out
	}
	head := func(title string) string { return title }
	fline := "  ▸ job #2619  foreman  claude/opus"
	oline := "  ▸ job #63  plan_review"
	needsLine := "! #2 should the sprocket be blue?"

	t.Run("reported case", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{foremanJob}
		want := []string{head("NEEDS YOU"), "nothing needs you", "", head("FOREMAN"), fline, "", head("PLANS")}
		if got := between(80, m); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("foreman only with needs-you entries", func(t *testing.T) {
		m := calm()
		m.status.NeedsYou = busyTopModel().status.NeedsYou
		m.status.Running = []engine.StatusJob{foremanJob}
		got := strings.Join(between(80, m), "\n")
		want := strings.Join([]string{head("NEEDS YOU 1"), needsLine, "", head("FOREMAN"), fline, "", head("PLANS")}, "\n")
		if got != want || strings.Contains(got, "JOBS") {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("non-foreman job only", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{otherJob}
		got := strings.Join(between(80, m), "\n")
		want := strings.Join([]string{head("NEEDS YOU"), "nothing needs you", "", head("JOBS"), oline, "", head("PLANS")}, "\n")
		if got != want || strings.Contains(got, "FOREMAN") {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("both blocks, foreman first", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{otherJob, foremanJob}
		got := strings.Join(between(80, m), "\n")
		want := strings.Join([]string{head("NEEDS YOU"), "nothing needs you", "", head("FOREMAN"), fline, "",
			head("JOBS"), oline, "", head("PLANS")}, "\n")
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
		want := []string{head("NEEDS YOU"), "nothing needs you", "",
			head("FOREMAN"),
			"  ? job #61  held: " + api.HeldWait(m.status.Held[0]),
			topRetryLine(m.status.Retries[0], topNow, style, 80), "",
			head("JOBS"),
			"  ? job #64  held: " + api.HeldWait(m.status.Held[1]),
			topRetryLine(m.status.Retries[1], topNow, style, 80), "",
			head("PLANS")}
		if got := between(80, m); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
	t.Run("no plan-less job", func(t *testing.T) {
		got := plainTop(busyTopModel(), 100, 0)
		if strings.Contains(got, "FOREMAN") || strings.Contains(got, "JOBS") {
			t.Errorf("a head renders with no plan-less job:\n%s", got)
		}
		if !strings.Contains(got, "│\n╰") || !strings.Contains(got, "╰"+strings.Repeat("─", 98)+"╯\n╭─ PLANS") {
			t.Errorf("NEEDS YOU is not followed by PLANS with no blank row:\n%s", got)
		}
	})
	t.Run("narrow boxes stay", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{otherJob, foremanJob}
		got := between(59, m)
		for _, title := range []string{"FOREMAN", "JOBS"} {
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
		// Two rows that the landed task's hidden done row does not leave out.
		m.events = []history.Item{
			{Type: history.TypeEvent, ID: 13, Kind: "task_pushed", ProjectID: 1, TaskID: 3, CreatedAt: "2026-10-20T15:02:41.000000000Z"},
			m.events[1],
		}
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
	got := unboxTop(model, 100, 0)
	want := "! #2 should the sprocket be blue?" +
		"\n\nFOREMAN"
	rest := "\n  ▸ job #60  foreman  claude/opus" +
		"\n  ? job #61  held: foreman turn waiting: no runner offers claude/opus" +
		"\n  ↺ job #62 foreman: retry in 2m: could not resolve github.com" +
		"\n\nJOBS"
	tail := "\n  ▸ job #63  plan_review" +
		"\n\nPLANS"
	i := strings.Index(got, want)
	if i < 0 {
		t.Fatalf("view lacks the FOREMAN head after NEEDS YOU:\n%s", got)
	}
	// The heads carry a rule to the width; skip it to the line's end.
	body := got[i+len(want):]
	body = body[strings.Index(body, "\n"):]
	if !strings.HasPrefix(body, rest) {
		t.Errorf("FOREMAN block wrong:\n%s", got)
	}
	body = body[len(rest):]
	body = body[strings.Index(body, "\n"):]
	if !strings.HasPrefix(body, tail) {
		t.Errorf("JOBS block wrong:\n%s", got)
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
// finished plan folds into the +N finished line before any running line
// drops, and the header and NEEDS YOU survive at each height.
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
			if !strings.HasPrefix(lines[0], " ▙ brigada   r ") || strings.Contains(lines[0], "up ") ||
				!strings.HasPrefix(lines[1], " ✗ engine: connection refused") && !strings.HasPrefix(lines[2], " ✗ engine: connection refused") {
				t.Errorf("the header and its error band are not first:\n%s", got)
			}
			for _, w := range []string{"2 models offered", "NEEDS YOU", "! #1 question 1", "! #2 question 2", "! #3 question 3", "+2 more"} {
				if !strings.Contains(got, w) {
					t.Errorf("the frame lacks %q:\n%s", w, got)
				}
			}
			shown := strings.Count(got, "▸")
			finished := strings.Count(got, "done plan")
			if shown < running && finished > 0 {
				t.Errorf("a running line dropped (%d of %d) while %d finished plans stay:\n%s", shown, running, finished, got)
			}
			if strings.Contains(got, "+10 finished") {
				t.Errorf("a finished fold line shows:\n%s", got)
			}
			if height == 12 && shown >= running {
				t.Errorf("no running line dropped at 12 rows, the case is vacuous:\n%s", got)
			}
		})
	}
}

// TestRenderTopBelowTheFloor: below the lines the header and NEEDS YOU
// need, the height wins. The meters strip is the last line outside the two
// to go, and the hard clip cuts into NEEDS YOU from its bottom.
func TestRenderTopBelowTheFloor(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	header := " ▙ brigada   r · up 2h0m"
	needsHead := "╭─ NEEDS YOU "
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
		{"height 3 clips the box", busyTopModel(), 3, []string{header, needsHead, entry}},
		{"height 4 drops the meters", busyTopModel(), 4, []string{header, needsHead, entry, "╰"}},
		{"height 5 keeps the meters", busyTopModel(), 5, []string{header, " ━", needsHead, entry, "╰"}},
		{"hard clip", queue, 2, []string{header, needsHead}},
		{"hard clip into the entries", queue, 4, []string{header, needsHead, "│ ! #1 q", "│ ! #2 q"}},
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

// TestRenderTopFitsSize: the busy fixture at 40x10 fits the height (a box
// takes its borders with its last row, so it may fall a row or two short)
// with the header first and NEEDS YOU kept, and no line is wider than 40.
func TestRenderTopFitsSize(t *testing.T) {
	lines := renderTop(busyTopModel(), 40, 10, topNow, term.New(logo.Caps{UTF8: true}))
	got := strings.Join(lines, "\n")
	if len(lines) > 10 || len(lines) < 8 {
		t.Errorf("got %d lines, want 8 to 10:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], " ▙ brigada   r · up 2h0m") || !strings.HasSuffix(lines[0], "15:04:09") {
		t.Errorf("the header is not first, or its clock is off the edge: %q", lines[0])
	}
	if want := cutLine(" 3/8 slots  1 run  2 queue  1 held  1 needs", 40); lines[1] != want {
		t.Errorf("meters row = %q, want %q", lines[1], want)
	}
	for _, w := range []string{"╭─ NEEDS YOU 1 ", "│ ! #2 should the sprocket be blue?", "╭─ PLANS ", "│ #1 fix the frobnicator     ■!◆□  1/4 │"} {
		if !strings.Contains(got, w) {
			t.Errorf("the frame lacks %q:\n%s", w, got)
		}
	}
	for _, l := range lines {
		if n := term.CellWidth(l); n > 40 {
			t.Errorf("line %q is %d wide", l, n)
		}
		if (strings.HasPrefix(l, "│") || strings.HasPrefix(l, "╭") || strings.HasPrefix(l, "╰")) && term.CellWidth(l) != 40 {
			t.Errorf("box line %q is not exactly 40 wide", l)
		}
	}
}

// TestRenderTopHeightYieldOrder: lines beyond the height yield class by
// class, each from the bottom of the view upward, one line at a time:
// event row 2, the blank lines between sections, the
// held and parked lines, the running line, then the class of every other
// line — event row 1 with the EVENTS head after it, the agent rows
// bottom-up with the AGENTS head after the last, the plan's own line and
// the PLANS head, the meters strip last — and last the hard clip into
// NEEDS YOU. A head drops right after its section's last line, so every
// height here is met exactly. (Finished plans, queued lines and idle
// agents are in TestRenderTopFinishedFold, TestRenderTopQueuedYield and
// TestTopAgentsBudget.)
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
	tests := []struct {
		name   string
		height int
		lines  int // the frame's lines: a box's borders go with its last row, so a drop may undershoot
		want   []string
		lacks  []string
	}{
		{"everything fits", 20, 20, []string{
			"#1 fix the frobnicator",
			"╭─ AGENTS  all ≤ global 8 ─",
			"↺ codex/g...  □□□□□□□□  until 16:04  ✗ claude/...  ■■□□□□□□  login ex...",
			"╭─ EVENTS ─",
			"15:02  review      approve  task/3-1-oil-the-gears-after-the-frobnica...",
		}, nil},
		{"event row 2", 19, 19, []string{
			"╭─ EVENTS ─",
			"15:02  task    #3  done     oil the gears",
		}, []string{"review "}},
		{"the held line", 18, 18, []string{
			"  ! #2 wire the sprocket should the sprocket be blue?",
			"  ▸ #3  oil the gears",
		}, []string{"? #4  polish the knobs  held"}},
		{"the parked line", 17, 17, []string{
			"  ▸ #3  oil the gears",
		}, []string{"? #4  polish the knobs", "wire the sprocket should"}},
		{"the running line", 16, 16, []string{
			"#1 fix the frobnicator",
			"╭─ EVENTS ─",
			"15:02  task    #3  done     oil the gears",
		}, []string{"▸ #3  oil the gears  "}},
		{"event row 1, its box goes with it", 15, 13, []string{
			"#1 fix the frobnicator",
			"╭─ AGENTS  all ≤ global 8 ─",
		}, []string{"EVENTS", "task    #3  done"}},
		// The agents list broken and cooling first, then running, so the
		// running agent's row is the last and the first to go.
		{"the running agent's row", 12, 12, []string{
			"until 16:04  ✗ claude/...  ■■□□□□□□  login ex...",
		}, []string{"● claude/..."}},
		{"the broken and cooling row, its box goes with it", 11, 9, []string{
			"#1 fix the frobnicator",
		}, []string{"AGENTS", "codex/g...", "until 16:04"}},
		{"the plan line, its box goes with it", 8, 6, []string{
			"! claude/sonnet-long-n...  → brigada agent reset claude/sonnet-long-name",
		}, []string{"PLANS", "#1 fix the frobnicator"}},
		{"the slots row last", 5, 5, []string{
			"╭─ NEEDS YOU 2 ─",
			"! #2 should the sprocket be blue?",
			"! claude/sonnet-long-n...  → brigada agent reset claude/sonnet-long-name",
		}, []string{"slots"}},
		{"the hard clip", 4, 4, []string{
			"! #2 should the sprocket be blue?",
			"! claude/sonnet-long-n...  → brigada agent reset claude/sonnet-long-name",
		}, []string{"╰"}},
		{"the hard clip into the entries", 3, 3, []string{
			"! #2 should the sprocket be blue?",
		}, []string{"login expired"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := renderTop(model, 76, tt.height, topNow, term.New(logo.Caps{UTF8: true}))
			got := strings.Join(lines, "\n")
			if len(lines) != tt.lines {
				t.Fatalf("got %d lines, want %d:\n%s", len(lines), tt.lines, got)
			}
			if strings.Contains("\n"+got+"\n", "\n\n") {
				t.Errorf("a blank line:\n%s", got)
			}
			if tt.height >= 5 && strings.Count(got, "╭") != strings.Count(got, "╰") {
				t.Errorf("a box lost a border:\n%s", got)
			}
			if !strings.HasPrefix(lines[0], " ▙ brigada   r · up") {
				t.Errorf("first line is %q, want the header", lines[0])
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("view lacks %q:\n%s", w, got)
				}
			}
			for _, w := range tt.lacks {
				if strings.Contains(got, w) {
					t.Errorf("view keeps %q:\n%s", w, got)
				}
			}
			for _, l := range lines {
				if n := term.CellWidth(l); n > 76 {
					t.Errorf("line %q is %d wide", l, n)
				}
			}
		})
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
	const mark = " ▙ brigada   "
	for _, tt := range []struct {
		name        string
		projects    []engine.ProjectInfo
		projectsErr error
		repo        string
		want        string
	}{
		{"project flag", two, nil, repo, mark + "brigada · up 1h0m "},
		{"one project", one, nil, "", mark + "brigada · up 1h0m "},
		{"two projects", two, nil, "", mark + "2 projects · up 1h0m "},
		{"failed projects", nil, errors.New("projects down"), "", mark},
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
			if tt.projectsErr != nil && (!strings.Contains(got, "✗ engine: projects down") || strings.Contains(got, "ENGINE UNREACHABLE")) {
				t.Errorf("no error band:\n%s", got)
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
		{"two projects", two, "", "│ #101 brigada  Fix events strip" + strings.Repeat(" ", 96-30-3) + "0/0 │"},
		{"one project", one, "", "│ #101 Fix events strip" + strings.Repeat(" ", 96-21-3) + "0/0 │"},
		{"project flag", two, repo, "│ #101 Fix events strip" + strings.Repeat(" ", 96-21-3) + "0/0 │"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model, err := topFetch(context.Background(), &fakeTop{status: st, projects: tt.projects}, tt.repo)
			if err != nil {
				t.Fatal(err)
			}
			if got := plainTop(model, 100, 0); !strings.Contains(got, tt.want) {
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
	got := topPlanLine(live, "brigada", nil, newTopPlanLayout([]engine.StatusPlan{live}, nil, true, plain, 50), plain, 50)
	if !strings.HasPrefix(got, "#101 brigada  long") || !strings.HasSuffix(got, "... 0/0") {
		t.Errorf("line = %q, want the tag whole and the title cut", got)
	}
	if n := utf8.RuneCountInString(got); n != 50 {
		t.Errorf("line is %d cells, want 50: %q", n, got)
	}
	done := engine.StatusPlan{ID: 102, RepoURL: repo, Title: "shipped", State: "done"}
	if got := topPlanLine(done, "brigada", nil, newTopPlanLayout([]engine.StatusPlan{done}, nil, true, plain, 50), plain, 50); got != "#102 brigada  shipped"+strings.Repeat(" ", 50-21-4)+"done" {
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
	got := unboxTop(failed, 100, 0)
	if !strings.Contains(got, "engine: engine gone") || !strings.Contains(got, "claude/opus") || !strings.Contains(got, "15:02  task  #3  landed  oil the gears") {
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
	for _, l := range lines {
		l = strings.TrimPrefix(l, "│ ")
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
	if !strings.Contains(out.String(), "╭─ EVENTS ─") || !strings.Contains(unboxLines(lines), "15:02  task  #12  done\n15:01  task  #11  done\n") ||
		strings.Contains(out.String(), "#2 done") {
		t.Errorf("snapshot events are not the newest 10, newest first:\n%s", out.String())
	}
}

// TestRenderTopEventRowYield: a tall screen uses its spare rows for
// events, up to 10 and no more; rows 3 and up drop first, from the
// bottom, before row 2, row 2 before the report line, and the frame
// never exceeds the height.
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
		if len(lines) != n-k || topEventRowCount(lines) != 10-k || !strings.Contains(got, " All landed. ─╮") ||
			!strings.Contains(unboxLines(lines), "15:02  task  #14  done\n15:01  task  #13  done") {
			t.Errorf("at %d (rows 3+ drop one by one) got %d lines, %d rows:\n%s", n-k, len(lines), topEventRowCount(lines), got)
		}
		if k > 0 && strings.Contains(got, fmt.Sprintf("task  #%d ", 14-(10-k))) {
			t.Errorf("at %d the row #%d, the lowest, stays:\n%s", n-k, 14-(10-k), got)
		}
	}
	// Exactly room for 2 rows keeps both, with the report in the border.
	two := renderTop(model, 76, n-8, topNow, style)
	if topEventRowCount(two) != 2 || !strings.Contains(strings.Join(two, "\n"), "15:01  task  #13  done") {
		t.Errorf("room for 2 rows:\n%s", strings.Join(two, "\n"))
	}
	// One shorter drops row 2 alone.
	one := renderTop(model, 76, n-9, topNow, style)
	got := strings.Join(one, "\n")
	if len(one) != n-9 || topEventRowCount(one) != 1 || strings.Contains(got, "task  #13 ") || !strings.Contains(got, " All landed. ─╮") {
		t.Errorf("one line shorter should drop row 2 only:\n%s", got)
	}
	// Then the events are done: the held plan line goes before row 1 does,
	// and the report stays in the border with row 1.
	none := renderTop(model, 76, n-10, topNow, style)
	got = strings.Join(none, "\n")
	if len(none) != n-10 || !strings.Contains(got, " All landed. ─╮") || !strings.Contains(got, "15:02  task  #14  done") || strings.Contains(got, "? #4 polish") {
		t.Errorf("two lines shorter should drop the next class, not the event row:\n%s", got)
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
		{"busy", goldenBusyModel(), []string{"q quit  p pause", "╭─ NEEDS YOU 2 ", "╭─ PLANS  1 live · 0 finished ", "╭─ EVENTS ", " Plan 1 is half done; task 2 waits for your answer. ─╮"}},
		{"quiet", quietTopModel(), []string{"no events", "✓ nothing needs you"}},
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
	full := strings.Repeat("x", 20)
	const height = 3
	for _, tc := range []struct {
		name   string
		lines  []string
		breaks int
		short  bool
	}{
		{"short", []string{full, full}, 2, true},
		{"fills the height", []string{full, full, full}, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			screen := newTopScreen(&out)
			out.Reset()
			screen.draw(tc.lines, height)
			got := out.String()
			if !strings.HasPrefix(got, "\x1b[H") {
				t.Errorf("frame does not start with home: %q", got)
			}
			if strings.Contains(got, "\x1b[2J") {
				t.Errorf("frame clears by scrolling: %q", got)
			}
			if n := strings.Count(got, "\r\n"); n != tc.breaks {
				t.Errorf("frame has %d line breaks, want %d:\n%q", n, tc.breaks, got)
			}
			for _, erase := range []string{"\x1b[K", "\x1b[J"} {
				if strings.Contains(got, full+erase) {
					t.Errorf("%q follows a line's text: %q", erase, got)
				}
			}
			if tc.short {
				if !strings.HasSuffix(got, "\r\n\x1b[J") {
					t.Errorf("short frame does not end with a break and erase below: %q", got)
				}
			} else if !strings.HasSuffix(got, full) {
				t.Errorf("full frame does not end with the last line's text: %q", got)
			}
		})
	}
}

// TestTopEventsResizeRedrawsWithoutFetch: a resize draws and never
// fetches, running or paused.
func TestTopEventsResizeRedrawsWithoutFetch(t *testing.T) {
	keys := make(chan byte)
	ticks := make(chan time.Time)
	resize := make(chan os.Signal)
	refreshes, draws := 0, 0
	paused := false
	done := make(chan int)
	go func() {
		done <- topEvents(context.Background(), keys, ticks, resize,
			func() { refreshes++ }, func() { draws++ },
			func() { paused = !paused }, func() bool { return paused })
	}()
	resize <- syscall.SIGWINCH
	keys <- 'p' // pause: draws, no fetch
	resize <- syscall.SIGWINCH
	ticks <- time.Now() // paused tick: draws, no fetch
	keys <- 'q'
	if code := <-done; code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if refreshes != 0 {
		t.Errorf("%d fetches, want 0", refreshes)
	}
	if draws != 4 {
		t.Errorf("%d draws, want 4", draws)
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
		"3/8 slots",
		"1 run  2 queue  1 held  1 needs",
		"╭─ NEEDS YOU 1 ",
		"│ ! #2 should the sprocket be blue?",
		"╰",
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
		if n := term.CellWidth(l); n > 60 {
			t.Errorf("line %q is %d wide", l, n)
		}
	}
	tall := renderTop(busyTopModel(), 60, 40, topNow, term.New(logo.Caps{UTF8: true}))
	unbounded := renderTop(busyTopModel(), 60, 0, topNow, term.New(logo.Caps{UTF8: true}))
	if strings.Join(tall, "\n") != strings.Join(unbounded, "\n") {
		t.Errorf("tall screen trims:\n%s\nwant\n%s", strings.Join(tall, "\n"), strings.Join(unbounded, "\n"))
	}
}

// shortTopModel is busyTopModel with a header left short enough — 30
// columns — that the keys fit from width 49 up.
func shortTopModel() topModel {
	model := busyTopModel()
	model.status.StartedAt = topNow.UTC().Format(engine.TimeLayout)
	return model
}

// TestTopHeaderKeysFit: the keys show only when the left part, two
// spaces and the keys fit the width whole — a key is never cut
// mid-word — and otherwise a dim rule fills what remains of the width.
// busyTopModel's header left is 32 columns, so its keys fit only from
// width 51 up.
func TestTopHeaderKeysFit(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	tests := []struct {
		name  string
		model topModel
		width int
		keys  bool
	}{
		{"busy at 40 drops the keys", busyTopModel(), 40, false},
		{"busy at 50 drops the keys", busyTopModel(), 50, false},
		{"busy at 51 drops the keys", busyTopModel(), 51, false},
		{"busy at 52 shows the keys", busyTopModel(), 52, true},
		{"busy at 60 shows the keys", busyTopModel(), 60, true},
		{"short left at 49 drops the keys", shortTopModel(), 49, false},
		{"short left at 50 shows the keys", shortTopModel(), 50, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := topHeader(tt.model, topNow, style, tt.width)[0]
			if n := term.CellWidth(line); n != tt.width {
				t.Errorf("header is %d wide, want %d: %q", n, tt.width, line)
			}
			if tt.keys && !strings.HasSuffix(line, "15:04:09   q quit  p pause") {
				t.Errorf("header lacks the keys: %q", line)
			}
			if !tt.keys && (strings.Contains(line, "quit") || !strings.HasSuffix(line, "15:04:09")) {
				t.Errorf("header keeps the keys or the clock is off the edge: %q", line)
			}
		})
	}
	want := " ▙ brigada   r · up 0s" + strings.Repeat(" ", 19) + "15:04:09"
	if got := topHeader(shortTopModel(), topNow, style, 49)[0]; got != want {
		t.Errorf("keys-dropped header = %q, want %q", got, want)
	}
}

// TestTopEventsBorderStripsMarkdown: the foreman report's first line sits in
// the EVENTS border without its bold marks and backticks.
func TestTopEventsBorderStripsMarkdown(t *testing.T) {
	model := agentsModel()
	model.statusOK = true
	model.status.Report = &engine.StatusReport{JobID: 41, Text: "**Landed** `#5` and `#6`, **one** left.\nSecond `line`."}
	plain := term.New(logo.Caps{UTF8: true})
	for _, width := range []int{60, 80, 100} {
		got := strings.Join(renderTop(model, width, 0, topNow, plain), "\n")
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, "╭─ EVENTS") && (strings.Contains(l, "**") || strings.Contains(l, "`")) {
				t.Errorf("width %d: the EVENTS border shows markdown: %q", width, l)
			}
		}
		if width == 100 && !strings.Contains(got, "Landed #5 and #6, one left.") {
			t.Errorf("the EVENTS border lacks the plain report line:\n%s", got)
		}
	}
}

// TestTopWideCharactersFillBoxedRows: a plan title and a job title with CJK
// and emoji leave every boxed row exactly the frame's width in cells.
func TestTopWideCharactersFillBoxedRows(t *testing.T) {
	model := busyTopModel()
	model.status.Plans[0].Title = "日本語のとても長い計画の題名 🚀🚀 with a tail of words"
	for i := range model.status.Plans[0].Tasks {
		model.status.Plans[0].Tasks[i].Title = "歯車に油を差す 🛢 とても長いタスクの題名です"
	}
	model.status.Running[0].LastEvent = "tool_start 編集 🚀 ファイル"
	plain := term.New(logo.Caps{UTF8: true})
	for _, width := range []int{60, 80, 100} {
		lines := renderTop(model, width, 0, topNow, plain)
		wide := false
		for i, l := range lines {
			boxed := strings.HasPrefix(l, "│") || strings.HasPrefix(l, "╭") || strings.HasPrefix(l, "╰")
			if n := term.CellWidth(l); n > width || (boxed && n != width) {
				t.Errorf("width %d line %d %q is %d cells wide", width, i, l, n)
			}
			wide = wide || strings.Contains(l, "日本") || strings.Contains(l, "歯車")
		}
		if !wide {
			t.Errorf("width %d shows no CJK title:\n%s", width, strings.Join(lines, "\n"))
		}
	}
}

// TestRenderTopNarrowBoxes: below 60 columns the boxes stay, one column at
// every width, each border and row exactly the width, the lines beneath
// keep their indentation; and at every width there is no blank row between
// boxes.
func TestRenderTopNarrowBoxes(t *testing.T) {
	for _, width := range []int{40, 59, 60, 80, 100, 120} {
		lines := renderTop(busyTopModel(), width, 0, topNow, term.New(logo.Caps{UTF8: true}))
		got := strings.Join(lines, "\n")
		for _, head := range []string{"╭─ NEEDS YOU 1 ", "╭─ PLANS ", "╭─ AGENTS ", "╭─ EVENTS "} {
			if !strings.Contains(got, head) {
				t.Errorf("width %d lacks the box %q:\n%s", width, head, got)
			}
		}
		if strings.Contains(got, "\n\n") {
			t.Errorf("width %d has a blank row between boxes:\n%s", width, got)
		}
		for i, l := range lines {
			boxed := strings.HasPrefix(l, "│") || strings.HasPrefix(l, "╭") || strings.HasPrefix(l, "╰")
			if n := term.CellWidth(l); n > width || (boxed && n != width) {
				t.Errorf("width %d line %d %q is %d wide", width, i, l, n)
			}
		}
	}
	narrow := strings.Join(renderTop(busyTopModel(), 59, 0, topNow, term.New(logo.Caps{UTF8: true})), "\n")
	if !strings.Contains(narrow, "│   ▸ #3  oil") || !strings.Contains(narrow, "claude/sonnet@high  1m40s │") {
		t.Errorf("narrow view loses the indentation beneath a head:\n%s", narrow)
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

// TestTopAgentsOneColumn: under 72 inner columns the agents list one per
// row; the marks, the cells and the quota text read per state, and the
// styled text is the plain text with colour.
func TestTopAgentsOneColumn(t *testing.T) {
	model := agentsModel()
	plain := term.New(logo.Caps{UTF8: true})
	got := topAgents(model, topNow, plain, 71)
	if len(got) != 4 {
		t.Fatalf("section = %d lines, want head and 3 agents:\n%s", len(got), strings.Join(got, "\n"))
	}
	if want := "AGENTS"; got[0] != want {
		t.Errorf("head = %q", got[0])
	}
	cool := topNow.Add(time.Hour).In(topNow.Location()).Format("15:04")
	wants := []string{
		// Broken and cooling agents first, then the running one.
		"↺ " + fmt.Sprintf("%-23s", "codex/gpt-5") + "  □□□□□□□□  until " + cool,
		"✗ claude/sonnet-long-name  ■■□□□□□□  login expired",
		"● " + fmt.Sprintf("%-23s", "claude/opus") + "  ■□□□",
	}
	for i, w := range wants {
		if got[1+i] != w {
			t.Errorf("row %d = %q, want %q", i, got[1+i], w)
		}
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	styled := topAgents(model, topNow, colour, 71)
	if s := term.StripEscapes(strings.Join(styled, "\n")); s != strings.Join(got, "\n") {
		t.Errorf("styled text differs from plain:\n%s\nvs\n%s", s, strings.Join(got, "\n"))
	}
	if !strings.Contains(styled[1], colour.Warn("↺")) || !strings.Contains(styled[1], colour.Warn("until "+cool)) ||
		!strings.Contains(styled[2], colour.Bad("✗")) || !strings.Contains(styled[2], colour.Bad("login expired")) ||
		!strings.Contains(styled[3], colour.OK("●")) {
		t.Errorf("mark and quota styles wrong: %q", styled[1:4])
	}
}

// TestTopAgentsTwoColumns: from 72 inner columns the agents fill two columns
// of equal width row by row, the names padded to the widest listed name.
func TestTopAgentsTwoColumns(t *testing.T) {
	four := 4
	model := busyTopModel()
	model.capacity.Layers = nil
	cool := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	model.capacity.Agents = []engine.CapacityAgent{
		{Harness: "claude", Model: "sonnet", Cap: &four, CapLayer: "runner", Running: 1, Quota: "ready"},
		{Harness: "claude", Model: "opus", Cap: &four, CapLayer: "runner", Quota: "ready"},
		{Harness: "dsh", Model: "deepseek-flash", Cap: &four, CapLayer: "runner", Quota: "broken"},
		{Harness: "opencode", Model: "zai-coding-plan/glm-5.3-flash", Cap: &four, CapLayer: "runner", Quota: "cooling", CoolingUntil: cool},
		{Harness: "claude", Model: "haiku", Cap: &four, CapLayer: "runner", Quota: "ready"},
	}
	model.status.NeedsYou = []engine.NeedsYouItem{{Kind: "agent", State: "broken", Title: "dsh/deepseek-flash", Reason: "login expired"}}
	got := topAgents(model, topNow, term.New(logo.Caps{UTF8: true}), 90)
	clock := topNow.Add(time.Hour).In(topNow.Location()).Format("15:04")
	want := []string{
		"AGENTS",
		// Broken and cooling first, then running, then idle.
		"✗ dsh/deepseek-flash  □□□□  login expired     ↺ glm-5.3-flash       □□□□  until " + clock,
		"● claude/sonnet       ■□□□                    ● claude/opus         □□□□",
		"● claude/haiku        □□□□",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, line := range got[1:] {
		if i := strings.Index(line, "  ↺"); i >= 0 && utf8.RuneCountInString(line[:i]) != 44 {
			t.Errorf("the second column starts at %d, want 44: %q", utf8.RuneCountInString(line[:i]), line)
		}
	}
}

// TestTopAgentsCells: one cell per slot of the effective cap, filled up to
// the running count; an uncapped agent, a cap of 0 and a cap over 8 keep the
// cap's text.
func TestTopAgentsCells(t *testing.T) {
	two, nine, zero := 2, 9, 0
	plain := term.New(logo.Caps{UTF8: true})
	for _, tt := range []struct {
		name  string
		agent engine.CapacityAgent
		g     *int
		want  string
	}{
		{"idle", engine.CapacityAgent{Cap: &two, CapLayer: "runner"}, nil, "□□"},
		{"some", engine.CapacityAgent{Cap: &two, CapLayer: "runner", Running: 1}, nil, "■□"},
		{"full", engine.CapacityAgent{Cap: &two, CapLayer: "runner", Running: 2}, nil, "■■"},
		{"over the cap", engine.CapacityAgent{Cap: &two, CapLayer: "runner", Running: 3}, nil, "■■"},
		{"the global cap binds", engine.CapacityAgent{Cap: &nine, CapLayer: "runner", Running: 1}, &two, "■□"},
		{"uncapped", engine.CapacityAgent{CapLayer: "none", Running: 1}, nil, "1/none"},
		{"capped at 0", engine.CapacityAgent{Cap: &zero, CapLayer: "model", Running: 1}, nil, "1/0 (model)"},
		{"over 8", engine.CapacityAgent{Cap: &nine, CapLayer: "runner", Running: 1}, nil, "1/9 (runner)"},
	} {
		tt.agent.Harness, tt.agent.Model, tt.agent.Quota = "a", "b", "ready"
		cell := topAgentCell(tt.agent, tt.g, "a/b", 3, 60, "", topNow, plain)
		if want := "● a/b  " + tt.want; cell != want {
			t.Errorf("%s: cell = %q, want %q", tt.name, cell, want)
		}
	}
}

// TestTopAgentsBrokenFallbackAndCut: a broken agent with no needs-you
// entry reads "broken"; a reason is cut to its cell with "..."; a name is
// cut only when the cell cannot hold it.
func TestTopAgentsBrokenFallbackAndCut(t *testing.T) {
	model := agentsModel()
	model.status.NeedsYou = model.status.NeedsYou[:1]
	plain := term.New(logo.Caps{UTF8: true})
	got := topAgents(model, topNow, plain, 71)
	if !strings.HasSuffix(got[2], "✗ claude/sonnet-long-name  ■■□□□□□□  broken") {
		t.Errorf("fallback row = %q", got[2])
	}
	model = agentsModel()
	model.status.NeedsYou[len(model.status.NeedsYou)-1].Reason = strings.Repeat("login expired ", 6)
	cut := topAgents(model, topNow, plain, 60)
	if row := cut[2]; !strings.HasSuffix(row, "...") || utf8.RuneCountInString(row) != 60 {
		t.Errorf("a long reason is not cut to the cell: %q (%d)", row, utf8.RuneCountInString(row))
	}
	narrow := topAgents(model, topNow, plain, 20)
	for _, row := range narrow[1:] {
		if utf8.RuneCountInString(row) > 20 {
			t.Errorf("row %q is over the width", row)
		}
	}
}

// TestTopAgentsFamilyColour: a running agent's name takes its family colour,
// claude violet, glm blue, dsh mint, any other Dim; an idle agent's name is
// Dim; the same colours paint the agent column of a running job line, and
// none shows in plain style or at NO_COLOR.
func TestTopAgentsFamilyColour(t *testing.T) {
	four := 4
	model := busyTopModel()
	model.capacity.Layers = nil
	agent := func(harness, name string, running int) engine.CapacityAgent {
		return engine.CapacityAgent{Harness: harness, Model: name, Cap: &four, CapLayer: "runner", Running: running, Quota: "ready"}
	}
	model.capacity.Agents = []engine.CapacityAgent{
		agent("claude", "opus", 1), agent("opencode", "zai-coding-plan/glm-5.3", 1), agent("dsh", "flash", 1),
		agent("codex", "gpt-5", 1), agent("claude", "idle", 0),
	}
	colour := term.New(logo.Caps{Color: logo.Color256, UTF8: true})
	rows := strings.Join(topAgents(model, topNow, colour, 60), "\n")
	pad := func(name string) string { return fmt.Sprintf("%-11s", name) }
	for _, want := range []string{
		colour.Violet(pad("claude/opus")), colour.Blue(pad("glm-5.3")), colour.Mint(pad("dsh/flash")),
		colour.Dim(pad("codex/gpt-5")), colour.Dim(pad("claude/idle")),
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("no name painted %q in\n%s", want, rows)
		}
	}
	if strings.Contains(rows, colour.Violet(pad("claude/idle"))) {
		t.Errorf("an idle agent takes a family colour:\n%s", rows)
	}
	for name, fn := range map[string]func(string) string{
		"claude/opus": colour.Violet, "glm-5.3": colour.Blue, "dsh/flash": colour.Mint,
		"opencode/zai-coding-plan/glm-5.3@high": colour.Blue, "codex/gpt-5": colour.Dim, "claude": colour.Violet,
	} {
		if got := topFamily(colour, name)("x"); got != fn("x") {
			t.Errorf("topFamily(%q) paints %q, want %q", name, got, fn("x"))
		}
	}
	// The job line of a running job.
	jobs := strings.Join(renderTop(busyTopModel(), 100, 0, topNow, colour), "\n")
	if !strings.Contains(jobs, colour.Violet("claude/sonnet@high")) {
		t.Errorf("the job line's agent is not violet:\n%q", jobs)
	}
	for _, style := range []term.Style{term.New(logo.Caps{UTF8: true}), term.New(logo.Caps{})} {
		plain := strings.Join(renderTop(busyTopModel(), 100, 0, topNow, style), "\n")
		if strings.Contains(plain, "\x1b") {
			t.Errorf("a plain style paints:\n%q", plain)
		}
	}
	if s := normPlanGlyphs(term.StripEscapes(jobs)); s != normPlanGlyphs(strings.Join(renderTop(busyTopModel(), 100, 0, topNow, term.New(logo.Caps{UTF8: true})), "\n")) {
		t.Errorf("styled text differs from plain:\n%s", s)
	}
}

// TestTopAgentsWidths: no line is over the width, at 60, 80, 100 and 120
// columns, and a box line is exactly as wide.
func TestTopAgentsWidths(t *testing.T) {
	for _, width := range []int{60, 80, 100, 120} {
		for _, style := range []term.Style{term.New(logo.Caps{UTF8: true}), term.New(logo.Caps{Color: logo.Color256, UTF8: true})} {
			for _, l := range renderTop(agentsModel(), width, 0, topNow, style) {
				if n := term.CellWidth(l); n > width {
					t.Errorf("width %d: line %q is %d wide", width, l, n)
				}
			}
		}
	}
}

// TestTopAgentsASCII: without UTF-8 the AGENTS box has none of its own
// non-ASCII.
func TestTopAgentsASCII(t *testing.T) {
	ascii := term.New(logo.Caps{})
	lines := topAgentLines(agentsModel(), topNow, ascii, 90)
	var all []string
	for _, l := range lines {
		switch {
		case l.box != nil:
			all = append(all, topBoxTop(*l.box, ascii, 94))
		case l.foot:
			all = append(all, topBoxBottom(false, ascii, 94))
		default:
			all = append(all, topBoxRow(l.text, false, 0, ascii, 94))
		}
	}
	for _, l := range all {
		for _, r := range l {
			if r > 127 {
				t.Errorf("line %q has %q", l, r)
			}
		}
	}
	if got := strings.Join(all, "\n"); !strings.Contains(got, "*  claude/opus") || !strings.Contains(got, "all <= global 8") {
		t.Errorf("ASCII frame:\n%s", got)
	}
}

// TestTopAgentsBudget: class e of the drop order — idle agent rows leave
// the AGENTS section first, uncounted; the busy, cooling and broken agents
// stay. A row of two cells is idle only when both agents are.
func TestTopAgentsBudget(t *testing.T) {
	model := agentsModel()
	model.capacity.Agents = append(model.capacity.Agents,
		engine.CapacityAgent{Harness: "codex", Model: "a", CapLayer: "none", Quota: "ready"},
		engine.CapacityAgent{Harness: "codex", Model: "b", CapLayer: "none", Quota: "ready"},
		engine.CapacityAgent{Harness: "codex", Model: "c", CapLayer: "none", Quota: "ready"})
	model.events = nil
	model.status.Running, model.status.Held, model.status.Plans = nil, nil, nil
	style := term.New(logo.Caps{UTF8: true})
	full := renderTop(model, 76, 0, topNow, style)
	base := len(full)
	agentLines := func(lines []string) string {
		var rows []string
		in := false
		for _, l := range lines {
			switch {
			case strings.HasPrefix(l, "╭─ AGENTS"):
				in = true
			case strings.HasPrefix(l, "╰"):
				in = false
			case in:
				rows = append(rows, strings.TrimRight(strings.TrimSuffix(strings.TrimPrefix(l, "│ "), " │"), " "))
			}
		}
		return strings.Join(rows, "\n")
	}
	lines := renderTop(model, 76, base, topNow, style)
	if len(lines) != base || !strings.Contains(agentLines(lines), "codex/b") {
		t.Errorf("at %d the idle agents are not all kept:\n%s", base, strings.Join(lines, "\n"))
	}
	// The row of two idle agents goes, with nothing counting it; the row
	// that pairs the busy opus with an idle one stays.
	lines = renderTop(model, 76, base-1, topNow, style)
	got := agentLines(lines)
	if len(lines) != base-1 || strings.Contains(got, "codex/b") || strings.Contains(got, "codex/c") ||
		!strings.Contains(got, "■□□□") || !strings.Contains(got, "codex/a") || strings.Contains(got, "idle") {
		t.Errorf("at %d the AGENTS section is\n%s\nin\n%s", base-1, got, strings.Join(lines, "\n"))
	}
	// The cooling, the broken and the busy agent outlast the idle ones: the
	// events row, the lowest of the rest, goes next, its borders with it.
	lines = renderTop(model, 76, base-2, topNow, style)
	got = agentLines(lines)
	if len(lines) > base-2 || strings.Contains(strings.Join(lines, "\n"), "EVENTS") ||
		!strings.Contains(got, "until 16:04") || !strings.Contains(got, "login ex") || !strings.Contains(got, "■□□□") {
		t.Errorf("at %d:\n%s", base-2, strings.Join(lines, "\n"))
	}
}

// TestRenderTopFinishedFold: class b — finished plans fold into the dim
// +N finished line, bottom of the view first, and the first fold pays for
// the line it adds, so it gains nothing alone.
func TestRenderTopFinishedFold(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	model := busyTopModel()
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
	// One line less: the last finished plan's line goes, with no fold line.
	got := strings.Join(renderTop(model, 100, n-1, topNow, style), "\n")
	if strings.Contains(got, "old plan 8") || !strings.Contains(got, "#7 old plan 7") || strings.Contains(got, "finished\n") || len(strings.Split(got, "\n")) != n-1 {
		t.Errorf("at %d:\n%s", n-1, got)
	}
	got = strings.Join(renderTop(model, 100, n-2, topNow, style), "\n")
	if strings.Contains(got, "old plan") || strings.Contains(got, "+2 finished") || len(strings.Split(got, "\n")) != n-2 {
		t.Errorf("at %d:\n%s", n-2, got)
	}
	// The count lives in the PLANS note, hidden plans included.
	model.status.HiddenFinishedPlans = 3
	model.statusOK = true
	if got := plainTop(model, 100, 0); !strings.Contains(got, "╭─ PLANS  1 live · 5 finished ─") {
		t.Errorf("PLANS note lacks the count of listed and hidden finished plans:\n%s", got)
	}
	model.statusOK = false
	if got := plainTop(model, 100, 0); !strings.Contains(got, "╭─ PLANS ─") {
		t.Errorf("PLANS has a note before the first status:\n%s", got)
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
		engine.CapacityAgent{Harness: "codex", Model: "a", CapLayer: "none", Quota: "ready"},
		engine.CapacityAgent{Harness: "codex", Model: "b", CapLayer: "none", Quota: "ready"})
	full := strings.Join(renderTop(model, 100, 0, topNow, style), "\n")
	for _, w := range []string{"· #5  five", "· #6  six", "(waiting on #5)", "· #7  seven", "(review)", "codex/a", "codex/b"} {
		if !strings.Contains(full, w) {
			t.Fatalf("the full view lacks %q:\n%s", w, full)
		}
	}
	base := len(strings.Split(full, "\n")) - strings.Count(full+"\n", "\n\n")
	tests := []struct {
		height int
		lacks  []string
		keeps  []string
	}{
		{base, nil, []string{"(review)", "(waiting on #5)", "(queued)", "codex/a", "? #4  polish the knobs"}},
		{base - 1, []string{"(review)"}, []string{"(waiting on #5)", "(queued)", "codex/a"}},
		{base - 2, []string{"(review)", "(waiting on #5)"}, []string{"· #5  five", "(queued)", "codex/a"}},
		{base - 3, []string{"(review)", "(waiting on #5)", "(queued)"}, []string{"codex/a", "codex/b", "? #4  polish the knobs"}},
		// The row of the idle agent alone goes, uncounted; codex/a shares
		// the busy agent's row and stays.
		{base - 4, []string{"(queued)", "codex/b"}, []string{"codex/a", "? #4  polish the knobs"}},
		{base - 5, []string{"(queued)", "codex/b", "? #4  polish the knobs"}, []string{"codex/a", "▸ #3  oil the gears"}},
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
			lines := topNeedsYou(s, tt.sole, term.New(logo.Caps{UTF8: true}), 120)
			if len(lines) != 2 || squashTail(lines[1]) != tt.want {
				t.Errorf("lines = %q, want the entry %q", lines, tt.want)
			}
		})
	}
}

// TestTopNeedsYouTailsAlign: the tails start in one column, after the
// widest reason; a narrow width cuts the reason and keeps the tail whole.
func TestTopNeedsYouTailsAlign(t *testing.T) {
	hold := engine.NeedsYouItem{Kind: "task", ID: 312, State: "done",
		Reason: "held by rule approval_tiers: heavy; release: brigada land approve 312; change the rule: brigada land approve --and-stop-asking 312"}
	plan := engine.NeedsYouItem{Kind: "plan", ID: 9, State: "pending_approval", Title: "Top polish"}
	plain := term.New(logo.Caps{UTF8: true})
	st := api.Status{NeedsYou: []engine.NeedsYouItem{hold, plan}}
	got := topNeedsYou(st, false, plain, 100)
	// The widest tail ends at the inner right edge and the others start in
	// its column.
	want := []string{
		"! #312 held by rule approval_tiers: heavy" + strings.Repeat(" ", 100-26-len("! #312 held by rule approval_tiers: heavy")) + "→ brigada land approve 312",
		"! #9 Top polish awaiting approval" + strings.Repeat(" ", 100-26-len("! #9 Top polish awaiting approval")) + "→ brigada plan approve 9",
	}
	if got[1] != want[0] || got[2] != want[1] {
		t.Errorf("lines =\n%s\n%s\nwant\n%s\n%s", got[1], got[2], want[0], want[1])
	}
	if a, b := strings.Index(got[1], "→"), strings.Index(got[2], "→"); a != b {
		t.Errorf("tails start at byte %d and %d, want one column", a, b)
	}

	narrow := topNeedsYou(api.Status{NeedsYou: []engine.NeedsYouItem{hold}}, false, plain, 60)[1]
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

// TestTopReportLine: the foreman's last report shows as one dim line above
// the EVENTS head, only when there is one, and yields right after the
// list's second row.
func TestTopReportLine(t *testing.T) {
	model := agentsModel()
	model.events = []history.Item{
		{Type: history.TypeEvent, ID: 11, Kind: "review_verdict", ProjectID: 1, JobID: 5,
			Payload:   []byte(`{"status":"approve","branch":"task/3-1-oil-the-gears-after-the-frobnicator-fix"}`),
			CreatedAt: "2026-10-20T15:02:41.000000000Z"},
		model.events[1],
	}
	style := term.New(logo.Caps{UTF8: true})
	if got := strings.Join(renderTop(model, 76, 0, topNow, style), "\n"); strings.Contains(got, "report #") {
		t.Errorf("a status with no report shows a report line:\n%s", got)
	}
	model.status.Report = &engine.StatusReport{ProjectID: 1, JobID: 41, Text: "All landed, but #5 needs a look.\nSecond line."}
	full := renderTop(model, 76, 0, topNow, style)
	tail := full[len(full)-4:]
	want := []string{
		"╭─ EVENTS " + strings.Repeat("─", 30) + " All landed, but #5 needs a look. ─╮",
		topBoxRow("15:02  task    #3  done     oil the gears", false, topTintNone, style, 76),
		topBoxRow("15:02  review      approve  task/3-1-oil-the-gears-after-the-frobnica...", false, topTintNone, style, 76),
		"╰" + strings.Repeat("─", 74) + "╯",
	}
	if strings.Join(tail, "\n") != strings.Join(want, "\n") {
		t.Errorf("tail =\n%s\nwant\n%s", strings.Join(tail, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(strings.Join(full, "\n"), "report #") {
		t.Errorf("the report keeps its prefix:\n%s", strings.Join(full, "\n"))
	}
	// The report is Dim and cut to fit with cutLine.
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	top := topBoxTop(topBox{title: "EVENTS", right: "All landed, but #5 needs a look."}, colour, 23)
	if want := colour.Dim("All l..."); !strings.Contains(top, want) {
		t.Errorf("top border = %q, want the report Dim and cut: %q", top, want)
	}
	// The report is no row of the box, and event row 2 is the first to go.
	n := len(full)
	one := renderTop(model, 76, n-1, topNow, style)
	if got := strings.Join(one, "\n"); len(one) != n-1 || strings.Contains(got, "review approve") || !strings.Contains(got, "needs a look") {
		t.Errorf("one line less should drop event row 2 alone:\n%s", got)
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
			switch {
			case l.foot:
			case l.box != nil:
				out = append(out, l.box.title)
			default:
				out = append(out, l.text)
			}
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
		"FOREMAN",
		"  ▸ job #30  foreman",
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
	for _, l := range foreman[2 : len(foreman)-1] {
		if l.class != yieldHeld || l.section != topSecForeman {
			t.Errorf("note %q is class %d section %d, want held / FOREMAN", l.text, l.class, l.section)
		}
	}
	// The headless turn whose job runs is skipped; one message reads singular,
	// and a block shows with only a note.
	foreman, _ = topPlanless(api.Status{ForemanMessages: 1,
		Slots: []engine.Slot{{State: engine.SlotTurn, HolderID: 30, Label: "foreman turn", Since: stamp}}}, topNow, plain, 80)
	if got := texts(foreman); len(got) != 3 || got[1] != "  1 message for the foreman" {
		t.Errorf("FOREMAN block = %q", got)
	}
	foreman, _ = topPlanless(api.Status{Running: []engine.StatusJob{{ID: 30, Kind: "foreman"}},
		Slots: []engine.Slot{{State: engine.SlotTurn, HolderID: 30, Label: "foreman turn", Since: stamp}}}, topNow, plain, 80)
	if got := texts(foreman); len(got) != 2 {
		t.Errorf("the running turn's slot repeats its job line: %q", got)
	}
	// A pending runner restart sits in the JOBS block, only while something runs.
	_, jobs = topPlanless(api.Status{RunnerDraining: true,
		Running: []engine.StatusJob{{ID: 63, Kind: "plan_review"}}}, topNow, plain, 80)
	if got := texts(jobs); len(got) != 3 || got[2] != "  runner restarting after 1 running job" || jobs[2].class != yieldHeld || jobs[2].section != topSecJobs {
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
	if foreman[1].text != "  "+colour.Dim("1 message for the foreman") {
		t.Errorf("note = %q, want Dim", foreman[1].text)
	}
}

// TestTopMetersZeroCountersPlain: held and needs colour only above zero.
func TestTopMetersZeroCountersPlain(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	model := busyTopModel()
	model.status.Held, model.status.NeedsYou = nil, nil
	got := topMeters(model, style, 100)[0]
	want := " " + style.Accent(strings.Repeat("━", 6)) + style.Dim(strings.Repeat("━", 12)) + "  3/8 slots" + strings.Repeat(" ", 100-1-18-2-9-len("1 run  2 queue  0 held  0 needs")) +
		style.Bold("1 run") + "  2 queue  0 held  0 needs"
	if got != want {
		t.Errorf("meters = %q, want %q", got, want)
	}
	if plain := topMeters(model, term.New(logo.Caps{UTF8: true}), 100)[0]; plain != " ━━━━━━━━━━━━━━━━━━  3/8 slots"+strings.Repeat(" ", 100-1-18-2-9-31)+"1 run  2 queue  0 held  0 needs" {
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
	// One column, so a row is an agent.
	got := topAgents(model, topNow, term.New(logo.Caps{UTF8: true}), 60)
	var names []string
	for _, l := range got[1:] {
		names = append(names, strings.Fields(l)[1])
	}
	if want := "x/cool x/broke x/run x/offbusy x/idle1 x/idle2"; strings.Join(names, " ") != want {
		t.Errorf("order = %q, want %q", strings.Join(names, " "), want)
	}
	if note := topAgentLines(model, topNow, term.New(logo.Caps{UTF8: true}), 60)[0].box.note; note != "all ≤ global 8 · 1 off" {
		t.Errorf("note = %q, want the off count", note)
	}
	// The global cap makes every ready, idle agent off.
	model.capacity.Layers = []engine.CapacityLayer{{Layer: "global", Cap: &zero}}
	got = topAgents(model, topNow, term.New(logo.Caps{UTF8: true}), 60)
	if joined := strings.Join(got, "\n"); strings.Contains(joined, "x/idle1") || !strings.Contains(joined, "x/run") {
		t.Errorf("a global cap of 0 should fold the idle agents out:\n%s", joined)
	}
}

// agentNoteModel is a model with the given agents and layers, nothing else
// the AGENTS note reads.
func agentNoteModel(agents []engine.CapacityAgent, layers ...engine.CapacityLayer) topModel {
	var model topModel
	model.capacity = engine.CapacitySnapshot{Agents: agents, Layers: layers}
	return model
}

// TestTopAgentNote: the AGENTS border's note in each form: the global cap
// when it sits below some agent's own cap or an agent has none, the off
// count, both joined, `all ready` when nothing is broken, cooling, off or
// capped, and nothing otherwise; Dim, cut or left out when it does not fit.
func TestTopAgentNote(t *testing.T) {
	two, four, zero := 2, 4, 0
	cool := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	ready := engine.CapacityAgent{Harness: "a", Model: "b", CapLayer: "none", Quota: "ready"}
	capped := engine.CapacityAgent{Harness: "a", Model: "c", Cap: &four, CapLayer: "runner", Quota: "ready"}
	off := engine.CapacityAgent{Harness: "a", Model: "d", Cap: &zero, CapLayer: "model", Quota: "ready"}
	cooling := engine.CapacityAgent{Harness: "a", Model: "e", CapLayer: "none", Quota: "cooling", CoolingUntil: cool}
	global := func(n *int) engine.CapacityLayer { return engine.CapacityLayer{Layer: "global", Cap: n} }
	tests := []struct {
		name  string
		model topModel
		want  string
	}{
		{"global below an own cap", agentNoteModel([]engine.CapacityAgent{capped}, global(&two)), "all ≤ global 2"},
		{"global with an uncapped agent", agentNoteModel([]engine.CapacityAgent{capped, ready}, global(&four)), "all ≤ global 4"},
		{"global above every own cap", agentNoteModel([]engine.CapacityAgent{capped}, global(&four)), ""},
		{"global and off", agentNoteModel([]engine.CapacityAgent{ready, off}, global(&two)), "all ≤ global 2 · 1 off"},
		{"off alone", agentNoteModel([]engine.CapacityAgent{capped, off}), "1 off"},
		{"all ready", agentNoteModel([]engine.CapacityAgent{ready}, engine.CapacityLayer{Layer: "runner", Cap: nil}), "all ready"},
		{"cooling", agentNoteModel([]engine.CapacityAgent{ready, cooling}), ""},
		{"an own cap", agentNoteModel([]engine.CapacityAgent{capped}), ""},
		{"a capped layer", agentNoteModel([]engine.CapacityAgent{ready}, engine.CapacityLayer{Layer: "runner", Cap: &four}), ""},
		{"no agents", agentNoteModel(nil, global(&two)), ""},
	}
	plain := term.New(logo.Caps{UTF8: true})
	for _, tt := range tests {
		if got := topAgentLines(tt.model, topNow, plain, 90)[0].box.note; got != tt.want {
			t.Errorf("%s: note = %q, want %q", tt.name, got, tt.want)
		}
	}
	// In the border, Dim, and cut or left out when it does not fit.
	model := agentNoteModel([]engine.CapacityAgent{ready, off}, global(&two))
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	box := *topAgentLines(model, topNow, colour, 86)[0].box
	if got := topBoxTop(box, colour, 90); !strings.Contains(got, colour.Dim("all ≤ global 2 · 1 off")) || term.CellWidth(got) != 90 {
		t.Errorf("border = %q", got)
	}
	if got := topBoxTop(box, plain, 24); !strings.Contains(got, "all ≤ ") || term.CellWidth(got) != 24 {
		t.Errorf("a cut note: %q", got)
	}
	if got := topBoxTop(box, plain, 12); strings.Contains(got, "global") || term.CellWidth(got) != 12 {
		t.Errorf("a note that cannot fit stays out: %q", got)
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
	const first = "no plans yet. Give the foreman something to do:"
	const rows = first + "\n\n  $ brigada ask \"fix #12\"\n\nPlans and tasks appear here. What needs you shows above.\n"
	const repo = "https://example.invalid/r.git"
	projects := []engine.ProjectInfo{{ProjectID: 7, RepoURL: repo}}
	fetched, err := topFetch(context.Background(), &fakeTop{status: quietTopModel().status, projects: projects}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := unboxTop(fetched, 80, 0); !strings.Contains(got, "\n"+rows) || strings.Contains(got, "\nnone\n") || !strings.Contains(got, "\nno events yet\n") {
		t.Errorf("a fetched empty status lacks the first-run rows:\n%s", got)
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	lines := newTopPlans(fetched.status, false, true, topNow, colour, 80)
	if len(lines) != 7 || lines[1].text != first || lines[5].text != colour.Dim("Plans and tasks appear here. What needs you shows above.") {
		t.Errorf("first-run rows = %+v", lines)
	}
	for _, l := range lines[1:6] {
		if l.class != yieldRest {
			t.Errorf("first-run row %q yields in class %d, want h", l.text, l.class)
		}
	}
	// Plans hidden behind the finished ones are not a first run.
	hidden := fetched
	hidden.status.HiddenFinishedPlans = 2
	if got := unboxTop(hidden, 80, 0); !strings.Contains(got, "\nnone\n") || strings.Contains(got, "no plans yet") {
		t.Errorf("hidden finished plans read as a first run:\n%s", got)
	}
	// A failed status fetch has said nothing; the loop's kept status neither.
	failed, err := topFetch(context.Background(), &fakeTop{statusErr: errors.New("down"), projects: projects}, "")
	if err == nil || failed.statusOK {
		t.Fatalf("failed fetch: err %v, statusOK %t", err, failed.statusOK)
	}
	if got := unboxTop(failed, 80, 0); !strings.Contains(got, "\nnone\n") || strings.Contains(got, "no plans yet") {
		t.Errorf("a failed fetch reads as a first run:\n%s", got)
	}
	if got := unboxTop(topModel{}, 80, 0); !strings.Contains(got, "\nnone\n") {
		t.Errorf("a model never fetched lacks none:\n%s", got)
	}
	kept := failed.keepPrevious(fetched)
	if got := unboxTop(kept, 80, 0); !strings.Contains(got, "\n"+first+"\n") {
		t.Errorf("the kept status lost its first-run line:\n%s", got)
	}
}

// topFinishedModel is a fetched status whose only plans are finished.
func topFinishedModel() topModel {
	m := busyTopModel()
	m.statusOK = true
	m.status.Running, m.status.Held, m.status.NeedsYou = nil, nil, nil
	m.status.Plans = []engine.StatusPlan{
		{ID: 1, Title: "fix the frobnicator", State: "done"},
		{ID: 2, Title: "oil the gears", State: "cancelled"},
	}
	return m
}

// TestTopIdlePlans: with plans and none live the PLANS note reads `none live
// · N finished`, the finished plans show as their lines and a Dim idle row
// names the newest landing; with no landing kept the row is left out, and
// with only hidden finished plans the box keeps `none`. The idle row yields
// with the finished lines.
func TestTopIdlePlans(t *testing.T) {
	m := topFinishedModel()
	got := unboxTop(m, 80, 0)
	for _, w := range []string{"PLANS  none live · 2 finished", "#1 fix the frobnicator", "#2 oil the gears", "idle: last work landed 15:02\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("idle view lacks %q:\n%s", w, got)
		}
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	lines := topPlansBox(m.status, false, true, m.events, topNow, colour, 76)
	if last := lines[len(lines)-2]; last.text != colour.Dim("idle: last work landed 15:02") || last.class != yieldFinished {
		t.Errorf("idle row = %+v, want Dim in the finished class", last)
	}
	// The newest landing wins.
	m.events = append(m.events, history.Item{Type: history.TypeEvent, ID: 13, Kind: "task_landed", ProjectID: 1, TaskID: 4,
		Payload: []byte(`{"branch":"main","pushed":"pushed"}`), CreatedAt: "2026-10-20T15:03:00.000000000Z"})
	if got := unboxTop(m, 80, 0); !strings.Contains(got, "landed 15:03\n") {
		t.Errorf("idle row is not the newest landing:\n%s", got)
	}
	// No landing kept: no idle row.
	m.events = m.events[1:2]
	if got := unboxTop(m, 80, 0); strings.Contains(got, "idle:") || !strings.Contains(got, "none live · 2 finished") {
		t.Errorf("idle row without a landing:\n%s", got)
	}
	// Only hidden finished plans: no row, so the box keeps none.
	hidden := topFinishedModel()
	hidden.events = hidden.events[1:2]
	hidden.status.Plans, hidden.status.HiddenFinishedPlans = nil, 3
	if got := unboxTop(hidden, 80, 0); !strings.Contains(got, "none live · 3 finished\nnone\n") || strings.Contains(got, "no plans yet") {
		t.Errorf("hidden-only view:\n%s", got)
	}
	hidden = topFinishedModel()
	hidden.status.Plans, hidden.status.HiddenFinishedPlans = nil, 3
	if got := unboxTop(hidden, 80, 0); !strings.Contains(got, "none live · 3 finished\nidle: last work landed 15:02\n") {
		t.Errorf("hidden-only view with a landing:\n%s", got)
	}
}

// topErrorModel is a model whose last good fetch was 42s before topNow.
func topErrorModel(dial bool) topModel {
	m := busyTopModel()
	m.statusOK = true
	m.fetchedAt = topNow.Add(-42 * time.Second)
	m.dialFailed = dial
	m.err = errors.New("connection refused")
	return m
}

// bandOf is the line of a frame that holds marker.
func bandOf(t *testing.T, lines []string, marker string) string {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(l, marker) {
			return l
		}
	}
	t.Fatalf("no line holds %q:\n%s", marker, strings.Join(lines, "\n"))
	return ""
}

// TestTopErrorBand: a failed dial shows `✗ ENGINE UNREACHABLE` with the age of
// the data and `brigada start` at the right edge; a fetch error from a running
// engine shows `✗ engine: <text>` with no action; with no good fetch the age
// is `no data yet`. The middle part goes before the first is cut, ASCII says
// `x`, and the band reads the same in plain style.
func TestTopErrorBand(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	dial := renderTop(topErrorModel(true), 90, 0, topNow, plain)
	want := " ✗ ENGINE UNREACHABLE   retrying · data is 42s old" + strings.Repeat(" ", 90-1-20-3-26-13-1) + "brigada start "
	if got := bandOf(t, dial, "ENGINE"); got != want {
		t.Errorf("dial band = %q, want %q", got, want)
	}
	if strings.Contains(strings.Join(dial, "\n"), "engine: ") {
		t.Errorf("the old engine line shows:\n%s", strings.Join(dial, "\n"))
	}
	fetch := renderTop(topErrorModel(false), 90, 0, topNow, plain)
	want = " ✗ engine: connection refused   retrying · data is 42s old" + strings.Repeat(" ", 90-1-28-3-26)
	got := bandOf(t, fetch, "engine: ")
	if got != want || strings.Contains(strings.Join(fetch, "\n"), "brigada start") {
		t.Errorf("fetch band = %q, want %q and no action", got, want)
	}
	nodata := topErrorModel(true)
	nodata.fetchedAt = time.Time{}
	if got := bandOf(t, renderTop(nodata, 90, 0, topNow, plain), "ENGINE"); !strings.Contains(got, "retrying · no data yet") {
		t.Errorf("no-data band = %q", got)
	}
	// Tight: the middle part goes first, then the first part is cut.
	if got := bandOf(t, renderTop(topErrorModel(true), 50, 0, topNow, plain), "ENGINE"); got != " ✗ ENGINE UNREACHABLE"+strings.Repeat(" ", 50-1-20-13-1)+"brigada start " {
		t.Errorf("tight dial band = %q", got)
	}
	long := topErrorModel(false)
	long.err = errors.New(strings.Repeat("down ", 20))
	if got := bandOf(t, renderTop(long, 40, 0, topNow, plain), "engine: "); term.CellWidth(got) != 40 || !strings.Contains(got, "...") || strings.Contains(got, "retrying") {
		t.Errorf("cut band = %q", got)
	}
	ascii := renderTop(topErrorModel(true), 90, 0, topNow, term.New(logo.Caps{}))
	if got := bandOf(t, ascii, "ENGINE"); !strings.HasPrefix(got, " x ENGINE UNREACHABLE   retrying | data is 42s old") {
		t.Errorf("ASCII band = %q", got)
	}
}

// TestTopErrorDimsEverythingBelow: while the fetch fails every line under the
// band is the one Dim colour with no tint, NEEDS YOU's red and the family
// colours included; the header keeps its mark's colour and has a Bad clock
// and no uptime, and the band is solid Bad.
func TestTopErrorDimsEverythingBelow(t *testing.T) {
	m := topErrorModel(true)
	m.status.Running[0].Model = "sonnet"
	for _, caps := range []logo.Caps{
		{Color: logo.Color16, UTF8: true},
		{Color: logo.Color256, UTF8: true},
		{Color: logo.TrueColor, UTF8: true},
	} {
		style := term.New(caps)
		lines := renderTop(m, 100, 0, topNow, style)
		band := style.BandBad("")
		open := strings.TrimSuffix(band, "\x1b[0m")
		bandAt := -1
		for i, l := range lines {
			switch {
			case i == 0:
				if !strings.Contains(l, style.Bad("15:04:09")) || strings.Contains(l, "up ") || !strings.HasPrefix(l, " "+style.OneLine()) {
					t.Errorf("depth %d header = %q", caps.Color, l)
				}
			case strings.HasPrefix(l, open):
				bandAt = i
				if !strings.Contains(l, style.Bold("brigada start")) {
					t.Errorf("band = %q lacks the Bold action", l)
				}
			case l == "":
			default:
				if l != style.Dim(term.StripEscapes(l)) {
					t.Errorf("depth %d line %d = %q, want one Dim colour", caps.Color, i, l)
				}
			}
		}
		if bandAt < 0 {
			t.Errorf("depth %d: no band in the frame", caps.Color)
		}
	}
}

// TestTopPausedBand: a pause shows a solid Warn band with the pause time and
// the right-edge `p resumes`; the keys stay `q quit  p pause`, the sections
// keep their colours and tints, the job ages stop while the header clock and
// uptime run on; an error shows over a pause.
func TestTopPausedBand(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	m := busyTopModel()
	m.statusOK = true
	m.paused, m.pausedAt = true, topNow
	later := topNow.Add(30 * time.Second)
	lines := renderTop(m, 90, 0, later, plain)
	want := " || PAUSED   at 15:04:09 · ages frozen" + strings.Repeat(" ", 90-1-9-3-25-9-1) + "p resumes "
	if got := bandOf(t, lines, "PAUSED"); got != want {
		t.Errorf("paused band = %q, want %q", got, want)
	}
	got := strings.Join(lines, "\n")
	for _, w := range []string{"15:04:39   q quit  p pause", "up 2h0m", "1m40s"} {
		if !strings.Contains(got, w) {
			t.Errorf("paused view lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "paused; p resumes") || strings.Contains(got, "2m10s") {
		t.Errorf("paused view:\n%s", got)
	}
	m.paused, m.pausedAt = false, time.Time{}
	if got := strings.Join(renderTop(m, 90, 0, later, plain), "\n"); !strings.Contains(got, "2m10s") || strings.Contains(got, "PAUSED") {
		t.Errorf("running view:\n%s", got)
	}
	// Sections keep their colours and tints.
	m.paused, m.pausedAt = true, topNow
	style := term.New(logo.Caps{Color: logo.TrueColor, UTF8: true})
	lines = renderTop(m, 100, 0, topNow, style)
	if got := bandOf(t, lines, "PAUSED"); !strings.HasPrefix(got, "\x1b[48;2;255;215;95;38;2;17;17;17m") || !strings.Contains(got, style.Bold("p resumes")) {
		t.Errorf("paused band = %q", got)
	}
	if got := bandOf(t, lines, "should the sprocket"); !strings.Contains(got, "\x1b[48;2;59;20;24m") {
		t.Errorf("NEEDS YOU lost its tint: %q", got)
	}
	if got := lines[0]; strings.Contains(got, style.Bad("15:04:09")) || !strings.HasPrefix(got, " "+style.OneLine()) {
		t.Errorf("paused header = %q", got)
	}
	// Error over paused.
	m.err, m.dialFailed = errors.New("down"), true
	both := strings.Join(renderTop(m, 90, 0, topNow, plain), "\n")
	if !strings.Contains(both, "ENGINE UNREACHABLE") || strings.Contains(both, "PAUSED") {
		t.Errorf("error over paused:\n%s", both)
	}
}

// TestTopErrorSequence: a failed dial, then a good dial whose fetch fails,
// reads as a fetch error from a running engine — no `brigada start`, no
// ENGINE UNREACHABLE; a clean fetch clears the band and sets the good time; a
// failed fetch keeps the last good time and the pause carries across.
func TestTopErrorSequence(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	model := busyTopModel()
	model.statusOK = true
	model = afterFetch(topModel{}, model, nil, topNow.Add(-time.Minute))
	model = model.afterDial(errors.New("cannot reach the engine"))
	if !model.dialFailed || model.err == nil || model.status.Pid != 4242 {
		t.Fatalf("after a failed dial: %+v", model)
	}
	got := plainTop(model, 100, 0)
	if !strings.Contains(got, "ENGINE UNREACHABLE") || !strings.Contains(got, "data is 1m old") {
		t.Errorf("failed dial:\n%s", got)
	}
	model = model.afterDial(nil)
	model = afterFetch(model, topModel{}, errors.New("status: 500"), topNow)
	if model.dialFailed || !model.fetchedAt.Equal(topNow.Add(-time.Minute)) {
		t.Fatalf("after the failed fetch: dialFailed %t, fetchedAt %s", model.dialFailed, model.fetchedAt)
	}
	got = strings.Join(renderTop(model, 100, 0, topNow, plain), "\n")
	if !strings.Contains(got, "✗ engine: status: 500") || strings.Contains(got, "brigada start") || strings.Contains(got, "ENGINE UNREACHABLE") {
		t.Errorf("fetch error after a dial failure:\n%s", got)
	}
	if !strings.Contains(got, "data is 1m old") || !strings.Contains(got, "claude/opus") {
		t.Errorf("the age or the kept data is missing:\n%s", got)
	}
	fresh := busyTopModel()
	fresh.statusOK = true
	model = afterFetch(model, fresh, nil, topNow)
	if model.err != nil || !model.fetchedAt.Equal(topNow) || strings.Contains(plainTop(model, 100, 0), "retrying") {
		t.Errorf("after a clean fetch: err %v, fetchedAt %s", model.err, model.fetchedAt)
	}
	model = model.togglePause(topNow)
	if !model.paused || !model.pausedAt.Equal(topNow) {
		t.Fatalf("paused: %+v", model)
	}
	model = afterFetch(model, fresh, nil, topNow)
	if !model.paused || !model.pausedAt.Equal(topNow) {
		t.Errorf("the pause did not carry across the fetch")
	}
	if model = model.togglePause(topNow.Add(time.Second)); model.paused || !model.pausedAt.IsZero() {
		t.Errorf("resumed: %+v", model)
	}
}

// TestTopBandFramesFit: in each state (running, first run, idle, dial error,
// fetch error, paused, error over paused) at 12, 24 and 40 rows and 60, 80,
// 100 and 120 columns, no frame is taller or wider than the terminal; the
// styled frames read as the plain one; NO_COLOR has no escape; the ASCII frame
// has no Unicode; and the header and NEEDS YOU survive.
func TestTopBandFramesFit(t *testing.T) {
	states := map[string]topModel{}
	running := crowdedTopModel()
	running.statusOK = true
	states["running"] = running
	states["first run"] = topModel{statusOK: true, scope: "r", status: api.Status{StartedAt: busyTopModel().status.StartedAt}}
	states["idle"] = topFinishedModel()
	states["dial"] = topErrorModel(true)
	states["fetch"] = topErrorModel(false)
	paused := crowdedTopModel()
	paused.statusOK, paused.paused, paused.pausedAt = true, true, topNow
	states["paused"] = paused
	both := paused
	both.err, both.dialFailed, both.fetchedAt = errors.New("down"), true, topNow.Add(-5*time.Second)
	states["both"] = both
	plain := term.New(logo.Caps{UTF8: true})
	for name, m := range states {
		for _, height := range []int{12, 24, 40} {
			for _, width := range []int{60, 80, 100, 120} {
				base := renderTop(m, width, height, topNow, plain)
				if len(base) > height {
					t.Errorf("%s %dx%d: %d lines", name, width, height, len(base))
				}
				for _, l := range base {
					if term.CellWidth(l) > width {
						t.Errorf("%s %dx%d: line %q is wider", name, width, height, l)
					}
				}
				if !strings.Contains(base[0], "brigada") || !strings.Contains(strings.Join(base, "\n"), "NEEDS YOU") {
					t.Errorf("%s %dx%d: header or NEEDS YOU lost:\n%s", name, width, height, strings.Join(base, "\n"))
				}
				for _, caps := range []logo.Caps{
					{Color: logo.NoColor, UTF8: true},
					{Color: logo.Color16, UTF8: true},
					{Color: logo.Color256, UTF8: true},
					{Color: logo.TrueColor, UTF8: true},
				} {
					styled := renderTop(m, width, height, topNow, term.New(caps))
					if a, b := normPlanGlyphs(term.StripEscapes(strings.Join(styled, "\n"))), normPlanGlyphs(strings.Join(base, "\n")); a != b {
						t.Errorf("%s %dx%d depth %d: styled text differs:\n%s\nvs\n%s", name, width, height, caps.Color, a, b)
					}
					if caps.Color == logo.NoColor && strings.Contains(strings.Join(styled, "\n"), "\x1b") {
						t.Errorf("%s %dx%d: NO_COLOR frame has escapes", name, width, height)
					}
				}
				ascii := renderTop(m, width, height, topNow, term.New(logo.Caps{}))
				if len(ascii) > height {
					t.Errorf("%s %dx%d ASCII: %d lines", name, width, height, len(ascii))
				}
				for _, l := range ascii {
					if term.CellWidth(l) > width || len(l) != utf8.RuneCountInString(l) {
						t.Errorf("%s %dx%d ASCII line %q", name, width, height, l)
					}
				}
			}
		}
	}
}

// TestTopHintUnderHeader: the unregistered-models hint is a Dim line under
// the header, in the snapshot as in the loop, the same text in plain style.
func TestTopHintUnderHeader(t *testing.T) {
	const hint = "2 models your CLIs offer but no agent registers: brigada agents"
	model := busyTopModel()
	model.hint = hint
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	if lines := topHint(model, colour, 100); len(lines) != 1 || lines[0] != colour.Dim(hint) {
		t.Errorf("hint lines = %q, want the hint Dim", lines)
	}
	lines := renderTop(model, 100, 0, topNow, term.New(logo.Caps{UTF8: true}))
	if lines[2] != hint || !strings.HasPrefix(lines[1], " ━") || !strings.HasPrefix(lines[3], "╭─ NEEDS YOU") {
		t.Errorf("view opens %q, want the header, the meters, the hint and the first box", lines[:4])
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
	for _, width := range []int{80, 50} {
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
		" ==================  3/8 slots",
		"+- NEEDS YOU 3 ---",
		"-> brigada plan approve 2",
		"#1 fix the frobnicator",
		"#!>.=.  1/6 |",
		"# landed  = done  > running  . queued -+",
		"  > #3  oil the gears     work  claude/sonnet@high  1m40s  Edit",
		"  ^ #5  ship it           landing: queued",
		"  -> task #6 work: retry in 5m: dns",
		"+- AGENTS  all <= global 8 ---",
		"| -> codex/gpt-5  ........  until 16:04  x  claude/s...  ##......  login ex... |",
		"| *  claude/opus  #...",
		"+- EVENTS ---",
		" All landed. -+",
		"| 15:02  task  #3  landed  oil the gears",
		"+-----",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("ASCII view lacks %q:\n%s", w, got)
		}
	}
}

// TestRenderTopFinalOrder: header, meters, NEEDS YOU, PLANS, AGENTS and
// the events section in order, no blank line between them.
func TestRenderTopFinalOrder(t *testing.T) {
	got := plainTop(agentsModel(), 100, 0)
	last := -1
	for _, h := range []string{"▙ brigada", "slots", "╭─ NEEDS YOU", "╭─ PLANS", "╭─ AGENTS", "╭─ EVENTS", "15:02  task  #3  landed"} {
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
