package engine

import (
	"slices"
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
// run on the foreman's account, and the held job waits past the queue
// TTL: it is stamped, never parked.
func TestHeadroomAtCapOneNeverLeases(t *testing.T) {
	e := headroomEnv(t, 1, nil)
	held := e.submit(e.workOnHarness("claude"))
	if g := e.lease(e.runner1, []string{"claude"}, nil); g != nil {
		t.Fatalf("a task leased job %d on the foreman's account at cap 1", g.JobID)
	}
	e.assertHeld(held.JobID, caps.LayerAccount)

	e.clock.Advance(e.eng.cfg.QueueTTL / 2)
	if g := e.lease(e.runner1, []string{"claude"}, nil); g != nil {
		t.Fatalf("a task leased job %d on the foreman's account at cap 1", g.JobID)
	}
	e.clock.Advance(e.eng.cfg.QueueTTL/2 + 2*time.Minute)
	rep := e.mustSweep()
	if slices.Contains(rep.QueueTimedOut, held.JobID) {
		t.Errorf("sweep timed out the headroom-held job %d; it waits, never parks", held.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", held.JobID); status != statusQueued {
		t.Errorf("held job status = %q, want queued", status)
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
