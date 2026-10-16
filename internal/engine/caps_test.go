package engine

import (
	"maps"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// seedAgent gives a seeded runner one advertised agent, the way a
// register would. The agent takes every class: these tests place jobs
// by harness and caps, not by class.
func (e *env) seedAgent(runner int64, adapter, harness, model, account string) {
	e.t.Helper()
	e.exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '["implement","review","answer"]', ?, ?)`, runner, adapter, harness, model, adapter, account, seedTS, seedTS)
}

// addPlan adds another plan to the fixture's project, and addPlanTask a
// work submit for a task of it.
func (e *env) addPlan() int64 {
	e.t.Helper()
	e.exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
		VALUES (?, 'another plan', 'active', ?, ?)`, e.project, seedTS, seedTS)
	return e.int("SELECT id FROM plans WHERE title = 'another plan'")
}

func (e *env) addPlanTask(plan int64) SubmitParams {
	e.t.Helper()
	e.exec(`INSERT INTO tasks (plan_id, title, spec, class, state, base_ref, caps, created_at, updated_at)
		VALUES (?, 'a task of another plan', 'the spec', 'implement', 'ready', 'main', '{}', ?, ?)`, plan, seedTS, seedTS)
	p := e.work()
	p.TaskID = e.int("SELECT id FROM tasks WHERE plan_id = ?", plan)
	p.Key = keyOf(p.TaskID)
	return p
}

func equalUsage(a, b caps.Usage) bool {
	return a.Global == b.Global && maps.Equal(a.Runner, b.Runner) &&
		maps.Equal(a.Account, b.Account) && maps.Equal(a.Model, b.Model) && maps.Equal(a.Plan, b.Plan)
}

// TestCapsUsageCountsLiveJobs builds the §8.1 usage from the store: a
// few leased jobs on two runners, two accounts and two plans, counted
// per runner (the lease's holder), per account and model (the recorded
// agent), per plan (the task's) and globally — with a running job as
// live as a leased one, and the terminal and the still-queued jobs
// counting nowhere.
func TestCapsUsageCountsLiveJobs(t *testing.T) {
	e := newEnv(t)
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.seedAgent(e.runner2, "kimi", "kimi", "k2", "personal")
	plan2 := e.addPlan()

	first := e.submit(e.work())              // plan 1, runner 1
	second := e.submit(e.addPlanTask(plan2)) // plan 2, runner 2
	g1 := e.leaseAny(e.runner1)
	g2 := e.leaseAny(e.runner2)
	if g1 == nil || g1.JobID != first.JobID || g2 == nil || g2.JobID != second.JobID {
		t.Fatalf("leases = %v, %v; want jobs %d, %d", g1, g2, first.JobID, second.JobID)
	}
	turn := e.submit(e.foremanTurn()) // runner 1 again, on its first agent
	gf := e.leaseClaude(e.runner1)
	if gf == nil || gf.JobID != turn.JobID {
		t.Fatalf("foreman lease = %v, want job %d", gf, turn.JobID)
	}

	usage, err := e.eng.CapsUsage(e.ctx)
	if err != nil {
		t.Fatalf("caps usage: %v", err)
	}
	want := caps.Usage{
		Runner:  map[int64]int{e.runner1: 2, e.runner2: 1},
		Account: map[string]int{"work": 2, "personal": 1},
		Model:   map[string]int{"sonnet": 2, "k2": 1},
		Plan:    map[int64]int{e.plan: 1, plan2: 1},
		Global:  3,
	}
	if !equalUsage(usage, want) {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}

	// A running job is as live as a leased one.
	e.exec("UPDATE jobs SET status = 'running' WHERE id = ?", g1.JobID)
	if usage, err = e.eng.CapsUsage(e.ctx); err != nil {
		t.Fatalf("caps usage: %v", err)
	}
	if !equalUsage(usage, want) {
		t.Errorf("usage with a running job = %+v, want it still counted", usage)
	}

	// A completed job frees every layer; a queued one holds none.
	if ans := e.complete(gf, done()); ans != AnsOK {
		t.Fatalf("complete the foreman turn: %s", ans)
	}
	e.submit(e.addPlanTask(plan2))
	if usage, err = e.eng.CapsUsage(e.ctx); err != nil {
		t.Fatalf("caps usage: %v", err)
	}
	want = caps.Usage{
		Runner:  map[int64]int{e.runner1: 1, e.runner2: 1},
		Account: map[string]int{"work": 1, "personal": 1},
		Model:   map[string]int{"sonnet": 1, "k2": 1},
		Plan:    map[int64]int{e.plan: 1, plan2: 1},
		Global:  2,
	}
	if !equalUsage(usage, want) {
		t.Errorf("usage after one ends and one queues = %+v, want %+v", usage, want)
	}
}
