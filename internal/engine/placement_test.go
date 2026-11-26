package engine

import "testing"

// The acceptance tests of the unified placement pipeline (issue #263): the
// requested tier is a capability floor for every agent-ranked job, the
// prefer family order applies within that floor, and a merely-full cap
// falls through to another family group at the same tier under prefer
// alone. The hard placements — the foreman's exact pair and the runner-only
// land and sweep jobs — keep their invariants.

// TestPreferStandardReviewKeepsTheFloor: a standard review takes a ready
// standard same-family reviewer over a light cross-family one — the light
// reviewer is below the floor, so the family preference never reaches it.
func TestPreferStandardReviewKeepsTheFloor(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Tier = map[string]string{"cross/google-1": "light"}
	})
	author := e.agented("author", "anthropic", "implement")
	same := e.agented("same", "anthropic", "review")
	cross := e.agented("cross", "google", "review")
	e.exec("UPDATE tasks SET review_tier = 'standard' WHERE id = ?", e.task)
	review := e.runToReview(author)

	if g := e.leaseAny(cross); g != nil {
		t.Fatalf("the light cross-family reviewer leased below the standard floor: %+v", g)
	}
	g := e.leaseAny(same)
	if g == nil || g.JobID != review {
		t.Fatalf("the standard same-family reviewer leased %+v, want the review %d", g, review)
	}
	if p := e.pickEvents(review); len(p) != 1 || p[0].TierAsked != "standard" || p[0].TierServed != "standard" {
		t.Errorf("pick = %+v, want the standard ask served standard", p)
	}
	if got := e.pickPlacement(review); got != placeSameFamily {
		t.Errorf("placement = %q, want %q", got, placeSameFamily)
	}
}

// TestAgentRankedJobNeverStepsDown: a standard work task whose only ready
// agent is light waits unplaced — the floor never serves the lighter agent,
// and a ready agent below the floor is no cooling hold either.
func TestAgentRankedJobNeverStepsDown(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "haiku", "light")
	job := e.tierWork("standard")
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased %+v below the standard floor, want no placement", g)
	}
	if got := e.heldLayer(job.JobID); got != "" {
		t.Errorf("hold layer = %q, want none: a ready light agent is no candidate for a standard ask", got)
	}
}

// TestPreferCapFullFamilyFallsThroughAtTheSameTier: a cap-full cross-family
// reviewer falls through to a same-family reviewer at the same tier under
// prefer; under require the cross-family wall holds and the job waits at the
// full cap instead. Capacity never moves the tier.
func TestPreferCapFullFamilyFallsThroughAtTheSameTier(t *testing.T) {
	for _, family := range []string{ReviewFamilyPrefer, ReviewFamilyRequire} {
		t.Run(family, func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) {
				c.Review.Family = family
				c.Caps.Account = map[string]int{"google": 1}
			})
			author := e.agented("author", "anthropic", "implement")
			// One runner offers a cross-family and a same-family reviewer,
			// both standard, so the fall-through stays at the asked tier.
			rv, err := e.eng.Register(e.ctx, "reviewers", 0, []AgentAdvert{
				{Adapter: "google", Harness: "cross", Model: "google-1", Family: "google", Account: "google", Classes: []string{"implement", "review"}},
				{Adapter: "anthropic", Harness: "same", Model: "anthropic-1", Family: "anthropic", Account: "anthropic", Classes: []string{"review"}},
			})
			if err != nil {
				t.Fatalf("register the reviewers: %v", err)
			}
			// Fill the cross-family reviewer's account cap with a running job.
			fill := e.submit(e.workOn(e.addTask("{}")))
			fg := e.lease(rv, []string{"cross"}, nil)
			if fg == nil || fg.JobID != fill.JobID {
				t.Fatalf("filling lease = %+v, want job %d on the cross-family agent", fg, fill.JobID)
			}
			e.exec("UPDATE tasks SET review_tier = 'standard' WHERE id = ?", e.task)
			review := e.runToReview(author)

			g := e.leaseAny(rv)
			if family == ReviewFamilyPrefer {
				if g == nil || g.JobID != review {
					t.Fatalf("lease = %+v, want the review %d fallen through to the same-family reviewer", g, review)
				}
				if got := e.pickPlacement(review); got != placeSameFamily {
					t.Errorf("placement = %q, want %q", got, placeSameFamily)
				}
				if p := e.pickEvents(review); len(p) != 1 || p[0].TierServed != "standard" {
					t.Errorf("pick = %+v, want the fall-through served at the same standard tier", p)
				}
			} else {
				if g != nil {
					t.Fatalf("require fell through to another family: %+v", g)
				}
				e.assertHeld(review, "account")
			}
		})
	}
}

// TestForemanExactPairIgnoresTheFloor: a foreman turn is placed on its
// project's exact harness/model pair alone — the tier floor and the family
// order never touch it, and an agent of another model on the same harness
// does not take the turn.
func TestForemanExactPairIgnoresTheFloor(t *testing.T) {
	e := newEnv(t)
	// A heavy answer-class agent of the foreman's harness but another model:
	// not the pair, so it never takes the turn.
	e.addAgent(e.runner1, "claude", "opus", "work")
	turn := e.submit(e.foremanTurn())
	if g := e.leaseClaude(e.runner1); g != nil {
		t.Fatalf("a non-pair agent leased the foreman turn: %+v", g)
	}
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "work")
	g := e.leaseClaude(e.runner1)
	if g == nil || g.JobID != turn.JobID || e.jobAgent(g.JobID) != sonnet {
		t.Fatalf("lease = %+v, want the foreman turn %d on its exact pair", g, turn.JobID)
	}
}
