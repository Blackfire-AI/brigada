package api

import (
	"fmt"
	"net/http"
	"testing"
)

// TestStatusEndpoint covers /v1/status (§13): the engine's own state —
// pid, uptime start, jobs by status — behind the client role.
func TestStatusEndpoint(t *testing.T) {
	e := newEnv(t)
	own := e.submitWork()

	// The role check: a runner token may not read the status.
	if code, _, _ := e.do(http.MethodGet, "/v1/status", "r1", "", nil); code != http.StatusForbidden {
		t.Fatalf("runner reads status: %d, want 403", code)
	}
	code, _, data := e.do(http.MethodGet, "/v1/status", "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("client reads status: %d %s", code, data)
	}
	st := decode[struct {
		Pid       int64          `json:"pid"`
		StartedAt string         `json:"started_at"`
		Jobs      map[string]int `json:"jobs"`
	}](t, data)
	if st.Pid != 0 || st.StartedAt != "" {
		t.Errorf("instance = %d/%q, want unrecorded zeros", st.Pid, st.StartedAt)
	}
	if st.Jobs["queued"] != 1 {
		t.Errorf("jobs = %v, want the one queued job counted", st.Jobs)
	}
	for _, s := range []string{"queued", "leased", "running", "done", "failed", "cancelled", "stalled"} {
		if _, ok := st.Jobs[s]; !ok {
			t.Errorf("jobs is missing %s: %v", s, st.Jobs)
		}
	}
	_ = own
}

// TestTaskSubmitEndpoint covers /v1/tasks (§12.1): the debug submit —
// one task created whole, its first work job queued — as the admin's
// to call, with the caller's mistakes refused as 400s.
func TestTaskSubmitEndpoint(t *testing.T) {
	e := newEnv(t)

	// Only the admin submits tasks, like jobs.
	if code, _, _ := e.do(http.MethodPost, "/v1/tasks", "alice", "", []byte(`{}`)); code != http.StatusForbidden {
		t.Fatalf("client submits a task: %d, want 403", code)
	}

	body := `{"repo_url":"https://example.invalid/three.git","base_ref":"main","spec":"the spec","check_cmd":"go test ./...","prompt":"the composed prompt","harness":"claude"}`
	code, _, data := e.do(http.MethodPost, "/v1/tasks", "root", "", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("admin submits a task: %d %s", code, data)
	}
	res := decode[struct {
		Answer    string `json:"answer"`
		ProjectID int64  `json:"project_id"`
		TaskID    int64  `json:"task_id"`
		JobID     int64  `json:"job_id"`
		Run       int    `json:"run"`
	}](t, data)
	if res.Answer != "created" || res.TaskID == 0 || res.JobID == 0 || res.Run != 1 {
		t.Fatalf("submit = %+v, want created with task, job and run 1", res)
	}

	// The queued job carries the harness constraint and the composed
	// prompt verbatim, and a runner may lease it (§8).
	_, _, jobBody := e.do(http.MethodGet, fmt.Sprintf("/v1/jobs/%d", res.JobID), "root", "", nil)
	job := decode[struct {
		Key     string   `json:"key"`
		Prompt  string   `json:"prompt"`
		Harness string   `json:"harness"`
		Status  string   `json:"status"`
		Labels  []string `json:"labels"`
	}](t, jobBody)
	if job.Key != fmt.Sprintf("task:%d", res.TaskID) || job.Status != "queued" {
		t.Errorf("job = %+v, want queued on the task's key", job)
	}
	if job.Prompt != "the composed prompt" || job.Harness != "claude" {
		t.Errorf("job prompt/harness = %q/%q, want what the caller sent", job.Prompt, job.Harness)
	}
	e.register("r1")
	grant := `{"wait":0,"harnesses":["claude"]}`
	code, _, data = e.do(http.MethodPost, "/v1/runner/lease", "r1", "", []byte(grant))
	if code != http.StatusOK {
		t.Fatalf("lease the submitted task: %d %s", code, data)
	}
	if got := decode[struct {
		Job *struct {
			JobID    int64  `json:"job_id"`
			RepoURL  string `json:"repo_url"`
			CheckCmd string `json:"check_cmd"`
			BaseRef  string `json:"base_ref"`
		} `json:"job"`
	}](t, data).Job; got == nil || got.JobID != res.JobID ||
		got.RepoURL != "https://example.invalid/three.git" || got.CheckCmd != "go test ./..." || got.BaseRef != "main" {
		t.Errorf("the grant = %+v, want the task's workspace fields", got)
	}

	// The caller's mistakes are 400s naming the field (§12.1).
	for name, bad := range map[string]string{
		"no repo":    `{"base_ref":"main","spec":"s","prompt":"p"}`,
		"no spec":    `{"repo_url":"https://example.invalid/x.git","base_ref":"main","prompt":"p"}`,
		"no prompt":  `{"repo_url":"https://example.invalid/x.git","base_ref":"main","spec":"s"}`,
		"bad encode": `{"repo_url":42}`,
	} {
		code, _, data := e.do(http.MethodPost, "/v1/tasks", "root", "", []byte(bad))
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s, want 400", name, code, data)
		}
	}
}
