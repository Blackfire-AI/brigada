package engine

import (
	"database/sql"
	"testing"
)

// failTurns runs foreman turns to a completion each, on one agent.
func failTurns(e *env, cs ...Completion) (agent int64) {
	e.t.Helper()
	for _, c := range cs {
		e.submit(e.foremanTurn())
		g := e.leaseClaude(e.runner1)
		if g == nil {
			e.t.Fatal("the foreman turn was not leased")
		}
		agent = e.int("SELECT agent_id FROM jobs WHERE id = ?", g.JobID)
		if ans := e.complete(g, c); ans != AnsOK {
			e.t.Fatalf("complete: %s", ans)
		}
	}
	return agent
}

// TestPublishFailureNeverCountsTowardTheFailureStreak: a publish failure
// (§5.4) is not the agent's health, so it neither counts as one of the
// last N failures nor masks the streak when it sits between real ones.
func TestPublishFailureNeverCountsTowardTheFailureStreak(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	publish := Completion{Error: "publish the branch, lease refused: moved", ErrorKind: ErrorKindPublish}
	real := Completion{Error: "the harness is on fire"}

	agent := failTurns(e, real, publish)
	if h := e.str("SELECT health FROM agents WHERE id = ?", agent); h != "ready" {
		t.Fatalf("health after a failure and a publish failure = %q, want ready: the publish failure is not a second failure", h)
	}
	failTurns(e, publish, publish)
	if h := e.str("SELECT health FROM agents WHERE id = ?", agent); h != "ready" {
		t.Fatalf("health after more publish failures = %q, want ready", h)
	}
	failTurns(e, real)
	if h, why := e.str("SELECT health FROM agents WHERE id = ?", agent), e.str("SELECT health_reason FROM agents WHERE id = ?", agent); h != "broken" || why != brokenFailures {
		t.Errorf("health after two real failures with publish failures between = %q (%q), want broken(%q)", h, why, brokenFailures)
	}
}

// TestPublishFailureEndsTheInfraStreak: a publish failure is not an infra
// failure, so infraStreakTx counts only the infra failures after it.
func TestPublishFailureEndsTheInfraStreak(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	infra := Completion{Error: "could not resolve host", ErrorKind: ErrorKindInfra}
	publish := Completion{Error: "publish the branch, lease refused: moved", ErrorKind: ErrorKindPublish}
	failTurns(e, infra, infra, publish, infra)

	var n int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		n, _, err = infraStreakTx(tx, e.str("SELECT key FROM jobs ORDER BY id DESC LIMIT 1"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("infra streak after infra, infra, publish, infra = %d, want 1", n)
	}
}
