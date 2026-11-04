package history

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const wordsStamp = "2026-10-20T15:02:45.000000000Z"

func wordsEvent(kind string, task, job int64, payload string) Item {
	return Item{Type: TypeEvent, ID: 1, Kind: kind, ProjectID: 1, TaskID: task, JobID: job,
		Payload: []byte(payload), CreatedAt: wordsStamp}
}

// TestEventWords: every allowlisted kind in its plain words, and the kinds
// and types that have none.
func TestEventWords(t *testing.T) {
	clock := func(stamp string) string { return "16:04" }
	tests := []struct {
		name string
		item Item
		want string
	}{
		{"plan_done", wordsEvent("plan_done", 0, 0, `{"plan":99,"status":"done"}`), "plan #99 done"},
		{"plan_done without a plan", wordsEvent("plan_done", 0, 0, `{}`), "plan done"},
		{"plan_approved", wordsEvent("plan_approved", 0, 0, `{"plan":7,"status":"active"}`), "plan #7 approved"},
		{"plan_awaiting_approval", wordsEvent("plan_awaiting_approval", 0, 0, `{"plan":8}`), "plan #8 awaiting approval"},
		{"task_done", wordsEvent("task_done", 3, 0, `{"status":"done"}`), "#3 done"},
		{"task_landed", wordsEvent("task_landed", 328, 0, `{"pushed":"pushed"}`), "#328 landed"},
		{"task_landed no push state", wordsEvent("task_landed", 328, 0, ``), "#328 landed"},
		{"task_landed waiting", wordsEvent("task_landed", 328, 0, `{"pushed":"waiting"}`), "#328 landed, push waits"},
		{"task_landed local", wordsEvent("task_landed", 328, 0, `{"pushed":"local"}`), "#328 landed, push waits"},
		{"task_pushed", wordsEvent("task_pushed", 328, 0, `{}`), "#328 pushed"},
		{"task_failed", wordsEvent("task_failed", 4, 0, `{"status":"failed"}`), "#4 failed"},
		{"task_land_failed", wordsEvent("task_land_failed", 4, 0, `{"reason":"conflict"}`), "#4 land failed"},
		{"review approve", wordsEvent("review_verdict", 0, 9, `{"status":"approve","branch":"fix/x"}`), "review approve fix/x"},
		{"review changes", wordsEvent("review_verdict", 0, 9, `{"status":"changes","branch":"fix/x"}`), "review changes fix/x"},
		{"review no verdict", wordsEvent("review_verdict", 0, 9, `{"status":"no_verdict"}`), "review no verdict"},
		{"agent_broken", wordsEvent("agent_broken", 0, 0, `{"harness":"codex","model":"gpt-5","reason":"auth"}`), "codex/gpt-5 broken"},
		{"agent_recovered", wordsEvent("agent_recovered", 0, 0, `{"harness":"codex","model":"gpt-5"}`), "codex/gpt-5 recovered"},
		{"agent_cooling", wordsEvent("agent_cooling", 3, 5, `{"account":"work","until":"x"}`), "work cooling until 16:04"},
		{"agent_cooling without a deadline", wordsEvent("agent_cooling", 3, 5, `{"account":"work"}`), "work cooling"},
		{"foreman_report", wordsEvent("foreman_report", 0, 41, `{"report":"all landed"}`), "report #41"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EventWords(tt.item, clock)
			if !ok || got != tt.want {
				t.Errorf("words = %q, %v, want %q", got, ok, tt.want)
			}
		})
	}
	for _, out := range []Item{
		{Type: TypeMessage, ID: 1, Kind: "worker_result", Author: "worker", Content: `{"status":"done"}`, CreatedAt: wordsStamp},
		{Type: TypeMessage, ID: 2, Kind: "task_done", Author: "worker", CreatedAt: wordsStamp},
		wordsEvent("job_created", 3, 5, `{"branch":"task/3-1"}`),
		wordsEvent("job_leased", 3, 5, `{}`),
		wordsEvent("job_cap_held", 3, 5, `{}`),
		wordsEvent("job_agent_picked", 3, 5, `{}`),
		wordsEvent("task_running", 3, 0, `{}`),
	} {
		if words, ok := EventWords(out, nil); ok {
			t.Errorf("%s %s has words %q", out.Type, out.Kind, words)
		}
	}
}

func readable(t *testing.T, items []Item, full bool) string {
	t.Helper()
	var b bytes.Buffer
	if err := RenderReadable(&b, items, ReadableOptions{Full: full, Loc: time.UTC}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func readableItems() []Item {
	return []Item{
		{Type: TypeMessage, ID: 1, Kind: "task_spec", ProjectID: 1, TaskID: 1, Author: "foreman", Content: "Do it.\nCarefully.", CreatedAt: "2026-10-20T15:00:00.000000000Z"},
		wordsEvent("job_created", 1, 5, `{"branch":"task/1"}`),
		wordsEvent("job_leased", 1, 5, `{"lease":"x"}`),
		wordsEvent("job_cap_held", 1, 5, `{}`),
		wordsEvent("job_agent_picked", 1, 5, `{"agent":2}`),
		{Type: TypeMessage, ID: 2, Kind: "worker_result", ProjectID: 1, TaskID: 1, JobID: 5, Author: "worker",
			Content: `{"status":"done","summary":"All good.","discoveries":"a quirk"}`, CreatedAt: wordsStamp},
		wordsEvent("task_landed", 1, 5, `{"status":"done","branch":"main","commit":"9f21ab3c4d5e6f708192a3b4c5d6e7f890123456","pushed":"local","push_cmd":"git push x","after_land_error":"exit 3: boom"}`),
		wordsEvent("foreman_report", 0, 12, `{"report":"line one\nline two"}`),
		wordsEvent("review_verdict", 0, 9, `{"status":"changes","branch":"fix/x","findings":"`+strings.Repeat("finding ", 700)+`END"}`),
	}
}

// TestRenderReadableBare: the bare view shows the shared words only, with
// a landing's record whole, and hides internal kinds and messages.
func TestRenderReadableBare(t *testing.T) {
	got := readable(t, readableItems(), false)
	for _, want := range []string{
		"10-20 15:02  #1 landed, push waits\n",
		"  branch=main commit=9f21ab3c4d5e6f708192a3b4c5d6e7f890123456\n",
		"  push_cmd=git push x\n",
		"  after_land_error=exit 3: boom\n",
		"10-20 15:02  report #12\n",
		"10-20 15:02  job 9 review changes fix/x\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bare view lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"job_leased", "job_cap_held", "job_created", "job_agent_picked", "Do it.", "{", "000000000Z", "line one", "finding"} {
		if strings.Contains(got, bad) {
			t.Errorf("bare view shows %q:\n%s", bad, got)
		}
	}
}

// TestRenderReadableFull: --task and --job show every item readably, the
// report and a long verdict's findings whole, a worker_result as text.
func TestRenderReadableFull(t *testing.T) {
	got := readable(t, readableItems(), true)
	for _, want := range []string{
		"10-20 15:00  foreman task spec: Do it.\n  Carefully.\n",
		"job 5 job created: branch=task/1",
		"job 5 job leased",
		"job 5 job cap held",
		"job 5 job agent picked",
		"worker result: status=done\n  summary: All good.\n  discoveries: a quirk\n",
		"#1 landed, push waits: branch=main status=done commit=9f21ab3c4d5e6f708192a3b4c5d6e7f890123456 pushed=local push_cmd=git push x after_land_error=exit 3: boom",
		"report #12: report=line one\n  line two\n",
		"job 9 review changes fix/x: branch=fix/x status=changes",
		"finding END\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("full view lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "finding "); n < 700 || len(got) < 4096 {
		t.Errorf("findings cut: %d repeats, %d bytes", n, len(got))
	}
	for _, bad := range []string{`"status"`, "000000000Z"} {
		if strings.Contains(got, bad) {
			t.Errorf("full view shows %q:\n%s", bad, got)
		}
	}
}

// TestRenderReadableOddInput: a stamp that does not parse shows as it is,
// and worker_result content that is not JSON prints whole.
func TestRenderReadableOddInput(t *testing.T) {
	items := []Item{
		{Type: TypeMessage, ID: 1, Kind: "worker_result", Author: "worker", Content: "plain text, not JSON", CreatedAt: "yesterday"},
	}
	if got := readable(t, items, true); got != "yesterday  worker result: plain text, not JSON\n" {
		t.Errorf("got %q", got)
	}
}

// TestRenderReadableLocalTime: the time is in the given zone.
func TestRenderReadableLocalTime(t *testing.T) {
	var b bytes.Buffer
	loc := time.FixedZone("x", 2*3600)
	items := []Item{wordsEvent("task_done", 3, 0, `{}`)}
	if err := RenderReadable(&b, items, ReadableOptions{Loc: loc}); err != nil {
		t.Fatal(err)
	}
	if b.String() != "10-20 17:02  #3 done\n" {
		t.Errorf("got %q", b.String())
	}
}
