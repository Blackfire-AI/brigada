package engine

import (
	"strings"
	"testing"
	"time"
)

func (e *env) tierWork(tier string) SubmitResult {
	e.t.Helper()
	task := e.addTask("{}")
	if tier != "" {
		e.exec("UPDATE tasks SET tier = ?, effective_tier = ? WHERE id = ?", tier, tier, task)
	}
	return e.submit(e.workOn(task))
}

func (e *env) assertTierPick(job, agent int64, asked, served string) {
	e.t.Helper()
	if got := e.jobAgent(job); got != agent {
		e.t.Errorf("picked agent %d, want %d", got, agent)
	}
	evs := e.pickEvents(job)
	if len(evs) != 1 {
		e.t.Fatalf("pick events = %+v, want one", evs)
	}
	if ev := evs[0]; ev.TierAsked != asked || ev.TierServed != served {
		e.t.Errorf("pick event = %+v, want tier asked %s, served %s", ev, asked, served)
	}
}

func TestLeaseServesTheTaskTier(t *testing.T) {
	e := newEnv(t)
	haiku := e.addAgent(e.runner1, "claude", "haiku", "light")
	opus := e.addAgent(e.runner1, "claude", "opus", "heavy")
	e.record(opus, 4, 4) // 0.9; haiku has the cold-start score of 0.5.
	job := e.tierWork("light")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, haiku, "light", "light")
}

func TestLeaseStandardByDefault(t *testing.T) {
	e := newEnv(t)
	opus := e.addAgent(e.runner1, "claude", "opus", "heavy")
	haiku := e.addAgent(e.runner1, "claude", "haiku", "light")
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
	e.record(opus, 8, 8)
	e.record(haiku, 8, 8)
	job := e.tierWork("")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, sonnet, "standard", "standard")
}

func TestTierClimbsWhenNoneRegistered(t *testing.T) {
	e := newEnv(t)
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "work")
	job := e.tierWork("heavy")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, sonnet, "heavy", "standard")
}

func TestTierClimbsPastBrokenAgents(t *testing.T) {
	e := newEnv(t)
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
	opus := e.addAgent(e.runner1, "claude", "opus", "heavy")
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", sonnet)
	job := e.tierWork("standard")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, opus, "standard", "heavy")
}

func TestTierStepsDownFromHeavy(t *testing.T) {
	for _, brokenStandard := range []bool{false, true} {
		name := "standard ready"
		if brokenStandard {
			name = "standard broken"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			opus := e.addAgent(e.runner1, "claude", "opus", "heavy")
			haiku := e.addAgent(e.runner1, "claude", "haiku", "light")
			sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
			e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", opus)
			want, tier := sonnet, "standard"
			if brokenStandard {
				e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", sonnet)
				want, tier = haiku, "light"
			}
			job := e.tierWork("heavy")
			if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
				t.Fatalf("lease = %+v, want job %d", g, job.JobID)
			}
			e.assertTierPick(job.JobID, want, "heavy", tier)
		})
	}
}

func TestTierWaitsWhileCooling(t *testing.T) {
	e := newEnv(t)
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
	e.addAgent(e.runner1, "claude", "opus", "heavy")
	e.coolUntil(sonnet, 31*time.Minute)
	job := e.tierWork("standard")
	for i := 0; i < 3; i++ {
		e.clock.Advance(10 * time.Minute)
		if g := e.leaseAny(e.runner1); g != nil {
			t.Fatalf("leased %+v while standard tier cools", g)
		}
		e.assertHeld(job.JobID, layerCooling)
		if got := e.str("SELECT status FROM jobs WHERE id = ?", job.JobID); got != statusQueued {
			t.Fatalf("job status = %s, want queued past the TTL", got)
		}
	}
	e.clock.Advance(2 * time.Minute)
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease after reset = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, sonnet, "standard", "standard")
}

func TestTierWaitsAtCapRoom(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Caps.Account = map[string]int{"standard": 1} })
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
	e.addAgent(e.runner1, "claude", "opus", "heavy")
	first := e.leaseNew(e.runner1)
	e.assertTierPick(first.JobID, sonnet, "standard", "standard")
	job := e.tierWork("standard")
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased %+v while standard account is full", g)
	}
	e.assertHeld(job.JobID, "account")
	if got := e.str("SELECT cap_held_key FROM jobs WHERE id = ?", job.JobID); got != "standard" {
		t.Errorf("held account = %s, want standard", got)
	}
	if got := e.pickEvents(job.JobID); len(got) != 0 {
		t.Errorf("held job has pick events %+v", got)
	}
}

func TestSamplingStaysInTier(t *testing.T) {
	e := newEnv(t)
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
	gpt := e.addAgent(e.runner1, "codex", "gpt-5", "standard")
	opus := e.addAgent(e.runner1, "claude", "opus", "heavy")
	e.record(opus, 8, 8)
	e.record(sonnet, 4, 1)
	e.record(gpt, 4, 4)
	job := e.tierWork("standard")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, gpt, "standard", "standard")
	if ev := e.pickEvents(job.JobID)[0]; ev.Pick != pickSampled || len(ev.Candidates) != 2 {
		t.Errorf("pick event = %+v, want a sampled pick over the two standard agents", ev)
	}
}

// A tier whose agents are all capped at exactly 0 serves like a tier with no
// ready agent: the task climbs. A cap that is merely full still waits.
func TestTierFallsBackFromZeroCap(t *testing.T) {
	for name, adjust := range map[string]func(*Config){
		"account": func(c *Config) { c.Caps.Account = map[string]int{"standard": 0} },
		"model":   func(c *Config) { c.Caps.Model = map[string]int{"sonnet": 0} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnvCfg(t, adjust)
			e.addAgent(e.runner1, "claude", "sonnet", "standard")
			opus := e.addAgent(e.runner1, "claude", "opus", "heavy")
			job := e.tierWork("standard")
			if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
				t.Fatalf("lease = %+v, want job %d", g, job.JobID)
			}
			e.assertTierPick(job.JobID, opus, "standard", "heavy")
		})
	}
	t.Run("a zero cap on every tier waits", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) { c.Caps.Account = map[string]int{"standard": 0, "heavy": 0} })
		e.addAgent(e.runner1, "claude", "sonnet", "standard")
		e.addAgent(e.runner1, "claude", "opus", "heavy")
		job := e.tierWork("standard")
		if g := e.leaseAny(e.runner1); g != nil {
			t.Fatalf("leased %+v with every account capped at 0", g)
		}
		e.assertHeld(job.JobID, "account")
	})
}

func TestFixRoundPinIgnoresTier(t *testing.T) {
	e := newEnv(t)
	haiku := e.addAgent(e.runner1, "claude", "haiku", "light")
	e.addAgent(e.runner1, "claude", "sonnet", "standard")
	job := e.pinnedFix(haiku) // The fixture task defaults to standard.
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want pinned fix %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, haiku, "standard", "light")
}

func TestForemanTurnIgnoresClassFilter(t *testing.T) {
	e := newEnv(t)
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "foreman")
	e.exec("UPDATE agents SET classes = '[]' WHERE id = ?", sonnet)
	work := e.tierWork("standard")
	turn := e.submit(e.foremanTurn())
	if g := e.leaseClaude(e.runner1); g == nil || g.JobID != turn.JobID || e.jobAgent(g.JobID) != sonnet {
		t.Fatalf("lease = %+v, want foreman turn %d on class-less agent", g, turn.JobID)
	}
	if g := e.leaseClaude(e.runner1); g != nil {
		t.Fatalf("class-less agent leased work %+v", g)
	}
	if got := e.str("SELECT status FROM jobs WHERE id = ?", work.JobID); got != statusQueued {
		t.Errorf("work status = %s, want queued", got)
	}
}

func TestTierServed(t *testing.T) {
	// Each row lists the expected service tier for asked light, standard,
	// heavy, respectively, for every combination of ready-health tiers.
	for _, tt := range []struct {
		ready []string
		want  [3]string
	}{
		{nil, [3]string{"light", "standard", "heavy"}},
		{[]string{"light"}, [3]string{"light", "light", "light"}},
		{[]string{"standard"}, [3]string{"standard", "standard", "standard"}},
		{[]string{"heavy"}, [3]string{"heavy", "heavy", "heavy"}},
		{[]string{"light", "standard"}, [3]string{"light", "standard", "standard"}},
		{[]string{"light", "heavy"}, [3]string{"light", "heavy", "heavy"}},
		{[]string{"standard", "heavy"}, [3]string{"standard", "standard", "heavy"}},
		{[]string{"light", "standard", "heavy"}, [3]string{"light", "standard", "heavy"}},
	} {
		t.Run(strings.Join(tt.ready, "/"), func(t *testing.T) {
			hasReady := map[string]bool{}
			for _, tier := range tt.ready {
				hasReady[tier] = true
			}
			for i, asked := range []string{"light", "standard", "heavy"} {
				if got := tierServed(asked, hasReady); got != tt.want[i] {
					t.Errorf("tierServed(%s, %v) = %s, want %s", asked, hasReady, got, tt.want[i])
				}
			}
		})
	}
}

func TestTierSelectionAcrossRunners(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "opus", "heavy")
	sonnet := e.addAgent(e.runner2, "claude", "sonnet", "standard")
	job := e.tierWork("standard")
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased heavier model while another runner offers standard: %+v", g)
	}
	if g := e.leaseAny(e.runner2); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, sonnet, "standard", "standard")
}

func TestTierOverrideInDispatch(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Caps.Tier = map[string]string{"claude/opus": "light"} })
	opus := e.addAgent(e.runner1, "claude", "opus", "work")
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.record(sonnet, 8, 8)
	job := e.tierWork("light")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, opus, "light", "light")
}

func TestTierQueueTTLNamesAskedTier(t *testing.T) {
	e := newEnv(t)
	job := e.tierWork("heavy")
	e.clock.Advance(11 * time.Minute)
	e.mustSweep()
	if got := e.str("SELECT result FROM jobs WHERE id = ?", job.JobID); !strings.Contains(got, "no agent of tier heavy, or of any tier, offers implement") {
		t.Errorf("park reason = %s, want the asked tier and class", got)
	}
}

// tierReview submits a review of a task of the given tier, written by the
// claude family, so only another family may review it (§7).
func (e *env) tierReview(tier string) SubmitResult {
	e.t.Helper()
	task := e.addTask("{}")
	e.exec("UPDATE tasks SET tier = ?, effective_tier = ? WHERE id = ?", tier, tier, task)
	p := e.workOn(task)
	p.Kind = KindReview
	p.ExcludeFamily = "claude"
	return e.submit(p)
}

// A tier whose only other-family reviewer cools for days counts like a
// broken one: the review climbs to a standard reviewer of another family.
func TestHeavyReviewSkipsLongCoolingReviewer(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "opus", "heavy")
	codex := e.addAgent(e.runner1, "codex", "gpt-5-pro", "codex")
	glm := e.addAgent(e.runner1, "opencode", "glm-5", "opencode")
	e.coolUntil(codex, 3*24*time.Hour)
	job := e.tierReview("heavy")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want review %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, glm, "heavy", "standard")
}

// A cooling of half an hour is a wait worth having.
func TestHeavyReviewWaitsForShortCoolingReviewer(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "opus", "heavy")
	codex := e.addAgent(e.runner1, "codex", "gpt-5-pro", "codex")
	e.addAgent(e.runner1, "opencode", "glm-5", "opencode")
	e.coolUntil(codex, 30*time.Minute)
	job := e.tierReview("heavy")
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased %+v while the heavy reviewer cools for 30 minutes", g)
	}
	e.assertHeld(job.JobID, layerCooling)
	e.clock.Advance(31 * time.Minute)
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease after reset = %+v, want review %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, codex, "heavy", "heavy")
}

func TestLightTaskClimbsPastLongCooling(t *testing.T) {
	e := newEnv(t)
	haiku := e.addAgent(e.runner1, "claude", "haiku", "light")
	e.addAgent(e.runner1, "claude", "opus", "heavy")
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
	e.coolUntil(haiku, 3*24*time.Hour)
	job := e.tierWork("light")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, sonnet, "light", "standard")
}

// With no tier available, the job waits for the earliest reset, in whichever
// tier that is.
func TestEveryTierLongCoolingHoldsForTheEarliestReset(t *testing.T) {
	e := newEnv(t)
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
	opus := e.addAgent(e.runner1, "claude", "opus", "heavy")
	e.coolUntil(sonnet, 5*24*time.Hour)
	soon := e.coolUntil(opus, 2*24*time.Hour)
	job := e.tierWork("standard")
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased %+v while every agent cools", g)
	}
	e.assertHeld(job.JobID, layerCooling)
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(info.Held) != 1 || info.Held[0].Agent != "claude/opus" || info.Held[0].Until != fmtTime(soon) {
		t.Fatalf("status held = %+v, want claude/opus until %s", info.Held, fmtTime(soon))
	}
}

// A pinned fix round never moves to another tier: it waits for its agent,
// however long.
func TestPinnedFixWaitsForLongCooling(t *testing.T) {
	e := newEnv(t)
	haiku := e.addAgent(e.runner1, "claude", "haiku", "light")
	e.addAgent(e.runner1, "claude", "sonnet", "standard")
	e.coolUntil(haiku, 3*24*time.Hour)
	job := e.pinnedFix(haiku)
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased %+v while the pinned agent cools", g)
	}
	e.assertHeld(job.JobID, layerCooling)
}

// A cooling hold lifts once the served tier has moved to an available agent.
func TestCoolingHoldLiftsWhenTierMoves(t *testing.T) {
	e := newEnv(t)
	haiku := e.addAgent(e.runner1, "claude", "haiku", "light")
	e.coolUntil(haiku, 40*time.Minute)
	job := e.tierWork("light")
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased %+v while the light tier cools", g)
	}
	e.assertHeld(job.JobID, layerCooling)

	// The light agent's cooling turns out to be long, and a standard agent
	// is there to take over.
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "standard")
	e.coolUntil(haiku, 3*24*time.Hour)
	e.mustSweep()
	if got := e.heldLayer(job.JobID); got != "" {
		t.Fatalf("hold layer = %q after the tier moved, want it lifted", got)
	}
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, sonnet, "light", "standard")
}
