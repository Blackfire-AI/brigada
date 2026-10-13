package engine

import (
	"fmt"
	"strings"
	"testing"
)

func TestSubmitTaskCreatesProjectPlanTaskAndJob(t *testing.T) {
	e := newEnv(t)

	res := e.submitTask(TaskParams{
		RepoURL:  "https://example.invalid/brigada.git",
		BaseRef:  "main",
		Spec:     "do the thing",
		CheckCmd: "go test ./...",
		Prompt:   "the composed prompt",
	})
	if res.Answer != AnsCreated || res.Run != 1 {
		t.Fatalf("submit task = %+v, want created run 1", res)
	}
	if got := e.str("SELECT repo_url FROM projects WHERE id = ?", res.ProjectID); got != "https://example.invalid/brigada.git" {
		t.Errorf("project repo = %q", got)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks WHERE id = ?", res.TaskID); got != 1 {
		t.Errorf("task %d does not exist", res.TaskID)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", res.TaskID); state != "ready" {
		t.Errorf("task state = %q, want ready", state)
	}
	if class := e.str("SELECT class FROM tasks WHERE id = ?", res.TaskID); class != "implement" {
		t.Errorf("task class = %q, want the default implement", class)
	}
	if check := e.str("SELECT COALESCE(check_cmd, '') FROM tasks WHERE id = ?", res.TaskID); check != "go test ./..." {
		t.Errorf("task check = %q", check)
	}
	if base := e.str("SELECT base_ref FROM tasks WHERE id = ?", res.TaskID); base != "main" {
		t.Errorf("task base ref = %q", base)
	}
	job := e.str("SELECT status FROM jobs WHERE id = ?", res.JobID)
	if job != statusQueued {
		t.Errorf("queued job status = %q, want queued", job)
	}
	if key := e.str("SELECT key FROM jobs WHERE id = ?", res.JobID); key != fmt.Sprintf("task:%d", res.TaskID) {
		t.Errorf("job key = %q, want task:%d", key, res.TaskID)
	}
	if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", res.JobID); prompt != "the composed prompt" {
		t.Errorf("job prompt = %q, want the caller's composed prompt", prompt)
	}
	// The submit's task_spec message rode along (§9).
	if got := e.int("SELECT COUNT(*) FROM messages WHERE kind = 'task_spec' AND event_id = (SELECT id FROM events WHERE job_id = ?)", res.JobID); got != 1 {
		t.Errorf("task_spec messages for the job = %d, want 1", got)
	}
}

func TestSubmitTaskReusesTheProjectOfTheRepo(t *testing.T) {
	e := newEnv(t)
	first := e.submitTask(TaskParams{RepoURL: "https://example.invalid/one.git", BaseRef: "main", Spec: "first", Prompt: "p1"})
	second := e.submitTask(TaskParams{RepoURL: "https://example.invalid/one.git", BaseRef: "main", Spec: "second", Prompt: "p2"})
	if first.ProjectID != second.ProjectID {
		t.Errorf("two submits of one repo made projects %d and %d; want one", first.ProjectID, second.ProjectID)
	}
	if first.TaskID == second.TaskID {
		t.Errorf("two submits made one task %d; want two", first.TaskID)
	}
	// The repo's project keeps the first submit's default branch.
	if got := e.str("SELECT default_branch FROM projects WHERE id = ?", first.ProjectID); got != "main" {
		t.Errorf("project default branch = %q, want main", got)
	}
}

func TestSubmitTaskValidates(t *testing.T) {
	e := newEnv(t)
	for name, drop := range map[string]func(p *TaskParams){
		"no repo":   func(p *TaskParams) { p.RepoURL = "" },
		"no base":   func(p *TaskParams) { p.BaseRef = "" },
		"no spec":   func(p *TaskParams) { p.Spec = "" },
		"no prompt": func(p *TaskParams) { p.Prompt = "" },
	} {
		p := TaskParams{RepoURL: "https://example.invalid/one.git", BaseRef: "main", Spec: "s", Prompt: "p"}
		drop(&p)
		if _, err := e.eng.SubmitTask(e.ctx, p); err == nil {
			t.Errorf("%s: accepted, want refusal", name)
		}
	}
	if got := e.int("SELECT COUNT(*) FROM jobs"); got != 0 {
		t.Errorf("jobs after the refusals = %d, want 0", got)
	}
}

func TestSubmitTaskTitleDefaultsToTheSpecsFirstLine(t *testing.T) {
	e := newEnv(t)
	res := e.submitTask(TaskParams{
		RepoURL: "https://example.invalid/one.git", BaseRef: "main",
		Spec: "\n\nfix the frobnicator\nsecond line", Prompt: "p",
	})
	if got := e.str("SELECT title FROM tasks WHERE id = ?", res.TaskID); got != "fix the frobnicator" {
		t.Errorf("task title = %q, want the spec's first non-empty line", got)
	}
	long := strings.Repeat("x", 200)
	res = e.submitTask(TaskParams{RepoURL: "https://example.invalid/one.git", BaseRef: "main", Spec: long, Prompt: "p"})
	if got := e.str("SELECT title FROM tasks WHERE id = ?", res.TaskID); len(got) != 83 || !strings.HasSuffix(got, "…") {
		t.Errorf("task title = %q (%d bytes), want 80 bytes plus the ellipsis", got, len(got))
	}
}

func TestStatusCountsJobsAndReadsTheInstance(t *testing.T) {
	e := newEnv(t)
	st, err := e.eng.Status(e.ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Pid != 0 || st.StartedAt != "" {
		t.Errorf("fresh status instance = %d/%q, want unrecorded zeros", st.Pid, st.StartedAt)
	}
	for _, s := range jobStatuses {
		if _, ok := st.Jobs[s]; !ok {
			t.Errorf("status jobs is missing %s: %v", s, st.Jobs)
		}
	}

	first := e.submit(e.work())
	if st, err = e.eng.Status(e.ctx); err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Jobs[statusQueued] != 1 {
		t.Errorf("jobs with one queued = %v, want queued 1", st.Jobs)
	}
	_ = first
	e.leaseAny(e.runner1)
	if st, err = e.eng.Status(e.ctx); err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Jobs[statusQueued] != 0 || st.Jobs[statusLeased] != 1 {
		t.Errorf("jobs with one leased = %v, want queued 0 leased 1", st.Jobs)
	}

	if err := e.eng.RecordInstance(e.ctx, 4242, e.clock.Now()); err != nil {
		t.Fatalf("record instance: %v", err)
	}
	if st, err = e.eng.Status(e.ctx); err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Pid != 4242 || st.StartedAt == "" {
		t.Errorf("recorded instance = %d/%q, want 4242 and a timestamp", st.Pid, st.StartedAt)
	}
}

func TestLiveJobsCountsTheLiveStatuses(t *testing.T) {
	e := newEnv(t)
	job := e.submit(e.work()).JobID
	if n, err := e.eng.LiveJobs(e.ctx); err != nil || n != 1 {
		t.Fatalf("live jobs with one queued = %d (%v), want 1", n, err)
	}
	e.leaseAny(e.runner1)
	if n, err := e.eng.LiveJobs(e.ctx); err != nil || n != 1 {
		t.Fatalf("live jobs with one leased = %d (%v), want 1", n, err)
	}
	e.exec("UPDATE jobs SET status = 'running' WHERE id = ?", job)
	if n, err := e.eng.LiveJobs(e.ctx); err != nil || n != 1 {
		t.Fatalf("live jobs with one running = %d (%v), want 1", n, err)
	}
	e.exec("UPDATE jobs SET status = 'done' WHERE id = ?", job)
	if n, err := e.eng.LiveJobs(e.ctx); err != nil || n != 0 {
		t.Fatalf("live jobs with one done = %d (%v), want 0", n, err)
	}
}
