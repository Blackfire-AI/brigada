package engine

import (
	"testing"
)

// TestInfraFailureIsRetriedAsInfrastructure pins issue #46's fix: a
// fetch that could not be done — right after a wake the network is not
// up and the host does not resolve — completes as an infrastructure
// error and is retried as a transient error (§3), capped like a failed
// setup (§5.7). The task parks only once the retries are out.
func TestInfraFailureIsRetriedAsInfrastructure(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	fail := func() *LeaseGrant {
		g := e.leaseAny(author)
		if g == nil {
			t.Fatal("nothing to lease")
		}
		if ans := e.complete(g, Completion{
			Error:     "fetch the mirror of https://github.com/org/repo.git: exit status 128: fatal: unable to access 'https://github.com/org/repo.git/': Could not resolve host: github.com",
			ErrorKind: ErrorKindInfra,
		}); ans != AnsOK {
			t.Fatalf("complete = %s", ans)
		}
		return g
	}
	for i := 1; i <= infraRetries; i++ {
		fail()
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
			t.Fatalf("after infrastructure failure %d: task state = %s, want ready (retried)", i, state)
		}
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none while retries remain", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != infraRetries {
		t.Errorf("retry jobs = %d, want %d", n, infraRetries)
	}
	// The retries are spent: the foreman judges it.
	fail()
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %s, want needs_foreman once the retries are out", state)
	}
	if n := e.countEvents(evTaskFailed); n != 1 {
		t.Errorf("task_failed events = %d, want one", n)
	}
	// Neither agent health nor a fix round is charged: three failed
	// jobs on one agent are past the failure streak.
	if h := e.str("SELECT health FROM agents WHERE id = ?", e.agentOf(author)); h != "ready" {
		t.Errorf("agent health = %s, want ready", h)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 0 {
		t.Errorf("fix jobs = %d, want none", n)
	}
}
