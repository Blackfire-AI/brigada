package history

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const readableStamp = "2026-10-20T15:02:45.123456789Z"

func readableEvent(kind string, task, job int64, payload string) Item {
	return Item{Type: TypeEvent, ID: 1, Kind: kind, ProjectID: 1, TaskID: task, JobID: job,
		Payload: []byte(payload), CreatedAt: readableStamp}
}

func testClock(stamp string) string { return "<" + stamp + ">" }

func TestEventWords(t *testing.T) {
	tests := []struct {
		name string
		item Item
		want string
	}{
		{"plan_done", readableEvent("plan_done", 0, 0, `{"plan":99,"status":"done"}`), "plan #99 done"},
		{"plan_done without a plan", readableEvent("plan_done", 0, 0, `{}`), "plan done"},
		{"plan_approved", readableEvent("plan_approved", 0, 0, `{"plan":7,"status":"active"}`), "plan #7 approved"},
		{"plan_awaiting_approval", readableEvent("plan_awaiting_approval", 0, 0, `{"plan":8}`), "plan #8 awaiting approval"},
		{"task_done", readableEvent("task_done", 3, 0, `{"status":"done"}`), "#3 done"},
		{"task_landed", readableEvent("task_landed", 328, 0, `{"pushed":"pushed"}`), "#328 landed"},
		{"task_landed no push state", readableEvent("task_landed", 328, 0, ``), "#328 landed"},
		{"task_landed waiting", readableEvent("task_landed", 328, 0, `{"pushed":"waiting"}`), "#328 landed, push waits"},
		{"task_landed local", readableEvent("task_landed", 328, 0, `{"pushed":"local"}`), "#328 landed, push waits"},
		{"task_pushed", readableEvent("task_pushed", 328, 0, `{}`), "#328 pushed"},
		{"task_failed", readableEvent("task_failed", 4, 0, `{"status":"failed"}`), "#4 failed"},
		{"task_land_failed", readableEvent("task_land_failed", 4, 0, `{"reason":"conflict"}`), "#4 land failed"},
		{"review approve", readableEvent("review_verdict", 0, 9, `{"status":"approve","branch":"fix/x"}`), "review approve fix/x"},
		{"review changes", readableEvent("review_verdict", 0, 9, `{"status":"changes","branch":"fix/x"}`), "review changes fix/x"},
		{"review no verdict", readableEvent("review_verdict", 0, 9, `{"status":"no_verdict"}`), "review no verdict"},
		{"agent_broken", readableEvent("agent_broken", 0, 0, `{"harness":"codex","model":"gpt-5","reason":"auth"}`), "codex/gpt-5 broken"},
		{"agent_recovered", readableEvent("agent_recovered", 0, 0, `{"harness":"codex","model":"gpt-5"}`), "codex/gpt-5 recovered"},
		{"agent_cooling", readableEvent("agent_cooling", 3, 5, `{"account":"work","until":"U"}`), "work cooling until <U>"},
		{"agent_cooling without a deadline", readableEvent("agent_cooling", 3, 5, `{"account":"work"}`), "work cooling"},
		{"foreman_report", readableEvent("foreman_report", 0, 41, `{"report":"all landed"}`), "report #41"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EventWords(tt.item, testClock)
			if !ok || got != tt.want {
				t.Errorf("EventWords = %q, %v, want %q", got, ok, tt.want)
			}
		})
	}
	for _, out := range []Item{
		{Type: TypeMessage, ID: 1, Kind: "worker_result", Author: "worker", Content: `{"status":"done"}`, CreatedAt: readableStamp},
		{Type: TypeMessage, ID: 2, Kind: "task_done", Author: "worker", CreatedAt: readableStamp},
		readableEvent("job_created", 3, 5, `{"branch":"task/3-1"}`),
		readableEvent("job_leased", 3, 5, `{}`),
		readableEvent("job_cap_held", 3, 5, `{}`),
		readableEvent("job_agent_picked", 3, 5, `{}`),
		readableEvent("task_running", 3, 0, `{}`),
		readableEvent("review_changes", 3, 0, `{}`),
	} {
		if words, ok := EventWords(out, testClock); ok {
			t.Errorf("%s %s is worded as %q", out.Type, out.Kind, words)
		}
	}
}

func renderReadable(t *testing.T, items []Item, all bool) string {
	t.Helper()
	var b bytes.Buffer
	if err := RenderReadable(&b, items, time.UTC, all); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestRenderReadableBare(t *testing.T) {
	sha := "9f21ab3c4d5e6f708192a3b4c5d6e7f890123456"
	items := []Item{
		readableEvent("job_created", 3, 5, `{"branch":"task/3-1"}`),
		readableEvent("job_leased", 3, 5, `{}`),
		readableEvent("job_cap_held", 3, 5, `{}`),
		readableEvent("job_agent_picked", 3, 5, `{}`),
		{Type: TypeMessage, ID: 2, Kind: "task_spec", Author: "foreman", Content: "Do it.", CreatedAt: readableStamp},
		readableEvent("task_landed", 3, 0, `{"branch":"main","commit":"`+sha+`","pushed":"local","push_cmd":"git push origin FETCH_HEAD:main","after_land_error":"exit 3: boom"}`),
		readableEvent("review_verdict", 0, 9, `{"status":"approve","branch":"fix/x","findings":"long findings"}`),
		readableEvent("foreman_report", 0, 12, `{"report":"All landed.\nSecond line."}`),
	}
	want := "10-20 15:02 #3 landed, push waits branch=main commit=" + sha + " push_cmd=git push origin FETCH_HEAD:main after_land_error=exit 3: boom\n" +
		"10-20 15:02 job 9 review approve fix/x\n" +
		"10-20 15:02 report #12\n"
	if got := renderReadable(t, items, false); got != want {
		t.Errorf("bare view =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderReadableTimeZone(t *testing.T) {
	loc := time.FixedZone("x", 2*3600)
	var b bytes.Buffer
	items := []Item{readableEvent("task_done", 3, 0, `{}`), {Type: TypeEvent, Kind: "task_done", TaskID: 4, CreatedAt: "garbage"}}
	if err := RenderReadable(&b, items, loc, false); err != nil {
		t.Fatal(err)
	}
	if want := "10-20 17:02 #3 done\ngarbage #4 done\n"; b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}

func TestRenderReadableDetail(t *testing.T) {
	findings := strings.Repeat("finding line that runs long\n", 400) // past 4 KB
	result := `{"status":"done","summary":"Did it.\nTwo lines.","question":"which?","discoveries":"a quirk","commit_message":"feat: x"}`
	items := []Item{
		{Type: TypeMessage, ID: 1, Kind: "task_spec", Author: "foreman", Content: "Spec line one.\nSpec line two.", CreatedAt: readableStamp},
		readableEvent("job_leased", 3, 5, `{"lease":"abc"}`),
		{Type: TypeMessage, ID: 3, Kind: "worker_result", Author: "worker", Content: result, CreatedAt: readableStamp},
		{Type: TypeMessage, ID: 4, Kind: "worker_result", Author: "worker", Content: "not json at all", CreatedAt: readableStamp},
		readableEvent("review_verdict", 0, 9, `{"status":"changes","branch":"fix/x","findings":`+quote(findings)+`}`),
		readableEvent("foreman_report", 0, 12, `{"report":"All landed.\nSecond line."}`),
		readableEvent("task_asked", 3, 5, `{"question":"which way?"}`),
	}
	got := renderReadable(t, items, true)
	for _, want := range []string{
		"10-20 15:02 foreman task spec: Spec line one.\n  Spec line two.\n",
		"10-20 15:02 job 5 job leased: lease=abc\n",
		"10-20 15:02 worker worker result: status=done\n  summary=Did it.\n  Two lines.\n  question=which?\n  discoveries=a quirk\n  commit_message=feat: x\n",
		"worker worker result: not json at all\n",
		"10-20 15:02 job 9 review changes fix/x: branch=fix/x status=changes findings=finding line that runs long\n  finding line",
		"10-20 15:02 report #12: report=All landed.\n  Second line.\n",
		"10-20 15:02 job 5 task asked: question=which way?\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail view lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "finding line that runs long"); n != 400 {
		t.Errorf("findings printed %d lines, want 400 whole", n)
	}
	if strings.Contains(got, `{"status"`) || strings.Contains(got, ".123456789") {
		t.Errorf("detail view shows JSON or nanoseconds:\n%s", got)
	}
}

func TestRenderReadableDetailKeepsRaw(t *testing.T) {
	// --raw is Render, findings absent as today.
	it := readableEvent("review_verdict", 0, 9, `{"status":"changes","findings":"secret text"}`)
	var b bytes.Buffer
	if err := Render(&b, []Item{it}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "secret text") {
		t.Errorf("Render changed: %s", b.String())
	}
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}
