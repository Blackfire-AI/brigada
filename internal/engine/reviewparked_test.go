package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSpentReviewRetryEventSaysTheTaskWaitsForTheForeman: when the retry
// is spent the event reads, to the foreman and to status alike, that the
// task is parked and waits for the foreman, one line per review, and the
// options — and stays within the event bound.
func TestSpentReviewRetryEventSaysTheTaskWaitsForTheForeman(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "implement", "review")
	second := e.agented("second", "meta", "implement", "review")
	e.submit(e.work())
	e.runWork(author, doneOn("task/1-1", passCheck))
	e.runReview(first, `{"verdict":"approve","findings":"ok"}`)
	e.runReview(second, `{}`)

	p, ok := e.eventPayloadOf(evReviewNoVerdict)
	if !ok {
		t.Fatalf("no review_no_verdict event; have %v", e.eventKinds())
	}
	head := "task 1 is parked (needs_foreman): its review retry is spent"
	if !strings.HasPrefix(p.Reason, head) {
		t.Errorf("event text = %q, want it to start %q", p.Reason, head)
	}
	for _, want := range []string{
		"first/google-1: thin approve", "second/meta-1: no verdict",
		"options: task_restart with a note, or review it by hand (brigada review --branch brigada/task-1) and land it",
	} {
		if !strings.Contains(p.Reason, want) {
			t.Errorf("event text = %q, want %q in it", p.Reason, want)
		}
	}
	if len(p.Reason) > 500 {
		t.Errorf("event text is %d bytes, want it bounded", len(p.Reason))
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	payload := string(raw)
	if got := wakeEventText(evReviewNoVerdict, payload); !strings.HasPrefix(got, head) {
		t.Errorf("the foreman's text = %q, want it to start %q", got, head)
	}
	if got := needsYouReason(evReviewNoVerdict, payload); !strings.HasPrefix(got, head) {
		t.Errorf("the status line = %q, want it to start %q", got, head)
	}
}
