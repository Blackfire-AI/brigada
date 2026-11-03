package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// fakeTop stubs the top client: what it serves and what fails.
type fakeTop struct {
	status      api.Status
	capacity    engine.CapacitySnapshot
	projects    []engine.ProjectInfo
	page        history.Page
	statusErr   error
	capacityErr error
	projectsErr error
	historyErr  error
}

func (f *fakeTop) Status(context.Context) (api.Status, error) {
	return f.status, f.statusErr
}

func (f *fakeTop) Capacity(context.Context, int64) (engine.CapacitySnapshot, error) {
	return f.capacity, f.capacityErr
}

func (f *fakeTop) Projects(context.Context) ([]engine.ProjectInfo, error) {
	return f.projects, f.projectsErr
}

func (f *fakeTop) History(context.Context, api.HistoryQuery) (history.Page, error) {
	return f.page, f.historyErr
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
			{Type: history.TypeEvent, ID: 11, Kind: "job_created", ProjectID: 1, TaskID: 3, JobID: 5,
				Payload: []byte(`{"branch":"task/3-1"}`), CreatedAt: "2026-10-20T15:02:41.000000000Z"},
			{Type: history.TypeEvent, ID: 12, Kind: "task_done", ProjectID: 1, TaskID: 3,
				Payload: []byte(`{"status":"done"}`), CreatedAt: "2026-10-20T15:02:45.000000000Z"},
		},
	}
}

func plainTop(model topModel, width, height int) string {
	return strings.Join(renderTop(model, width, height, topNow, term.New(logo.Caps{})), "\n")
}

// TestRenderTopSections: the view's order — header, meters, NEEDS YOU,
// the loose jobs, the PLANS tree, AGENTS, then the events strip — with
// every section's text in its place.
func TestRenderTopSections(t *testing.T) {
	model := busyTopModel()
	model.status.Running = append(model.status.Running,
		engine.StatusJob{ID: 30, Kind: "foreman", Status: "running"})
	got := plainTop(model, 100, 0)
	want := []string{
		"brigada top · pid 4242 · up 2h0m0s · 15:04:09",
		"SLOTS ▓▓▓░░░░░ 3/8   2 run   2 queue   1 held   1 needs",
		"NEEDS YOU",
		"! #2 should the sprocket be blue?",
		"\n\n  ▸ job #30 foreman\n\nPLANS ─",
		"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		"  ▸ task #3 work · claude/sonnet@high · 1m40s · Edit",
		"  ! #2 wire the sprocket should the sprocket be blue?",
		"  ? task #4 work held: waiting for a runner slot",
		"AGENTS",
		"claude/opus",
		"all ≤ global 8",
		"15:02 task_done task 3: status=done · 15:02 job_created job 5: branch=task/3-1",
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

// TestTopHeaderRuleAndKeys: the header is one line — pid, uptime and
// clock, a rule filling the width that remains, the keys at the right
// end — and fills the width exactly.
func TestTopHeaderRuleAndKeys(t *testing.T) {
	style := term.New(logo.Caps{})
	left := "brigada top · pid 4242 · up 2h0m0s · 15:04:09"
	rule := 80 - term.VisibleWidth(left) - term.VisibleWidth("q quit · p pause") - 2
	want := left + " " + strings.Repeat("─", rule) + " q quit · p pause"
	lines := topHeader(busyTopModel(), topNow, style, 80)
	if lines[0] != want {
		t.Errorf("header = %q, want %q", lines[0], want)
	}
	if got := term.VisibleWidth(lines[0]); got != 80 {
		t.Errorf("header is %d wide, want 80", got)
	}
	colour := term.New(logo.Caps{Color: logo.Color16})
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
			got := topMeters(model, term.New(logo.Caps{}), 100)[0]
			if !strings.HasPrefix(got, tt.want+"   ") {
				t.Errorf("meters = %q, want the slots cell %q", got, tt.want)
			}
		})
	}
}

// TestTopMetersCountersAndColours: the counters follow the cell — run
// bold, queue plain, held warn, needs bad — three spaces apart.
func TestTopMetersCountersAndColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16})
	got := topMeters(busyTopModel(), style, 100)[0]
	want := "SLOTS ▓▓▓░░░░░ 3/8   " + style.Bold("1 run") + "   2 queue   " + style.Warn("1 held") + "   " + style.Bad("1 needs")
	if got != want {
		t.Errorf("meters = %q, want %q", got, want)
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
		{"plan", engine.NeedsYouItem{Kind: "plan", ID: 9, State: "pending_approval", Reason: "over the approval limit"}, "! #9 over the approval limit"},
		{"agent", engine.NeedsYouItem{Kind: "agent", ID: 4, State: "broken", Title: "dsh/large", Reason: "auth: no key"}, "! dsh/large auth: no key"},
		{"foreman", engine.NeedsYouItem{Kind: "foreman", ID: 1, State: "outage", Title: "https://example.invalid/r.git", Reason: "turns failed on the infrastructure"}, "! https://example.invalid/r.git turns failed on the infrastructure"},
		{"project", engine.NeedsYouItem{Kind: "project", ID: 2, State: "duplicate", Title: "https://example.invalid/r.git", Reason: "projects 1 and 2 are the same repository"}, "! https://example.invalid/r.git projects 1 and 2 are the same repository"},
		{"no reason shows the state", engine.NeedsYouItem{Kind: "task", ID: 7, State: "needs_user"}, "! #7 needs_user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := topNeedsYou(api.Status{NeedsYou: []engine.NeedsYouItem{tt.item}}, term.New(logo.Caps{}), 120)
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
	got := strings.Join(topNeedsYou(api.Status{NeedsYou: items}, term.New(logo.Caps{}), 100), "\n")
	want := strings.Join([]string{
		"NEEDS YOU " + strings.Repeat("─", 90),
		"! #1 why 1", "! #2 why 2", "! #3 why 3",
		"+2 more",
	}, "\n")
	if got != want {
		t.Errorf("lines =\n%s\nwant\n%s", got, want)
	}
}

// TestTopNeedsYouColours: the entries are Bad, the "+N more" and the
// empty queue's line dim, the header's rule fill dim too.
func TestTopNeedsYouColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16})
	st := api.Status{NeedsYou: []engine.NeedsYouItem{
		{Kind: "task", ID: 2, State: "needs_foreman", Reason: "should the sprocket be blue?"},
		{Kind: "agent", ID: 4, State: "broken", Title: "dsh/large", Reason: "auth: no key"},
	}}
	lines := topNeedsYou(st, style, 100)
	if want := style.Bold("NEEDS YOU") + " " + style.Dim(strings.Repeat("─", 90)); lines[0] != want {
		t.Errorf("header = %q, want a bold title and a dim rule", lines[0])
	}
	if lines[1] != style.Bad("! #2 should the sprocket be blue?") || lines[2] != style.Bad("! dsh/large auth: no key") {
		t.Errorf("entries = %q", lines)
	}
	var four []engine.NeedsYouItem
	for i := 0; i < 4; i++ {
		four = append(four, engine.NeedsYouItem{Kind: "task", ID: int64(i + 1), Reason: "why"})
	}
	if got := topNeedsYou(api.Status{NeedsYou: four}, style, 100); got[4] != style.Dim("+1 more") {
		t.Errorf("cap line = %q, want dim +1 more", got[4])
	}
	if got := topNeedsYou(api.Status{}, style, 100); got[1] != style.Dim("nothing needs you") {
		t.Errorf("empty queue = %q, want dim nothing needs you", got)
	}
}

// TestTopNeedsYouEmptyQueue: the section stays visible when nothing
// needs the user.
func TestTopNeedsYouEmptyQueue(t *testing.T) {
	lines := topNeedsYou(api.Status{}, term.New(logo.Caps{}), 80)
	want := "NEEDS YOU " + strings.Repeat("─", 70) + "\nnothing needs you"
	if got := strings.Join(lines, "\n"); got != want {
		t.Errorf("empty queue =\n%s\nwant\n%s", got, want)
	}
}

// TestTopEventsStrip: the strip joins the newest items, newest first —
// one line when they fit, two when a whole item fits on the second
// line, one cut line when it does not — and renders one dim line when
// nothing happened.
func TestTopEventsStrip(t *testing.T) {
	items := busyTopModel().events
	style := term.New(logo.Caps{})
	want := "15:02 task_done task 3: status=done · 15:02 job_created job 5: branch=task/3-1"
	if got := topEvents(items, topNow, style, 200); len(got) != 1 || got[0] != want {
		t.Errorf("strip = %q, want %q", got, want)
	}
	wrapped := topEvents(items, topNow, style, 76)
	if len(wrapped) != 2 || wrapped[0] != "15:02 task_done task 3: status=done" ||
		wrapped[1] != "15:02 job_created job 5: branch=task/3-1" {
		t.Errorf("wrapped strip = %q", wrapped)
	}
	if got := topEvents(items, topNow, style, 30)[0]; got != "15:02 task_done task 3: sta..." {
		t.Errorf("cut = %q", got)
	}
	if got := topEvents(nil, topNow, style, 80); got[0] != "no events" {
		t.Errorf("empty = %q", got)
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
	style := term.New(logo.Caps{})
	model := busyTopModel()
	strip := strings.Join(topEvents(model.events, topNow, style, 200), "\n")
	if !strings.Contains(strip, "15:02 task_done") ||
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
	style := term.New(logo.Caps{Color: logo.Color16})
	tests := []struct {
		name string
		item history.Item
		want string
	}{
		{"done", history.Item{Type: history.TypeEvent, ID: 1, Kind: "task_done", TaskID: 3,
			Payload: []byte(`{"status":"done"}`), CreatedAt: "2026-10-20T15:02:45.000000000Z"},
			"15:02 task_" + style.OK("done") + " task 3: status=" + style.OK("done")},
		{"recovered", history.Item{Type: history.TypeEvent, ID: 2, Kind: "agent_recovered", ProjectID: 1,
			CreatedAt: "2026-10-20T15:02:46.000000000Z"},
			"15:02 agent_" + style.OK("recovered") + " project 1:"},
		{"failed", history.Item{Type: history.TypeEvent, ID: 3, Kind: "task_failed", TaskID: 4,
			Payload: []byte(`{"status":"failed"}`), CreatedAt: "2026-10-20T15:02:47.000000000Z"},
			"15:02 task_" + style.Bad("failed") + " task 4: status=" + style.Bad("failed")},
		{"broken", history.Item{Type: history.TypeEvent, ID: 4, Kind: "agent_broken", ProjectID: 1,
			Payload: []byte(`{"reason":"auth: no key"}`), CreatedAt: "2026-10-20T15:02:48.000000000Z"},
			"15:02 agent_" + style.Bad("broken") + " project 1: reason=auth: no key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topEventItem(tt.item, topNow, style); got != tt.want {
				t.Errorf("item = %q, want %q", got, tt.want)
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
		"\n! #8 held by rule gate_land: gate_land is ask; release: brigada land approve 8; change the rule: brigada land approve --and-stop-asking 8" +
		"\n! #9 waits to push: task/9-1@abc123"
	tree := "#1 fix the frobnicator 3/6 ▓▓▓▓▓░░░░░░" +
		"\n  ▸ task #3 work · claude/sonnet@high · 1m40s · Edit" +
		"\n  ▸ task #9 land · 1m40s" +
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
			"#96 Rework brigada top 2/12 ▓░░░░░░░░░░",
			"  ▸ task #317 work · opencode/glm-5.3 · 14m0s · editing cmdtop.go",
			"  ▸ task #313 land · 14m0s",
			"  ! #314 finish rounds spent",
			"  ! #315 ask needs_user",
			"  · #316 audit (review)",
			"  · #318 tree (queued)",
			"  · #319 docs (waiting on tree, #317)",
			"  ? task #320 fix held: waiting for a runner slot",
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
			"#83 Role instructions 2/3 ✓",
			"#80 Abandoned ✗",
			"+41 finished",
		}},
		{"a live plan's review job under its plan", api.Status{
			Plans: []engine.StatusPlan{{ID: 12, Title: "Review me", State: "reviewing", ReviewJob: 50,
				Tasks: []engine.StatusTask{{ID: 1, Title: "first", State: "pending"}}}},
			Running: []engine.StatusJob{{ID: 50, Kind: "plan_review", Harness: "claude", Model: "opus", LeasedAt: leased}},
		}, []string{
			head,
			"#12 Review me 0/1 ░░░░░░░░░░░",
			"  ▸ job #50 plan_review · claude/opus · 14m0s",
			"  · #1 first (queued)",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(topPlansTree(tt.st, topNow, term.New(logo.Caps{}), 80), "\n")
			if want := strings.Join(tt.want, "\n"); got != want {
				t.Errorf("tree =\n%s\nwant\n%s", got, want)
			}
			if loose := topLoose(tt.st, topNow, term.New(logo.Caps{}), 80); len(loose) != 0 {
				t.Errorf("a plan's job is loose: %q", loose)
			}
		})
	}
}

// TestTopPlansTreeColours: the live plan's id Bold, the bar's fill OK and
// its empty cells Dim, the finished marks OK and Bad, the line forms in
// their colours.
func TestTopPlansTreeColours(t *testing.T) {
	style := term.New(logo.Caps{Color: logo.Color16})
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
		"  " + style.Accent("▸") + " task #2 work",
		"  " + style.Dim("· #3 q (queued)"),
		"  " + style.Warn("? task #4 work held: waiting for a runner slot"),
		"  " + style.Bad("! #5 stuck needs_foreman"),
		"#2 done 1/1 " + style.OK("✓"),
		"#3 gone " + style.Bad("✗"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("tree =\n%q\nwant\n%q", got, want)
	}
}

// TestTopLoose: the jobs no listed live plan claims — a taskless running
// turn, a taskless held one, a taskless retry, the review of a plan the
// tree does not list — render in the loose block, and it is absent from
// the view when every job has its plan.
func TestTopLoose(t *testing.T) {
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
		"\n\n  ▸ job #60 foreman · claude/opus" +
		"\n  ▸ job #63 plan_review" +
		"\n  ? job #61 foreman held: foreman turn waiting: no runner offers claude/opus" +
		"\n  ↺ job #62 foreman: retry in 2m: could not resolve github.com" +
		"\n\nPLANS ─"
	if !strings.Contains(got, want) {
		t.Errorf("view lacks the loose block %q:\n%s", want, got)
	}

	got = plainTop(busyTopModel(), 100, 0)
	if !strings.Contains(got, "! #2 should the sprocket be blue?\n\nPLANS ─") {
		t.Errorf("an empty loose block renders:\n%s", got)
	}
}

// TestRenderTopFitsSize: a screen shorter than the view's floor still
// renders the floor whole — the busy fixture at 40x10 keeps every line
// within 40 columns while the count passes the height, because nothing
// inside the floor is cut or dropped.
func TestRenderTopFitsSize(t *testing.T) {
	lines := renderTop(busyTopModel(), 40, 10, topNow, term.New(logo.Caps{}))
	want := []string{
		"brigada top · pid 4242 · up 2h0m0s · ...",
		"",
		"SLOTS ▓▓▓░░░░░ 3/8   1 run   2 queue ...",
		"",
		"NEEDS YOU",
		"! #2 should the sprocket be blue?",
		"",
		"PLANS",
		"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		"",
		"AGENTS",
		"claude/opus  1/8 (global)  ✓",
		"all ≤ global 8",
		"",
		"15:02 task_done task 3: status=done",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("floor =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if len(lines) <= 10 {
		t.Errorf("got %d lines, want the floor past the height", len(lines))
	}
	for _, l := range lines {
		if n := term.VisibleWidth(l); n > 40 {
			t.Errorf("line %q is %d wide", l, n)
		}
	}
}

// TestRenderTopHeightYieldOrder: lines beyond the height yield in order
// — the events strip's second line, then plan task lines from the last
// listed plan upward, then agent lines from the bottom of the list
// upward — while every plan's own line and the +N more (brigada agents)
// line stay.
func TestRenderTopHeightYieldOrder(t *testing.T) {
	model := agentsModel()
	tests := []struct {
		name   string
		height int
		lines  int
		want   []string
		lacks  []string
	}{
		{"everything fits", 22, 22, []string{
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
			"✗ login expired",
			"15:02 job_created job 5: branch=task/3-1",
		}, nil},
		{"events second line", 21, 21, []string{
			"15:02 task_done task 3: status=done",
		}, []string{"job_created"}},
		{"last plan task line", 20, 20, []string{
			"  ▸ task #3 work · claude/sonnet@high · 1m40s · Edit",
			"  ! #2 wire the sprocket should the sprocket be blue?",
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		}, []string{"? task #4 work held", "job_created"}},
		{"then the parked line", 19, 19, []string{
			"  ▸ task #3 work · claude/sonnet@high · 1m40s · Edit",
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		}, []string{"wire the sprocket should", "? task #4 work held", "job_created"}},
		{"then the running line", 18, 18, []string{
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		}, []string{"▸ task #3", "wire the sprocket should", "? task #4 work held", "job_created"}},
		{"then agent lines", 17, 17, []string{
			"claude/opus",
			"+2 more (brigada agents)",
			"all ≤ global 8",
		}, []string{"codex/gpt-5", "✗ login expired", "▸ task #3"}},
		{"down to none", 16, 16, []string{
			"+3 more (brigada agents)",
			"all ≤ global 8",
			"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		}, []string{"✗ login expired"}},
		{"the floor overflows", 15, 16, []string{
			"+3 more (brigada agents)",
			"all ≤ global 8",
		}, []string{"claude/opus", "✗ login expired"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := renderTop(model, 76, tt.height, topNow, term.New(logo.Caps{}))
			got := strings.Join(lines, "\n")
			if len(lines) != tt.lines {
				t.Fatalf("got %d lines, want %d:\n%s", len(lines), tt.lines, got)
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

func TestTopKeepPrevious(t *testing.T) {
	prev := busyTopModel()
	failed := topModel{err: errors.New("engine gone")}.keepPrevious(prev)
	got := plainTop(failed, 100, 0)
	if !strings.Contains(got, "engine: engine gone") || !strings.Contains(got, "claude/opus") || !strings.Contains(got, "job_created") {
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
	if code := printTopSnapshot(context.Background(), fake, "https://example.invalid/r.git", &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "brigada top") {
		t.Errorf("snapshot lacks the header:\n%s", out.String())
	}
	fake.projectsErr = errors.New("down")
	out.Reset()
	if code := printTopSnapshot(context.Background(), fake, "https://example.invalid/r.git", &out, &errOut); code != 1 {
		t.Errorf("exit %d, want 1", code)
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

// TestRenderTopScreenHeights: a screen shorter than the floor returns
// the floor whole — more lines than the height, the header first,
// nothing inside the floor cut — and a screen taller than the content
// changes nothing.
func TestRenderTopScreenHeights(t *testing.T) {
	lines := renderTop(busyTopModel(), 60, 5, topNow, term.New(logo.Caps{}))
	if len(lines) <= 5 {
		t.Fatalf("got %d lines, want the floor past the height:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "brigada top") {
		t.Errorf("first line is %q, want the header", lines[0])
	}
	got := strings.Join(lines, "\n")
	for _, w := range []string{
		"NEEDS YOU",
		"! #2 should the sprocket be blue?",
		"#1 fix the frobnicator 1/4 ▓▓░░░░░░░░░",
		"all ≤ global 8",
		"15:02 task_done task 3: status=done",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("floor lacks %q:\n%s", w, got)
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
	tall := renderTop(busyTopModel(), 60, 40, topNow, term.New(logo.Caps{}))
	unbounded := renderTop(busyTopModel(), 60, 0, topNow, term.New(logo.Caps{}))
	if strings.Join(tall, "\n") != strings.Join(unbounded, "\n") {
		t.Errorf("tall screen trims:\n%s\nwant\n%s", strings.Join(tall, "\n"), strings.Join(unbounded, "\n"))
	}
}

// shortTopModel is busyTopModel with a header left short enough — 39
// columns — that the keys fit from width 58 up.
func shortTopModel() topModel {
	model := busyTopModel()
	model.status.Pid = 99
	model.status.StartedAt = topNow.UTC().Format(engine.TimeLayout)
	return model
}

// TestTopHeaderKeysFit: the keys show only when the left part, two
// spaces and the keys fit the width whole — a key is never cut
// mid-word — and otherwise a dim rule fills what remains of the width.
// busyTopModel's header left is 45 columns, so its keys fit only from
// width 64 up, at 60 too they give way.
func TestTopHeaderKeysFit(t *testing.T) {
	style := term.New(logo.Caps{})
	tests := []struct {
		name  string
		model topModel
		width int
		keys  bool
	}{
		{"busy at 59 drops the keys", busyTopModel(), 59, false},
		{"busy at 60 drops the keys too", busyTopModel(), 60, false},
		{"busy at 63 drops the keys", busyTopModel(), 63, false},
		{"busy at 64 shows the keys", busyTopModel(), 64, true},
		{"short left at 57 drops the keys", shortTopModel(), 57, false},
		{"short left at 58 shows the keys", shortTopModel(), 58, true},
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
	want := "brigada top · pid 99 · up 0s · 15:04:09 " + strings.Repeat("─", 17)
	if got := topHeader(shortTopModel(), topNow, style, 57)[0]; got != want {
		t.Errorf("keys-dropped header = %q, want %q", got, want)
	}
}

// TestRenderTopNarrowSectionHeads: below 60 columns the section heads
// drop their rule fills and keep their titles, the lines beneath keep
// their indentation, and at 60 the fills return.
func TestRenderTopNarrowSectionHeads(t *testing.T) {
	narrow := strings.Join(renderTop(busyTopModel(), 59, 0, topNow, term.New(logo.Caps{})), "\n")
	for _, head := range []string{"NEEDS YOU", "PLANS", "AGENTS"} {
		if !strings.Contains(narrow, "\n"+head+"\n") {
			t.Errorf("narrow view lacks the bare head %q:\n%s", head, narrow)
		}
	}
	if !strings.Contains(narrow, "  ▸ task #3 work · claude/sonnet@high · 1m40s · Edit") {
		t.Errorf("narrow view loses the indentation beneath a head:\n%s", narrow)
	}
	wide := strings.Join(renderTop(busyTopModel(), 60, 0, topNow, term.New(logo.Caps{})), "\n")
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
	plain := term.New(logo.Caps{})
	got := topAgents(model, topNow, plain, 100, 0)
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
		"claude/opus              " + api.RunningCap(a[0], &eight) + "  ✓",
		"codex/gpt-5              " + api.RunningCap(a[1], &eight) + "  ↺ " + cool,
		"claude/sonnet-long-name  " + api.RunningCap(a[2], &eight) + "  ✗ login expired",
	}
	for i, w := range wants {
		if got[1+i] != w {
			t.Errorf("line %d = %q, want %q", i, got[1+i], w)
		}
	}
	if got[4] != "all ≤ global 8" {
		t.Errorf("global line = %q", got[4])
	}
	colour := term.New(logo.Caps{Color: logo.Color16})
	styled := topAgents(model, topNow, colour, 100, 0)
	if s := term.StripEscapes(strings.Join(styled, "\n")); s != strings.Join(got, "\n") {
		t.Errorf("styled text differs from plain:\n%s\nvs\n%s", s, strings.Join(got, "\n"))
	}
	if !strings.Contains(styled[1], colour.OK("✓")) || !strings.Contains(styled[2], colour.Warn("↺ "+cool)) ||
		!strings.Contains(styled[3], colour.Bad("✗ login expired")) {
		t.Errorf("quota styles wrong: %q", styled[1:4])
	}
}

// TestTopAgentsBrokenFallbackAndCut: a broken agent with no needs-you
// entry reads "broken"; a name longer than the width is cut with "...".
func TestTopAgentsBrokenFallbackAndCut(t *testing.T) {
	model := agentsModel()
	model.status.NeedsYou = model.status.NeedsYou[:1]
	plain := term.New(logo.Caps{})
	got := topAgents(model, topNow, plain, 100, 0)
	if !strings.HasSuffix(got[3], "✗ broken") {
		t.Errorf("fallback line = %q", got[3])
	}
	cut := topAgents(model, topNow, plain, 12, 0)
	if !strings.HasPrefix(cut[3], "claude/so...") {
		t.Errorf("long name not cut: %q", cut)
	}
}

// TestTopAgentsGlobalLineOmitted: no global cap, no `all ≤` line.
func TestTopAgentsGlobalLineOmitted(t *testing.T) {
	model := agentsModel()
	model.capacity.Layers = nil
	got := topAgents(model, topNow, term.New(logo.Caps{}), 100, 0)
	if len(got) != 4 || strings.Contains(strings.Join(got, "\n"), "all ≤") {
		t.Errorf("section = %q", got)
	}
}

// TestTopAgentsBudget: a budget trims the agent lines and counts them.
func TestTopAgentsBudget(t *testing.T) {
	model := agentsModel()
	plain := term.New(logo.Caps{})
	got := topAgents(model, topNow, plain, 100, 4)
	if len(got) != 4 || got[2] != "+2 more (brigada agents)" || got[3] != "all ≤ global 8" || !strings.HasPrefix(got[1], "claude/opus") {
		t.Errorf("trimmed section = %q", got)
	}
	if all := topAgents(model, topNow, plain, 100, 5); len(all) != 5 {
		t.Errorf("a fitting budget trimmed: %q", all)
	}
}

// TestRenderTopFinalOrder: header, meters, NEEDS YOU, PLANS, AGENTS and
// the events strip in order, one blank line between sections only.
func TestRenderTopFinalOrder(t *testing.T) {
	got := plainTop(agentsModel(), 100, 0)
	last := -1
	for _, h := range []string{"brigada top", "SLOTS", "NEEDS YOU", "PLANS", "AGENTS", "15:02 task_done"} {
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
