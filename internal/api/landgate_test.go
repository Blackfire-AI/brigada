package api

import (
	"fmt"
	"net/http"
	"testing"
)

// seedLandHold puts the fixture task at the land gate, the state the
// land decision endpoints act on (§6.3).
func (e *env) seedLandHold() {
	e.t.Helper()
	e.exec(`INSERT INTO land_holds (task_id, reason, state, created_at) VALUES (?, 'heavy', 'held', ?)`, e.task, seedTS)
}

// TestLandGateEndpointsRefuseForeman pins the roles of the land and push
// gate endpoints (§6.3): the user's client and admin tokens may release
// them, the foreman's token is refused — the foreman has no tool to pass
// either gate, and they are in no MCP set.
func TestLandGateEndpointsRefuseForeman(t *testing.T) {
	e := newEnv(t)
	e.seedLandHold()

	// The foreman token is refused at every gate endpoint.
	for _, path := range []string{
		fmt.Sprintf("/v1/tasks/%d/land/approve", e.task),
		fmt.Sprintf("/v1/tasks/%d/land/reject", e.task),
		"/v1/push/approve",
	} {
		code, _, data := e.do(http.MethodPost, path, "foreman1", "", []byte(`{}`))
		if code != http.StatusForbidden {
			t.Errorf("foreman on %s = %d %s, want 403", path, code, data)
		}
	}

	// The client token may decide the land gate.
	code, _, data := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/land/approve", e.task), "alice", "", []byte(`{}`))
	if code != http.StatusOK {
		t.Fatalf("client approves the land gate: %d %s, want 200", code, data)
	}
	res := decode[struct {
		TaskID   int64 `json:"task_id"`
		Released bool  `json:"released"`
	}](t, data)
	if res.TaskID != e.task || !res.Released {
		t.Errorf("land approve = %+v, want the task released", res)
	}
	if n := e.count(t, fmt.Sprintf("SELECT COUNT(*) FROM land_holds WHERE task_id = %d", e.task)); n != 0 {
		t.Errorf("land_holds rows after approve = %d, want none", n)
	}
}

// TestLandRejectEndpoint: the client's reject cancels the landing, keeps
// the branch, and marks the hold rejected.
func TestLandRejectEndpoint(t *testing.T) {
	e := newEnv(t)
	e.seedLandHold()

	code, _, data := e.do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/land/reject", e.task), "root", "", []byte(`{}`))
	if code != http.StatusOK {
		t.Fatalf("admin rejects the land gate: %d %s, want 200", code, data)
	}
	res := decode[struct {
		TaskID   int64 `json:"task_id"`
		Released bool  `json:"released"`
	}](t, data)
	if res.TaskID != e.task || res.Released {
		t.Errorf("land reject = %+v, want the task not released", res)
	}
	if state := e.str("SELECT state FROM land_holds WHERE task_id = ?", e.task); state != "rejected" {
		t.Errorf("land_holds.state after reject = %q, want rejected", state)
	}
}

// TestPushApproveEndpoint: the client's push approve releases a waiting
// landing, queueing a push job.
func TestPushApproveEndpoint(t *testing.T) {
	e := newEnv(t)
	// Seed a landing recorded as waiting at the push gate.
	e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
		VALUES (?, ?, 'main', 'abc123', 'waiting', ?)`, e.task, e.project1, seedTS)

	code, _, data := e.do(http.MethodPost, "/v1/push/approve", "alice", "", []byte(`{"repo_url":"https://example.invalid/one.git"}`))
	if code != http.StatusOK {
		t.Fatalf("client approves the push: %d %s, want 200", code, data)
	}
	res := decode[struct {
		Released int `json:"released"`
	}](t, data)
	if res.Released != 1 {
		t.Errorf("push approve = %+v, want 1 released", res)
	}
	if n := e.count(t, fmt.Sprintf("SELECT COUNT(*) FROM jobs WHERE task_id = %d AND kind = 'land' AND status = 'queued'", e.task)); n != 1 {
		t.Errorf("queued push jobs = %d, want 1", n)
	}
}
