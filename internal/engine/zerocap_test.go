package engine

import (
	"strings"
	"testing"
	"time"
)

// Issue #195: an agent capped at exactly 0 can never take a job, so it is
// no placement candidate — not for an automatic retry's pin, an ordinary
// draw, a prefer review or the cooling hold. A merely full cap still waits.

// zeroCapScenarios cap one family's agent at exactly 0 by model or by
// account; agented names the model family-1 and the account family.
var zeroCapScenarios = map[string]func(c *Config){
	"model":   func(c *Config) { c.Caps.Model = map[string]int{"zero-1": 0} },
	"account": func(c *Config) { c.Caps.Account = map[string]int{"zero": 0} },
}

// An automatic retry never pins an agent capped at 0, even one the outcome
// record would favour: the lease gate would hold such a pin forever.
func TestAutoRestartNeverPinsZeroCappedAgent(t *testing.T) {
	for name, adjust := range zeroCapScenarios {
		t.Run(name, func(t *testing.T) {
			e := newEnvCfg(t, adjust)
			author := e.agented("author", "fa", "implement")
			zero := e.agented("zero", "zero", "implement")
			good := e.agented("good", "fg", "implement")
			e.record(e.agentOf(zero), 40, 40)
			e.submit(e.work())
			g := e.leaseAny(author)
			if g == nil {
				t.Fatal("work job was not leased")
			}
			e.complete(g, Completion{Error: "worker CLI failed: temporarily unavailable", ErrorKind: "other"})
			retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND restart = 1", e.task)
			if retry == 0 {
				t.Fatal("no restart job was queued")
			}
			if pin := e.int("SELECT require_agent FROM jobs WHERE id = ?", retry); pin != e.agentOf(good) {
				t.Fatalf("retry pin = %d, want the placeable agent %d, never the zero-capped %d", pin, e.agentOf(good), e.agentOf(zero))
			}
			if fresh := e.leaseAny(good); fresh == nil || fresh.JobID != retry {
				t.Fatalf("placeable agent leased %+v, want retry %d", fresh, retry)
			}
		})
	}
}

// With the only other agent of the class capped at 0 there is no restart:
// the task takes the ordinary failure path.
func TestAutoRestartSkipsWhenOnlyOtherAgentIsZeroCapped(t *testing.T) {
	for name, adjust := range zeroCapScenarios {
		t.Run(name, func(t *testing.T) {
			e := newEnvCfg(t, adjust)
			author := e.agented("author", "fa", "implement")
			e.agented("zero", "zero", "implement")
			e.submit(e.work())
			g := e.leaseAny(author)
			if g == nil {
				t.Fatal("work job was not leased")
			}
			e.complete(g, Completion{Error: "worker CLI failed", ErrorKind: "other"})
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Errorf("task state = %s, want needs_foreman", state)
			}
			if n := e.countEvents(evTaskRestarted); n != 0 {
				t.Errorf("restart events = %d, want none", n)
			}
			if n := e.countEvents(evTaskFailed); n != 1 {
				t.Errorf("failure events = %d, want one", n)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", e.task); n != 1 {
				t.Errorf("task jobs = %d, want only the failed work", n)
			}
		})
	}
}

// A job pinned to an agent capped at 0 — by model or account, cooling or
// not — is an unmet placement: no hold is stamped, the cooling hold does
// not wait on it, and the queue TTL parks the job naming the pin and cap.
func TestPinnedJobOnZeroCappedAgentParksNamingIt(t *testing.T) {
	for _, layer := range []string{"model", "account"} {
		for _, cooling := range []bool{false, true} {
			name := layer
			if cooling {
				name += " cooling"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnvCfg(t, func(c *Config) {
					if layer == "model" {
						c.Caps.Model = map[string]int{"gpt-5": 0}
					} else {
						c.Caps.Account = map[string]int{"work": 0}
					}
				})
				codex := e.addAgent(e.runner1, "codex", "gpt-5", "work")
				if cooling {
					e.coolUntil(codex, 2*time.Hour)
				}
				fix := e.pinnedFix(codex)

				if g := e.lease(e.runner1, nil, nil); g != nil {
					t.Fatalf("leased %+v for a pin capped at 0", g)
				}
				e.elapse(10*time.Minute + time.Second)
				if got := e.heldLayer(fix.JobID); got != "" {
					t.Errorf("hold layer = %q, want none", got)
				}
				if layers := e.capHeldLayers(fix.JobID); len(layers) != 0 {
					t.Errorf("job_cap_held events = %v, want none", layers)
				}
				if status := e.str("SELECT status FROM jobs WHERE id = ?", fix.JobID); status != statusFailed {
					t.Fatalf("status = %q, want failed by the queue TTL", status)
				}
				result := e.str("SELECT result FROM jobs WHERE id = ?", fix.JobID)
				for _, want := range []string{"queue TTL exceeded", "pinned to codex/gpt-5", "capped at 0", layer + " layer"} {
					if !strings.Contains(result, want) {
						t.Errorf("result = %q, want it to contain %q", result, want)
					}
				}
			})
		}
	}
}

// Under review.family=prefer a review never lands on a reviewer whose
// model or account is capped at 0: the author's family's sibling takes it,
// and with no sibling the job waits.
func TestPreferNeverPlacesOnZeroCappedReviewer(t *testing.T) {
	for name, adjust := range map[string]func(c *Config){
		"model":   func(c *Config) { c.Caps.Model = map[string]int{"google-1": 0} },
		"account": func(c *Config) { c.Caps.Account = map[string]int{"google": 0} },
	} {
		t.Run(name+" with a sibling", func(t *testing.T) {
			e := newEnvCfg(t, adjust)
			author := e.agented("author", "anthropic", "implement")
			sibling := e.agented("sibling", "anthropic", "review")
			reviewer := e.agented("reviewer", "google", "review")
			review := e.runToReview(author)
			if g := e.leaseAny(reviewer); g != nil {
				t.Fatalf("zero-capped reviewer leased %+v", g)
			}
			g := e.leaseAny(sibling)
			if g == nil || g.JobID != review {
				t.Fatalf("sibling leased %+v, want the review %d", g, review)
			}
		})
		t.Run(name+" alone", func(t *testing.T) {
			e := newEnvCfg(t, adjust)
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "review")
			e.runToReview(author)
			if g := e.leaseAny(reviewer); g != nil {
				t.Fatalf("zero-capped reviewer leased %+v", g)
			}
		})
	}
}

// A foreman turn is exempt from the model cap but not from the account
// cap: a model capped at 0 still places it, an account capped at 0
// refuses it (§8.1).
func TestForemanTurnZeroCaps(t *testing.T) {
	t.Run("model capped at 0 still places", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) { c.Caps.Model = map[string]int{"sonnet": 0} })
		e.seedAgent(e.runner1, "claude", "claude", "sonnet", "claude")
		turn := e.submit(e.foremanTurn())
		if g := e.leaseClaude(e.runner1); g == nil || g.JobID != turn.JobID {
			t.Fatalf("foreman lease = %+v, want the turn %d on a model capped at 0", g, turn.JobID)
		}
	})
	t.Run("account capped at 0 refuses", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) { c.Caps.Account = map[string]int{"claude": 0} })
		e.seedAgent(e.runner1, "claude", "claude", "sonnet", "claude")
		e.submit(e.foremanTurn())
		if g := e.leaseClaude(e.runner1); g != nil {
			t.Fatalf("foreman lease = %+v, want none on an account capped at 0", g)
		}
	})
}
