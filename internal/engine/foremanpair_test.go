package engine

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// setForeman chooses the fixture project's foreman pair.
func (e *env) setForeman(harness, model string) {
	e.t.Helper()
	if _, err := e.eng.SetProjectForeman(e.ctx, effortRepo, harness, model); err != nil {
		e.t.Fatalf("choose the foreman %s/%s: %v", harness, model, err)
	}
}

// heldNow reads the status's held jobs.
func (e *env) heldNow() []HeldJob {
	e.t.Helper()
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	return info.Held
}

// A foreman turn goes to the agent that offers the project's model, not
// to the first agent of the harness, and the grant names that agent's
// adapter and model for the runner to run. Workers' grants carry none.
func TestForemanTurnPicksTheChosenModel(t *testing.T) {
	e := newEnv(t)
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work") // the lower id
	e.seedAgent(e.runner1, "claude", "claude", "opus", "work")
	e.setForeman("claude", "opus")

	res, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
	if err != nil {
		t.Fatalf("submit the turn: %v", err)
	}
	g := e.leaseClaude(e.runner1)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("the lease did not take the turn: %+v", g)
	}
	if g.Agent != "claude" || g.AgentAdapter != "claude" || g.AgentModel != "opus" {
		t.Errorf("grant agent = %q adapter %q model %q, want claude/claude/opus", g.Agent, g.AgentAdapter, g.AgentModel)
	}
	if got, want := e.int("SELECT agent_id FROM jobs WHERE id = ?", res.JobID), e.int("SELECT id FROM agents WHERE model = 'opus'"); got != want {
		t.Errorf("the turn's agent = %d, want the opus agent %d", got, want)
	}
	if got := e.str("SELECT model FROM jobs WHERE id = ?", res.JobID); got != "opus" {
		t.Errorf("the turn's recorded model = %q, want opus", got)
	}

	// A worker's lease is what it was: the harness names its agent.
	e.submit(e.work())
	w := e.leaseAny(e.runner1)
	if w == nil || w.Kind != KindWork {
		t.Fatalf("the worker was not leased: %+v", w)
	}
	if w.AgentAdapter != "" || w.AgentModel != "" {
		t.Errorf("a worker grant names adapter %q model %q, want none", w.AgentAdapter, w.AgentModel)
	}
}

// Two registrations may share a harness and model and differ in adapter:
// when the first is ineligible the second serves the turn, and the grant
// names it.
func TestForemanTurnSkipsAnIneligibleRegistration(t *testing.T) {
	e := newEnv(t)
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.seedAgent(e.runner1, "claude-alt", "claude", "sonnet", "work")
	first := e.int("SELECT id FROM agents WHERE adapter = 'claude'")
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", first)

	res := e.submit(e.foremanTurn())
	g := e.leaseClaude(e.runner1)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("the lease did not take the turn: %+v", g)
	}
	if g.AgentAdapter != "claude-alt" || g.AgentModel != "sonnet" {
		t.Errorf("grant adapter %q model %q, want the second registration claude-alt/sonnet", g.AgentAdapter, g.AgentModel)
	}
}

// With no registered agent for the pair — the agents table empty, or only
// other models — the turn is not served by anything else: it is held past
// the queue TTL, the status says why, and registering the pair releases it.
func TestForemanTurnWaitsForAnAgentOfItsPair(t *testing.T) {
	e := newEnv(t)
	e.setForeman("claude", "opus")
	res, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
	if err != nil {
		t.Fatalf("submit the turn: %v", err)
	}
	if g := e.leaseClaude(e.runner1); g != nil {
		t.Fatalf("an agentless runner took the turn: %+v", g)
	}
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work") // another model
	if g := e.leaseClaude(e.runner1); g != nil {
		t.Fatalf("a sonnet agent took the opus turn: %+v", g)
	}

	// Past the TTL it is still queued, held, and named.
	e.clock.Advance(e.eng.cfg.QueueTTL + time.Minute)
	if g := e.leaseClaude(e.runner1); g != nil {
		t.Fatalf("a sonnet agent took the opus turn: %+v", g)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", res.JobID); status != statusQueued {
		t.Fatalf("the turn's status = %q, want queued: it waits, never expires", status)
	}
	held := e.heldNow()
	if len(held) != 1 || held[0].ID != res.JobID || held[0].Layer != layerNoPair || held[0].Agent != "claude/opus" {
		t.Fatalf("status held = %+v, want job %d held for claude/opus", held, res.JobID)
	}
	e.clock.Advance(e.eng.cfg.QueueTTL + time.Minute)
	e.leaseClaude(e.runner1)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", res.JobID); status != statusQueued {
		t.Fatalf("the turn's status after a second TTL = %q, want queued", status)
	}

	// An unrelated runner polling clears nothing.
	e.leaseClaude(e.runner2)
	if held := e.heldNow(); len(held) != 1 || held[0].Layer != layerNoPair {
		t.Errorf("status held after an unrelated poll = %+v, want the hold kept", held)
	}

	// The pair registers: the next poll takes the turn.
	e.seedAgent(e.runner1, "claude", "claude", "opus", "work")
	g := e.leaseClaude(e.runner1)
	if g == nil || g.JobID != res.JobID || g.AgentModel != "opus" {
		t.Fatalf("the lease after the pair registered = %+v, want the turn on opus", g)
	}
	if held := e.heldNow(); len(held) != 0 {
		t.Errorf("status held after the lease = %+v, want none", held)
	}
}

// A registered agent that is cooling or broken is a different wait from no
// agent at all: the choice does not warn, and the turn is not held as
// missing a pair.
func TestForemanChoiceWarnsOnlyWhenNoAgentIsRegistered(t *testing.T) {
	e := newEnv(t)
	got, err := e.eng.SetProjectForeman(e.ctx, effortRepo, "claude", "opus")
	if err != nil || !got.Unserved {
		t.Fatalf("choice with no agents = %+v, %v; want Unserved", got, err)
	}
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	if got, err = e.eng.SetProjectForeman(e.ctx, effortRepo, "claude", "opus"); err != nil || !got.Unserved {
		t.Fatalf("choice with only another model = %+v, %v; want Unserved", got, err)
	}
	opus := e.addAgent(e.runner1, "claude", "opus", "work")
	e.coolUntil(opus, time.Hour)
	if got, err = e.eng.SetProjectForeman(e.ctx, effortRepo, "claude", "opus"); err != nil || got.Unserved {
		t.Fatalf("choice with a cooling agent = %+v, %v; want served", got, err)
	}
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", opus)
	if got, err = e.eng.SetProjectForeman(e.ctx, effortRepo, "claude", "opus"); err != nil || got.Unserved {
		t.Fatalf("choice with a broken agent = %+v, %v; want served", got, err)
	}
	read, err := e.eng.ProjectForeman(e.ctx, effortRepo)
	if err != nil || read == nil || *read != got {
		t.Fatalf("the setting read back = %+v, %v; want %+v", read, err, got)
	}

	// The cooling agent holds its turn as cooling, not as a missing pair.
	e.exec("UPDATE agents SET health = 'ready' WHERE id = ?", opus)
	res, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
	if err != nil {
		t.Fatalf("submit the turn: %v", err)
	}
	e.clock.Advance(time.Minute)
	e.leaseClaude(e.runner1)
	if layer := e.heldLayer(res.JobID); layer != layerCooling {
		t.Errorf("a turn whose only agent cools is held at %q, want %q", layer, layerCooling)
	}
}

// A runner registered for codex but polling only for claude must not
// receive the project's codex turn, however the turn was submitted (the
// admin path leaves its harness empty), and its poll must not hold or
// reserve capacity for it.
func TestForemanTurnNarrowedPoll(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Caps.Global = limitOf(1) })
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
	e.seedAgent(e.runner1, "codex", "codex", "default", "other")
	e.seedAgent(e.runner2, "claude", "claude", "sonnet", "work")
	e.setForeman("codex", "default")

	turn := e.submit(e.foremanTurn()) // empty harness and model: the project's pair
	if got := e.str("SELECT harness FROM jobs WHERE id = ?", turn.JobID); got != "" {
		t.Fatalf("the admin turn's harness = %q, want it empty", got)
	}
	// The global cap is full, so a poll that reached the turn would hold
	// it at the global layer and reserve that layer.
	e.submit(e.work())
	fill := e.lease(e.runner2, []string{"claude"}, nil)
	if fill == nil || fill.Kind != KindWork {
		t.Fatalf("the filling lease = %+v, want the worker", fill)
	}
	if g := e.lease(e.runner1, []string{"claude"}, nil); g != nil {
		t.Fatalf("a runner polling for claude received %+v", g)
	}
	if layer := e.heldLayer(turn.JobID); layer != "" {
		t.Errorf("the narrowed poll held the codex turn at %q, want no hold", layer)
	}
	if layers := e.capHeldLayers(turn.JobID); len(layers) != 0 {
		t.Errorf("job_cap_held events of the turn = %v, want none", layers)
	}

	// A poll offering codex does reach it (and finds the cap full).
	if g := e.lease(e.runner1, []string{"codex"}, nil); g != nil {
		t.Fatalf("a poll through the full global cap took %+v", g)
	}
	e.assertHeld(turn.JobID, caps.LayerGlobal)
	if ans := e.complete(fill, done()); ans != AnsOK {
		t.Fatalf("free the slot: %s", ans)
	}
	g := e.lease(e.runner1, []string{"codex"}, nil)
	if g == nil || g.JobID != turn.JobID || g.Agent != "codex" || g.AgentModel != "default" {
		t.Fatalf("the codex poll after the slot freed = %+v, want the turn on codex/default", g)
	}
}

// The busy-runner rule counts a runner as a candidate for a foreman turn
// only when its poll offers the turn's resolved harness: a full runner
// registered for codex but polling for claude leaves the codex turn to
// the plain queue TTL, while one polling for codex keeps it waiting.
func TestForemanTurnBusyRunnerCount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		harnesses  []string
		wantStatus string
		wantLayer  string
	}{
		{"polling for claude only", []string{"claude"}, statusFailed, ""},
		{"polling for codex too", []string{"claude", "codex"}, statusQueued, "runner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) { c.LeaseTTL = time.Hour })
			e.seedAgent(e.runner1, "claude", "claude", "sonnet", "work")
			e.seedAgent(e.runner1, "codex", "codex", "default", "other")
			e.setForeman("codex", "default")
			e.submit(e.work())
			g := e.lease(e.runner1, tc.harnesses, nil) // the runner is now full
			if g == nil || g.Kind != KindWork {
				t.Fatalf("nothing to lease: %+v", g)
			}
			turn := e.submit(e.foremanTurn())
			for i := 0; i < 2; i++ {
				e.clock.Advance(9 * time.Minute)
				if ans := e.heartbeat(g); ans != AnsOK {
					t.Fatalf("heartbeat = %s; want ok", ans)
				}
			}
			if got := e.str("SELECT status FROM jobs WHERE id = ?", turn.JobID); got != tc.wantStatus {
				t.Errorf("the turn's status = %q, want %q", got, tc.wantStatus)
			}
			if tc.wantLayer != "" {
				if layer := e.heldLayer(turn.JobID); layer != tc.wantLayer {
					t.Errorf("the turn's hold = %q, want %q", layer, tc.wantLayer)
				}
			}
		})
	}
}

// A turn queued on another harness with no model recorded (a legacy row)
// is unrecoverable: it is held, never run on whatever model a runner
// offers.
func TestUnrecoverableForemanTurnIsHeld(t *testing.T) {
	e := newEnv(t)
	e.seedAgent(e.runner1, "codex", "codex", "default", "other")
	e.exec(`INSERT INTO jobs (project_id, kind, key, mode, prompt, status, harness, queued_at, created_at, updated_at)
		VALUES (?, 'foreman', ?, 'session', 'p', 'queued', 'codex', ?, ?, ?)`,
		e.project, "foreman:legacy", seedTS, seedTS, seedTS)
	e.clock.Advance(time.Minute)
	if g := e.lease(e.runner1, []string{"codex"}, nil); g != nil {
		t.Fatalf("the codex runner took the unrecoverable turn: %+v", g)
	}
	held := e.heldNow()
	if len(held) != 1 || held[0].Layer != layerNoPair || held[0].Agent != "codex/?" {
		t.Fatalf("status held = %+v, want the turn held with no recoverable pair", held)
	}
}

// A turn leased under one harness that outlives a switch away and back
// still leaves no session: each harness change moved the project's session
// generation on, and the turn's lease is bound to the old one.
func TestTurnAcrossASwitchAwayAndBackLeavesNoSession(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	res := e.submit(e.foremanTurn())
	g := e.leaseClaude(e.runner1)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("the lease did not take the turn: %+v", g)
	}
	e.setForeman("codex", "default")
	e.setForeman("claude", "sonnet")
	if ans := e.complete(g, Completion{Result: "judged", SessionID: "stale-sess"}); ans != AnsOK {
		t.Fatalf("complete: %s", ans)
	}
	key := fmt.Sprintf("foreman:%d", e.project)
	if got := e.str("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", key); got != "" {
		t.Errorf("session after a completion from an older generation = %q, want none", got)
	}

	// A turn leased under the current generation still writes it back.
	res = e.submit(e.foremanTurn())
	g = e.leaseClaude(e.runner1)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("the second lease did not take the turn: %+v", g)
	}
	if ans := e.complete(g, Completion{Result: "judged", SessionID: "fresh-sess"}); ans != AnsOK {
		t.Fatalf("complete: %s", ans)
	}
	if got := e.str("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", key); got != "fresh-sess" {
		t.Errorf("session after a current completion = %q, want fresh-sess", got)
	}
}

// The headroom rule reserves the account of the agent the foreman's turns
// run on — the project's pair — not every account of the harness.
func TestHeadroomReservesTheChosenPairsAccount(t *testing.T) {
	e := headroomEnv(t, 3, nil) // claude/sonnet on "work", kimi on "personal"
	e.seedAgent(e.runner1, "claude", "claude", "opus", "opus-acct")
	e.setForeman("claude", "opus")

	var accounts []string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		accounts, err = foremanAccountsTx(tx, e.project)
		return err
	}); err != nil {
		t.Fatalf("read the foreman accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0] != "opus-acct" {
		t.Errorf("reserved accounts = %v, want only the opus agent's [opus-acct]", accounts)
	}
}

// A turn that ran on one harness and completes after the project switched
// to another must not leave its session id behind: the new foreman would
// resume the old CLI's session.
func TestStaleHarnessTurnLeavesNoSession(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	res := e.submit(e.foremanTurn())
	g := e.leaseClaude(e.runner1)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("the lease did not take the turn: %+v", g)
	}
	e.setForeman("codex", "default")
	if ans := e.complete(g, Completion{Result: "judged", SessionID: "claude-sess"}); ans != AnsOK {
		t.Fatalf("complete: %s", ans)
	}
	key := fmt.Sprintf("foreman:%d", e.project)
	if got := e.str("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", key); got != "" {
		t.Errorf("session after a stale-harness completion = %q, want none", got)
	}
}
