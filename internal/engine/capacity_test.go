package engine

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// addAgent advertises one agent on a runner, the way a register would,
// and returns its id. The agent takes every class: these tests place
// jobs by harness and caps, not by class.
func (e *env) addAgent(runner int64, harness, model, account string) int64 {
	e.t.Helper()
	var id int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '["implement","review","answer"]', ?, ?)`, runner, harness, harness, model, harness, account, seedTS, seedTS)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		e.t.Fatalf("add agent %s/%s: %v", harness, model, err)
	}
	return id
}

// workHarnessed is a work submit for another task of the plan, pinned
// to one harness.
func (e *env) workHarnessed(harness string) SubmitParams {
	e.t.Helper()
	p := e.workOn(e.addTask("{}"))
	p.Harness = harness
	return p
}

// snapAgent finds one agent entry of a snapshot, failing the test when
// the agent is missing.
func snapAgent(t *testing.T, snap CapacitySnapshot, id int64) CapacityAgent {
	t.Helper()
	for _, a := range snap.Agents {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("agent %d is missing from the snapshot's %d agents", id, len(snap.Agents))
	return CapacityAgent{}
}

// layerKeyOf names a layer entry the way the tests ask for one:
// "runner 1", "account work", "model opus", "plan 5" or "global".
func layerKeyOf(l CapacityLayer) string {
	switch l.Layer {
	case caps.LayerRunner:
		return fmt.Sprintf("runner %d", l.RunnerID)
	case caps.LayerAccount:
		return "account " + l.Account
	case caps.LayerModel:
		return "model " + l.Model
	case caps.LayerPlan:
		return fmt.Sprintf("plan %d", l.PlanID)
	default:
		return caps.LayerGlobal
	}
}

// snapLayer finds one layer entry of a snapshot by layerKeyOf's name,
// failing the test when there is no such entry.
func snapLayer(t *testing.T, snap CapacitySnapshot, key string) CapacityLayer {
	t.Helper()
	for _, l := range snap.Layers {
		if layerKeyOf(l) == key {
			return l
		}
	}
	t.Fatalf("layer %q is missing from the snapshot's layers %v", key, snap.Layers)
	return CapacityLayer{}
}

// assertNoLayer fails the test when the snapshot carries a layer entry
// of the given kind — a draft plan, say, which holds no tasks (§12.2).
func assertNoLayer(t *testing.T, snap CapacitySnapshot, kind string) {
	t.Helper()
	for _, l := range snap.Layers {
		if l.Layer == kind {
			t.Errorf("layer %q present, want no %s layer at all", layerKeyOf(l), kind)
		}
	}
}

// TestCapacitySnapshot builds the §12.2 view over two runners, two
// accounts and a capped model: the agents' effective caps with the
// layer that sets each, the running jobs, the quota states (a cooling
// and a broken agent among the ready ones), the cost weights, and each
// layer's cap, use and queue depth — including the cap-held queued jobs
// attributed to their instances by the holds the lease stamped.
func TestCapacitySnapshot(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Global = limitOf(6)
		c.Caps.Plan = limitOf(3)
		c.Caps.Runner = limitOf(2)
		c.Caps.Account = map[string]int{"work": 3, "personal": 1}
		c.Caps.Model = map[string]int{"opus": 1}
		c.Caps.Cost = map[string]int{"codex/opus": 2}
	})
	sonnet := e.addAgent(e.runner1, "claude", "sonnet", "work")
	opus := e.addAgent(e.runner1, "codex", "opus", "work")
	k2 := e.addAgent(e.runner2, "kimi", "k2", "personal")
	opus2 := e.addAgent(e.runner2, "codex", "opus", "work")
	cooling := e.addAgent(e.runner1, "claude", "sonnet", "personal")
	broken := e.addAgent(e.runner1, "claude", "sonnet", "personal")
	// runner 2 advertises its own cap below the config's: the folded cap
	// is the smaller of the two.
	e.exec("UPDATE runners SET max_parallel = 1 WHERE id = ?", e.runner2)
	coolUntil := fmtTime(e.clock.Now().Add(time.Hour))
	e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", coolUntil, cooling)
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", broken)

	plan2 := e.addPlan() // another active plan of the project, never used
	e.exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
		VALUES (?, 'a draft', 'draft', ?, ?)`, e.project, seedTS, seedTS)

	// The running jobs: an opus/work job leased on runner 1's codex
	// agent, a sonnet/work job running on runner 1's claude agent, and a
	// kimi/personal job leased on runner 2.
	first := e.submit(e.workHarnessed("codex"))
	g1 := e.lease(e.runner1, []string{"codex"}, nil)
	if g1 == nil || g1.JobID != first.JobID {
		t.Fatalf("first lease = %+v, want job %d on the codex agent", g1, first.JobID)
	}
	second := e.submit(e.work())
	g2 := e.leaseAny(e.runner1)
	if g2 == nil || g2.JobID != second.JobID {
		t.Fatalf("second lease = %+v, want job %d on the claude agent", g2, second.JobID)
	}
	e.exec("UPDATE jobs SET status = 'running' WHERE id = ?", g2.JobID)

	// The capped model holds two queued codex jobs: the model layer is
	// full while runner 2's account and runner caps still have room, so
	// the holds stamp the model layer with the opus instance.
	third := e.submit(e.workHarnessed("codex"))
	fourth := e.submit(e.workHarnessed("codex"))
	if g := e.lease(e.runner2, []string{"codex"}, nil); g != nil {
		t.Fatalf("a lease through the full opus cap took job %d", g.JobID)
	}
	e.assertHeld(third.JobID, caps.LayerModel)
	e.assertHeld(fourth.JobID, caps.LayerModel)

	sixth := e.submit(e.workHarnessed("kimi"))
	g6 := e.lease(e.runner2, []string{"kimi"}, nil)
	if g6 == nil || g6.JobID != sixth.JobID {
		t.Fatalf("kimi lease = %+v, want job %d", g6, sixth.JobID)
	}

	// Runner 1's own cap is full, so one more open job — offered only to
	// its claude agents — waits on the runner layer, stamped with runner
	// 1's own id.
	fifth := e.submit(e.workOn(e.addTask("{}")))
	if g := e.lease(e.runner1, []string{"claude"}, nil); g != nil {
		t.Fatalf("a lease through the full runner 1 cap took job %d", g.JobID)
	}
	e.assertHeld(fifth.JobID, caps.LayerRunner)

	// Add a model after leasing so its registration cannot affect this setup's dispatch.
	haiku := e.addAgent(e.runner1, "claude", "haiku", "work")
	snap, err := e.eng.Capacity(e.ctx, e.project)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	for id, want := range map[int64]string{
		sonnet: "standard", opus: "heavy", opus2: "heavy", haiku: "light",
		k2: "standard", cooling: "standard", broken: "standard",
	} {
		if got := snapAgent(t, snap, id).Tier; got != want {
			t.Errorf("agent %d tier = %q, want %q", id, got, want)
		}
	}

	agents := []struct {
		id                      int64
		runner                  int64
		harness, model, account string
		cap                     int
		layer                   string
		running                 int
		quota, coolingUntil     string
		cost                    int
	}{
		{sonnet, e.runner1, "claude", "sonnet", "work", 2, caps.LayerRunner, 1, quotaReady, "", 1},
		{opus, e.runner1, "codex", "opus", "work", 1, caps.LayerModel, 1, quotaReady, "", 2},
		{k2, e.runner2, "kimi", "k2", "personal", 1, caps.LayerRunner, 1, quotaReady, "", 1},
		{opus2, e.runner2, "codex", "opus", "work", 1, caps.LayerRunner, 0, quotaReady, "", 2},
		{cooling, e.runner1, "claude", "sonnet", "personal", 1, caps.LayerAccount, 0, quotaCooling, coolUntil, 1},
		{broken, e.runner1, "claude", "sonnet", "personal", 1, caps.LayerAccount, 0, quotaBroken, "", 1},
	}
	for _, want := range agents {
		got := snapAgent(t, snap, want.id)
		if got.RunnerID != want.runner || got.Harness != want.harness || got.Model != want.model || got.Account != want.account {
			t.Errorf("agent %d = %d %s/%s %s, want %d %s/%s %s",
				want.id, got.RunnerID, got.Harness, got.Model, got.Account,
				want.runner, want.harness, want.model, want.account)
		}
		if got.Cap == nil || *got.Cap != want.cap || got.CapLayer != want.layer {
			t.Errorf("agent %d cap = %v (%s), want %d (%s)", want.id, got.Cap, got.CapLayer, want.cap, want.layer)
		}
		if got.Running != want.running {
			t.Errorf("agent %d running = %d, want %d", want.id, got.Running, want.running)
		}
		if got.Quota != want.quota || got.CoolingUntil != want.coolingUntil {
			t.Errorf("agent %d quota = %s/%q, want %s/%q", want.id, got.Quota, got.CoolingUntil, want.quota, want.coolingUntil)
		}
		if got.Cost != want.cost {
			t.Errorf("agent %d cost = %d, want %d", want.id, got.Cost, want.cost)
		}
	}

	layers := []struct {
		key        string
		cap        int
		use, queue int
	}{
		{"runner 1", 2, 2, 1},         // the held open job waits on runner 1 itself
		{"runner 2", 1, 1, 0},         // the advertised 1 folds under the config's 2
		{"account work", 3, 2, 0},     //
		{"account personal", 1, 1, 0}, //
		{"model opus", 1, 1, 2},       // both held codex jobs wait on the capped model
		{fmt.Sprintf("plan %d", e.plan), 3, 3, 0},
		{fmt.Sprintf("plan %d", plan2), 3, 0, 0},
		{"global", 6, 3, 0},
	}
	for _, want := range layers {
		got := snapLayer(t, snap, want.key)
		if got.Cap == nil || *got.Cap != want.cap || got.Use != want.use || got.Queue != want.queue {
			t.Errorf("layer %s = cap %v use %d queue %d, want cap %d use %d queue %d",
				want.key, got.Cap, got.Use, got.Queue, want.cap, want.use, want.queue)
		}
	}
	// Uncapped models earn no layer entry (§12.2), and neither does a
	// plan that is not active.
	for _, l := range snap.Layers {
		if l.Layer == caps.LayerModel && l.Model != "opus" {
			t.Errorf("model layer %q present, want only the capped opus", l.Model)
		}
		if l.Layer == caps.LayerPlan && l.PlanID != e.plan && l.PlanID != plan2 {
			t.Errorf("plan layer %d present, want only the active plans %d and %d", l.PlanID, e.plan, plan2)
		}
	}

	// The project's scope: another project id leaves everything but the
	// plan layer alone, and 0 — every project's — lists the same plans.
	other, err := e.eng.Capacity(e.ctx, e.project+1000)
	if err != nil {
		t.Fatalf("capacity of another project: %v", err)
	}
	assertNoLayer(t, other, caps.LayerPlan)
	if len(other.Agents) != len(snap.Agents) {
		t.Errorf("another project's agents = %d, want the same %d", len(other.Agents), len(snap.Agents))
	}
	all, err := e.eng.Capacity(e.ctx, 0)
	if err != nil {
		t.Fatalf("capacity of every project: %v", err)
	}
	snapLayer(t, all, fmt.Sprintf("plan %d", e.plan))
	snapLayer(t, all, fmt.Sprintf("plan %d", plan2))

	e.eng.cfg.Caps.Tier = map[string]string{"claude/sonnet": "heavy"}
	overridden, err := e.eng.Capacity(e.ctx, e.project)
	if err != nil {
		t.Fatalf("capacity with override: %v", err)
	}
	for _, id := range []int64{sonnet, cooling, broken} {
		if got := snapAgent(t, overridden, id).Tier; got != "heavy" {
			t.Errorf("agent %d overridden tier = %q, want heavy", id, got)
		}
	}
	if got := snapAgent(t, overridden, haiku).Tier; got != "light" {
		t.Errorf("haiku tier after sonnet override = %q, want light", got)
	}
}

// TestCapacityUncappedAgent covers the agent no layer caps: its cap
// reads nil with the layer "none", and a cooling deadline already past
// reads ready, not cooling.
func TestCapacityUncappedAgent(t *testing.T) {
	e := newEnv(t) // the zero Limits cap nothing
	uncapped := e.addAgent(e.runner1, "claude", "sonnet", "work")
	lapsed := e.addAgent(e.runner1, "claude", "sonnet", "personal")
	e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?",
		fmtTime(e.clock.Now().Add(-time.Minute)), lapsed)

	snap, err := e.eng.Capacity(e.ctx, e.project)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	got := snapAgent(t, snap, uncapped)
	if got.Cap != nil || got.CapLayer != capLayerNone {
		t.Errorf("uncapped agent's cap = %v (%s), want nil (none)", got.Cap, got.CapLayer)
	}
	if got.Cost != 1 {
		t.Errorf("uncapped agent's cost = %d, want the default weight 1", got.Cost)
	}
	if got.Quota != quotaReady {
		t.Errorf("uncapped agent's quota = %s, want ready", got.Quota)
	}
	if got := snapAgent(t, snap, lapsed); got.Quota != quotaReady || got.CoolingUntil != "" {
		t.Errorf("lapsed cooling reads %s/%q, want ready with no deadline", got.Quota, got.CoolingUntil)
	}
	for _, key := range []string{"runner 1", "runner 2", "account work", "account personal", "global"} {
		if l := snapLayer(t, snap, key); l.Cap != nil {
			t.Errorf("layer %s cap = %d, want nil: nothing is capped", key, *l.Cap)
		}
	}
	assertNoLayer(t, snap, caps.LayerModel)
}

// TestCapacityAgentTools covers the one field the agent registration
// records for the foreman: the worker scope as advertised, empty for an
// agent that advertised none.
func TestCapacityAgentTools(t *testing.T) {
	e := newEnv(t)
	scoped := e.addAgent(e.runner1, "claude", "sonnet", "work")
	plain := e.addAgent(e.runner1, "codex", "gpt", "work")
	e.exec("UPDATE agents SET tools = ? WHERE id = ?", "Bash(npm test:*)+Bash(npx jest:*)", scoped)

	snap, err := e.eng.Capacity(e.ctx, e.project)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if got := snapAgent(t, snap, scoped).Tools; got != "Bash(npm test:*)+Bash(npx jest:*)" {
		t.Errorf("scoped agent's tools = %q", got)
	}
	if got := snapAgent(t, snap, plain).Tools; got != "" {
		t.Errorf("unscoped agent's tools = %q, want empty", got)
	}
}

// TestCapacityModelHoldInstance covers the queue depth's attribution by
// the hold's stamped instance, not the harness's first agent (§12.2):
// the first codex agent by id — the one a harness guess would pick —
// sits on the uncapped gpt model, yet the job the full opus cap holds
// counts at the opus layer. Guessing would credit gpt, which no layer
// entry carries, and the foreman would read queue 0 while work waits.
func TestCapacityModelHoldInstance(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Model = map[string]int{"opus": 1}
	})
	// Ids in creation order: the gpt agent is the first codex agent by id,
	// on runner 1; the capped opus agents sit behind it and on runner 2.
	e.addAgent(e.runner1, "codex", "gpt", "work")
	e.addAgent(e.runner1, "codex", "opus", "work")
	e.addAgent(e.runner2, "codex", "opus", "work")

	// Fill the opus cap through runner 2, whose codex agent carries opus.
	first := e.submit(e.workHarnessed("codex"))
	if g := e.lease(e.runner2, []string{"codex"}, nil); g == nil || g.JobID != first.JobID {
		t.Fatalf("first lease = %+v, want job %d on runner 2's opus agent", g, first.JobID)
	}

	// Another codex job: runner 2's poll finds the opus cap full and
	// stamps the hold with the opus instance.
	second := e.submit(e.workHarnessed("codex"))
	if g := e.lease(e.runner2, []string{"codex"}, nil); g != nil {
		t.Fatalf("a lease through the full opus cap took job %d", g.JobID)
	}
	e.assertHeld(second.JobID, caps.LayerModel)
	if got := e.str("SELECT COALESCE(cap_held_key, '') FROM jobs WHERE id = ?", second.JobID); got != "opus" {
		t.Fatalf("cap_held_key = %q, want the held instance %q", got, "opus")
	}

	snap, err := e.eng.Capacity(e.ctx, e.project)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if l := snapLayer(t, snap, "model opus"); l.Queue != 1 || l.Use != 1 {
		t.Errorf("model opus = use %d queue %d, want 1/1: the held job waits on the capped model",
			l.Use, l.Queue)
	}
	for _, l := range snap.Layers {
		if l.Layer == caps.LayerModel && l.Model != "opus" {
			t.Errorf("model layer %q present with queue %d, want only the capped opus carrying the hold",
				l.Model, l.Queue)
		}
	}
}
