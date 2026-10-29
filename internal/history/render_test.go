package history

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// goldenItems is one small story: a task's spec, the run's events, the
// worker's answer — the shapes `brigada history` will render.
func goldenItems() []Item {
	return []Item{
		{
			Type: TypeEvent, ID: 1, Kind: "job_created", ProjectID: 7, TaskID: 3, JobID: 11,
			Payload:   []byte(`{"answer":"created","run":1}`),
			CreatedAt: "2026-10-12T12:00:00.000000000Z",
		},
		{
			Type: TypeMessage, ID: 1, Kind: "task_spec", ProjectID: 7, EventID: 1,
			Author:    "engine",
			Content:   "Add a README.\n\nIt should explain:\n" + "- what the tool does\n- how to run it",
			CreatedAt: "2026-10-12T12:00:00.000000000Z",
		},
		{
			Type: TypeEvent, ID: 2, Kind: "job_leased", ProjectID: 7, TaskID: 3, JobID: 11,
			Payload:   []byte(`{"lease":"2-abcd123","owner":2,"attempt":1}`),
			CreatedAt: "2026-10-12T12:00:01.000000000Z",
		},
		{
			Type: TypeEvent, ID: 3, Kind: "job_completed", ProjectID: 7, TaskID: 3, JobID: 11,
			Payload:   []byte(`{"status":"done","lease":"2-abcd123"}`),
			CreatedAt: "2026-10-12T12:04:00.000000000Z",
		},
		{
			Type: TypeMessage, ID: 2, Kind: "worker_result", ProjectID: 7, EventID: 3,
			Author:    "worker",
			Content:   `{"status":"done","summary":"added the README","branch":"task/3-1"}`,
			CreatedAt: "2026-10-12T12:04:00.000000000Z",
		},
		{
			Type: TypeEvent, ID: 4, Kind: "foreman_report", ProjectID: 7, JobID: 12,
			Payload:   []byte(`{"report":"the task's question is answered\nnothing needs you"}`),
			CreatedAt: "2026-10-12T12:05:00.000000000Z",
		},
	}
}

// TestRenderGolden is the plain-text rendering `brigada history` will
// sit on (§9): one line per item, bodies that run over several lines
// indented so items stay readable as blocks.
func TestRenderGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, goldenItems()); err != nil {
		t.Fatalf("render: %v", err)
	}
	golden := filepath.Join("testdata", "timeline.golden")
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	if buf.String() != string(want) {
		t.Errorf("the rendering:\n%s\nwant the golden:\n%s", buf.String(), want)
	}
}

func TestRenderTierBump(t *testing.T) {
	it := Item{Type: TypeEvent, Kind: "task_tier_bumped", Payload: []byte(`{"reason":"fix_rounds spent","tier_asked":"light","tier_served":"standard"}`)}
	if got := summary(it); got != "task tier light -> standard: fix_rounds spent" {
		t.Errorf("summary = %q", got)
	}
}

// TestRenderResumedReview: a re-review's task_reviewing event shows in the
// text timeline as the reason line the engine writes for it, and a fresh
// review after a changes verdict says why it is fresh.
func TestRenderResumedReview(t *testing.T) {
	resumed := Item{Type: TypeEvent, Kind: "task_reviewing", Payload: []byte(`{"status":"reviewing","branch":"brigada/task-1","pick":"resumed","agent":4,"reason":"resumed review: the previous reviewer's session on agent 4"}`)}
	if got := summary(resumed); got != "status=reviewing reason=resumed review: the previous reviewer's session on agent 4" {
		t.Errorf("summary = %q", got)
	}
	fresh := Item{Type: TypeEvent, Kind: "task_reviewing", Payload: []byte(`{"status":"reviewing","reason":"fresh review: the previous reviewer's agent is broken"}`)}
	if got := summary(fresh); got != "status=reviewing reason=fresh review: the previous reviewer's agent is broken" {
		t.Errorf("summary = %q", got)
	}
}

func TestRenderEffortBump(t *testing.T) {
	it := Item{Type: TypeEvent, Kind: "task_effort_bumped", Payload: []byte(`{"reason":"no-result","effort_from":"medium","effort_to":"high"}`)}
	if got := summary(it); got != "task effort medium -> high: no-result" {
		t.Errorf("summary = %q", got)
	}
}

// TestRenderLanding: the landing events show the record — the target
// branch, the resulting commit, the push state the gate made, the exact
// push command of gate_push never, and an after_land failure reported
// and never undone (§6.3, §7.1).
func TestRenderLanding(t *testing.T) {
	landed := Item{Type: TypeEvent, Kind: "task_landed", Payload: []byte(`{"status":"done","branch":"main","commit":"9f21ab3","pushed":"local",` +
		`"push_cmd":"git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main",` +
		`"after_land_error":"exit 3: after-land boom"}`)}
	want := "branch=main status=done commit=9f21ab3 pushed=local" +
		" push_cmd=git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main" +
		" after_land_error=exit 3: after-land boom"
	if got := summary(landed); got != want {
		t.Errorf("task_landed summary = %q, want %q", got, want)
	}
	pushed := Item{Type: TypeEvent, Kind: "task_pushed", Payload: []byte(`{"status":"done","branch":"main","commit":"9f21ab3","pushed":"pushed"}`)}
	if got := summary(pushed); got != "branch=main status=done commit=9f21ab3 pushed=pushed" {
		t.Errorf("task_pushed summary = %q", got)
	}
}

// TestRenderLeasedPromptDigests: a lease event names the template and
// the shortened digests of the instruction and skill texts (§9).
func TestRenderLeasedPromptDigests(t *testing.T) {
	it := Item{Type: TypeEvent, Kind: "job_leased", Payload: []byte(`{"lease":"abc","prompt":{"template":2,"instructions":"0123456789abcdef","skills":["aaaaaaaaaaaaaaaa","bbbbbbbbbbbbbbbb"]}}`)}
	want := "lease=abc template=2 instructions=0123456789ab skills=aaaaaaaaaaaa,bbbbbbbbbbbb"
	if got := summary(it); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
