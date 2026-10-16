package engine

import "testing"

// A setup failure whose retry cannot be queued, the restart budget being
// spent, parks the task at the foreman and commits the completion.
func TestSetupFailureWithSpentRestartBudgetParks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.exec(`UPDATE tasks SET caps = '{"restart":0}' WHERE id = ?`, e.task)
	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, Completion{Error: "setup failed: exit code 1", ErrorKind: ErrorKindSetup}); ans != AnsOK {
		t.Fatalf("complete = %s, want ok", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %s, want needs_foreman", state)
	}
}
