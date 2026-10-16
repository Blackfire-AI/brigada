package notify

import (
	"strings"
	"testing"
)

func TestMessage(t *testing.T) {
	cases := []struct {
		name  string
		ev    Event
		title string
		body  string
		ok    bool
	}{
		{
			name:  "plan awaiting approval",
			ev:    Event{Kind: KindPlanAwaitingApproval, PlanID: 9, Reason: "6 tasks, over the limit of 5"},
			title: "brigada",
			body:  "plan #9 needs your approval: 6 tasks, over the limit of 5: `brigada plan approve 9` or `brigada plan reject 9`",
			ok:    true,
		},
		{
			name:  "plan done",
			ev:    Event{Kind: KindPlanDone, PlanID: 7, PlanTitle: "Fix the parser"},
			title: "brigada",
			body:  "plan #7 done: Fix the parser",
			ok:    true,
		},
		{
			name:  "foreman report reads its first line",
			ev:    Event{Kind: KindForemanReport, Reason: "plan #2 done: merge task/6-1 then task/7-1\nthe rest waits for you"},
			title: "brigada foreman",
			body:  "plan #2 done: merge task/6-1 then task/7-1",
			ok:    true,
		},
		{
			name:  "foreman report needs no park",
			ev:    Event{Kind: KindForemanReport, Reason: "all quiet; nothing needs you"},
			title: "brigada foreman",
			body:  "all quiet; nothing needs you",
			ok:    true,
		},
		{
			name:  "foreman report falls back to the kind",
			ev:    Event{Kind: KindForemanReport},
			title: "brigada foreman",
			body:  "foreman_report",
			ok:    true,
		},
		{
			name:  "check failed",
			ev:    Event{Kind: KindTaskCheckFailed, Parked: true, TaskID: 3, Reason: "exit 1: parser_test.go:42"},
			title: "brigada",
			body:  "task #3 needs you: exit 1: parser_test.go:42",
			ok:    true,
		},
		{
			name:  "needs help reads the question",
			ev:    Event{Kind: KindTaskNeedsHelp, Parked: true, TaskID: 4, Reason: "ignored", PlanTitle: "x"},
			title: "brigada",
			body:  "task #4 needs you: ignored",
			ok:    true,
		},
		{
			name:  "run failed",
			ev:    Event{Kind: KindTaskFailed, Parked: true, TaskID: 5, Reason: "the CLI died mid-run"},
			title: "brigada",
			body:  "task #5 needs you: the CLI died mid-run",
			ok:    true,
		},
		{
			name:  "review changes read the findings",
			ev:    Event{Kind: KindReviewChanges, Parked: true, TaskID: 6, Reason: "the off-by-one in retry"},
			title: "brigada",
			body:  "task #6 needs you: the off-by-one in retry",
			ok:    true,
		},
		{
			name:  "no verdict",
			ev:    Event{Kind: KindReviewNoVerdict, Parked: true, TaskID: 7, Reason: "verdict block missing"},
			title: "brigada",
			body:  "task #7 needs you: verdict block missing",
			ok:    true,
		},
		{
			name:  "out of rounds, once the engine emits it",
			ev:    Event{Kind: KindTaskOutOfRounds, Parked: true, TaskID: 8, Reason: "fix rounds spent"},
			title: "brigada",
			body:  "task #8 needs you: fix rounds spent",
			ok:    true,
		},
		{
			name:  "foreman wake budget spent",
			ev:    Event{Kind: KindForemanWakeBudgetSpent, Reason: "20/20 today, resets 2026-10-17T00:00:00.000000000Z"},
			title: "brigada",
			body:  "the foreman's wake budget is spent (20/20 today, resets 2026-10-17T00:00:00.000000000Z); raise it with `brigada budget --wakes N` or send `brigada ask`",
			ok:    true,
		},
		{
			name:  "foreman wake budget spent, no window named",
			ev:    Event{Kind: KindForemanWakeBudgetSpent},
			title: "brigada",
			body:  "the foreman's wake budget is spent (resets at the next UTC day); raise it with `brigada budget --wakes N` or send `brigada ask`",
			ok:    true,
		},
		{
			name:  "empty reason falls back to the kind",
			ev:    Event{Kind: KindTaskCheckFailed, Parked: true, TaskID: 9},
			title: "brigada",
			body:  "task #9 needs you: task_check_failed",
			ok:    true,
		},
		{
			name:  "reason flattens to one line",
			ev:    Event{Kind: KindTaskCheckFailed, Parked: true, TaskID: 2, Reason: "first line\nsecond\tline"},
			title: "brigada",
			body:  "task #2 needs you: first line second line",
			ok:    true,
		},
		// Everything else in the log never notifies.
		{name: "job created", ev: Event{Kind: "job_created", TaskID: 1}},
		{name: "task done", ev: Event{Kind: "task_done", TaskID: 1}},
		{name: "plan created", ev: Event{Kind: "plan_created"}},
		{name: "foreman set", ev: Event{Kind: "project_foreman_set"}},
		{name: "lease events", ev: Event{Kind: "job_leased", TaskID: 1}},
		{name: "empty kind", ev: Event{}},
		// A failed check or a changes verdict that a fix round answers
		// lands without parking the task: no banner.
		{name: "check failed without park", ev: Event{Kind: KindTaskCheckFailed, TaskID: 1, Reason: "exit 1"}},
		{name: "review changes without park", ev: Event{Kind: KindReviewChanges, TaskID: 1, Reason: "nit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body, ok := Message(tc.ev)
			if ok != tc.ok {
				t.Fatalf("Message ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if title != tc.title {
				t.Errorf("title = %q, want %q", title, tc.title)
			}
			if body != tc.body {
				t.Errorf("body = %q, want %q", body, tc.body)
			}
			if strings.Contains(body, "\n") {
				t.Errorf("body %q is not one line", body)
			}
		})
	}
}

func TestMessageBodyLimit(t *testing.T) {
	ev := Event{Kind: KindTaskCheckFailed, Parked: true, TaskID: 1, Reason: strings.Repeat("x", bodyLimit*2)}
	_, body, ok := Message(ev)
	if !ok {
		t.Fatal("a long reason must still notify")
	}
	if len(body) > bodyLimit+len("task #1 needs you: ")+4 {
		t.Errorf("body is %d bytes, want it bounded near %d", len(body), bodyLimit)
	}
	if !strings.HasSuffix(body, "x") {
		t.Errorf("body ends mid-rune: %q", body)
	}

	// A report's banner is bounded the same way: its first line alone,
	// cut at the limit, the lines after it never joined in.
	report := Event{Kind: KindForemanReport, Reason: strings.Repeat("y", bodyLimit*2) + "\nsecond line"}
	title, body, ok := Message(report)
	if !ok {
		t.Fatal("a long report must still notify")
	}
	if title != foremanTitle {
		t.Errorf("title = %q, want %q", title, foremanTitle)
	}
	if len(body) > bodyLimit {
		t.Errorf("report body is %d bytes, want it bounded at %d", len(body), bodyLimit)
	}
	if strings.Contains(body, "second line") {
		t.Errorf("report body = %q, want the first line alone", body)
	}
}

func TestParkedBy(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"needs_foreman parks", `{"status":"needs_foreman","reason":"exit 1"}`, true},
		{"needs_user parks", `{"status":"needs_user"}`, true},
		{"no status does not park", `{"reason":"exit 1"}`, false},
		{"another state does not park", `{"status":"running"}`, false},
		{"empty payload does not park", "", false},
		{"malformed payload does not park", "{not json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parkedBy(tc.payload); got != tc.want {
				t.Errorf("parkedBy(%q) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

func TestReasonOf(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		payload string
		want    string
	}{
		{"question wins for needs help", KindTaskNeedsHelp,
			`{"question":"how deep?\nnow","reason":"tail"}`, "how deep? now"},
		{"findings win for review changes", KindReviewChanges,
			`{"findings":"off by one","reason":"tail"}`, "off by one"},
		{"a report keeps its first line", KindForemanReport,
			`{"report":"plan #3 done: merge task/4-1\nthen task/5-1"}`, "plan #3 done: merge task/4-1"},
		{"a report that names nothing yields nothing", KindForemanReport, `{"reason":"tail"}`, ""},
		{"the budget spent reads count and window end", KindForemanWakeBudgetSpent,
			`{"count":20,"until":"2026-10-17T00:00:00.000000000Z"}`, "20/20 today, resets 2026-10-17T00:00:00.000000000Z"},
		{"the budget spent without a count keeps the window", KindForemanWakeBudgetSpent,
			`{"until":"2026-10-17T00:00:00.000000000Z"}`, "resets 2026-10-17T00:00:00.000000000Z"},
		{"the budget spent that names nothing yields nothing", KindForemanWakeBudgetSpent, `{"reason":"tail"}`, ""},
		{"reason for the rest", KindTaskCheckFailed,
			`{"reason":"exit 3\nboom"}`, "exit 3 boom"},
		{"status-only payload yields nothing", KindTaskFailed, `{"status":"needs_foreman"}`, ""},
		{"empty payload yields nothing", KindTaskFailed, "", ""},
		{"malformed payload yields nothing", KindTaskFailed, "{not json", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasonOf(tc.kind, tc.payload); got != tc.want {
				t.Errorf("reasonOf = %q, want %q", got, tc.want)
			}
		})
	}
}
