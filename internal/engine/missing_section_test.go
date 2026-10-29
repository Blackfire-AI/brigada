package engine

import (
	"strings"
	"testing"
)

// TestNeedsHelpMissingSectionParksForTheForeman covers issue #129: a
// worker that cannot find a section its spec cites ends with needs_help;
// the task parks at needs_foreman, the foreman's wake carries the
// question verbatim, and no review or land job follows.
func TestNeedsHelpMissingSectionParksForTheForeman(t *testing.T) {
	e := newEnv(t)
	question := "design.md §9.9 is missing at brigada/landed@0123abc; which section did you mean?"
	e.park(question)

	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want %q", state, taskNeedsForeman)
	}
	job := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	want := "<event>\n" + e.wakeLine(evTaskNeedsHelp, question) + "\n</event>"
	if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", job); !strings.Contains(prompt, want) {
		t.Errorf("wake prompt = %q, want it to carry the question verbatim:\n%s", prompt, want)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind IN ('review', 'land')", e.task); n != 0 {
		t.Errorf("review or land jobs created = %d, want none", n)
	}
}
