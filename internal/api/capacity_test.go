package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// exec runs one statement against the raw store, for seeding the rows
// an endpoint reads.
func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	}); err != nil {
		e.t.Fatalf("exec %q: %v", query, err)
	}
}

// TestCapacityRoles covers /v1/capacity's admission (§12.1, §12.2): the
// runner role is refused, a missing token is 401, and the foreman,
// client and admin roles read the snapshot — the foreman only ever its
// own project's plans, walled off from another project's by
// mayTouchProject whether it asks for them or not.
func TestCapacityRoles(t *testing.T) {
	e := newEnv(t)
	plan2 := e.seedCapacityRows()

	// The roles that do not belong are refused, uniformly.
	code, _, data := e.do(http.MethodGet, "/v1/capacity", "", "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s, want 401", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeUnauthorized {
		t.Errorf("no token: code = %q, want %q", err.Code, codeUnauthorized)
	}
	code, _, data = e.do(http.MethodGet, "/v1/capacity", "r1", "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("runner reads capacity: %d %s, want 403", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeForbidden {
		t.Errorf("runner: code = %q, want %q", err.Code, codeForbidden)
	}
	code, _, data = e.do(http.MethodPost, "/v1/capacity", "alice", "", nil)
	if code != http.StatusMethodNotAllowed {
		t.Errorf("POST capacity: %d %s, want 405", code, data)
	}

	// A client and an admin read every project's plans.
	for _, token := range []string{"alice", "root"} {
		code, _, data = e.do(http.MethodGet, "/v1/capacity", token, "", nil)
		if code != http.StatusOK {
			t.Fatalf("%s reads capacity: %d %s, want 200", token, code, data)
		}
		snap := decode[engine.CapacitySnapshot](t, data)
		assertCapacityPlans(t, snap, e.plan, 0)
		if !hasPlanLayer(snap, plan2) {
			t.Errorf("%s reads capacity: project 2's plan %d missing, want every project's plans", token, plan2)
		}
	}

	// The foreman reads its own project's plans only: no ask means its
	// own project, and an explicit ask for another is a 403 — never the
	// other project's plans.
	code, _, data = e.do(http.MethodGet, "/v1/capacity", "foreman1", "", nil)
	if code != http.StatusOK {
		t.Fatalf("foreman reads capacity: %d %s, want 200", code, data)
	}
	snap := decode[engine.CapacitySnapshot](t, data)
	assertCapacityPlans(t, snap, e.plan, plan2)

	code, _, data = e.do(http.MethodGet, "/v1/capacity?project=2", "foreman1", "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("foreman asks for project 2: %d %s, want 403", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeForbidden {
		t.Errorf("foreman scope: code = %q, want %q", err.Code, codeForbidden)
	}
	code, _, data = e.do(http.MethodGet, "/v1/capacity?project=1", "foreman1", "", nil)
	if code != http.StatusOK {
		t.Fatalf("foreman asks for its own project: %d %s, want 200", code, data)
	}

	// A malformed project is the caller's mistake, not the engine's.
	code, _, data = e.do(http.MethodGet, "/v1/capacity?project=one", "alice", "", nil)
	if code != http.StatusBadRequest {
		t.Errorf("capacity?project=one: %d %s, want 400", code, data)
	}
}

// TestCapacityBody covers the snapshot the endpoint serves (§12.2):
// the agent row — harness, model, account, uncapped cap, running job,
// ready quota, default cost — and the layer rows' cap, use and queue
// depth, with the held queued jobs attributed to the plan and global
// layers that hold them.
func TestCapacityBody(t *testing.T) {
	e := newEnv(t)
	e.seedCapacityRows()

	code, _, data := e.do(http.MethodGet, "/v1/capacity", "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("capacity: %d %s, want 200", code, data)
	}
	snap := decode[engine.CapacitySnapshot](t, data)
	if len(snap.Agents) != 1 {
		t.Fatalf("agents = %d, want the one seeded agent", len(snap.Agents))
	}
	a := snap.Agents[0]
	if a.Harness != "claude" || a.Model != "sonnet" || a.Account != "work" {
		t.Errorf("agent = %s/%s %s, want claude/sonnet on the work account", a.Harness, a.Model, a.Account)
	}
	if a.Cap != nil || a.CapLayer != "none" {
		t.Errorf("agent cap = %v (%s), want nil (none): the test engine caps nothing", a.Cap, a.CapLayer)
	}
	if a.Running != 1 || a.Quota != "ready" || a.Cost != 1 {
		t.Errorf("agent running/quota/cost = %d/%s/%d, want 1/ready/1", a.Running, a.Quota, a.Cost)
	}

	layer := func(kind, key string) engine.CapacityLayer {
		t.Helper()
		for _, l := range snap.Layers {
			switch kind {
			case "runner":
				if l.Layer == "runner" && fmt.Sprint(l.RunnerID) == key {
					return l
				}
			case "account":
				if l.Layer == "account" && l.Account == key {
					return l
				}
			case "plan":
				if l.Layer == "plan" && fmt.Sprint(l.PlanID) == key {
					return l
				}
			case "global":
				if l.Layer == "global" {
					return l
				}
			}
		}
		t.Fatalf("layer %s %s missing from %v", kind, key, snap.Layers)
		return engine.CapacityLayer{}
	}
	if l := layer("runner", "1"); l.Use != 1 || l.Queue != 0 || l.Cap != nil {
		t.Errorf("runner layer = use %d queue %d cap %v, want use 1 queue 0 cap nil", l.Use, l.Queue, l.Cap)
	}
	if l := layer("account", "work"); l.Use != 1 || l.Queue != 0 {
		t.Errorf("work account = use %d queue %d, want 1/0", l.Use, l.Queue)
	}
	if l := layer("plan", fmt.Sprint(e.plan)); l.Use != 1 || l.Queue != 1 {
		t.Errorf("plan layer = use %d queue %d, want 1/1: the held task job waits on its plan", l.Use, l.Queue)
	}
	if l := layer("global", ""); l.Use != 1 || l.Queue != 1 {
		t.Errorf("global layer = use %d queue %d, want 1/1: the held foreman job waits on global", l.Use, l.Queue)
	}
}

// seedCapacityRows seeds one runner with one agent, a running job on
// the agent, a queued job held at its task's plan and a queued foreman
// job held at the global cap, plus an active plan in the second
// project. It returns the second project's plan id.
func (e *env) seedCapacityRows() (plan2 int64) {
	e.t.Helper()
	var runner int64
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO runners (name, created_at, last_seen_at) VALUES ('cap-runner', ?, ?)`, seedTS, seedTS)
		if err != nil {
			return err
		}
		runner, err = res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.Exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
			VALUES (?, 'claude', 'claude', 'sonnet', 'claude', 'work', '[]', ?, ?)`, runner, seedTS, seedTS)
		if err != nil {
			return err
		}
		agent, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// The running job: live, recorded on the agent and held by the
		// runner, so the usage view counts it at both layers.
		_, err = tx.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, agent_id, lease_owner, mode, prompt, status, created_at, updated_at)
			VALUES (?, ?, 'work', 'task:1', ?, ?, 'fresh', 'prompt', 'running', ?, ?)`, e.project1, e.task, agent, runner, seedTS, seedTS)
		if err != nil {
			return err
		}
		// A queued job the plan cap holds.
		_, err = tx.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, cap_held_at, cap_held_layer, created_at, updated_at)
			VALUES (?, ?, 'work', 'task:1-retry', 'fresh', 'prompt', 'queued', ?, 'plan', ?, ?)`,
			e.project1, e.task, seedTS, seedTS, seedTS)
		if err != nil {
			return err
		}
		// A queued foreman job the global cap holds.
		_, err = tx.Exec(`INSERT INTO jobs (project_id, kind, key, mode, prompt, status, cap_held_at, cap_held_layer, created_at, updated_at)
			VALUES (?, 'foreman', 'foreman:1', 'session', 'prompt', 'queued', ?, 'global', ?, ?)`,
			e.project1, seedTS, seedTS, seedTS)
		if err != nil {
			return err
		}
		res, err = tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (?, 'project 2 plan', 'active', ?, ?)`, e.project2, seedTS, seedTS)
		if err != nil {
			return err
		}
		plan2, err = res.LastInsertId()
		return err
	})
	if err != nil {
		e.t.Fatalf("seed capacity rows: %v", err)
	}
	return plan2
}

// hasPlanLayer reports whether the snapshot carries a layer entry for
// one plan.
func hasPlanLayer(snap engine.CapacitySnapshot, plan int64) bool {
	for _, l := range snap.Layers {
		if l.Layer == "plan" && l.PlanID == plan {
			return true
		}
	}
	return false
}

// assertCapacityPlans checks the plan layer of a snapshot: the wanted
// plan present with its use and queue depth, the forbidden one absent —
// the foreman's scope (§12.1).
func assertCapacityPlans(t *testing.T, snap engine.CapacitySnapshot, want, forbid int64) {
	t.Helper()
	for _, l := range snap.Layers {
		if l.Layer != "plan" {
			continue
		}
		if l.PlanID == want && (l.Use != 1 || l.Queue != 1) {
			t.Errorf("plan %d layer = use %d queue %d, want 1/1", l.PlanID, l.Use, l.Queue)
		}
		if l.PlanID == forbid {
			t.Errorf("plan %d present, want it scoped away", l.PlanID)
		}
	}
	if !hasPlanLayer(snap, want) {
		t.Errorf("plan %d missing from the plan layers", want)
	}
}
