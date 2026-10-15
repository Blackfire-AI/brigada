package engine

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/planfile"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// mirrorEnv is an env whose engine mirrors plan files (§6.4) to a local
// bare remote the project names, collecting the warnings the mirror logs.
type mirrorEnv struct {
	*env
	remote string

	mu    sync.Mutex
	warns []string
}

func newMirrorEnv(t *testing.T) *mirrorEnv {
	m := &mirrorEnv{remote: testsupport.NewGitRemote(t)}
	m.env = newEnvCfg(t, func(c *Config) {
		c.PlanFiles = true
		c.PlanFileRetry = time.Nanosecond // a failed push holds nothing back in a test
		c.Warnf = func(format string, args ...any) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.warns = append(m.warns, fmt.Sprintf(format, args...))
		}
	})
	m.setRepo(m.remote)
	return m
}

func (m *mirrorEnv) setRepo(url string) {
	m.t.Helper()
	m.exec("UPDATE projects SET repo_url = ? WHERE id = ?", url, m.project)
}

func (m *mirrorEnv) warnings() []string {
	m.eng.WaitPlanFiles()
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.warns...)
}

// file reads a plan's file from a branch of the remote; ok is false when
// the branch or the file is not there.
func (m *mirrorEnv) file(branch string, plan int64) (planfile.Plan, bool) {
	m.t.Helper()
	m.eng.WaitPlanFiles()
	out, err := exec.Command("git", "-C", m.remote, "show", branch+":"+planfile.Path(plan)).Output()
	if err != nil {
		return planfile.Plan{}, false
	}
	p, err := planfile.Parse(out)
	if err != nil {
		m.t.Fatalf("the file of plan %d on %s does not parse: %v\n%s", plan, branch, err, out)
	}
	return p, true
}

func (m *mirrorEnv) mustFile(branch string, plan int64) planfile.Plan {
	m.t.Helper()
	p, ok := m.file(branch, plan)
	if !ok {
		m.t.Fatalf("no file for plan %d on %s (warnings: %v)", plan, branch, m.warnings())
	}
	return p
}

func taskByName(t *testing.T, p planfile.Plan, name string) planfile.Task {
	t.Helper()
	for _, task := range p.Tasks {
		if task.Name == name {
			return task
		}
	}
	t.Fatalf("plan file has no task %q: %+v", name, p.Tasks)
	return planfile.Task{}
}

func TestPlanCreationPushesTheFileToANewPlansBranch(t *testing.T) {
	m := newMirrorEnv(t)
	res, err := m.eng.CreatePlan(m.ctx, m.project, "mirror me", []PlanTask{
		{Name: "a", Title: "first", Spec: "do a"},
		{Name: "b", Spec: "do b", Deps: []string{"a"}, Class: "review"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	p := m.mustFile("brigada/plans", res.PlanID)
	if p.ID != res.PlanID || p.Title != "mirror me" || p.State != "active" || p.Repo != m.remote {
		t.Errorf("header = %+v", p)
	}
	if len(p.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want 2", p.Tasks)
	}
	a, b := taskByName(t, p, "a"), taskByName(t, p, "b")
	if a.State != "ready" || a.Title != "first" || a.Class != "implement" || len(a.Deps) != 0 {
		t.Errorf("task a = %+v", a)
	}
	if b.State != "pending" || b.Class != "review" || len(b.Deps) != 1 || b.Deps[0] != "a" {
		t.Errorf("task b = %+v", b)
	}
	if a.ID != res.Tasks[0].TaskID || b.ID != res.Tasks[1].TaskID {
		t.Errorf("task ids = %d, %d; want %d, %d", a.ID, b.ID, res.Tasks[0].TaskID, res.Tasks[1].TaskID)
	}
	if w := m.warnings(); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
	// The mark is cleared once the file is pushed, and the default branch
	// is untouched.
	if got := m.int("SELECT COUNT(*) FROM plan_mirror_dirty"); got != 0 {
		t.Errorf("plans still owed a push = %d, want 0", got)
	}
	if _, ok := m.file("main", res.PlanID); ok {
		t.Error("the engine committed the plan file to the default branch on its own")
	}
}

func TestPlanChangesPushAgain(t *testing.T) {
	m := newMirrorEnv(t)
	res, err := m.eng.CreatePlan(m.ctx, m.project, "big", nTasks(6))
	if err != nil {
		t.Fatal(err)
	}
	if p := m.mustFile("brigada/plans", res.PlanID); p.State != planAwaitingApproval {
		t.Errorf("held plan's file state = %q, want %s", p.State, planAwaitingApproval)
	}

	// Approval.
	if _, err := m.eng.ApprovePlan(m.ctx, m.project, res.PlanID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	p := m.mustFile("brigada/plans", res.PlanID)
	if p.State != "active" || p.Tasks[0].State != "ready" {
		t.Errorf("after approval: plan %s, first task %s; want active, ready", p.State, p.Tasks[0].State)
	}

	// A task state change, with a worker's question open and a branch.
	task := res.Tasks[1].TaskID
	m.exec("UPDATE tasks SET state = 'needs_foreman', branch = 'brigada/task-x' WHERE id = ?", task)
	m.exec(`INSERT INTO events (kind, project_id, task_id, payload, created_at) VALUES ('task_needs_help', ?, ?, '{"question":"which one?"}', ?)`,
		m.project, task, seedTS)
	m.eng.SyncPlanFiles()
	got := m.mustFile("brigada/plans", res.PlanID).Tasks[1]
	if got.State != "needs_foreman" || got.Branch != "brigada/task-x" || got.Question != "which one?" {
		t.Errorf("task after the change = %+v", got)
	}

	// The answer closes the question.
	m.exec("UPDATE tasks SET state = 'running' WHERE id = ?", task)
	m.exec(`INSERT INTO events (kind, project_id, task_id, payload, created_at) VALUES ('task_answered', ?, ?, '{}', ?)`,
		m.project, task, seedTS)
	m.eng.SyncPlanFiles()
	if got := m.mustFile("brigada/plans", res.PlanID).Tasks[1]; got.State != "running" || got.Question != "" {
		t.Errorf("task after the answer = %+v", got)
	}

	// Done and cancel: the plan's last words.
	m.exec("UPDATE plans SET state = 'done' WHERE id = ?", res.PlanID)
	m.eng.SyncPlanFiles()
	if p := m.mustFile("brigada/plans", res.PlanID); p.State != "done" {
		t.Errorf("plan file state = %q, want done", p.State)
	}
	again, err := m.eng.CreatePlan(m.ctx, m.project, "to reject", nTasks(6))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.eng.RejectPlan(m.ctx, m.project, again.PlanID); err != nil {
		t.Fatal(err)
	}
	p = m.mustFile("brigada/plans", again.PlanID)
	if p.State != "cancelled" || p.Tasks[0].State != "cancelled" {
		t.Errorf("rejected plan's file = %s / %s, want cancelled", p.State, p.Tasks[0].State)
	}
	if w := m.warnings(); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
}

func TestPlanFileCountsRoundsSpent(t *testing.T) {
	m := newMirrorEnv(t)
	res, err := m.eng.CreatePlan(m.ctx, m.project, "rounds", []PlanTask{{Name: "a", Spec: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	task := res.Tasks[0].TaskID
	for _, round := range []string{"fix_rounds", "review_rounds", ""} {
		m.exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, round, created_at, updated_at)
			VALUES (?, ?, 'fix', ?, 'session', 'p', 'done', ?, ?, ?)`,
			m.project, task, fmt.Sprintf("old:%d:%s", task, round), round, seedTS, seedTS)
	}
	m.exec("UPDATE tasks SET state = 'running' WHERE id = ?", task)
	m.eng.SyncPlanFiles()
	if got := m.mustFile("brigada/plans", res.PlanID).Tasks[0].Rounds; got != 2 {
		t.Errorf("rounds = %d, want 2 (the answer-carrying fix job spends none)", got)
	}
}

func TestPlanFileFailureIsAWarningAndTheNextChangePushesAgain(t *testing.T) {
	m := newMirrorEnv(t)
	m.setRepo(filepath.Join(t.TempDir(), "gone.git"))

	res, err := m.eng.CreatePlan(m.ctx, m.project, "offline", []PlanTask{{Name: "a", Spec: "x"}})
	if err != nil {
		t.Fatalf("a mirror failure failed the plan: %v", err)
	}
	if res.PlanID == 0 || m.str("SELECT state FROM plans WHERE id = ?", res.PlanID) != "active" {
		t.Fatalf("plan not made: %+v", res)
	}
	warns := m.warnings()
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, "\n"), fmt.Sprintf("plan %d", res.PlanID)) ||
		!strings.Contains(warns[0], "catches up on the next change") {
		t.Fatalf("warnings = %q, want one naming the plan and the catch-up", warns)
	}
	if got := m.int("SELECT COUNT(*) FROM plan_mirror_dirty WHERE plan_id = ?", res.PlanID); got != 1 {
		t.Errorf("the failed plan's mark = %d, want it kept", got)
	}

	// The remote comes back; the next change pushes the new plan's file
	// and the one that fell behind.
	m.setRepo(m.remote)
	next, err := m.eng.CreatePlan(m.ctx, m.project, "back online", []PlanTask{{Name: "b", Spec: "y"}})
	if err != nil {
		t.Fatal(err)
	}
	m.mustFile("brigada/plans", next.PlanID)
	if p := m.mustFile("brigada/plans", res.PlanID); p.Title != "offline" {
		t.Errorf("the plan that fell behind = %+v", p)
	}
	if got := m.int("SELECT COUNT(*) FROM plan_mirror_dirty"); got != 0 {
		t.Errorf("plans still owed a push = %d, want 0", got)
	}
}

func TestMissingDefaultBranchIsAWarning(t *testing.T) {
	m := newMirrorEnv(t)
	empty := filepath.Join(t.TempDir(), "empty.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", empty).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	m.setRepo(empty)
	m.setSettings(`{"plan_file_branch": "main"}`)
	if _, err := m.eng.CreatePlan(m.ctx, m.project, "no main yet", []PlanTask{{Spec: "x"}}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	warns := m.warnings()
	if len(warns) == 0 || !strings.Contains(warns[0], "no default branch") {
		t.Errorf("warnings = %q, want one saying the remote has no default branch", warns)
	}
}

func TestPlanFileBranchSettingMovesTheTarget(t *testing.T) {
	m := newMirrorEnv(t)
	m.setSettings(`{"plan_file_branch": "mirrors/plans"}`)
	res, err := m.eng.CreatePlan(m.ctx, m.project, "elsewhere", []PlanTask{{Name: "a", Spec: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	m.mustFile("mirrors/plans", res.PlanID)
	if _, ok := m.file("brigada/plans", res.PlanID); ok {
		t.Error("the file went to the default plans branch despite the setting")
	}

	// The literal main commits to the default branch (§6.4's option), and
	// so does the repo's own default branch name.
	for _, setting := range []string{"main", "trunk"} {
		m := newMirrorEnv(t)
		branch := "main"
		if setting == "trunk" {
			branch = "trunk"
			m.exec("UPDATE projects SET default_branch = 'trunk' WHERE id = ?", m.project)
			out, err := exec.Command("git", "-C", m.remote, "branch", "-m", "main", "trunk").CombinedOutput()
			if err != nil {
				t.Fatalf("rename the remote's default branch: %v: %s", err, out)
			}
		}
		m.setSettings(fmt.Sprintf(`{"plan_file_branch": %q}`, setting))
		res, err := m.eng.CreatePlan(m.ctx, m.project, "on the default branch", []PlanTask{{Name: "a", Spec: "x"}})
		if err != nil {
			t.Fatal(err)
		}
		m.mustFile(branch, res.PlanID)
		if _, ok := m.file("brigada/plans", res.PlanID); ok {
			t.Errorf("setting %q: the file also went to the plans branch", setting)
		}
		if w := m.warnings(); len(w) != 0 {
			t.Errorf("setting %q: warnings = %v", setting, w)
		}
	}
}

func TestMirrorIsOffByDefault(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "quiet", []PlanTask{{Spec: "x"}}); err != nil {
		t.Fatal(err)
	}
	e.eng.WaitPlanFiles()
	// The fixture's repo URL names no reachable remote; a mirror that ran
	// would have warned and kept its marks, so this pins only that
	// nothing cleared them.
	if got := e.int("SELECT COUNT(*) FROM plan_mirror_dirty"); got == 0 {
		t.Error("marks cleared with the mirror off")
	}
}
