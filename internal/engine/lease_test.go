package engine

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLeaseTakesOldestQueuedFirst(t *testing.T) {
	e := newEnv(t)
	task2 := e.addTask("{}")

	old := e.submit(e.work())
	e.clock.Advance(time.Minute)
	p2 := e.work()
	p2.Key = keyOf(task2)
	p2.TaskID = task2
	fresh := e.submit(p2)

	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("lease found nothing")
	}
	if g.JobID != old.JobID {
		t.Errorf("lease took job %d; want the oldest job %d", g.JobID, old.JobID)
	}
	g = e.leaseAny(e.runner1)
	if g == nil || g.JobID != fresh.JobID {
		t.Errorf("second lease took %+v; want job %d", g, fresh.JobID)
	}
	if g = e.leaseAny(e.runner1); g != nil {
		t.Errorf("third lease took job %d; want nothing", g.JobID)
	}
}

func keyOf(task int64) string {
	return "task:" + strconv.FormatInt(task, 10)
}

func TestLeaseFiltersByHarness(t *testing.T) {
	e := newEnv(t)
	task2 := e.addTask("{}")

	gemini := e.work()
	gemini.Harness = "gemini"
	geminiJob := e.submit(gemini)
	e.clock.Advance(time.Minute)
	claude := e.work()
	claude.Harness = "claude"
	claude.Key = keyOf(task2)
	claude.TaskID = task2
	claudeJob := e.submit(claude)

	g := e.lease(e.runner1, []string{"claude"}, nil)
	if g == nil || g.JobID != claudeJob.JobID {
		t.Fatalf("a claude runner got %+v; want the claude job %d", g, claudeJob.JobID)
	}
	// The gemini job stays invisible to it, and it is the only job left.
	if g = e.lease(e.runner1, []string{"claude"}, nil); g != nil {
		t.Errorf("a claude runner got the gemini job %d; want nothing", g.JobID)
	}
	if g = e.lease(e.runner2, []string{"gemini", "claude"}, nil); g == nil || g.JobID != geminiJob.JobID {
		t.Errorf("a multi-harness runner got %+v; want the gemini job %d", g, geminiJob.JobID)
	}
}

func TestLeaseFiltersByLabels(t *testing.T) {
	e := newEnv(t)
	task2 := e.addTask("{}")

	needsGPU := e.work()
	needsGPU.Labels = []string{"gpu"}
	needsGPUJob := e.submit(needsGPU)
	e.clock.Advance(time.Minute)
	anywhere := e.work()
	anywhere.Key = keyOf(task2)
	anywhere.TaskID = task2
	anywhereJob := e.submit(anywhere)

	if g := e.lease(e.runner1, nil, nil); g == nil || g.JobID != anywhereJob.JobID {
		t.Fatalf("a plain runner got %+v; want the label-free job %d", g, anywhereJob.JobID)
	}
	if g := e.lease(e.runner1, nil, []string{"linux"}); g != nil {
		t.Errorf("a runner without the gpu label got job %d; want nothing", g.JobID)
	}
	if g := e.lease(e.runner1, nil, []string{"gpu", "linux"}); g == nil || g.JobID != needsGPUJob.JobID {
		t.Errorf("a gpu runner got %+v; want the gpu job %d", g, needsGPUJob.JobID)
	}
}

func TestLeaseFailsJobLostThreeTimes(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())

	for i := 1; i <= 3; i++ {
		g := e.leaseAny(e.runner1)
		if g == nil {
			t.Fatalf("lease %d found nothing", i)
		}
		e.expireLease()
	}
	if attempts := e.int("SELECT attempts FROM jobs WHERE id = ?", r.JobID); attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
		t.Fatalf("status = %q, want queued with the lease id kept", status)
	}

	// The next lease call fails the job instead of offering it a fourth
	// time, and finds nothing else for the runner.
	if g := e.leaseAny(e.runner2); g != nil {
		t.Errorf("lease offered the thrice-lost job %d again", g.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if result := e.str("SELECT result FROM jobs WHERE id = ?", r.JobID); !strings.Contains(result, "runner lost 3 times") {
		t.Errorf("result = %q, want the lost-run error", result)
	}
}

func TestHeartbeatRevivesThirdLease(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())

	var g *LeaseGrant
	for i := 1; i <= 3; i++ {
		g = e.leaseAny(e.runner1)
		if g == nil {
			t.Fatalf("lease %d found nothing", i)
		}
		e.expireLease()
	}

	// The lost-attempt check lives at lease time, not in the sweep: the
	// holder of the third lease can still revive it (§3).
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("heartbeat on a lapsed third lease = %s; want ok (revived)", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusLeased {
		t.Errorf("status = %q, want leased again", status)
	}
	if attempts := e.int("SELECT attempts FROM jobs WHERE id = ?", r.JobID); attempts != 3 {
		t.Errorf("attempts = %d, want 3: a revive is not a new lease", attempts)
	}

	// But once it lapses again, the next lease offer fails the job.
	e.expireLease()
	if g := e.leaseAny(e.runner2); g != nil {
		t.Errorf("lease offered job %d a fourth time", g.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusFailed {
		t.Errorf("status = %q, want failed", status)
	}
}

func TestReleaseExclusionAndLapse(t *testing.T) {
	t.Run("other runners go first", func(t *testing.T) {
		e := newEnv(t)
		r := e.submit(e.work())
		g := e.leaseAny(e.runner1)
		if g == nil {
			t.Fatal("nothing to lease")
		}
		if ans := e.release(e.runner1, g, "quota cooling"); ans != AnsOK {
			t.Fatalf("release = %s; want ok", ans)
		}
		if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
			t.Errorf("status = %q, want queued", status)
		}
		if attempts := e.int("SELECT attempts FROM jobs WHERE id = ?", r.JobID); attempts != 0 {
			t.Errorf("attempts = %d, want 0: the release refunded it", attempts)
		}
		if note := e.str("SELECT release_note FROM jobs WHERE id = ?", r.JobID); note != "quota cooling" {
			t.Errorf("release_note = %q, want the reason kept", note)
		}

		// The releasing runner is excluded; the other one takes it at once.
		if g := e.leaseAny(e.runner1); g != nil {
			t.Errorf("the releasing runner got job %d back at once", g.JobID)
		}
		if g := e.leaseAny(e.runner2); g == nil || g.JobID != r.JobID {
			t.Fatalf("another runner got %+v; want job %d", g, r.JobID)
		}
	})

	t.Run("the exclusion lapses for a lone runner", func(t *testing.T) {
		e := newEnv(t)
		r := e.submit(e.work())
		g := e.leaseAny(e.runner1)
		if g == nil {
			t.Fatal("nothing to lease")
		}
		if ans := e.release(e.runner1, g, ""); ans != AnsOK {
			t.Fatalf("release = %s; want ok", ans)
		}
		if g := e.leaseAny(e.runner1); g != nil {
			t.Fatalf("runner 1 got job %d inside the exclusion window", g.JobID)
		}
		e.clock.Advance(DefaultConfig().ReleaseExclude + time.Second)
		g = e.leaseAny(e.runner1)
		if g == nil || g.JobID != r.JobID {
			t.Fatalf("after the lapse, runner 1 got %+v; want job %d", g, r.JobID)
		}
	})
}

// TestLeaseRecordsTheAgent covers what the caps layers count by (§8.1):
// a lease sets the job's agent to the runner's first agent matching the
// job's harness — by id — or the runner's first agent when the job
// leaves the harness open, and to nothing when the runner has no such
// agent. Which job a lease takes does not change.
func TestLeaseRecordsTheAgent(t *testing.T) {
	e := newEnv(t)
	agented, err := e.eng.Register(e.ctx, "agented", 0, []AgentAdvert{
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Family: "anthropic", Account: "work"},
		{Adapter: "gemini", Harness: "gemini", Model: "flash", Family: "google", Account: "work"},
	})
	if err != nil {
		t.Fatalf("register an agented runner: %v", err)
	}
	claude := e.int("SELECT id FROM agents WHERE runner_id = ? AND harness = 'claude'", agented)
	gemini := e.int("SELECT id FROM agents WHERE runner_id = ? AND harness = 'gemini'", agented)
	if claude == 0 || gemini == 0 || claude > gemini {
		t.Fatalf("agents = claude %d, gemini %d; want both, claude first by id", claude, gemini)
	}

	// A job with a harness constraint gets the matching agent.
	task2 := e.addTask("{}")
	geminiJob := e.work()
	geminiJob.Harness = "gemini"
	geminiJob.Key = keyOf(task2)
	geminiJob.TaskID = task2
	e.submit(geminiJob)
	g := e.lease(agented, []string{"gemini", "claude"}, nil)
	if g == nil {
		t.Fatal("the agented runner found nothing")
	}
	if got := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", g.JobID); got != gemini {
		t.Errorf("agent_id = %d, want the gemini agent %d", got, gemini)
	}

	// A job that leaves the harness open gets the runner's first agent.
	open := e.submit(e.work())
	if g = e.lease(agented, []string{"gemini", "claude"}, nil); g == nil || g.JobID != open.JobID {
		t.Fatalf("second lease = %+v, want job %d", g, open.JobID)
	}
	if got := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", g.JobID); got != claude {
		t.Errorf("agent_id = %d, want the first agent %d", got, claude)
	}

	// A runner that advertises no agents records none.
	task3 := e.addTask("{}")
	plain := e.work()
	plain.Key = keyOf(task3)
	plain.TaskID = task3
	plainJob := e.submit(plain)
	if g = e.leaseAny(e.runner1); g == nil || g.JobID != plainJob.JobID {
		t.Fatalf("a plain runner got %+v, want job %d", g, plainJob.JobID)
	}
	if got := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", g.JobID); got != 0 {
		t.Errorf("agent_id = %d, want none for an agent-less runner", got)
	}
}

func TestSessionAffinity(t *testing.T) {
	e := newEnv(t)
	p := e.work()
	p.Mode = ModeSession
	r := e.submit(p)

	// The first lease has no session to respect and pins the key's
	// session to its runner.
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	if ans := e.complete(g, Completion{Result: "done", SessionID: "hs-1"}); ans != AnsOK {
		t.Fatalf("complete = %s; want ok", ans)
	}

	// A session job on that key is invisible to the other runner.
	fix := e.work()
	fix.Kind = KindFix
	fix.Mode = ModeSession
	if r2 := e.submit(fix); r2.Answer != AnsCreated {
		t.Fatalf("fix submit = %s; want created", r2.Answer)
	}
	if g := e.leaseAny(e.runner2); g != nil {
		t.Errorf("another runner got the session job %d; want it invisible", g.JobID)
	}
	g = e.leaseAny(e.runner1)
	if g == nil || g.JobID == r.JobID {
		t.Fatalf("the session's runner got %+v; want the new fix job", g)
	}

	// The completed first lease wrote the harness session id back (§4.2).
	if sid := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", p.Key); sid != "hs-1" {
		t.Errorf("harness session id = %q, want hs-1 written back", sid)
	}
	if runner := e.int("SELECT COALESCE(runner_id, 0) FROM sessions WHERE key = ?", p.Key); runner != e.runner1 {
		t.Errorf("session runner = %d, want %d", runner, e.runner1)
	}
}
