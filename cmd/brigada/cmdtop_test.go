package main

import (
	"bytes"
	"context"
	"errors"
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
		},
	}
}

func plainTop(model topModel, width, height int) string {
	return strings.Join(renderTop(model, width, height, topNow, term.New(logo.Caps{})), "\n")
}

func TestRenderTopSections(t *testing.T) {
	got := plainTop(busyTopModel(), 100, 0)
	for _, want := range []string{
		"brigada top · pid 4242 · up 2h0m0s · runners 0 · slots 3/8 · 15:04:09",
		"AGENTS", "claude/opus",
		"job #5 task #3 work claude/sonnet@high 1m40s tool_start Edit",
		"2 queued", "held: job #6 work by runner",
		"#1 active fix the frobnicator 1/2",
		"task #2 needs_foreman wire the sprocket: should the sprocket be blue?",
		"NEEDS YOU", "15:02:41 job_created job 5: branch=task/3-1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("view lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("plain view has escapes:\n%q", got)
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
		engine.NeedsYouItem{Kind: "task", ID: 8, State: "done", Title: "ship the gizmo", Reason: "waits to land: gate_land is ask"},
		engine.NeedsYouItem{Kind: "task", ID: 9, State: "done", Title: "push the widget", Reason: "waits to push: task/9-1@abc123"})
	got := plainTop(model, 100, 0)
	for _, want := range []string{
		"job #10 task #9 land 1m40s",
		"NEEDS YOU\n  task #2 needs_foreman: should the sprocket be blue?\n  task #8 done: waits to land: gate_land is ask\n  task #9 done: waits to push: task/9-1@abc123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("view lacks %q:\n%s", want, got)
		}
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
	if model.status.Pid != 4242 || len(model.events) != 1 || len(model.capacity.Agents) != 1 {
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
