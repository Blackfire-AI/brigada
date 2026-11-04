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

// TestRenderTopSections: the view's order — header, meters, NEEDS YOU,
// the FOREMAN block, the PLANS tree, AGENTS, then the events strip — with
// every section's text in its place.
func TestRenderTopSections(t *testing.T) {
	model := busyTopModel()
	model.status.Running = append(model.status.Running,
		engine.StatusJob{ID: 30, Kind: "foreman", Status: "running"})
	got := plainTop(model, 100, 0)
	want := []string{
		"brigada · r · up 2h0m · 15:04:09",
		"SLOTS ▓▓▓░░░░░ 3/8   2 run   2 queue   1 held   1 needs",
		"NEEDS YOU",
		"! #2 should the sprocket be blue?",
		"\n\nFOREMAN ─",
		"  ▸ job #30 foreman\n\nPLANS ─",
		"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		"  ▸ #3 oil the gears · work · claude/sonnet@high · 1m40s · Edit",
		"  ! #2 wire the sprocket should the sprocket be blue?",
		"  ? #4 polish the knobs  held: waiting for a runner slot",
		"AGENTS",
		"claude/opus",
		"all ≤ global 8",
		"EVENTS ─",
		"15:02  #3 done",
		"15:02  #3 landed",
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
	left := "brigada · r · up 2h0m · 15:04:09"
	rule := 80 - term.VisibleWidth(left) - term.VisibleWidth("q quit · p pause") - 2
	want := left + " " + strings.Repeat("─", rule) + " q quit · p pause"
	lines := topHeader(busyTopModel(), topNow, style, 80)
	if lines[0] != want {
		t.Errorf("header = %q, want %q", lines[0], want)
	}
	if got := term.VisibleWidth(lines[0]); got != 80 {
		t.Errorf("header is %d wide, want 80", got)
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	if got := topHeader(busyTopModel(), topNow, colour, 80)[0]; !strings.Contains(got, colour.Dim(strings.Repeat("─", rule))) {
		t.Errorf("rule is not dim:\n%q", got)
	}
	keys := "paused; p resumes, q quits"
	rule = 80 - term.VisibleWidth(left) - term.VisibleWidth(keys) - 2
	paused := busyTopModel()
	paused.paused = true
	if got := topHeader(paused, topNow, style, 80)[0]; got != left+" "+strings.Repeat("─", rule)+" "+keys {
		t.Errorf("paused header = %q", got)
	}
}

// TestTopMetersSlotsCell: the slots cell scales its bar to the cap —
// at most ten cells — never renders an all-empty bar for a sliver of
// use, and has no bar at all for a cap of zero, an uncapped layer or a
// failed fetch.
func TestTopMetersSlotsCell(t *testing.T) {
	two, five, twelve, zero := 2, 5, 12, 0
	tests := []struct {
		name   string
		layers []engine.CapacityLayer
		want   string
	}{
		{"cap 2", []engine.CapacityLayer{{Layer: "global", Cap: &two, Use: 1}}, "SLOTS ▓░ 1/2"},
		{"cap 5", []engine.CapacityLayer{{Layer: "global", Cap: &five, Use: 2}}, "SLOTS ▓▓░░░ 2/5"},
		{"cap above 10", []engine.CapacityLayer{{Layer: "global", Cap: &twelve, Use: 6}}, "SLOTS ▓▓▓▓▓░░░░░ 6/12"},
		{"at least one filled", []engine.CapacityLayer{{Layer: "global", Cap: &twelve, Use: 1}}, "SLOTS ▓░░░░░░░░░ 1/12"},
		{"cap 0 has no bar", []engine.CapacityLayer{{Layer: "global", Cap: &zero, Use: 2}}, "SLOTS 2/0"},
		{"uncapped has no bar", []engine.CapacityLayer{{Layer: "global", Use: 2}}, "SLOTS 2/none"},
		{"failed fetch", nil, "SLOTS ?/?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := busyTopModel()
			model.capacity.Layers = tt.layers
			got := topMeters(model, term.New(logo.Caps{UTF8: true}), 100)[0]
			if !strings.HasPrefix(got, tt.want+"   ") {
				t.Errorf("meters = %q, want the slots cell %q", got, tt.want)
			}
		})
	}
}

// TestTopMetersCountersAndColours: the counters follow the cell — run
// bold, queue plain, held warn, needs bad — three spaces apart.
func TestTopMetersCountersAndColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	got := topMeters(busyTopModel(), style, 100)[0]
	want := "SLOTS ▓▓▓░░░░░ 3/8   " + style.Bold("1 run") + "   2 queue   " + style.Warn("1 held") + "   " + style.Bad("1 needs")
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
			if len(lines) != 2 || lines[1] != tt.want {
				t.Errorf("lines = %q, want the entry %q", lines, tt.want)
			}
			if !strings.HasPrefix(lines[0], "NEEDS YOU ─") || term.VisibleWidth(lines[0]) != 120 {
				t.Errorf("header = %q, want a rule fill to the width", lines[0])
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
		"NEEDS YOU " + strings.Repeat("─", 90),
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
	if want := style.Bold("NEEDS YOU") + " " + style.Dim(strings.Repeat("─", 90)); lines[0] != want {
		t.Errorf("header = %q, want a bold title and a dim rule", lines[0])
	}
	if lines[1] != style.Bad("! #2 should the sprocket be blue?") || lines[2] != style.Bad("! dsh/large auth: no key  → brigada agent reset dsh/large") {
		t.Errorf("entries = %q", lines)
	}
	var four []engine.NeedsYouItem
	for i := 0; i < 4; i++ {
		four = append(four, engine.NeedsYouItem{Kind: "task", ID: int64(i + 1), Reason: "why"})
	}
	if got := topNeedsYou(api.Status{NeedsYou: four}, false, style, 100); got[4] != style.Dim("+1 more (brigada status)") {
		t.Errorf("cap line = %q, want dim +1 more (brigada status)", got[4])
	}
	if got := topNeedsYou(api.Status{}, false, style, 100); got[1] != style.Dim("nothing needs you") {
		t.Errorf("empty queue = %q, want dim nothing needs you", got)
	}
}

// TestTopNeedsYouEmptyQueue: the section stays visible when nothing
// needs the user.
func TestTopNeedsYouEmptyQueue(t *testing.T) {
	lines := topNeedsYou(api.Status{}, false, term.New(logo.Caps{UTF8: true}), 80)
	want := "NEEDS YOU " + strings.Repeat("─", 70) + "\nnothing needs you"
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

// TestTopEventsList: the EVENTS section is a head and one row per
// event, newest first, the time then two spaces then the words; at
// most 10 rows; a dim `no events` under the head when nothing happened.
func TestTopEventsList(t *testing.T) {
	items := busyTopModel().events
	style := term.New(logo.Caps{UTF8: true})
	head := "EVENTS " + strings.Repeat("─", 73)
	want := []string{head, "15:02  #3 done", "15:02  #3 landed"}
	if got := topEventTexts(items, topNow, style, 80); !reflect.DeepEqual(got, want) {
		t.Errorf("list = %q, want %q", got, want)
	}
	if got := topEventTexts(nil, topNow, style, 80); !reflect.DeepEqual(got, []string{head, "no events"}) {
		t.Errorf("empty = %q", got)
	}
	got := topEventTexts(topEventsFixture(14), topNow, style, 80)
	if len(got) != 11 || got[1] != "15:02  #14 done" || got[10] != "14:53  #5 done" {
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
	want := []string{"15:02        #2 done", "10-18 09:30  #1 done"}
	if got := topEventTexts(items, topNow, style, 80)[1:]; !reflect.DeepEqual(got, want) {
		t.Errorf("mixed days = %q, want %q", got, want)
	}
	// With ten newer rows the other-day event is not built and the column is 5 wide.
	many := append([]history.Item{items[0]}, topEventsFixture(10)...)
	if got := topEventTexts(many, topNow, style, 80); got[1] != "15:02  #10 done" || got[10] != "14:53  #1 done" {
		t.Errorf("dropped row shifted the column: %q", got)
	}
}

// TestTopEventsCut: a row of exactly the width stays whole and one
// column less cuts it with an ellipsis.
func TestTopEventsCut(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	items := busyTopModel().events
	if got := topEventTexts(items, topNow, style, 14)[1]; got != "15:02  #3 done" {
		t.Errorf("row of exactly the width = %q", got)
	}
	if got := topEventTexts(items, topNow, style, 13)[1]; got != "15:02  #3 ..." {
		t.Errorf("row one column too wide = %q", got)
	}
}

// TestTopEventWords: every allowlisted kind in its plain words, and the
// kinds and types the strip leaves out.
func TestTopEventWords(t *testing.T) {
	stamp := "2026-10-20T15:02:45.000000000Z"
	until := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	ev := func(kind string, task, job int64, payload string) history.Item {
		return history.Item{Type: history.TypeEvent, ID: 1, Kind: kind, ProjectID: 1, TaskID: task, JobID: job,
			Payload: []byte(payload), CreatedAt: stamp}
	}
	style := term.New(logo.Caps{UTF8: true})
	// The wording's own cases live in internal/history (TestEventWords);
	// top and history give the same words for one event, and top's row is
	// the time and those words.
	tests := []struct {
		name string
		item history.Item
		want string
	}{
		{"task_landed", ev("task_landed", 328, 0, `{"pushed":"waiting"}`), "15:02  #328 landed, push waits"},
		{"review approve", ev("review_verdict", 0, 9, `{"status":"approve","branch":"fix/x"}`), "15:02  review approve fix/x"},
		{"agent_cooling", ev("agent_cooling", 3, 5, `{"account":"work","until":"`+until+`"}`), "15:02  work cooling until 16:04"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topEventTexts([]history.Item{tt.item}, topNow, style, 200)[1]; got != tt.want {
				t.Errorf("row = %q, want %q", got, tt.want)
			}
			clock := func(stamp string) string { return api.LocalClock(stamp, topNow) }
			shared, _ := history.EventWords(tt.item, clock)
			if words, ok := topEventWords(tt.item, topNow); !ok || words != shared {
				t.Errorf("top words %q (%v), history words %q", words, ok, shared)
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
// leaves the previous tick's strip as it was, and two ticks that share
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
	if got := topEventTexts(merged, topNow, style, 200); len(got) != 3 || got[1] != "15:02  #3 done" || got[2] != "15:02  #3 landed" {
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

// TestTopClockMinutes: the events strip's pairs show HH:MM — the date
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
			if got := topEventTexts([]history.Item{tt.item}, topNow, style, 200)[1]; got != tt.want {
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
	got := plainTop(model, 200, 0)
	want := "NEEDS YOU " + strings.Repeat("─", 190) +
		"\n! #2 should the sprocket be blue?" +
		"\n! #8 held by rule gate_land: gate_land is ask  → brigada land approve 8" +
		"\n! #9 waits to push: task/9-1@abc123"
	tree := "#1 fix the frobnicator 3/6 ▓▓▓▓▓░░░░░░" +
		"\n  ▸ #3 oil the gears · work · claude/sonnet@high · 1m40s · Edit" +
		"\n  ▸ #9 push the widget · land · 1m40s" +
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
	got := plainTop(model, 200, 0)
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
	head := "PLANS " + strings.Repeat("─", 74)
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
			"#96 Rework brigada top 2/10 ▓▓░░░░░░░░░",
			"  ▸ #317 work · work · opencode/glm-5.3 · 14m0s · editing cmdtop.go",
			"  ▸ #313 landing · land · 14m0s",
			"  ! #314 finish rounds spent",
			"  ! #315 ask needs_user",
			"  · #316 audit (review)",
			"  · #318 tree (queued)",
			"  · #319 docs (waiting on tree, #317)",
			"  ? #320 fix  held: waiting for a runner slot",
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
			"#97 all done, not closed 2/2 ▓▓▓▓▓▓▓▓▓▓▓",
			"#83 Role instructions    2/2 ✓",
			"#80 Abandoned            ✗",
			"+41 finished",
		}},
		{"a live plan's review job under its plan", api.Status{
			Plans: []engine.StatusPlan{{ID: 12, Title: "Review me", State: "reviewing", ReviewRound: 2, ReviewJob: 50,
				Tasks: []engine.StatusTask{{ID: 1, Title: "first", State: "pending"}}}},
			Running: []engine.StatusJob{{ID: 50, Kind: "plan_review", Harness: "claude", Model: "opus", LeasedAt: leased}},
		}, []string{
			head,
			"#12 Review me reviewing round 2",
			"  ▸ job #50 plan_review · claude/opus · 14m0s",
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
		topSectionHead("PLANS", style, 80),
		style.Bold("#1") + " live 1/5 " + style.OK("▓▓") + style.Dim("░░░░░░░░░"),
		"  " + style.Accent("▸") + " #2 run · work",
		"  " + style.Dim("· #3 q (queued)"),
		"  " + style.Warn("? #4 held  held: waiting for a runner slot"),
		"  " + style.Bad("! #5 stuck needs_foreman"),
		"#2 done 1/1 " + style.OK("✓"),
		"#3 gone " + style.Bad("✗"),
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
		"#1 approve me awaiting approval",
		"#2 review me  reviewing round 3",
		"  · plan review queued",
		"#3 foreman    needs foreman",
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
	want := []string{
		"#101 brigada  Fix events strip reviewing round 2",
		"#102 brigada  Top polish       2/5 ▓▓▓▓░░░░░░░",
		"#9   brigada  Shipped          1/1 ✓",
	}
	if got := plain(80); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// At 40 columns the titles are cut to keep the widest tail whole, and
	// the tails still start in one column.
	narrow := plain(40)
	for _, l := range narrow {
		if n := term.VisibleWidth(l); n > 40 {
			t.Errorf("line %q is %d wide", l, n)
		}
	}
	col := -1
	for i, tail := range []string{"reviewing round 2", "2/5 ", "1/1 "} {
		at := utf8.RuneCountInString(narrow[i][:strings.LastIndex(narrow[i], tail)])
		if col >= 0 && at != col {
			t.Errorf("tail %q starts at column %d, want %d:\n%s", tail, at, col, strings.Join(narrow, "\n"))
		}
		col = at
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
		"#7 land me 2/5 ▓▓▓▓░░░░░░░",
		"  ⇡ #330 events wording  landing: waiting (retry at 11:10, last exit 1)",
		"  ⇡ #331 queued land  landing: queued",
		"  ⇡ #332 bare  landing: not landed",
	}
	if got := topPlansTree(st, topNow, term.New(logo.Caps{UTF8: true}), 80)[1:]; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := topLineClass(t, st, 80, want[1]); c != yieldHeld {
		t.Errorf("the ⇡ line is class %d, want the held class", c)
	}
	dim := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	if line := topLandingLine(tasks[2], topNow, dim, 80); line != "  "+dim.Dim("⇡ #332 bare  landing: not landed") {
		t.Errorf("the ⇡ line is not Dim: %q", line)
	}

	// A land job that speaks for the task shows as its own line, not the ⇡ one.
	st.Running = []engine.StatusJob{{ID: 9, TaskID: 332, Kind: "land"}}
	got := strings.Join(topPlanLines(st, 80), "\n")
	if !strings.Contains(got, "▸ #332 bare · land") || strings.Contains(got, "#332 bare  landing") {
		t.Errorf("a running land job and the ⇡ line both speak:\n%s", got)
	}

	// The bar reads complete only when the landing is; a class that never
	// lands and a finished plan count state done.
	last := func(state string, landed bool) string {
		p := engine.StatusPlan{ID: 8, Title: "p", State: state, Tasks: []engine.StatusTask{
			{ID: 1, State: "done", Class: "implement", Landed: true},
			{ID: 2, Title: "last", State: "done", Class: "implement", Landed: landed}}}
		return topPlanLine(p, "", newTopPlanLayout([]engine.StatusPlan{p}, false, term.New(logo.Caps{UTF8: true}), 80), term.New(logo.Caps{UTF8: true}), 80)
	}
	if got := last("active", false); got != "#8 p 1/2 ▓▓▓▓▓░░░░░░" {
		t.Errorf("the bar counts a task not landed: %q", got)
	}
	if got := last("active", true); got != "#8 p 2/2 ▓▓▓▓▓▓▓▓▓▓▓" {
		t.Errorf("the bar misses a landed task: %q", got)
	}
	if got := last("done", false); got != "#8 p 2/2 ✓" {
		t.Errorf("a finished plan = %q, want state done counted", got)
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
			"  ▸ #331 clip frame to height · work · claude/sonnet@high · 3m12s · Edit",
			"  ? #333 agent order  held: waiting for a free account slot",
		}},
		// 8 columns remain for the title: it is cut, the rest stays whole.
		{60, []string{
			"  ▸ #331 clip ... · work · claude/sonnet@high · 3m12s · Edit",
			"  ? #333 agent order  held: waiting for a free account slot",
		}},
	}
	for _, tt := range tests {
		got := topPlanLines(st, tt.width)
		if len(got) < 3 || got[1] != tt.want[0] || got[2] != tt.want[1] {
			t.Errorf("width %d lines =\n%s\nwant\n%s", tt.width, strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
		}
	}
	// Fewer than 4 columns for the title: it goes whole and the usual cut applies.
	got := topPlanLines(st, 55)[1]
	if !strings.HasPrefix(got, "  ▸ #331 clip frame to height") || !strings.HasSuffix(got, "...") || utf8.RuneCountInString(got) != 55 {
		t.Errorf("line = %q, want the title whole and the line cut to 55", got)
	}
	// A task with no title keeps today's line, byte for byte.
	st.Running = []engine.StatusJob{{ID: 42, TaskID: 334, Kind: "work", LeasedAt: leased}}
	st.Held = nil
	want := topRunningLine(st.Running[0], topNow, term.New(logo.Caps{UTF8: true}), 100)
	if got := topPlanLines(st, 100); got[1] != want {
		t.Errorf("untitled task line = %q, want %q", got[1], want)
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
	want := []string{"#96 Rework 8/8 ✓", "#97 Live   3/4 ▓▓▓▓▓▓▓▓░░░", "  · #6 go (queued)"}
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
			if strings.HasPrefix(l, "NEEDS YOU") {
				from = i
			}
			if strings.HasPrefix(l, "PLANS") {
				to = i
			}
		}
		if from < 0 || to < from {
			t.Fatalf("view lacks NEEDS YOU or PLANS:\n%s", strings.Join(lines, "\n"))
		}
		return lines[from : to+1]
	}
	head := func(title string) string { return topSectionHead(title, term.New(logo.Caps{UTF8: true}), 80) }
	fline := topRunningLine(foremanJob, topNow, term.New(logo.Caps{UTF8: true}), 80)
	oline := topRunningLine(otherJob, topNow, term.New(logo.Caps{UTF8: true}), 80)
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
		want := strings.Join([]string{head("NEEDS YOU"), needsLine, "", head("FOREMAN"), fline, "", head("PLANS")}, "\n")
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
			topHeldLine(m.status.Held[0], style, 80),
			topRetryLine(m.status.Retries[0], topNow, style, 80), "",
			head("JOBS"),
			topHeldLine(m.status.Held[1], style, 80),
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
		if !strings.Contains(got, "! #2 should the sprocket be blue?\n\nPLANS ─") {
			t.Errorf("NEEDS YOU is not followed by PLANS:\n%s", got)
		}
	})
	t.Run("narrow heads carry no rule", func(t *testing.T) {
		m := calm()
		m.status.Running = []engine.StatusJob{otherJob, foremanJob}
		got := between(59, m)
		for _, title := range []string{"FOREMAN", "JOBS"} {
			found := false
			for _, l := range got {
				if l == title {
					found = true
				}
				if strings.HasPrefix(l, title) && strings.Contains(l, "─") {
					t.Errorf("%s head has a rule: %q", title, l)
				}
			}
			if !found {
				t.Errorf("no bare %s head in\n%s", title, strings.Join(got, "\n"))
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
	got := plainTop(model, 100, 0)
	want := "! #2 should the sprocket be blue?" +
		"\n\nFOREMAN ─"
	rest := "\n  ▸ job #60 foreman · claude/opus" +
		"\n  ? job #61 foreman held: foreman turn waiting: no runner offers claude/opus" +
		"\n  ↺ job #62 foreman: retry in 2m: could not resolve github.com" +
		"\n\nJOBS ─"
	tail := "\n  ▸ job #63 plan_review" +
		"\n\nPLANS ─"
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
			if !strings.HasPrefix(lines[0], "brigada · r · up") || !strings.Contains(lines[1], "engine: connection refused") {
				t.Errorf("the header and its error line are not first:\n%s", got)
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
			if finished == 0 && shown == running && !strings.Contains(got, "+10 finished") {
				t.Errorf("the finished plans fold into no +10 finished line:\n%s", got)
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
	header := "brigada · r · up 2h0m · 15:04:09"
	needsHead := topSectionHead("NEEDS YOU", style, 80)
	entry := "! #2 should the sprocket be blue?"
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
		{"height 3 drops the meters", busyTopModel(), 3, []string{header, needsHead, entry}},
		{"height 4 keeps the meters", busyTopModel(), 4, []string{header, "SLOTS ", needsHead, entry}},
		{"hard clip", queue, 2, []string{header, needsHead}},
		{"hard clip into the entries", queue, 4, []string{header, needsHead, "! #1 q", "! #2 q"}},
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

// TestRenderTopFitsSize: the busy fixture at 40x10 is exactly 10 lines
// with the header first and NEEDS YOU kept, and no line is wider than 40.
func TestRenderTopFitsSize(t *testing.T) {
	lines := renderTop(busyTopModel(), 40, 10, topNow, term.New(logo.Caps{UTF8: true}))
	want := []string{
		"brigada · r · up 2h0m · 15:04:09 ───────",
		"SLOTS ▓▓▓░░░░░ 3/8   1 run   2 queue ...",
		"NEEDS YOU",
		"! #2 should the sprocket be blue?",
		"PLANS",
		"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		"AGENTS",
		"claude/opus  1/8 (global)  ✓",
		"all ≤ global 8",
		"EVENTS",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("frame =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if len(lines) != 10 {
		t.Errorf("got %d lines, want exactly 10", len(lines))
	}
	if !strings.HasPrefix(lines[0], "brigada · r · up") || !strings.Contains(strings.Join(lines, "\n"), "NEEDS YOU") {
		t.Errorf("the header is not first or NEEDS YOU is gone:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if n := term.VisibleWidth(l); n > 40 {
			t.Errorf("line %q is %d wide", l, n)
		}
	}
}

// TestRenderTopHeightYieldOrder: lines beyond the height yield class by
// class, each from the bottom of the view upward, one line at a time:
// event row 2, the blank lines between sections, the
// held and parked lines, the running line, then the class of every other
// line — event row 1 with the EVENTS head after it, the global line, the agent lines
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
		lines  int
		blanks int
		want   []string
		lacks  []string
	}{
		{"everything fits", 23, 23, 5, []string{
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
			"✗ login expired",
			"EVENTS ─",
			"15:02  review approve task/3-1-oil-the-gears-after-the-frobnicator-fix",
		}, nil},
		{"event row 2", 22, 22, 5, []string{
			"EVENTS ─",
			"15:02  #3 done",
		}, []string{"review approve"}},
		{"the lowest blank line", 21, 21, 4, []string{
			"all ≤ global 8\nEVENTS ─",
			"  ? #4 polish the knobs  held: waiting for a runner slot\n\nAGENTS",
		}, nil},
		{"no blank line", 17, 17, 0, []string{
			"all ≤ global 8\nEVENTS ─",
			"  ? #4 polish the knobs  held",
			"  ! #2 wire the sprocket should the sprocket be blue?",
		}, nil},
		{"the held line", 16, 16, 0, []string{
			"  ! #2 wire the sprocket should the sprocket be blue?",
			"  ▸ #3 oil the gears · work · claude/sonnet@high · 1m40s · Edit",
		}, []string{"? #4 polish the knobs  held"}},
		{"the parked line", 15, 15, 0, []string{
			"  ▸ #3 oil the gears · work · claude/sonnet@high · 1m40s · Edit",
		}, []string{"? #4 polish the knobs", "wire the sprocket should"}},
		{"the running line", 14, 14, 0, []string{
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
			"EVENTS ─────────────────────────────────────────────────────────────────────\n15:02  #3 done",
		}, []string{"▸ #3 oil the gears"}},
		{"event row 1, its head stays", 13, 13, 0, []string{
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
			"all ≤ global 8\nEVENTS ─",
		}, []string{"#3 done"}},
		{"the EVENTS head", 12, 12, 0, []string{
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
			"all ≤ global 8",
		}, []string{"EVENTS"}},
		{"the global line", 11, 11, 0, []string{
			"claude/opus              1/4 (runner)  ✓",
		}, []string{"all ≤ global 8"}},
		// The agents list broken and cooling first, then running, so the
		// running agent is the last line and the first to go.
		{"the running agent", 10, 10, 0, []string{
			"2/8 (global)  ✗ login expired",
		}, []string{"1/4 (runner)  ✓"}},
		{"the broken agent", 9, 9, 0, []string{
			"codex/gpt-5              0/8 (global)  ↺ 16:04",
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		}, []string{"2/8 (global)  ✗ login expired"}},
		{"the cooling agent, its head follows", 8, 8, 0, []string{
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░\nAGENTS ─",
		}, []string{"codex/gpt-5", "↺ 16:04"}},
		{"the AGENTS head", 7, 7, 0, []string{
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		}, []string{"AGENTS"}},
		{"the plan line, its head follows", 6, 6, 0, []string{
			"! claude/sonnet-long-name ...  → brigada agent reset claude/sonnet-long-name\nPLANS ─",
		}, []string{"#1 fix the frobnicator"}},
		{"the PLANS head", 5, 5, 0, []string{
			"SLOTS ▓▓▓░░░░░ 3/8",
			"! #2 should the sprocket be blue?",
			"! claude/sonnet-long-name ...  → brigada agent reset claude/sonnet-long-name",
		}, []string{"PLANS"}},
		{"the meters last", 4, 4, 0, []string{
			"! #2 should the sprocket be blue?",
			"! claude/sonnet-long-name ...  → brigada agent reset claude/sonnet-long-name",
		}, []string{"SLOTS"}},
		{"the hard clip", 3, 3, 0, []string{
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
			if blanks := strings.Count("\n"+got+"\n", "\n\n"); blanks != tt.blanks {
				t.Errorf("got %d blank lines, want %d:\n%s", blanks, tt.blanks, got)
			}
			if !strings.HasPrefix(lines[0], "brigada · r · up") {
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
				if n := term.VisibleWidth(l); n > 76 {
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
	up := "up 1h0m · 15:04:09"
	for _, tt := range []struct {
		name        string
		projects    []engine.ProjectInfo
		projectsErr error
		repo        string
		want        string
	}{
		{"project flag", two, nil, repo, "brigada · brigada · " + up + " "},
		{"one project", one, nil, "", "brigada · brigada · " + up + " "},
		{"two projects", two, nil, "", "brigada · 2 projects · " + up + " "},
		{"failed projects", nil, errors.New("projects down"), "", "brigada · " + up + " "},
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
		{"two projects", two, "", "#101 brigada  Fix events strip 0/0"},
		{"one project", one, "", "#101 Fix events strip 0/0"},
		{"project flag", two, repo, "#101 Fix events strip 0/0"},
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
	got := topPlanLine(live, "brigada", newTopPlanLayout([]engine.StatusPlan{live}, true, plain, 50), plain, 50)
	if !strings.HasPrefix(got, "#101 brigada  long") || !strings.Contains(got, "... 0/0 ") {
		t.Errorf("line = %q, want the tag whole and the title cut", got)
	}
	if n := utf8.RuneCountInString(got); n != 50 {
		t.Errorf("line is %d cells, want 50: %q", n, got)
	}
	done := engine.StatusPlan{ID: 102, RepoURL: repo, Title: "shipped", State: "done"}
	if got := topPlanLine(done, "brigada", newTopPlanLayout([]engine.StatusPlan{done}, true, plain, 50), plain, 50); got != "#102 brigada  shipped 0/0 ✓" {
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
	for _, want := range []string{"! #3 own task entry", "! claude/opus login expired", "own project plan", "✗ login expired", "2 needs"} {
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
	for _, want := range []string{"! #1 own task entry", "! #2 own task entry", "! claude/opus login expired", "+2 more", "✗ login expired"} {
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
	got := plainTop(failed, 100, 0)
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
	if !strings.HasPrefix(out.String(), "brigada · r · up ") {
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
	if !strings.Contains(out.String(), "EVENTS ─") || !strings.Contains(out.String(), "15:02  #12 done\n15:01  #11 done\n") ||
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
		if len(lines) != n-k || topEventRowCount(lines) != 10-k || !strings.Contains(got, "report #41: All landed.") ||
			!strings.Contains(got, "15:02  #14 done\n15:01  #13 done") {
			t.Errorf("at %d (rows 3+ drop one by one) got %d lines, %d rows:\n%s", n-k, len(lines), topEventRowCount(lines), got)
		}
		if k > 0 && strings.Contains(got, fmt.Sprintf("#%d done", 14-(10-k))) {
			t.Errorf("at %d the row #%d, the lowest, stays:\n%s", n-k, 14-(10-k), got)
		}
	}
	// Exactly room for 2 rows keeps both, with the report line.
	two := renderTop(model, 76, n-8, topNow, style)
	if topEventRowCount(two) != 2 || !strings.Contains(strings.Join(two, "\n"), "15:01  #13 done") {
		t.Errorf("room for 2 rows:\n%s", strings.Join(two, "\n"))
	}
	// One shorter drops row 2 before the report line.
	one := renderTop(model, 76, n-9, topNow, style)
	got := strings.Join(one, "\n")
	if len(one) != n-9 || topEventRowCount(one) != 1 || strings.Contains(got, "#13 done") || !strings.Contains(got, "report #41") {
		t.Errorf("one line shorter should drop row 2 only:\n%s", got)
	}
	// Then the report line goes, with row 1 and the head still shown.
	none := renderTop(model, 76, n-10, topNow, style)
	got = strings.Join(none, "\n")
	if len(none) != n-10 || strings.Contains(got, "report #41") || !strings.Contains(got, "EVENTS ─") || !strings.Contains(got, "15:02  #14 done") {
		t.Errorf("two lines shorter should drop the report line:\n%s", got)
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
		{"busy", goldenBusyModel(), []string{"q quit · p pause", "report #41: "}},
		{"quiet", quietTopModel(), []string{"no events", "nothing needs you"}},
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
	if !strings.Contains(lines[0], "brigada · r · up") {
		t.Errorf("first line is %q, want the header", lines[0])
	}
	got := strings.Join(lines, "\n")
	for _, w := range []string{
		"SLOTS ▓▓▓░░░░░ 3/8   1 run   2 queue   1 held   1 needs",
		"NEEDS YOU",
		"! #2 should the sprocket be blue?",
		"PLANS",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("frame lacks %q:\n%s", w, got)
		}
	}
	for _, w := range []string{"#1 fix the frobnicator", "all ≤ global 8", "#3 done"} {
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
		{"busy at 51 shows the keys", busyTopModel(), 51, true},
		{"busy at 60 shows the keys", busyTopModel(), 60, true},
		{"short left at 48 drops the keys", shortTopModel(), 48, false},
		{"short left at 49 shows the keys", shortTopModel(), 49, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := topHeader(tt.model, topNow, style, tt.width)[0]
			if n := term.VisibleWidth(line); n != tt.width {
				t.Errorf("header is %d wide, want %d: %q", n, tt.width, line)
			}
			if tt.keys && !strings.HasSuffix(line, "q quit · p pause") {
				t.Errorf("header lacks the keys: %q", line)
			}
			if !tt.keys && strings.Contains(line, "quit") {
				t.Errorf("header keeps the keys: %q", line)
			}
		})
	}
	want := "brigada · r · up 0s · 15:04:09 " + strings.Repeat("─", 17)
	if got := topHeader(shortTopModel(), topNow, style, 48)[0]; got != want {
		t.Errorf("keys-dropped header = %q, want %q", got, want)
	}
}

// TestRenderTopNarrowSectionHeads: below 60 columns the section heads
// drop their rule fills and keep their titles, the lines beneath keep
// their indentation, and at 60 the fills return.
func TestRenderTopNarrowSectionHeads(t *testing.T) {
	narrow := strings.Join(renderTop(busyTopModel(), 59, 0, topNow, term.New(logo.Caps{UTF8: true})), "\n")
	for _, head := range []string{"NEEDS YOU", "PLANS", "AGENTS"} {
		if !strings.Contains(narrow, "\n"+head+"\n") {
			t.Errorf("narrow view lacks the bare head %q:\n%s", head, narrow)
		}
	}
	// At 59 columns the title gives way and the agent, age and event stay whole.
	if !strings.Contains(narrow, "  ▸ #3 oil th... · work · claude/sonnet@high · 1m40s · Edit") {
		t.Errorf("narrow view loses the indentation beneath a head:\n%s", narrow)
	}
	wide := strings.Join(renderTop(busyTopModel(), 60, 0, topNow, term.New(logo.Caps{UTF8: true})), "\n")
	for _, head := range []string{"NEEDS YOU", "PLANS", "AGENTS"} {
		want := head + " " + strings.Repeat("─", 60-len(head)-1)
		if !strings.Contains(wide, want) {
			t.Errorf("wide view lacks the filled head %q:\n%s", want, wide)
		}
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
	if len(got) != 5 {
		t.Fatalf("section = %d lines, want head, 3 agents, global:\n%s", len(got), strings.Join(got, "\n"))
	}
	if want := "AGENTS " + strings.Repeat("─", 100-7); got[0] != want {
		t.Errorf("head = %q", got[0])
	}
	eight := 8
	a := model.capacity.Agents
	cool := topNow.Add(time.Hour).In(topNow.Location()).Format("15:04")
	wants := []string{
		// Broken and cooling agents first, then the running one.
		"codex/gpt-5              " + api.RunningCap(a[1], &eight) + "  ↺ " + cool,
		"claude/sonnet-long-name  " + api.RunningCap(a[2], &eight) + "  ✗ login expired",
		"claude/opus              " + api.RunningCap(a[0], &eight) + "  ✓",
	}
	for i, w := range wants {
		if got[1+i] != w {
			t.Errorf("line %d = %q, want %q", i, got[1+i], w)
		}
	}
	if got[4] != "all ≤ global 8" {
		t.Errorf("global line = %q", got[4])
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	styled := topAgents(model, topNow, colour, 100)
	if s := term.StripEscapes(strings.Join(styled, "\n")); s != strings.Join(got, "\n") {
		t.Errorf("styled text differs from plain:\n%s\nvs\n%s", s, strings.Join(got, "\n"))
	}
	if !strings.Contains(styled[3], colour.OK("✓")) || !strings.Contains(styled[1], colour.Warn("↺ "+cool)) ||
		!strings.Contains(styled[2], colour.Bad("✗ login expired")) {
		t.Errorf("quota styles wrong: %q", styled[1:4])
	}
}

// TestTopAgentsBrokenFallbackAndCut: a broken agent with no needs-you
// entry reads "broken"; a name longer than the width is cut with "...".
func TestTopAgentsBrokenFallbackAndCut(t *testing.T) {
	model := agentsModel()
	model.status.NeedsYou = model.status.NeedsYou[:1]
	plain := term.New(logo.Caps{UTF8: true})
	got := topAgents(model, topNow, plain, 100)
	if !strings.HasSuffix(got[2], "✗ broken") {
		t.Errorf("fallback line = %q", got[2])
	}
	cut := topAgents(model, topNow, plain, 12)
	if !strings.HasPrefix(cut[2], "claude/so...") {
		t.Errorf("long name not cut: %q", cut)
	}
}

// TestTopAgentsGlobalLineOmitted: no global cap, no `all ≤` line.
func TestTopAgentsGlobalLineOmitted(t *testing.T) {
	model := agentsModel()
	model.capacity.Layers = nil
	got := topAgents(model, topNow, term.New(logo.Caps{UTF8: true}), 100)
	if len(got) != 4 || strings.Contains(strings.Join(got, "\n"), "all ≤") {
		t.Errorf("section = %q", got)
	}
}

// TestTopAgentsBudget: class e of the drop order — idle agents leave the
// AGENTS section first and a dim +N idle (brigada capacity) line counts
// them, the global line under it; the busy, cooling and broken agents
// stay.
func TestTopAgentsBudget(t *testing.T) {
	model := agentsModel()
	model.capacity.Agents = append(model.capacity.Agents,
		engine.CapacityAgent{Harness: "codex", Model: "a", CapLayer: "none", Quota: "ready"},
		engine.CapacityAgent{Harness: "codex", Model: "b", CapLayer: "none", Quota: "ready"})
	model.events = nil
	model.status.Running, model.status.Held, model.status.Plans = nil, nil, nil
	style := term.New(logo.Caps{UTF8: true})
	full := renderTop(model, 76, 0, topNow, style)
	blanks := strings.Count(strings.Join(full, "\n")+"\n", "\n\n")
	// Without the blank lines, the next line to go is the last idle agent.
	base := len(full) - blanks
	agentLines := func(lines []string) string {
		from := -1
		for i, l := range lines {
			if strings.HasPrefix(l, "AGENTS") {
				from = i
			}
		}
		if from < 0 {
			return ""
		}
		rest := lines[from+1:]
		if n := len(rest); n > 0 && strings.HasPrefix(rest[n-1], "no events") {
			rest = rest[:n-1]
		}
		if n := len(rest); n > 0 && strings.HasPrefix(rest[n-1], "EVENTS") {
			rest = rest[:n-1]
		}
		return strings.Join(rest, "\n")
	}
	lines := renderTop(model, 76, base, topNow, style)
	if len(lines) != base || !strings.Contains(agentLines(lines), "codex/b") {
		t.Errorf("at %d the idle agents are not all kept:\n%s", base, strings.Join(lines, "\n"))
	}
	// The first idle agent folded pays for the +N more line: no gain yet,
	// so the next idle agent goes as well.
	lines = renderTop(model, 76, base-1, topNow, style)
	want := strings.Join([]string{
		"codex/gpt-5              0/8 (global)  ↺ 16:04",
		"claude/sonnet-long-name  2/8 (global)  ✗ login expired",
		"claude/opus              1/4 (runner)  ✓",
		"+2 idle (brigada capacity)",
		"all ≤ global 8",
	}, "\n")
	if len(lines) != base-1 || agentLines(lines) != want {
		t.Errorf("at %d the AGENTS section is\n%s\nwant\n%s\nin\n%s", base-1, agentLines(lines), want, strings.Join(lines, "\n"))
	}
	// The cooling and the broken agent outlast the idle ones: the events
	// line, the lowest of the rest, goes next.
	lines = renderTop(model, 76, base-2, topNow, style)
	if len(lines) != base-2 || !strings.Contains(agentLines(lines), "+2 idle (brigada capacity)") ||
		!strings.Contains(agentLines(lines), "✗ login expired") || strings.Contains(agentLines(lines), "codex/a") {
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
	// One line less: folding the last plan alone gains nothing, so both fold.
	got := strings.Join(renderTop(model, 100, n-1, topNow, style), "\n")
	if strings.Contains(got, "old plan") || !strings.Contains(got, "\n+2 finished\n") || len(strings.Split(got, "\n")) != n-1 {
		t.Errorf("at %d:\n%s", n-1, got)
	}
	// With finished plans already hidden, the first fold gains a line.
	model.status.HiddenFinishedPlans = 3
	n++
	got = strings.Join(renderTop(model, 100, n-1, topNow, style), "\n")
	if !strings.Contains(got, "#7 old plan 7") || strings.Contains(got, "old plan 8") || !strings.Contains(got, "\n+4 finished\n") ||
		len(strings.Split(got, "\n")) != n-1 {
		t.Errorf("at %d:\n%s", n-1, got)
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
	for _, w := range []string{"· #5 five (queued)", "· #6 six (waiting on #5)", "· #7 seven (review)", "codex/a"} {
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
		{base, nil, []string{"(review)", "(waiting on #5)", "(queued)", "codex/a", "? #4 polish the knobs"}},
		{base - 1, []string{"(review)"}, []string{"(waiting on #5)", "(queued)", "codex/a"}},
		{base - 2, []string{"(review)", "(waiting on #5)"}, []string{"· #5 five (queued)", "codex/a"}},
		{base - 3, []string{"(review)", "(waiting on #5)", "(queued)"}, []string{"codex/a", "? #4 polish the knobs"}},
		// The idle agent folds, paying for its +N more line, and the held
		// line goes with the next step.
		{base - 4, []string{"(queued)", "codex/a", "? #4 polish the knobs"}, []string{"+1 idle (brigada capacity)", "▸ #3 oil the gears"}},
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
			if len(lines) != 2 || lines[1] != tt.want {
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
	want := []string{
		"! #312 held by rule approval_tiers: heavy  → brigada land approve 312",
		"! #9 Top polish awaiting approval          → brigada plan approve 9",
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
		"report #41: All landed, but #5 needs a look.",
		"EVENTS " + strings.Repeat("─", 69),
		"15:02  #3 done",
		"15:02  review approve task/3-1-oil-the-gears-after-the-frobnicator-fix",
	}
	if strings.Join(tail, "\n") != strings.Join(want, "\n") {
		t.Errorf("tail =\n%s\nwant\n%s", strings.Join(tail, "\n"), strings.Join(want, "\n"))
	}
	n := len(full)
	one := renderTop(model, 76, n-1, topNow, style)
	if got := strings.Join(one, "\n"); len(one) != n-1 || strings.Contains(got, "review approve") || !strings.Contains(got, "report #41") {
		t.Errorf("one line less should drop only the strip's second line:\n%s", got)
	}
	two := renderTop(model, 76, n-2, topNow, style)
	if got := strings.Join(two, "\n"); len(two) != n-2 || strings.Contains(got, "report #41") || !strings.Contains(got, "15:02  #3 done") {
		t.Errorf("two lines less should drop the report line next:\n%s", got)
	}
	// The line is Dim and cut to width.
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	lines := topReportLine(model.status, colour, 20)
	if len(lines) != 1 || lines[0].text != colour.Dim("report #41: All l...") || lines[0].class != yieldEventsSecond {
		t.Errorf("report line = %+v", lines)
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
		topSectionHead("FOREMAN", plain, 80),
		"  ▸ job #30 foreman",
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
	for _, l := range foreman[2:] {
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
	want := "SLOTS ▓▓▓░░░░░ 3/8   " + style.Bold("1 run") + "   2 queue   0 held   0 needs"
	if got != want {
		t.Errorf("meters = %q, want %q", got, want)
	}
	if plain := topMeters(model, term.New(logo.Caps{UTF8: true}), 100)[0]; plain != "SLOTS ▓▓▓░░░░░ 3/8   1 run   2 queue   0 held   0 needs" {
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
	for _, l := range got[1 : len(got)-2] {
		names = append(names, strings.Fields(l)[0])
	}
	if want := "x/cool x/broke x/run x/offbusy x/idle1 x/idle2"; strings.Join(names, " ") != want {
		t.Errorf("order = %q, want %q", strings.Join(names, " "), want)
	}
	if fold := got[len(got)-2]; fold != "1 off (brigada capacity)" {
		t.Errorf("fold line = %q, want the off count", fold)
	}
	// The global cap makes every ready, idle agent off.
	model.capacity.Layers = []engine.CapacityLayer{{Layer: "global", Cap: &zero}}
	got = topAgents(model, topNow, term.New(logo.Caps{UTF8: true}), 100)
	if joined := strings.Join(got, "\n"); strings.Contains(joined, "x/idle1") || !strings.Contains(joined, "x/run") {
		t.Errorf("a global cap of 0 should fold the idle agents out:\n%s", joined)
	}
}

// TestTopAgentsFoldLine: the dim fold line counts the idle agents the
// height dropped and the off ones, either part left out when zero and no
// line when both are.
func TestTopAgentsFoldLine(t *testing.T) {
	plain := term.New(logo.Caps{UTF8: true})
	for _, tt := range []struct {
		idle, off int
		want      []string
	}{
		{4, 2, []string{"+4 idle · 2 off (brigada capacity)"}},
		{4, 0, []string{"+4 idle (brigada capacity)"}},
		{0, 2, []string{"2 off (brigada capacity)"}},
		{0, 0, nil},
	} {
		frame := &topFrame{lines: []topLine{{fold: foldIdle, count: tt.off}}, foldedIdle: tt.idle, style: plain}
		if got := frame.texts(); strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("idle %d off %d: fold = %q, want %q", tt.idle, tt.off, got, tt.want)
		}
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	frame := &topFrame{lines: []topLine{{fold: foldIdle, count: 1}}, foldedIdle: 1, style: colour}
	if got := frame.texts(); len(got) != 1 || got[0] != colour.Dim("+1 idle · 1 off (brigada capacity)") {
		t.Errorf("fold = %q, want Dim", got)
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
	if got := plainTop(fetched, 80, 0); !strings.Contains(got, "\n"+first+"\n") || strings.Contains(got, "\nnone\n") {
		t.Errorf("a fetched empty status lacks the first-run line:\n%s", got)
	}
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	if lines := newTopPlans(fetched.status, false, true, topNow, colour, 80); lines[1].text != colour.Dim(first) {
		t.Errorf("first-run line = %q, want Dim", lines[1].text)
	}
	// Plans hidden behind the finished ones are not a first run.
	hidden := fetched
	hidden.status.HiddenFinishedPlans = 2
	if got := plainTop(hidden, 80, 0); !strings.Contains(got, "\nnone\n") || strings.Contains(got, "no plans yet") {
		t.Errorf("hidden finished plans read as a first run:\n%s", got)
	}
	// A failed status fetch has said nothing; the loop's kept status neither.
	failed, err := topFetch(context.Background(), &fakeTop{statusErr: errors.New("down"), projects: projects}, "")
	if err == nil || failed.statusOK {
		t.Fatalf("failed fetch: err %v, statusOK %t", err, failed.statusOK)
	}
	if got := plainTop(failed, 80, 0); !strings.Contains(got, "\nnone\n") || strings.Contains(got, "no plans yet") {
		t.Errorf("a failed fetch reads as a first run:\n%s", got)
	}
	if got := plainTop(topModel{}, 80, 0); !strings.Contains(got, "\nnone\n") {
		t.Errorf("a model never fetched lacks none:\n%s", got)
	}
	kept := failed.keepPrevious(fetched)
	if got := plainTop(kept, 80, 0); !strings.Contains(got, "\n"+first+"\n") {
		t.Errorf("the kept status lost its first-run line:\n%s", got)
	}
}

// TestTopHintUnderHeader: the unregistered-models hint is a Dim line under
// the header, in the snapshot as in the loop, the same text in plain style.
func TestTopHintUnderHeader(t *testing.T) {
	const hint = "2 models your CLIs offer but no agent registers: brigada agents"
	model := busyTopModel()
	model.hint = hint
	colour := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	if lines := topHeader(model, topNow, colour, 100); len(lines) != 2 || lines[1] != colour.Dim(hint) {
		t.Errorf("header lines = %q, want the hint Dim", lines)
	}
	lines := renderTop(model, 100, 0, topNow, term.New(logo.Caps{UTF8: true}))
	if lines[1] != hint || lines[2] != "" || !strings.HasPrefix(lines[3], "SLOTS") {
		t.Errorf("view opens %q, want the header, the hint, a blank line and the meters", lines[:4])
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
	if got[1] != hint || strings.Contains(out.String(), "\x1b") || strings.HasSuffix(strings.TrimRight(out.String(), "\n"), hint) {
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
		"brigada | r | up 2h0m | 15:04:09 ----",
		"q quit | p pause",
		"SLOTS ###..... 3/8",
		"NEEDS YOU ----",
		"! #2 Plan two awaiting approval   -> brigada plan approve 2",
		"#1 fix the frobnicator 1/6 #..........",
		"  > #3 oil the gears | work | claude/sonnet@high | 1m40s | Edit",
		"  ^ #5 ship it  landing: queued",
		"  -> task #6 work: retry in 5m: dns",
		"codex/gpt-5              0/8 (global)  -> 16:04",
		"claude/sonnet-long-name  2/8 (global)  x login expired",
		"claude/opus              1/4 (runner)  ok",
		"all <= global 8",
		"report #41: All landed.\nEVENTS ----",
		"15:02  #3 done\n15:02  #3 landed",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("ASCII view lacks %q:\n%s", w, got)
		}
	}
}

// TestRenderTopFinalOrder: header, meters, NEEDS YOU, PLANS, AGENTS and
// the events strip in order, one blank line between sections only.
func TestRenderTopFinalOrder(t *testing.T) {
	got := plainTop(agentsModel(), 100, 0)
	last := -1
	for _, h := range []string{"brigada top", "SLOTS", "NEEDS YOU", "PLANS", "AGENTS", "EVENTS", "15:02  #3 done"} {
		i := strings.Index(got, h)
		if i < last {
			t.Errorf("%q out of order:\n%s", h, got)
		}
		last = i
	}
	if strings.Contains(got, "\n\n\n") || strings.HasSuffix(got, "\n") {
		t.Errorf("stray blank line:\n%q", got)
	}
}
