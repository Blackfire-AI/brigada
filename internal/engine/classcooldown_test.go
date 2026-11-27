package engine

import (
	"database/sql"
	"testing"
	"time"
)

// The representative outcome accounting and class-scoped reliability
// cooldown acceptance (issue #263, criterion 6): a changes verdict counts
// as a successful review, unusable verdicts count as failures, quota and
// infrastructure failures never touch quality history, the cooldown backs
// off on repeated contract failures and resets on a valid completion,
// and a zero cost weight bypasses nothing.

// classCooldownOf reads an agent's contract-failure state on a class; a
// missing row reads 0, "", 0.
func (e *env) classCooldownOf(agent int64, class string) (streak int64, until string, step int64) {
	e.t.Helper()
	return e.int("SELECT COALESCE((SELECT streak FROM class_cooldown WHERE agent_id = ? AND class = ?), 0)", agent, class),
		e.str("SELECT COALESCE((SELECT cooling_until FROM class_cooldown WHERE agent_id = ? AND class = ?), '')", agent, class),
		e.int("SELECT COALESCE((SELECT step FROM class_cooldown WHERE agent_id = ? AND class = ?), 0)", agent, class)
}

// stallReview leases the fixture task's review to the runner and
// completes it stalled: a contract failure of the review class.
func (e *env) stallReview(runner int64) {
	e.t.Helper()
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' AND status = 'queued'", e.task)
	g := e.leaseAny(runner)
	if g == nil || g.JobID != review {
		e.t.Fatalf("the reviewer leased %+v, want the review %d", g, review)
	}
	if ans := e.complete(g, Completion{Result: `{"status":"stalled","summary":"the review stalled"}`, Outcome: OutcomeStalled}); ans != AnsOK {
		e.t.Fatalf("complete the stalled review: %s", ans)
	}
}

// TestClassCooldownBacksOffOnRepeatedContractFailures: the first contract
// failure records no cooldown, the second cools the agent on the review
// class for the base window, and each further consecutive failure doubles
// it. While the cooldown lasts the agent never wins a lease — the job
// holds for the expiry like any cooling wait — and a valid completion
// once it lapses resets the streak, the window and the backoff step.
func TestClassCooldownBacksOffOnRepeatedContractFailures(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "review")
	agent := e.agentOf(reviewer)

	// The first failure is noise: a streak, no cooldown.
	e.runToReview(author)
	e.stallReview(reviewer)
	if streak, until, _ := e.classCooldownOf(agent, "review"); streak != 1 || until != "" {
		t.Fatalf("after one failure: streak %d until %q, want 1 and no cooldown", streak, until)
	}

	// The second consecutive failure starts the base window.
	e.task = e.addTask("{}")
	e.runToReview(author)
	e.stallReview(reviewer)
	base := e.clock.Now().Add(classCooldownBase)
	if streak, until, step := e.classCooldownOf(agent, "review"); streak != 2 || until != fmtTime(base) || step != 0 {
		t.Fatalf("after two failures: streak %d until %q step %d, want 2, %s, 0", streak, until, step, fmtTime(base))
	}

	// The third doubles it — once the second window has lapsed.
	e.clock.Advance(classCooldownBase + time.Second)
	e.task = e.addTask("{}")
	e.runToReview(author)
	e.stallReview(reviewer)
	doubled := e.clock.Now().Add(2 * classCooldownBase)
	if streak, until, step := e.classCooldownOf(agent, "review"); streak != 3 || until != fmtTime(doubled) || step != 1 {
		t.Fatalf("after three failures: streak %d until %q step %d, want 3, %s, 1", streak, until, step, fmtTime(doubled))
	}

	// Quarantined: the next review does not lease to the agent, and the
	// sweep holds it on the cooling layer for the expiry — the quarantine
	// is a bounded wait, not a park.
	e.task = e.addTask("{}")
	review := e.runToReview(author)
	if g := e.leaseAny(reviewer); g != nil {
		t.Fatalf("the quarantined reviewer leased job %d", g.JobID)
	}
	e.mustSweep()
	if got := e.heldLayer(review); got != layerCooling {
		t.Fatalf("hold layer = %q, want %q", got, layerCooling)
	}
	if st := e.str("SELECT status FROM jobs WHERE id = ?", review); st != statusQueued {
		t.Fatalf("review status = %q, want queued behind the cooldown", st)
	}

	// Once the window lapses the agent takes the review, and a
	// substantive approve resets the whole state.
	e.clock.Advance(2*classCooldownBase + time.Second)
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	if n := e.int("SELECT COUNT(*) FROM class_cooldown WHERE agent_id = ?", agent); n != 0 {
		t.Fatalf("class_cooldown rows after the valid completion = %d, want none", n)
	}

	// The backoff starts over: two fresh failures cool for the base
	// window again, not the doubled one.
	e.task = e.addTask("{}")
	e.runToReview(author)
	e.stallReview(reviewer)
	e.task = e.addTask("{}")
	e.runToReview(author)
	e.stallReview(reviewer)
	fresh := e.clock.Now().Add(classCooldownBase)
	if streak, until, step := e.classCooldownOf(agent, "review"); streak != 2 || until != fmtTime(fresh) || step != 0 {
		t.Fatalf("after the reset and two failures: streak %d until %q step %d, want 2, %s, 0", streak, until, step, fmtTime(fresh))
	}
}

// TestClassCooldownWindowCaps: the doubled window never grows past the
// cap, however long the failure streak runs.
func TestClassCooldownWindowCaps(t *testing.T) {
	e := newEnv(t)
	reviewer := e.agented("reviewer", "google", "review")
	agent := e.agentOf(reviewer)
	now := e.clock.Now()
	for i := 0; i < 12; i++ {
		if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
			return recordClassFailureTx(tx, agent, "review", now)
		}); err != nil {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	if streak, until, _ := e.classCooldownOf(agent, "review"); streak != 12 || until != fmtTime(now.Add(classCooldownMax)) {
		t.Fatalf("after 12 failures: streak %d until %q, want 12 and the %s cap", streak, until, classCooldownMax)
	}
}

// TestChangesVerdictResetsTheCooldown: a substantive changes verdict is a
// successful review — one attempt, one success — and a valid completion
// like any other, so it clears the failure streak a prior stall earned.
func TestChangesVerdictResetsTheCooldown(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "review")
	agent := e.agentOf(reviewer)

	e.runToReview(author)
	e.stallReview(reviewer)
	e.task = e.addTask("{}")
	e.runToReview(author)
	e.runReview(reviewer, `{"verdict":"changes","findings":"`+substantiveFindings+`"}`)
	if a, s := e.outcomeOf(agent, "review"); a != 2 || s != 1 {
		t.Errorf("outcome after the stall and the changes verdict = %d/%d, want 2/1", a, s)
	}
	if n := e.int("SELECT COUNT(*) FROM class_cooldown WHERE agent_id = ?", agent); n != 0 {
		t.Errorf("class_cooldown rows after the changes verdict = %d, want none", n)
	}
}

// TestQuarantinedReviewerNeverWinsTheLease: a quarantined agent stays
// visible to the placement's unavailable lookup — the cooling hold names
// it and waits for its expiry — but never wins a lease, whatever its cost
// weight: a free (weight 0) quarantined reviewer loses to a paid one, and
// alone it holds the job rather than taking it.
func TestQuarantinedReviewerNeverWinsTheLease(t *testing.T) {
	t.Run("a free quarantined reviewer loses to a paid one", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) {
			c.Caps.Cost = map[string]int{"cheap/google-1": 0}
		})
		author := e.agented("author", "anthropic", "implement")
		cheap := e.agented("cheap", "google", "review")
		spare := e.agented("spare", "meta", "review")
		until := e.clock.Now().Add(5 * time.Minute)
		e.exec(`INSERT INTO class_cooldown (agent_id, class, streak, cooling_until, step, updated_at)
			VALUES (?, 'review', 2, ?, 0, ?)`, e.agentOf(cheap), fmtTime(until), seedTS)

		review := e.runToReview(author)
		if g := e.leaseAny(cheap); g != nil {
			t.Fatalf("the free quarantined reviewer leased job %d", g.JobID)
		}
		g := e.leaseAny(spare)
		if g == nil || g.JobID != review {
			t.Fatalf("the spare reviewer leased %+v, want the review %d", g, review)
		}
		if got := e.jobAgent(review); got != e.agentOf(spare) {
			t.Errorf("review agent = %d, want the spare's %d", got, e.agentOf(spare))
		}
	})
	t.Run("alone it holds the job for the expiry", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) {
			c.Caps.Cost = map[string]int{"cheap/google-1": 0}
		})
		author := e.agented("author", "anthropic", "implement")
		cheap := e.agented("cheap", "google", "review")
		until := e.clock.Now().Add(5 * time.Minute)
		e.exec(`INSERT INTO class_cooldown (agent_id, class, streak, cooling_until, step, updated_at)
			VALUES (?, 'review', 2, ?, 0, ?)`, e.agentOf(cheap), fmtTime(until), seedTS)

		review := e.runToReview(author)
		if g := e.leaseAny(cheap); g != nil {
			t.Fatalf("the quarantined reviewer leased job %d", g.JobID)
		}
		e.mustSweep()
		if got := e.heldLayer(review); got != layerCooling {
			t.Fatalf("hold layer = %q, want %q", got, layerCooling)
		}
		e.clock.Advance(5*time.Minute + time.Second)
		g := e.leaseAny(cheap)
		if g == nil || g.JobID != review {
			t.Fatalf("lease after the expiry = %+v, want the review %d", g, review)
		}
	})
}

// TestZeroCostWeightBypassesNothing: a weight of 0 ranks before every
// positive weight, but the ranking runs after the placement's other
// stages — the attempt's exclusions and the capability floor hold a free
// agent out exactly as they hold a paid one.
func TestZeroCostWeightBypassesNothing(t *testing.T) {
	t.Run("the attempt's exclusion", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) {
			c.Caps.Cost = map[string]int{"cheap/google-1": 0}
		})
		author := e.agented("author", "anthropic", "implement")
		cheap := e.agented("cheap", "google", "review")
		spare := e.agented("spare", "meta", "review")
		e.runToReview(author)
		e.stallReview(cheap) // the free reviewer fails and joins the exclusion set
		retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
		if g := e.leaseAny(cheap); g != nil {
			t.Fatalf("the excluded free reviewer leased job %d", g.JobID)
		}
		g := e.leaseAny(spare)
		if g == nil || g.JobID != retry {
			t.Fatalf("the spare reviewer leased %+v, want the retry %d", g, retry)
		}
	})
	t.Run("the capability floor", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) {
			c.Caps.Cost = map[string]int{"cheap/claude-haiku-4": 0}
		})
		light := e.addAgent(e.runner1, "cheap", "claude-haiku-4", "cheap")
		std := e.addAgent(e.runner2, "std", "claude-sonnet-4", "std")
		e.exec("UPDATE tasks SET effective_tier = 'standard' WHERE id = ?", e.task)
		e.submit(e.work())
		if g := e.leaseAny(e.runner1); g != nil {
			t.Fatalf("the free light agent leased job %d below the floor", g.JobID)
		}
		g := e.leaseAny(e.runner2)
		if g == nil || g.Kind != KindWork {
			t.Fatalf("the standard agent leased %+v, want the work job", g)
		}
		if got := e.jobAgent(g.JobID); got != std {
			t.Errorf("work agent = %d, want the standard %d", got, std)
		}
		if light == std {
			t.Fatal("the two agents are one")
		}
	})
}

// TestAvailabilityFailuresNeverChargeQualityHistory: a reviewer that
// could not run at all — its account hit a limit, its login failed, its
// setup broke, the runner's plumbing failed — is an availability event:
// the review requeues or retries without an outcome row, without a
// failure streak, and without a cooldown.
func TestAvailabilityFailuresNeverChargeQualityHistory(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Completion
	}{
		{name: "quota", c: Completion{Error: "rate limited", ErrorKind: "limit"}},
		{name: "auth", c: Completion{Error: "login expired", ErrorKind: "auth"}},
		{name: "setup", c: Completion{Error: "setup failed: the checkout broke", ErrorKind: ErrorKindSetup}},
		{name: "infrastructure", c: Completion{Error: "the runner lost the worktree", ErrorKind: ErrorKindInfra}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "review")
			review := e.runToReview(author)
			g := e.leaseAny(reviewer)
			if g == nil || g.JobID != review {
				t.Fatalf("the reviewer leased %+v, want the review %d", g, review)
			}
			if ans := e.complete(g, tc.c); ans != AnsOK {
				t.Fatalf("complete the unavailable review: %s", ans)
			}
			if a, s := e.outcomeOf(e.agentOf(reviewer), "review"); a != 0 || s != 0 {
				t.Errorf("outcome after the %s failure = %d/%d, want 0/0", tc.name, a, s)
			}
			if n := e.int("SELECT COUNT(*) FROM class_cooldown"); n != 0 {
				t.Errorf("class_cooldown rows after the %s failure = %d, want none", tc.name, n)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
				t.Errorf("task state after the %s failure = %q, want reviewing", tc.name, state)
			}
		})
	}
}
