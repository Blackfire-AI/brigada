package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
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

// TestStatusEndpointPlansAndNeedsYou covers /v1/status's plans and
// "needs you" sections (§6.1, §6.2, §10): the active plan with its
// task, a task parked at needs_foreman with its event's reason, and the
// foreman's scope — the token's project alone, where a client reads
// every project.
func TestStatusEndpointPlansAndNeedsYou(t *testing.T) {
	e := newEnv(t)
	e.submitWork()

	// Seed what the env does not carry: the fixture's task parked for
	// the foreman with the event that parked it, and a plan with a parked
	// task in the other project.
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		now := e.clock.Now().UTC().Format(engine.TimeLayout)
		if _, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('task_needs_help', ?, ?, NULL, '{"question":"which API should the client call?"}', ?)`,
			e.project1, e.task, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('foreman_report', ?, NULL, NULL, '{"report":"woke for the question of task 1; parked for you"}', ?)`,
			e.project1, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET state = 'needs_foreman' WHERE id = ?`, e.task); err != nil {
			return err
		}
		plan, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (?, 'the other plan', 'active', ?, ?)`, e.project2, now, now)
		if err != nil {
			return err
		}
		planID, err := plan.LastInsertId()
		if err != nil {
			return err
		}
		task, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (?, 'the other task', 'the spec', NULL, 'implement', 'needs_foreman', 'main', '{}', ?, ?)`,
			planID, now, now)
		if err != nil {
			return err
		}
		taskID, err := task.LastInsertId()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('task_failed', ?, ?, NULL, '{"reason":"their adapter died"}', ?)`,
			e.project2, taskID, now)
		return err
	})
	if err != nil {
		t.Fatalf("seed the parked tasks: %v", err)
	}

	// The client reads every project: both plans, both parked tasks.
	code, _, data := e.do(http.MethodGet, "/v1/status", "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("client reads status: %d %s", code, data)
	}
	st := decode[engine.StatusInfo](t, data)
	if len(st.Plans) != 2 || len(st.NeedsYou) != 2 {
		t.Fatalf("the client's status = %d plans, %+v, want both projects' plans and queues", len(st.Plans), st.NeedsYou)
	}
	byID := map[int64]engine.StatusPlan{}
	for _, p := range st.Plans {
		byID[p.ID] = p
	}
	own := byID[e.plan]
	if own.State != "active" || own.RepoURL != "https://example.invalid/one.git" || len(own.Tasks) != 1 ||
		own.Tasks[0].ID != e.task || own.Tasks[0].State != "needs_foreman" {
		t.Errorf("the own plan = %+v, want the active plan with its parked task", own)
	}
	reasons := map[int64]string{}
	for _, it := range st.NeedsYou {
		reasons[it.ID] = it.Reason
	}
	if got := reasons[e.task]; got != "which API should the client call?" {
		t.Errorf("the own task's reason = %q, want the question the event carries", got)
	}

	// The foreman is walled into its own project: its plan and its task
	// alone, the other project's nowhere.
	code, _, data = e.do(http.MethodGet, "/v1/status", "foreman1", "", nil)
	if code != http.StatusOK {
		t.Fatalf("the foreman reads status: %d %s", code, data)
	}
	fst := decode[engine.StatusInfo](t, data)
	if len(fst.Plans) != 1 || fst.Plans[0].ID != e.plan {
		t.Errorf("the foreman's plans = %+v, want its own project alone", fst.Plans)
	}
	if len(fst.NeedsYou) != 1 || fst.NeedsYou[0].ID != e.task ||
		fst.NeedsYou[0].Reason != "which API should the client call?" {
		t.Errorf("the foreman's needs you = %+v, want its own task with its reason", fst.NeedsYou)
	}
	// The latest report rides the same scoping: the foreman's own
	// project's, with its text verbatim.
	if fst.Report == nil || fst.Report.ProjectID != e.project1 ||
		fst.Report.Text != "woke for the question of task 1; parked for you" {
		t.Errorf("the foreman's report = %+v, want its own project's with its text", fst.Report)
	}
}

// TestStatusEndpointTrimsFinishedPlans covers /v1/status's plans bound
// (§10): the default lists the newest finished plans only and counts
// the rest in hidden_finished_plans; `all` lists every finished plan.
func TestStatusEndpointTrimsFinishedPlans(t *testing.T) {
	e := newEnv(t)
	now := e.clock.Now()
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		for i := 0; i < 4; i++ {
			at := now.Add(-time.Duration(i+1) * time.Hour).UTC().Format(engine.TimeLayout)
			if _, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
				VALUES (?, ?, 'done', ?, ?)`, e.project1, fmt.Sprintf("done %d", i), at, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed the finished plans: %v", err)
	}

	read := func(path string) engine.StatusInfo {
		t.Helper()
		code, _, data := e.do(http.MethodGet, path, "alice", "", nil)
		if code != http.StatusOK {
			t.Fatalf("read %s: %d %s", path, code, data)
		}
		return decode[engine.StatusInfo](t, data)
	}

	st := read("/v1/status")
	// The fixture's active plan plus the three newest done ones; the
	// oldest done plan hides behind the count.
	if len(st.Plans) != 4 || st.HiddenFinishedPlans != 1 {
		t.Fatalf("plans = %+v hidden %d, want the active and three done plans, one hidden", st.Plans, st.HiddenFinishedPlans)
	}

	all := read("/v1/status?all=1")
	if len(all.Plans) != 5 || all.HiddenFinishedPlans != 0 {
		t.Fatalf("all plans = %+v hidden %d, want every plan, none hidden", all.Plans, all.HiddenFinishedPlans)
	}
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

	body := `{"repo_url":"https://example.invalid/three.git","base_ref":"main","spec":"the spec","check_cmd":"go test ./...","harness":"claude"}`
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

	// The queued job carries the harness constraint, and the engine
	// composed its prompt from the template plus the spec verbatim
	// (§1.4, §5.3) — no instructions section, for the project has no
	// worker text (§5.9 'Three states, no hidden fallback'); a runner
	// may lease it (§8).
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
	if job.Prompt != prompt.ComposeWorkerPrompt("the spec", "go test ./...", "") || !strings.Contains(job.Prompt, "<task-spec>\nthe spec\n</task-spec>") {
		t.Errorf("job prompt = %q, want the engine's composed prompt with the spec verbatim", job.Prompt)
	}
	if job.Harness != "claude" {
		t.Errorf("job harness = %q, want what the caller sent", job.Harness)
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
		"no repo":    `{"base_ref":"main","spec":"s"}`,
		"no spec":    `{"repo_url":"https://example.invalid/x.git","base_ref":"main"}`,
		"bad encode": `{"repo_url":42}`,
	} {
		code, _, data := e.do(http.MethodPost, "/v1/tasks", "root", "", []byte(bad))
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s, want 400", name, code, data)
		}
	}
}

func TestStatusRoleGuidanceEndpoint(t *testing.T) {
	e := newEnv(t)
	code, _, data := e.do(http.MethodGet, fmt.Sprintf("/v1/status?project=%d&instructions=1", e.project1), "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status: %d %s", code, data)
	}
	st := decode[Status](t, data)
	if len(st.RoleInstructions) != 4 || !strings.Contains(st.RoleInstructions[0].Text, "<foreman-instructions>") {
		t.Fatalf("roles: %+v", st.RoleInstructions)
	}
}
