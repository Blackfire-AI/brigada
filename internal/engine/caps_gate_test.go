package engine

import (
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// limitOf is how a test names one configured cap: a set limit, as the
// config would carry it (§8.1).
func limitOf(n int) *int { return &n }

// workOn is the standard work submit, for another task of the plan.
func (e *env) workOn(task int64) SubmitParams {
	e.t.Helper()
	p := e.work()
	p.Key = keyOf(task)
	p.TaskID = task
	return p
}

// capHeldLayers reads the layers of the job's job_cap_held events,
// oldest first (§8.1): one entry per hold that started or moved.
func (e *env) capHeldLayers(job int64) []string {
	e.t.Helper()
	var layers []string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT payload FROM events WHERE kind = ? AND job_id = ? ORDER BY id", evJobCapHeld, job)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			var p eventPayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				layers = append(layers, p.Layer)
			}
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatalf("read job_cap_held events of job %d: %v", job, err)
	}
	return layers
}

// assertHeld checks what a hold leaves on a job (§8.1): queued, the
// full layer named on it, the hold stamped, and exactly one event.
func (e *env) assertHeld(job int64, layer string) {
	e.t.Helper()
	if status := e.str("SELECT status FROM jobs WHERE id = ?", job); status != statusQueued {
		e.t.Errorf("held job status = %q, want queued: a held job waits (§8.1)", status)
	}
	if got := e.str("SELECT COALESCE(cap_held_layer, '') FROM jobs WHERE id = ?", job); got != layer {
		e.t.Errorf("cap_held_layer = %q, want %q", got, layer)
	}
	if at := e.str("SELECT COALESCE(cap_held_at, '') FROM jobs WHERE id = ?", job); at == "" {
		e.t.Errorf("cap_held_at is empty; want the hold stamped")
	}
	if layers := e.capHeldLayers(job); len(layers) != 1 || layers[0] != layer {
		e.t.Errorf("job_cap_held events = %v, want exactly [%s]", layers, layer)
	}
}

// TestLeaseGateHoldsAtFullLayer walks the §8.1 layers: a lease poll
// that reaches a job whose path has no room leases nothing, holds the
// job at the full layer, and the lease goes through once the layer
// frees.
func TestLeaseGateHoldsAtFullLayer(t *testing.T) {
	tests := []struct {
		name   string
		limits func(*caps.Limits)
		seed   func(e *env)
		fill   string // "1" or "2": the runner whose lease fills the layer
		poll   string // "1" or "2": the runner whose next lease must find it full
		layer  string
	}{
		{
			name:   "runner",
			limits: func(l *caps.Limits) { l.Runner = limitOf(1) },
			fill:   "1",
			poll:   "1",
			layer:  caps.LayerRunner,
		},
		{
			name:   "account, shared by two runners",
			limits: func(l *caps.Limits) { l.Account = map[string]int{"work": 1} },
			seed: func(e *env) {
				e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
				e.seedAgent(e.runner2, "claude", "claude", "sonnet", "work")
			},
			fill:  "1",
			poll:  "2",
			layer: caps.LayerAccount,
		},
		{
			name:   "model, across accounts",
			limits: func(l *caps.Limits) { l.Model = map[string]int{"sonnet": 1} },
			seed: func(e *env) {
				e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
				e.seedAgent(e.runner2, "kimi", "kimi", "sonnet", "personal")
			},
			fill:  "1",
			poll:  "2",
			layer: caps.LayerModel,
		},
		{
			name:   "global",
			limits: func(l *caps.Limits) { l.Global = limitOf(1) },
			fill:   "1",
			poll:   "2",
			layer:  caps.LayerGlobal,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) { tc.limits(&c.Caps) })
			if tc.seed != nil {
				tc.seed(e)
			}
			runner := func(which string) int64 {
				if which == "2" {
					return e.runner2
				}
				return e.runner1
			}
			first := e.submit(e.work())
			g := e.leaseAny(runner(tc.fill))
			if g == nil || g.JobID != first.JobID {
				t.Fatalf("the filling lease = %+v, want job %d", g, first.JobID)
			}
			held := e.submit(e.workOn(e.addTask("{}")))

			if got := e.leaseAny(runner(tc.poll)); got != nil {
				t.Fatalf("a lease through the full %s layer took job %d", tc.layer, got.JobID)
			}
			e.assertHeld(held.JobID, tc.layer)

			if ans := e.complete(g, done()); ans != AnsOK {
				t.Fatalf("complete the filling job: %s", ans)
			}
			if got := e.leaseAny(runner(tc.poll)); got == nil || got.JobID != held.JobID {
				t.Fatalf("lease after the layer freed = %+v, want job %d", got, held.JobID)
			}
			if layer := e.str("SELECT COALESCE(cap_held_layer, '') FROM jobs WHERE id = ?", held.JobID); layer != "" {
				t.Errorf("cap_held_layer after the lease = %q, want the hold cleared", layer)
			}
		})
	}
}

// TestLeaseGatePlanCapHoldsOnePlanNotAnother: at plan cap 1 the second
// task of a running plan is held, and the hold does not block a later
// job of another plan that has room (§8.1).
func TestLeaseGatePlanCapHoldsOnePlanNotAnother(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Caps.Plan = limitOf(1) })
	plan2 := e.addPlan()
	first := e.submit(e.work())                 // the plan's first task
	held := e.submit(e.workOn(e.addTask("{}"))) // its second: the plan fills
	other := e.submit(e.addPlanTask(plan2))

	g1 := e.leaseAny(e.runner1)
	if g1 == nil || g1.JobID != first.JobID {
		t.Fatalf("first lease = %+v, want job %d", g1, first.JobID)
	}
	// The held job is the older of the two left, but a hold is not a
	// refusal of everything behind it: the other plan's job goes through
	// in the same call.
	g2 := e.leaseAny(e.runner1)
	if g2 == nil || g2.JobID != other.JobID {
		t.Fatalf("second lease = %+v, want the other plan's job %d", g2, other.JobID)
	}
	e.assertHeld(held.JobID, caps.LayerPlan)

	// The plan frees with its running task, and the held task follows.
	if ans := e.complete(g1, done()); ans != AnsOK {
		t.Fatalf("complete the plan's running task: %s", ans)
	}
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != held.JobID {
		t.Fatalf("lease after the plan freed = %+v, want job %d", g, held.JobID)
	}
}

// TestCapHeldJobSurvivesQueueTTL: the queue TTL is for jobs no runner
// can serve; a job held only by a full cap layer waits as long as it
// takes (§8.1), every hold restarting the clock — while a job whose
// harness no runner offers is never held and still expires.
func TestCapHeldJobSurvivesQueueTTL(t *testing.T) {
	// The filling lease must outlive the TTL window: a lease that lapses
	// mid-window requeues its job and frees the layer for it.
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Runner = limitOf(1)
		c.LeaseTTL = time.Hour
	})
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	held := e.submit(e.workOn(e.addTask("{}")))
	unservable := e.work()
	unservable.TaskID = e.addTask("{}")
	unservable.Key = keyOf(unservable.TaskID)
	unservable.Harness = "gemini" // no runner offers it
	unservableJob := e.submit(unservable)

	if got := e.leaseAny(e.runner1); got != nil {
		t.Fatalf("a second lease at runner cap 1 took job %d", got.JobID)
	}
	heldAt := e.str("SELECT cap_held_at FROM jobs WHERE id = ?", held.JobID)

	// Halfway through the TTL window the runner polls again: the hold is
	// restamped, and the unservable job still queues unheld.
	e.clock.Advance(e.eng.cfg.QueueTTL / 2)
	if got := e.leaseAny(e.runner1); got != nil {
		t.Fatalf("the held job %d leaked out mid-window", got.JobID)
	}
	restamped := e.str("SELECT cap_held_at FROM jobs WHERE id = ?", held.JobID)
	if restamped == "" || restamped == heldAt {
		t.Fatalf("cap_held_at = %q after a second hold; want it restamped from %q", restamped, heldAt)
	}
	if at := e.str("SELECT COALESCE(cap_held_at, '') FROM jobs WHERE id = ?", unservableJob.JobID); at != "" {
		t.Errorf("the unservable job was held at %q; a job no runner can try is never held", at)
	}

	// Past the TTL from queued_at the unservable job fails; the held one
	// waits, its clock having restarted at the last hold.
	e.clock.Advance(e.eng.cfg.QueueTTL/2 + 2*time.Minute)
	rep := e.mustSweep()
	if !slices.Contains(rep.QueueTimedOut, unservableJob.JobID) {
		t.Errorf("sweep timed out = %v, want the unservable job %d in it", rep.QueueTimedOut, unservableJob.JobID)
	}
	if slices.Contains(rep.QueueTimedOut, held.JobID) {
		t.Errorf("sweep timed out the cap-held job %d; it waits as long as it takes (§8.1)", held.JobID)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", held.JobID); status != statusQueued {
		t.Errorf("held job status = %q, want queued", status)
	}
}

// TestJobCapHeldEventNotPerPoll: the event lands when a hold starts and
// when it moves to another layer; the polls in between stay quiet.
func TestJobCapHeldEventNotPerPoll(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Runner = limitOf(1)
		c.Caps.Account = map[string]int{"work": 1}
	})
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.seedAgent(e.runner2, "claude", "claude", "sonnet", "work")
	e.submit(e.work())
	if g := e.leaseAny(e.runner1); g == nil {
		t.Fatal("nothing to lease")
	}
	held := e.submit(e.workOn(e.addTask("{}")))

	// Runner 1 is full at its own layer; each poll holds the job there.
	for i := 0; i < 2; i++ {
		if g := e.leaseAny(e.runner1); g != nil {
			t.Fatalf("poll %d: runner 1 took job %d through its own full layer", i, g.JobID)
		}
	}
	if layers := e.capHeldLayers(held.JobID); len(layers) != 1 || layers[0] != caps.LayerRunner {
		t.Fatalf("job_cap_held events after runner 1's polls = %v, want one runner hold", layers)
	}

	// Runner 2 has runner room but shares the spent account: the hold
	// moves to the account layer, and that move is an event.
	if g := e.leaseAny(e.runner2); g != nil {
		t.Fatalf("runner 2 took job %d through the full account", g.JobID)
	}
	if layers := e.capHeldLayers(held.JobID); len(layers) != 2 || layers[1] != caps.LayerAccount {
		t.Fatalf("job_cap_held events = %v, want runner then account", layers)
	}
	// The same layer again stays quiet.
	if g := e.leaseAny(e.runner2); g != nil {
		t.Fatalf("runner 2 took job %d", g.JobID)
	}
	if layers := e.capHeldLayers(held.JobID); len(layers) != 2 {
		t.Fatalf("job_cap_held events after a repeat poll = %v, want no new one", layers)
	}
}
