package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// headroomEnv is an env with the headroom rule on (§8.2), the work
// account capped at accountCap, and the fixture's claude foreman
// advertised on that account (runner 1) beside a kimi agent on the
// personal account.
func headroomEnv(t *testing.T, accountCap int, adjust func(*Config)) *env {
	t.Helper()
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.NoHeadroom = false
		c.Caps.Account = map[string]int{"work": accountCap, "personal": accountCap}
		if adjust != nil {
			adjust(c)
		}
	})
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.seedAgent(e.runner1, "kimi", "kimi", "k2", "personal")
	return e
}

func (e *env) workOnHarness(harness string) SubmitParams {
	e.t.Helper()
	return e.workHarnessed(harness)
}

// TestHeadroomLowersTaskCap: a task job on the foreman's account sees
// the account cap minus one, so at cap 2 one task runs and the next is
// held at the account layer.
func TestHeadroomLowersTaskCap(t *testing.T) {
	e := headroomEnv(t, 2, nil)
	first := e.submit(e.workOnHarness("claude"))
	if g := e.lease(e.runner1, []string{"claude"}, nil); g == nil || g.JobID != first.JobID {
		t.Fatalf("first task lease = %+v, want job %d", g, first.JobID)
	}
	second := e.submit(e.workOnHarness("claude"))
	if g := e.lease(e.runner1, []string{"claude"}, nil); g != nil {
		t.Fatalf("a second task on the foreman's account leased job %d; one slot is reserved", g.JobID)
	}
	e.assertHeld(second.JobID, caps.LayerAccount)
}

// TestHeadroomAtCapOneNeverLeases: at account cap 1 task workers never
// run on the foreman's account. The headroom-zeroed account counts as
// cap-0 (§8), so no cap-hold is stamped for it; with no other tier or
// account able to serve the claude-harnessed task, it is an unmet
// placement the queue TTL parks — never a forever hold.
func TestHeadroomAtCapOneNeverLeases(t *testing.T) {
	e := headroomEnv(t, 1, func(c *Config) { c.RunnerStale = time.Hour })
	job := e.submit(e.workOnHarness("claude"))
	if g := e.lease(e.runner1, []string{"claude"}, nil); g != nil {
		t.Fatalf("a task leased job %d on the foreman's account at cap 1", g.JobID)
	}
	if got := e.heldLayer(job.JobID); got != "" {
		t.Errorf("hold layer = %q, want none: a headroom-zeroed account is cap-0", got)
	}
	if layers := e.capHeldLayers(job.JobID); len(layers) != 0 {
		t.Errorf("job_cap_held events = %v, want none", layers)
	}
	e.elapse(e.eng.cfg.QueueTTL + time.Minute)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", job.JobID); status != statusFailed {
		t.Fatalf("job status = %q, want failed by the queue TTL, not a forever hold", status)
	}
	result := e.str("SELECT result FROM jobs WHERE id = ?", job.JobID)
	for _, want := range []string{"queue TTL exceeded", "no agent of tier standard"} {
		if !strings.Contains(result, want) {
			t.Errorf("result = %q, want it to contain %q", result, want)
		}
	}
}

// TestHeadroomZeroedTierFallsBack: the asked tier is served only by a
// headroom-zeroed agent on the foreman's account, while a ready agent of
// another tier sits on another account. The job climbs to a higher tier per
// §8 and leases there, never cap-held on the foreman account; but the floor
// never steps down (issue #263) — a heavy ask whose only ready agent is
// light waits unplaced.
func TestHeadroomZeroedTierFallsBack(t *testing.T) {
	t.Run("climb", func(t *testing.T) {
		e := headroomEnv(t, 1, nil)
		// The only standard agent must be the foreman-account one the
		// fixture seeds; the personal standard agent would serve the tier.
		e.exec("DELETE FROM agents WHERE harness = 'kimi'")
		opus := e.addAgent(e.runner1, "claude", "opus", "personal")
		job := e.tierWork("standard")
		g := e.leaseAny(e.runner1)
		if g == nil || g.JobID != job.JobID {
			t.Fatalf("lease = %+v, want task %d to climb to the heavy tier", g, job.JobID)
		}
		e.assertTierPick(job.JobID, opus, "standard", "heavy")
	})
	t.Run("never steps down", func(t *testing.T) {
		e := headroomEnv(t, 1, nil)
		e.exec("DELETE FROM agents WHERE harness = 'kimi'")
		e.seedAgent(e.runner1, "claude", "claude", "opus", "work") // heavy on the foreman account, zeroed too
		e.addAgent(e.runner1, "claude", "haiku", "personal")       // light, below the heavy floor
		job := e.tierWork("heavy")
		if g := e.leaseAny(e.runner1); g != nil {
			t.Fatalf("leased %+v below the heavy floor, want no placement", g)
		}
		if got := e.heldLayer(job.JobID); got != "" {
			t.Errorf("hold layer = %q, want none: a headroom-zeroed account is cap-0", got)
		}
	})
}

// TestHeadroomZeroedReviewFallsBack: a review whose asked tier is served
// only by a headroom-zeroed reviewer on the foreman's account climbs to an
// available reviewer of a higher tier, as a work job does; the floor never
// steps it down (issue #263).
func TestHeadroomZeroedReviewFallsBack(t *testing.T) {
	e := headroomEnv(t, 1, func(c *Config) {
		c.Caps.Tier = map[string]string{"opencode/glm-5": "heavy"}
	})
	e.exec("DELETE FROM agents WHERE harness = 'kimi'")
	glm := e.addAgent(e.runner1, "opencode", "glm-5", "personal")
	task := e.addTask("{}")
	e.exec("UPDATE tasks SET tier = 'standard', effective_tier = 'standard', review_tier = 'standard' WHERE id = ?", task)
	p := e.workOn(task)
	p.Kind = KindReview
	p.ExcludeFamily = "codex"
	job := e.submit(p)
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want review %d to climb to the heavy tier", g, job.JobID)
	}
	e.assertTierPick(job.JobID, glm, "standard", "heavy")
}

// TestHeadroomPinnedJobParksNamingAccount: a job pinned to a
// headroom-zeroed agent has no candidate, so it is neither cap-held nor
// cooling-held (even when the agent also cools); the queue TTL parks it
// with a reason naming the pin and the account layer, and the capacity
// snapshot reads the pin as capped_at_0.
func TestHeadroomPinnedJobParksNamingAccount(t *testing.T) {
	for _, cool := range []bool{false, true} {
		name := "account"
		if cool {
			name += " cooling"
		}
		t.Run(name, func(t *testing.T) {
			e := headroomEnv(t, 1, func(c *Config) { c.RunnerStale = time.Hour })
			sonnet := e.int("SELECT id FROM agents WHERE harness = 'claude'")
			if cool {
				e.coolUntil(sonnet, time.Hour) // longer than the TTL
			}
			fix := e.pinnedFix(sonnet)

			e.elapse(5 * time.Minute)
			if g := e.lease(e.runner1, nil, nil); g != nil {
				t.Fatalf("leased job %d on a headroom-zeroed agent", g.JobID)
			}
			e.mustSweep()
			if got := e.heldLayer(fix.JobID); got != "" {
				t.Errorf("hold layer = %q, want none: a headroom-zeroed account is an unmet placement", got)
			}
			if layers := e.capHeldLayers(fix.JobID); len(layers) != 0 {
				t.Errorf("job_cap_held events = %v, want none", layers)
			}
			snap, err := e.eng.Capacity(e.ctx, e.project)
			if err != nil {
				t.Fatalf("capacity: %v", err)
			}
			if len(snap.Pins) != 1 || snap.Pins[0].JobID != fix.JobID || snap.Pins[0].State != PinCappedAtZero {
				t.Errorf("pins = %+v, want job %d capped_at_0", snap.Pins, fix.JobID)
			}

			e.elapse(5*time.Minute + time.Second)
			if status := e.str("SELECT status FROM jobs WHERE id = ?", fix.JobID); status != statusFailed {
				t.Fatalf("status = %q, want failed by the queue TTL", status)
			}
			result := e.str("SELECT result FROM jobs WHERE id = ?", fix.JobID)
			for _, want := range []string{"queue TTL exceeded", "pinned to claude/sonnet", "capped at 0 at the account layer"} {
				if !strings.Contains(result, want) {
					t.Errorf("result = %q, want it to contain %q", result, want)
				}
			}
		})
	}
}

// TestContinueFallsBackWhenAuthorHeadroomZeroed: a continuing task whose
// dep author sits on a headroom-zeroed account starts fresh and unpinned,
// as it does for an author capped at a configured 0.
func TestContinueFallsBackWhenAuthorHeadroomZeroed(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Caps.NoHeadroom = true })
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	res := e.continuePlan("answer")
	g := e.lease(e.runner1, []string{"claude"}, nil)
	if g == nil {
		t.Fatal("build's work job was not leased")
	}
	// Headroom on and the foreman account capped at 1: the author is
	// headroom-zeroed when the dependent starts.
	e.eng.cfg.Caps.NoHeadroom = false
	e.eng.cfg.Caps.Account = map[string]int{"work": 1}
	c := doneOn(branchOf(g.TaskID), "")
	c.SessionID = "hs-build-1"
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete build: %s", ans)
	}
	job := e.shipJob(res)
	if got := e.str("SELECT mode FROM jobs WHERE id = ?", job); got != string(ModeFresh) {
		t.Errorf("mode = %q, want fresh", got)
	}
	if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", job); agent != 0 {
		t.Errorf("required agent = %d, want none", agent)
	}
}

// TestHeadroomForemanTurnSeesFullCap: the foreman's own turn on the
// account runs against the full cap.
func TestHeadroomForemanTurnSeesFullCap(t *testing.T) {
	e := headroomEnv(t, 1, nil)
	turn := e.submit(e.foremanTurn())
	if g := e.lease(e.runner1, []string{"claude"}, nil); g == nil || g.JobID != turn.JobID {
		t.Fatalf("foreman turn lease = %+v, want job %d", g, turn.JobID)
	}
}

// TestHeadroomUnaffected: no foreman, an unadvertised foreman harness
// and another account all see the full cap.
func TestHeadroomUnaffected(t *testing.T) {
	tests := []struct {
		name    string
		harness string // the task's harness
		setup   func(e *env)
	}{
		{"no foreman", "claude", func(e *env) { e.exec("UPDATE projects SET foreman_harness = '' WHERE id = ?", e.project) }},
		{"unadvertised foreman harness", "claude", func(e *env) {
			e.exec("UPDATE projects SET foreman_harness = 'gemini' WHERE id = ?", e.project)
		}},
		{"a different account", "kimi", func(e *env) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := headroomEnv(t, 1, nil)
			tt.setup(e)
			job := e.submit(e.workOnHarness(tt.harness))
			if g := e.lease(e.runner1, []string{tt.harness}, nil); g == nil || g.JobID != job.JobID {
				t.Fatalf("lease = %+v, want job %d at the full cap", g, job.JobID)
			}
		})
	}
}

// TestHeadroomOffDisablesIt: with the rule off, behavior is today's.
func TestHeadroomOffDisablesIt(t *testing.T) {
	e := headroomEnv(t, 1, func(c *Config) { c.Caps.NoHeadroom = true })
	job := e.submit(e.workOnHarness("claude"))
	if g := e.lease(e.runner1, []string{"claude"}, nil); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d with headroom off", g, job.JobID)
	}
	snap, err := e.eng.Capacity(e.ctx, e.project)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if len(snap.Headroom) != 0 {
		t.Errorf("headroom = %+v with the rule off, want none", snap.Headroom)
	}
}

// TestCapacityNamesReservedAccount: the snapshot names the foreman's
// account and the cap task workers see on it.
func TestCapacityNamesReservedAccount(t *testing.T) {
	e := headroomEnv(t, 3, nil)
	snap, err := e.eng.Capacity(e.ctx, e.project)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if len(snap.Headroom) != 1 {
		t.Fatalf("headroom = %+v, want one reserved account", snap.Headroom)
	}
	h := snap.Headroom[0]
	if h.ProjectID != e.project || h.Account != "work" || h.Cap == nil || *h.Cap != 2 {
		t.Errorf("headroom = %+v, want project %d account work cap 2", h, e.project)
	}
}
