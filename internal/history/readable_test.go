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

// TestEventColumns: the columns of every allowlisted kind, and the same
// allowlist as EventWords for every kind, allowlisted or not, and for a
// message of an allowlisted kind's name.
func TestEventColumns(t *testing.T) {
	tests := []struct {
		item Item
		want EventCols
	}{
		{readableEvent("plan_done", 12, 0, `{"plan":99}`), EventCols{Kind: "plan", Number: 99, What: "done"}},
		{readableEvent("plan_done", 12, 0, `{}`), EventCols{Kind: "plan", What: "done"}},
		{readableEvent("plan_approved", 0, 0, `{"plan":7}`), EventCols{Kind: "plan", Number: 7, What: "approved"}},
		{readableEvent("plan_awaiting_approval", 5, 0, `{"plan":8}`), EventCols{Kind: "plan", Number: 8, What: "awaiting approval"}},
		{readableEvent("task_done", 3, 0, `{}`), EventCols{Kind: "task", Number: 3, What: "done"}},
		{readableEvent("task_done", 0, 0, `{}`), EventCols{Kind: "task", What: "done"}},
		{readableEvent("task_landed", 3, 0, `{"pushed":"pushed"}`), EventCols{Kind: "task", Number: 3, What: "landed"}},
		{readableEvent("task_landed", 3, 0, `{"pushed":"local"}`), EventCols{Kind: "task", Number: 3, What: "landed, push waits"}},
		{readableEvent("task_pushed", 3, 0, `{}`), EventCols{Kind: "task", Number: 3, What: "pushed"}},
		{readableEvent("task_failed", 3, 0, `{}`), EventCols{Kind: "task", Number: 3, What: "failed"}},
		{readableEvent("task_land_failed", 3, 0, `{}`), EventCols{Kind: "task", Number: 3, What: "land failed"}},
		{readableEvent("review_verdict", 3, 9, `{"status":"approve","branch":"fix/x"}`), EventCols{Kind: "review", Number: 3, What: "approve", Subject: "fix/x"}},
		{readableEvent("review_verdict", 0, 9, `{"status":"changes","branch":"fix/x"}`), EventCols{Kind: "review", What: "changes", Subject: "fix/x"}},
		{readableEvent("review_verdict", 0, 9, `{}`), EventCols{Kind: "review", What: "no verdict"}},
		{readableEvent("agent_broken", 0, 0, `{"harness":"codex","model":"gpt-5"}`), EventCols{Kind: "agent", What: "broken", Subject: "codex/gpt-5"}},
		{readableEvent("agent_recovered", 0, 0, `{"harness":"codex","model":"gpt-5"}`), EventCols{Kind: "agent", What: "recovered", Subject: "codex/gpt-5"}},
		{readableEvent("agent_cooling", 3, 5, `{"account":"work","until":"U"}`), EventCols{Kind: "agent", What: "cooling", Subject: "work until <U>"}},
		{readableEvent("agent_cooling", 3, 5, `{"account":"work"}`), EventCols{Kind: "agent", What: "cooling", Subject: "work"}},
		{readableEvent("foreman_report", 0, 41, `{}`), EventCols{Kind: "report", What: "#41"}},
	}
	for _, tt := range tests {
		got, ok := EventColumns(tt.item, testClock)
		if !ok || got != tt.want {
			t.Errorf("%s: EventColumns = %+v, %v, want %+v", tt.item.Kind, got, ok, tt.want)
		}
	}
	for _, kind := range []string{"plan_done", "plan_approved", "plan_awaiting_approval", "task_done", "task_landed",
		"task_pushed", "task_failed", "task_land_failed", "review_verdict", "agent_broken", "agent_recovered",
		"agent_cooling", "foreman_report", "job_created", "job_leased", "task_running", "review_changes", "nonsense", ""} {
		for _, typ := range []string{TypeEvent, TypeMessage} {
			it := Item{Type: typ, Kind: kind, CreatedAt: readableStamp}
			_, wordsOK := EventWords(it, testClock)
			_, colsOK := EventColumns(it, testClock)
			if wordsOK != colsOK {
				t.Errorf("%s %s: EventWords says %v, EventColumns %v", typ, kind, wordsOK, colsOK)
			}
		}
	}
}

// TestRenderReadablePinsEveryKind pins history's output for every
// allowlisted kind with literal lines, so a change to the column words top
// reads cannot move what `brigada history` prints.
func TestRenderReadablePinsEveryKind(t *testing.T) {
	tests := []struct {
		item Item
		bare string
		all  string
	}{
		{readableEvent("plan_done", 12, 0, `{"plan":99,"status":"done"}`), "10-20 15:02 plan #99 done", "10-20 15:02 plan #99 done: status=done"},
		{readableEvent("plan_approved", 0, 0, `{"plan":7,"status":"active"}`), "10-20 15:02 plan #7 approved", "10-20 15:02 plan #7 approved: status=active"},
		{readableEvent("plan_awaiting_approval", 0, 0, `{"plan":8}`), "10-20 15:02 plan #8 awaiting approval", "10-20 15:02 plan #8 awaiting approval"},
		{readableEvent("task_done", 3, 0, `{"status":"done"}`), "10-20 15:02 #3 done", "10-20 15:02 #3 done: status=done"},
		{readableEvent("task_landed", 328, 0, `{"pushed":"waiting","branch":"main","commit":"abc123"}`),
			"10-20 15:02 #328 landed, push waits branch=main commit=abc123", "10-20 15:02 #328 landed, push waits: branch=main commit=abc123 pushed=waiting"},
		{readableEvent("task_landed", 329, 0, `{"pushed":"pushed"}`), "10-20 15:02 #329 landed", "10-20 15:02 #329 landed: pushed=pushed"},
		{readableEvent("task_pushed", 328, 0, `{"branch":"main"}`), "10-20 15:02 #328 pushed branch=main", "10-20 15:02 #328 pushed: branch=main"},
		{readableEvent("task_failed", 4, 0, `{"status":"failed"}`), "10-20 15:02 #4 failed", "10-20 15:02 #4 failed: status=failed"},
		{readableEvent("task_land_failed", 4, 0, `{"reason":"conflict"}`), "10-20 15:02 #4 land failed", "10-20 15:02 #4 land failed: reason=conflict"},
		{readableEvent("review_verdict", 3, 9, `{"status":"approve","branch":"fix/x"}`), "10-20 15:02 job 9 review approve fix/x", "10-20 15:02 job 9 review approve fix/x: branch=fix/x status=approve"},
		{readableEvent("review_verdict", 3, 9, `{"status":"changes","branch":"fix/x"}`), "10-20 15:02 job 9 review changes fix/x", "10-20 15:02 job 9 review changes fix/x: branch=fix/x status=changes"},
		{readableEvent("review_verdict", 0, 9, `{"status":"no_verdict"}`), "10-20 15:02 job 9 review no verdict", "10-20 15:02 job 9 review no verdict: status=no_verdict"},
		{readableEvent("agent_broken", 0, 0, `{"harness":"codex","model":"gpt-5","reason":"auth"}`), "10-20 15:02 codex/gpt-5 broken", "10-20 15:02 codex/gpt-5 broken: reason=auth"},
		{readableEvent("agent_recovered", 0, 0, `{"harness":"codex","model":"gpt-5"}`), "10-20 15:02 codex/gpt-5 recovered", "10-20 15:02 codex/gpt-5 recovered"},
		{readableEvent("agent_cooling", 3, 5, `{"account":"work","until":"2026-10-20T16:04:00.000000000Z"}`), "10-20 15:02 work cooling until 10-20 16:04", "10-20 15:02 work cooling until 10-20 16:04"},
		{readableEvent("foreman_report", 0, 41, `{"report":"all landed"}`), "10-20 15:02 report #41", "10-20 15:02 report #41: report=all landed"},
	}
	for _, tt := range tests {
		for _, all := range []bool{false, true} {
			var b bytes.Buffer
			if err := RenderReadable(&b, []Item{tt.item}, time.UTC, all); err != nil {
				t.Fatal(err)
			}
			want := tt.bare
			if all {
				want = tt.all
			}
			if got := strings.TrimSuffix(b.String(), "\n"); got != want {
				t.Errorf("%s all=%v: line = %q, want %q", tt.item.Kind, all, got, want)
			}
		}
	}
}
