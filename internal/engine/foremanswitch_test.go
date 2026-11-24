package engine

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
)

// A foreman switch carries every queued turn that has a recorded model to
// the new pair. The turn keeps its prompt and queue position, and the answer
// names the moves in job order; an effort-only choice moves nothing.
func TestForemanSwitchCarriesWaitingTurns(t *testing.T) {
	e := newEnv(t)
	e.setForeman("claude", "opus")
	first, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
	if err != nil {
		t.Fatalf("submit the first turn: %v", err)
	}
	e.exec(`INSERT INTO jobs (project_id, kind, key, mode, prompt, status, harness, model, queued_at, created_at, updated_at)
		VALUES (?, 'foreman', 'foreman:second', 'session', 'second prompt', 'queued', 'claude', 'opus', ?, ?, ?)`,
		e.project, seedTS, seedTS, seedTS)
	second := e.int("SELECT id FROM jobs WHERE key = 'foreman:second'")
	prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", first.JobID)
	queuedAt := e.str("SELECT queued_at FROM jobs WHERE id = ?", first.JobID)
	e.seedAgent(e.runner1, "codex", "codex", "default", "other")

	got, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, "codex", "default", "xhigh")
	if err != nil {
		t.Fatalf("switch the foreman: %v", err)
	}
	if want := []int64{first.JobID, second}; !reflect.DeepEqual(got.MovedTurns, want) {
		t.Fatalf("moved turns = %v, want %v", got.MovedTurns, want)
	}
	if got.Unserved {
		t.Error("the served pair is marked unserved")
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE id IN (?, ?) AND harness = 'codex' AND model = 'default'", first.JobID, second); n != 2 {
		t.Errorf("turns on the new pair = %d, want 2", n)
	}
	if got := e.str("SELECT prompt FROM jobs WHERE id = ?", first.JobID); got != prompt {
		t.Errorf("the moved turn's prompt = %q, want it unchanged", got)
	}
	if got := e.str("SELECT queued_at FROM jobs WHERE id = ?", first.JobID); got != queuedAt {
		t.Errorf("the moved turn's queue stamp = %q, want %q", got, queuedAt)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND job_id IN (?, ?)", evForemanTurnMoved, first.JobID, second); n != 2 {
		t.Errorf("move events = %d, want one per turn", n)
	}
	var payload eventPayload
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRow("SELECT payload FROM events WHERE kind = ? AND job_id = ?", evForemanTurnMoved, first.JobID).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal([]byte(raw), &payload)
	}); err != nil {
		t.Fatalf("read the move event: %v", err)
	}
	if payload.Harness != "codex" || payload.Model != "default" || payload.FromHarness != "claude" || payload.FromModel != "opus" {
		t.Errorf("move event payload = %+v, want claude/opus to codex/default", payload)
	}

	effort, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, "codex", "default", "high")
	if err != nil {
		t.Fatalf("change only the effort: %v", err)
	}
	if effort.MovedTurns != nil {
		t.Errorf("an effort-only choice moved %v, want none", effort.MovedTurns)
	}
	g := e.lease(e.runner1, []string{"codex"}, nil)
	if g == nil || g.JobID != first.JobID || g.AgentModel != "default" {
		t.Fatalf("the lease after the switch = %+v, want the first turn on codex/default", g)
	}
}

// A switch to a pair nobody serves still moves the turn: its hold names the
// new pair immediately, and registering that pair releases it.
func TestForemanSwitchToUnservedPairMovesTheHold(t *testing.T) {
	e := newEnv(t)
	e.setForeman("claude", "opus")
	turn, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
	if err != nil {
		t.Fatalf("submit the turn: %v", err)
	}
	got, err := e.eng.SetProjectForeman(e.ctx, effortRepo, "codex", "default")
	if err != nil {
		t.Fatalf("switch the foreman: %v", err)
	}
	if want := []int64{turn.JobID}; !reflect.DeepEqual(got.MovedTurns, want) {
		t.Fatalf("moved turns = %v, want %v", got.MovedTurns, want)
	}
	if !got.Unserved {
		t.Error("the unserved pair is not marked unserved")
	}
	held := e.heldNow()
	if len(held) != 1 || held[0].ID != turn.JobID || held[0].Layer != layerNoPair || held[0].Agent != "codex/default" {
		t.Fatalf("status held = %+v, want the turn held for codex/default", held)
	}
	e.seedAgent(e.runner1, "codex", "codex", "default", "other")
	if g := e.lease(e.runner1, []string{"codex"}, nil); g == nil || g.JobID != turn.JobID {
		t.Fatalf("the lease after codex/default registered = %+v, want the moved turn", g)
	}
}

// A leased turn is not moved by a switch. If its lease lapses or its runner
// releases it, the requeue puts it on the pair the project holds then.
func TestForemanTurnRequeuedAcrossSwitchTakesTheProjectPair(t *testing.T) {
	for _, tc := range []struct {
		name    string
		requeue func(*env, *LeaseGrant)
	}{
		{"lapsed lease", func(e *env, _ *LeaseGrant) { e.expireLease() }},
		{"runner release", func(e *env, g *LeaseGrant) {
			if ans := e.release(g.LeaseOwner, g, "transient"); ans != AnsOK {
				e.t.Fatalf("release = %s, want ok", ans)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.offerForeman(e.runner1)
			turn, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
			if err != nil {
				t.Fatalf("submit the turn: %v", err)
			}
			g := e.leaseClaude(e.runner1)
			if g == nil || g.JobID != turn.JobID {
				t.Fatalf("the lease = %+v, want the foreman turn", g)
			}
			got, err := e.eng.SetProjectForeman(e.ctx, effortRepo, "codex", "default")
			if err != nil {
				t.Fatalf("switch the foreman: %v", err)
			}
			if got.MovedTurns != nil {
				t.Fatalf("the switch moved the leased turn: %v", got.MovedTurns)
			}
			if harness, model := e.str("SELECT harness FROM jobs WHERE id = ?", turn.JobID), e.str("SELECT model FROM jobs WHERE id = ?", turn.JobID); harness != "claude" || model != "sonnet" {
				t.Fatalf("the leased turn's pair = %s/%s, want claude/sonnet", harness, model)
			}
			tc.requeue(e, g)
			if status := e.str("SELECT status FROM jobs WHERE id = ?", turn.JobID); status != statusQueued {
				t.Fatalf("the requeued turn's status = %q, want queued", status)
			}
			if harness, model := e.str("SELECT harness FROM jobs WHERE id = ?", turn.JobID), e.str("SELECT model FROM jobs WHERE id = ?", turn.JobID); harness != "codex" || model != "default" {
				t.Errorf("the requeued turn's pair = %s/%s, want codex/default", harness, model)
			}
			if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND job_id = ?", evForemanTurnMoved, turn.JobID); n != 1 {
				t.Errorf("move events = %d, want 1", n)
			}
		})
	}
}

// A legacy queued turn on another harness with no model recorded stays
// unresolved when the project's foreman changes.
func TestForemanSwitchLeavesAnUnresolvedLegacyTurn(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO jobs (project_id, kind, key, mode, prompt, status, harness, queued_at, created_at, updated_at)
		VALUES (?, 'foreman', 'foreman:legacy', 'session', 'p', 'queued', 'codex', ?, ?, ?)`,
		e.project, seedTS, seedTS, seedTS)
	turn := e.int("SELECT id FROM jobs WHERE key = 'foreman:legacy'")
	got, err := e.eng.SetProjectForeman(e.ctx, effortRepo, "claude", "opus")
	if err != nil {
		t.Fatalf("switch the foreman: %v", err)
	}
	if got.MovedTurns != nil {
		t.Errorf("the switch moved %v, want none", got.MovedTurns)
	}
	if harness, model := e.str("SELECT harness FROM jobs WHERE id = ?", turn), e.str("SELECT model FROM jobs WHERE id = ?", turn); harness != "codex" || model != "" {
		t.Errorf("the legacy turn's pair = %s/%s, want codex with no model", harness, model)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND job_id = ?", evForemanTurnMoved, turn); n != 0 {
		t.Errorf("move events = %d, want none", n)
	}
}
