package api

import (
	"fmt"
	"net/http"
	"testing"
)

// The five decisions the user makes — a plan's yes or no, a landing's yes
// or no, a push's yes — are role-checked calls: a client token (and the
// admin's) makes them, a foreman token is refused.

// decisionAct is one decision endpoint, with its seed and its check that the
// act ran.
type decisionAct struct {
	name   string
	path   func(e *env) string
	fields string
	seed   func(e *env)
	ran    func(e *env) bool
}

func (e *env) awaitingPlan() int64 {
	e.t.Helper()
	e.exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (?, 'to approve', 'pending_approval', ?, ?)`, e.project1, seedTS, seedTS)
	id := int64(e.count(e.t, "SELECT MAX(id) FROM plans"))
	e.exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
		VALUES (?, 'its task', 'spec', 'go test ./...', 'implement', 'pending', 'main', '{}', ?, ?)`, id, seedTS, seedTS)
	return id
}

func decisionActs() []decisionAct {
	var plan int64
	return []decisionAct{
		{
			name: "plan approve",
			seed: func(e *env) { plan = e.awaitingPlan() },
			path: func(e *env) string { return fmt.Sprintf("/v1/plans/%d/approve", plan) },
			ran:  func(e *env) bool { return e.str("SELECT state FROM plans WHERE id = ?", plan) == "active" },
		},
		{
			name: "plan reject",
			seed: func(e *env) { plan = e.awaitingPlan() },
			path: func(e *env) string { return fmt.Sprintf("/v1/plans/%d/reject", plan) },
			ran:  func(e *env) bool { return e.str("SELECT state FROM plans WHERE id = ?", plan) == "cancelled" },
		},
		{
			name: "land approve",
			seed: func(e *env) { e.seedLandHold() },
			path: func(e *env) string { return fmt.Sprintf("/v1/tasks/%d/land/approve", e.task) },
			ran: func(e *env) bool {
				return e.count(e.t, "SELECT COUNT(*) FROM land_holds WHERE state = 'held'") == 0 &&
					e.count(e.t, "SELECT COUNT(*) FROM land_holds WHERE state = 'rejected'") == 0
			},
		},
		{
			name: "land reject",
			seed: func(e *env) { e.seedLandHold() },
			path: func(e *env) string { return fmt.Sprintf("/v1/tasks/%d/land/reject", e.task) },
			ran: func(e *env) bool {
				return e.str("SELECT state FROM land_holds WHERE task_id = ?", e.task) == "rejected"
			},
		},
		{
			name: "push approve",
			seed: func(e *env) {
				e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
					VALUES (?, ?, 'main', 'abc123', 'waiting', ?)`, e.task, e.project1, seedTS)
			},
			path:   func(e *env) string { return "/v1/push/approve" },
			fields: `{"repo_url":"https://example.invalid/one.git"}`,
			ran: func(e *env) bool {
				return e.count(e.t, fmt.Sprintf("SELECT COUNT(*) FROM jobs WHERE task_id = %d AND kind = 'land' AND status = 'queued'", e.task)) == 1
			},
		},
	}
}

// TestDecisionsAreRoleChecked: for every decision, a foreman token is
// refused and the act does not run; a client token makes it in one call.
func TestDecisionsAreRoleChecked(t *testing.T) {
	for _, act := range decisionActs() {
		t.Run(act.name, func(t *testing.T) {
			e := newEnv(t)
			act.seed(e)
			path := act.path(e)
			var body []byte
			if act.fields != "" {
				body = []byte(act.fields)
			}

			code, _, data := e.do(http.MethodPost, path, "foreman1", "", body)
			if code != http.StatusForbidden {
				t.Fatalf("foreman = %d %s, want 403", code, data)
			}
			if act.ran(e) {
				t.Fatal("the act ran for a foreman token")
			}

			code, _, data = e.do(http.MethodPost, path, "alice", "", body)
			if code != http.StatusOK {
				t.Fatalf("client = %d %s, want 200", code, data)
			}
			if !act.ran(e) {
				t.Error("the act did not run for a client token")
			}
		})
	}
}
