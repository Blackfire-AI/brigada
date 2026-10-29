package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The plan review on the runner (issue #50), end to end against the real
// engine with the plan review on: one runner advertising two fake agents,
// the foreman's family on the first and the reviewer's on the other, and
// a scripted reviewer that records what it saw.

// planReviewWorker is the plan reviewer: it records its prompt, its
// checkout and whether it left anything behind, then ends with @RESULT@.
// Any other prompt (the work an approved plan queues) ends idle.
const planReviewWorker = `
prompt=$(cat)
case "$prompt" in
*"brigada reviewer"*)
	echo '{"type":"system_init","session_id":"sess-pr"}'
	echo '{"type":"tool_start","name":"Read"}'
	M="$BRIGADA_TEST_MARKERS"
	printf '%s' "$prompt" > "$M/pr-prompt"
	git rev-parse HEAD > "$M/pr-head" 2>&1
	git rev-parse --abbrev-ref HEAD > "$M/pr-branch" 2>&1
	git status --porcelain > "$M/pr-status" 2>&1
	ls > "$M/pr-ls" 2>&1
	echo '{"type":"tool_end","name":"Read"}'
	@RESULT@
	;;
*)
	echo '{"type":"result","text":"idle"}'
	;;
esac
`

const planReviewChangesFindings = "task b chains behind task a though it touches no file a changes, so the plan serializes unrelated work; task a names no check that could fail"

var planReviewChangesResult = fakeResultScript("reviewed the plan. ```json\n{\"verdict\":\"changes\",\"findings\":\"" + planReviewChangesFindings + "\"}\n```")

// planReviewFixture is a plan waiting in review on a runner whose
// reviewer ends with a scripted result.
type planReviewFixture struct {
	e       *testEnv
	remote  string
	main    string // the remote's main when the plan was created
	markers string
	plan    int64
	p       *runnerProc
}

// newPlanReviewFixture runs the engine with the plan review on, the
// project's foreman on the first fake agent, the runner up with both
// agents registered, and a two-task plan created and handed to review.
func newPlanReviewFixture(t *testing.T, result string) *planReviewFixture {
	t.Helper()
	remote := newRemote(t)
	on := true
	e := newTestEnv(t, engine.Config{PlanReview: &on})
	e.seedProject(remote)
	e.setProjectSettings(`{"default_check": "go test ./..."}`) // task a's check: the plan gate passes it
	e.setForeman("fake", "fake-1")
	markers := markerDir(t)
	main := remoteBranchCommit(t, remote, "main")
	p := e.startRunner(writeWorkerScript(t, strings.ReplaceAll(planReviewWorker, "@RESULT@", result)),
		100*time.Millisecond, withReviewerAgent)
	e.waitUntil("both agents to register", func() bool {
		return e.int64Of("SELECT COUNT(*) FROM agents") == 2
	})
	res, err := e.eng.CreatePlan(e.ctx, e.project, "two tasks", []engine.PlanTask{
		{Title: "a", Name: "a", Spec: "the spec of task a", Check: "go test ./..."},
		{Title: "b", Name: "b", Spec: "the spec of task b", Deps: []string{"a"}},
	})
	if err != nil {
		t.Fatalf("create the plan: %v", err)
	}
	return &planReviewFixture{e: e, remote: remote, main: main, markers: markers, plan: res.PlanID, p: p}
}

// job waits for the plan review job to be created and to finish.
func (f *planReviewFixture) job() int64 {
	f.e.t.Helper()
	var job int64
	f.e.waitUntil("the plan review job", func() bool {
		job = f.e.int64Of("SELECT COALESCE(MIN(id), 0) FROM jobs WHERE kind = 'plan_review'")
		return job != 0
	})
	f.e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	return job
}

// assertReviewerSaw holds the reviewer's own record: the prompt carried
// the plan's specs verbatim as data, the worktree sat at the remote's
// main, detached and clean, and the remote gained no branch.
func (f *planReviewFixture) assertReviewerSaw(t *testing.T) {
	t.Helper()
	saw := reviewerMarker(t, f.markers, "pr-prompt")
	for _, want := range []string{
		"<plan-review-input>", "the spec of task a", "the spec of task b",
		"End your final message with exactly one JSON verdict block", "```json",
		`head of "main", the project's default branch`,
	} {
		if !strings.Contains(saw, want) {
			t.Errorf("the plan review prompt lacks %q:\n%s", want, saw)
		}
	}
	if strings.Contains(saw, "already on that ref") {
		t.Error("unconfigured plan review carries a hard-coded judgment check")
	}
	if head := strings.TrimSpace(reviewerMarker(t, f.markers, "pr-head")); head != f.main {
		t.Errorf("the plan review worktree sat at %q, want main %q", head, f.main)
	}
	if br := strings.TrimSpace(reviewerMarker(t, f.markers, "pr-branch")); br != "HEAD" {
		t.Errorf("the plan review worktree is on branch %q, want a detached HEAD", br)
	}
	if st := strings.TrimSpace(reviewerMarker(t, f.markers, "pr-status")); st != "" {
		t.Errorf("the plan review worktree is dirty:\n%s", st)
	}
	if got := remoteBranchCommit(t, f.remote, "main"); got != f.main {
		t.Errorf("the remote's main moved during the plan review: %s -> %s", f.main, got)
	}
	if refs := remoteRefs(t, f.remote); len(refs) != 1 || refs[0] != "main" {
		t.Errorf("the remote holds %v, want main alone: a plan review pushes nothing", refs)
	}
	if _, err := os.Stat(filepath.Join(f.markers, "checks")); err == nil {
		t.Error("a check ran for the plan review")
	}
}

// TestRunnerPlanReviewApproveActivatesPlan: an approve completes the job
// with the verdict block verbatim and the plan leaves review for active.
func TestRunnerPlanReviewApproveActivatesPlan(t *testing.T) {
	f := newPlanReviewFixture(t, reviewApproveResult)
	defer f.p.stop(t)
	e := f.e
	job := f.job()
	e.waitUntil("the plan to activate", func() bool {
		return e.strOf("SELECT state FROM plans WHERE id = ?", f.plan) == "active"
	})
	want := `{"verdict":"approve","findings":"` + reviewFindings + `"}`
	if got := e.reviewResultOf(t, job); got != want {
		t.Errorf("plan review result = %q\nwant the verdict block\n%q", got, want)
	}
	if v := e.strOf("SELECT review_verdict FROM plans WHERE id = ?", f.plan); v != "approve" {
		t.Errorf("recorded verdict = %q, want approve", v)
	}
	if fam := e.strOf("SELECT a.family FROM jobs j JOIN agents a ON a.id = j.agent_id WHERE j.id = ?", job); fam != "otherfam" {
		t.Errorf("the plan review ran on family %q, want the other family than the foreman's", fam)
	}
	f.assertReviewerSaw(t)
}

// TestRunnerPlanReviewChangesWakesForeman: a changes verdict parks the
// plan at needs_foreman with the findings recorded, and wakes the
// foreman.
func TestRunnerPlanReviewChangesWakesForeman(t *testing.T) {
	f := newPlanReviewFixture(t, planReviewChangesResult)
	defer f.p.stop(t)
	e := f.e
	f.job()
	e.waitUntil("the foreman to be woken", func() bool {
		return e.int64Of("SELECT COUNT(*) FROM jobs WHERE kind = 'foreman' AND project_id = ?", e.project) > 0
	})
	if st := e.strOf("SELECT state FROM plans WHERE id = ?", f.plan); st != "needs_foreman" {
		t.Errorf("plan state = %q, want needs_foreman while the foreman answers", st)
	}
	if v := e.strOf("SELECT review_verdict FROM plans WHERE id = ?", f.plan); v != "changes" {
		t.Errorf("recorded verdict = %q, want changes", v)
	}
	if got := e.strOf("SELECT review_findings FROM plans WHERE id = ?", f.plan); got != planReviewChangesFindings {
		t.Errorf("recorded findings = %q, want the reviewer's", got)
	}
	f.assertReviewerSaw(t)
}

// TestRunnerPlanReviewNoVerdict: a reviewer that fences no verdict
// completes with the empty verdict, which the engine reads as none, and
// the plan does not take an approve from it.
func TestRunnerPlanReviewNoVerdict(t *testing.T) {
	f := newPlanReviewFixture(t, reviewNoVerdictResult)
	defer f.p.stop(t)
	e := f.e
	job := f.job()
	if got := e.reviewResultOf(t, job); got != emptyVerdictBlock {
		t.Errorf("plan review result = %q, want the empty verdict %q", got, emptyVerdictBlock)
	}
	if v := e.strOf("SELECT review_verdict FROM plans WHERE id = ?", f.plan); v == "approve" {
		t.Errorf("a review with no verdict recorded %q", v)
	}
	f.assertReviewerSaw(t)
}
