package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestMissingSectionNeedsHelpParksForTheForeman (issue #129): a worker
// that cannot find a docs section its spec cites as its source stops
// with needs_help, naming the missing section and the ref and commit it
// read. The task parks at needs_foreman, the foreman's one wake carries
// the question verbatim, and nothing is reviewed or landed behind it —
// the question blocks the task until the foreman fixes the base or the
// spec.
func TestMissingSectionNeedsHelpParksForTheForeman(t *testing.T) {
	e := newEnv(t)
	question := "the spec's source, docs/design.md §5.9 (Three states, Bounds, Versioning), " +
		"is missing from this checkout (base brigada/landed, commit 0123456789abcdef): " +
		"fix the base or the spec; the section is not mine to write"
	e.park(question)

	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want needs_foreman", state)
	}
	if p, ok := e.eventPayloadOf(evTaskNeedsHelp); !ok || p.Question != question {
		t.Errorf("needs_help payload = %+v, ok=%v, want the question verbatim", p, ok)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns after the park = %d, want 1", got)
	}
	job := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	want := "<event>\n" + e.wakeLine(evTaskNeedsHelp, question) + "\n</event>"
	if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", job); !strings.Contains(prompt, want) {
		t.Errorf("wake prompt = %q, want it to carry the question verbatim:\n%s", prompt, want)
	}
	for _, kind := range []string{"review", "fix", "land"} {
		if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = ?", e.task, kind); n != 0 {
			t.Errorf("%s jobs = %d, want none behind a parked task", kind, n)
		}
	}
	if n := e.countEvents(evTaskLanded); n != 0 {
		t.Errorf("task_landed events = %d, want none; events %v", n, e.eventKinds())
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landings rows = %d, want none", n)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != "active" {
		t.Errorf("plan state = %q, want active with the task parked", state)
	}
	if got := e.inboxStates(); len(got) != 1 || !strings.HasPrefix(got[0], "delivered:"+fmt.Sprint(job)) {
		t.Errorf("inbox after the wake = %v, want the one event delivered to job %d", got, job)
	}
}
