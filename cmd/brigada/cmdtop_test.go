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

// busyTopModel is one full view's data: an agent, a running job, a held
// one, an active plan with a parked task, the needs-you queue and events.
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
// then the AGENTS, RUNNING, QUEUE and PLANS sections, then the events
// strip — with every section's text in its place.
func TestRenderTopSections(t *testing.T) {
	got := plainTop(busyTopModel(), 100, 0)
	want := []string{
		"brigada top · pid 4242 · up 2h0m0s · 15:04:09",
		"SLOTS ▓▓▓░░░░░ 3/8   1 run   2 queue   1 held   1 needs",
		"NEEDS YOU",
		"! #2 should the sprocket be blue?",
		"AGENTS",
		"claude/opus",
		"all ≤ global 8",
		"job #5 task #3 work claude/sonnet@high 1m40s tool_start Edit",
		"2 queued",
		"held: job #6 work by runner",
		"#1 active fix the frobnicator 1/2",
		"task #2 needs_foreman wire the sprocket: should the sprocket be blue?",
		"15:02:45 task_done task 3: status=done · 15:02:41 job_created job 5: branch=task/3-1",
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

// TestTopEventsStrip: the strip joins the newest items, newest first,
// cuts at width and renders one dim line when nothing happened.
func TestTopEventsStrip(t *testing.T) {
	items := busyTopModel().events
	want := "15:02:45 task_done task 3: status=done · 15:02:41 job_created job 5: branch=task/3-1"
	if got := topEvents(items, topNow, term.New(logo.Caps{}), 200)[0]; got != want {
		t.Errorf("strip = %q, want %q", got, want)
	}
	if got := topEvents(items, topNow, term.New(logo.Caps{}), 30)[0]; got != "15:02:45 task_done task 3: ..." {
		t.Errorf("cut = %q", got)
	}
	if got := topEvents(nil, topNow, term.New(logo.Caps{}), 80); got[0] != "no events" {
		t.Errorf("empty = %q", got)
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
			"15:02:45 task_" + style.OK("done") + " task 3: status=" + style.OK("done")},
		{"recovered", history.Item{Type: history.TypeEvent, ID: 2, Kind: "agent_recovered", ProjectID: 1,
			CreatedAt: "2026-10-20T15:02:46.000000000Z"},
			"15:02:46 agent_" + style.OK("recovered") + " project 1:"},
		{"failed", history.Item{Type: history.TypeEvent, ID: 3, Kind: "task_failed", TaskID: 4,
			Payload: []byte(`{"status":"failed"}`), CreatedAt: "2026-10-20T15:02:47.000000000Z"},
			"15:02:47 task_" + style.Bad("failed") + " task 4: status=" + style.Bad("failed")},
		{"broken", history.Item{Type: history.TypeEvent, ID: 4, Kind: "agent_broken", ProjectID: 1,
			Payload: []byte(`{"reason":"auth: no key"}`), CreatedAt: "2026-10-20T15:02:48.000000000Z"},
			"15:02:48 agent_" + style.Bad("broken") + " project 1: reason=auth: no key"},
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

// TestRenderTopShowsGateHoldsAndLandJobs: a land job a runner holds is
// a running line like any job, and the land-gate and push-gate holds
// sit in NEEDS YOU with the rule that tripped and the task id.
func TestRenderTopShowsGateHoldsAndLandJobs(t *testing.T) {
	model := busyTopModel()
	leased := topNow.Add(-100 * time.Second).UTC().Format(engine.TimeLayout)
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
	for _, w := range []string{
		"job #10 task #9 land 1m40s",
		want,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("view lacks %q:\n%s", w, got)
		}
	}
}

func TestRenderTopShowsAnInfraRetry(t *testing.T) {
	model := busyTopModel()
	nb := topNow.Add(5 * time.Minute).UTC().Format(engine.TimeLayout)
	model.status.Retries = []engine.InfraRetry{{ID: 9, TaskID: 4, Kind: "work", NotBefore: nb, Reason: "could not resolve github.com"}}
	got := plainTop(model, 200, 0)
	want := "waiting: task #4 work: retry in 5m: could not resolve github.com"
	if !strings.Contains(got, want) {
		t.Errorf("view lacks %q:\n%s", want, got)
	}
}

func TestRenderTopFitsSize(t *testing.T) {
	lines := renderTop(busyTopModel(), 40, 10, topNow, term.New(logo.Caps{}))
	if len(lines) != 10 {
		t.Fatalf("got %d lines, want 10", len(lines))
	}
	for _, l := range lines {
		if n := term.VisibleWidth(l); n > 40 {
			t.Errorf("line %q is %d wide", l, n)
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

func TestRenderTopTallerThanScreenKeepsHeader(t *testing.T) {
	lines := renderTop(busyTopModel(), 60, 5, topNow, term.New(logo.Caps{}))
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5", len(lines))
	}
	if !strings.Contains(lines[0], "brigada top") {
		t.Errorf("first line is %q, want the header", lines[0])
	}
}
