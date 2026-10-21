package runner

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// The land jobs on the runner (§7.1), end to end against the real
// engine: the dual-agent runner of the review tests, which offers the
// git label a land job's placement asks for by default, one local bare
// repo as the remote, and the review-scripted worker driving the task
// to the approval a landing reads. The landing itself runs in a clone
// under the runner's state directory — never a worktree of the mirror —
// and what each test judges is the remote's main and the landing record
// the engine kept.

// setProjectSettings replaces the project's settings JSON: how the
// land tests set the push gates, the default check and after_land.
func (e *testEnv) setProjectSettings(settings string) {
	e.t.Helper()
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE projects SET settings = ? WHERE id = ?", settings, e.project)
		return err
	})
	if err != nil {
		e.t.Fatalf("set the project settings: %v", err)
	}
}

// landing reads the task's landing record (§7.1); ok is false until
// the engine wrote one.
func (e *testEnv) landing(task int64) (pushed, commit, pushCmd, afterErr string, ok bool) {
	e.t.Helper()
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT pushed, commit_sha, COALESCE(push_cmd, ''), COALESCE(after_land_error, '')
			FROM landings WHERE task_id = ?`, task).Scan(&pushed, &commit, &pushCmd, &afterErr)
	})
	if err == sql.ErrNoRows {
		return "", "", "", "", false
	}
	if err != nil {
		e.t.Fatalf("read the landing of task %d: %v", task, err)
	}
	return pushed, commit, pushCmd, afterErr, true
}

// waitLanding polls until the task's landing exists and pushed holds.
func (e *testEnv) waitLanding(task int64, pushed string) string {
	e.t.Helper()
	var commit string
	e.waitUntil("the landing of task "+strconv.FormatInt(task, 10)+" to be "+pushed, func() bool {
		p, c, _, _, ok := e.landing(task)
		commit = c
		return ok && p == pushed
	})
	return commit
}

// landJobID reads the task's land job id; 0 when none was queued.
func (e *testEnv) landJobID(task int64) int64 {
	return e.int64Of("SELECT COALESCE(MIN(id), 0) FROM jobs WHERE task_id = ? AND kind = 'land'", task)
}

// landTest drives one task through work and an approving review on the
// land-capable runner, and returns nothing: the state the tests wait
// for — the landing, the hold, the park — is the land job's own doing.
// The runner is built the normal way — no git-label helper — so the
// default that every runner offers git (§7.1) is what takes the land
// job.
func (e *testEnv) landTest(t *testing.T, remote, settings, taskSpec, taskCheck, workerScript string, adjust ...func(*Config)) (task int64, work int64) {
	e.t.Helper()
	e.seedProject(remote)
	e.setProjectSettings(settings)
	task = e.seedTask("", taskCheck)
	work = e.submitWork(task, prompt.ComposeWorkerPrompt(taskSpec, "", ""))
	t.Setenv("BRIGADA_TEST_BRANCH", taskBranch(task, 1))
	e.startLandRunner(t, workerScript, adjust...)
	return task, work
}

// startLandRunner starts the land-capable dual-agent runner on the
// scripted worker, and returns it for the tests that stop it.
func (e *testEnv) startLandRunner(t *testing.T, workerScript string, adjust ...func(*Config)) *runnerProc {
	e.t.Helper()
	// The landing clone commits with the host's identity, and a CI host
	// has none.
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "land test")
		t.Setenv(k+"_EMAIL", "land@brigada.invalid")
	}
	adjust = append(adjust, withReviewerAgent)
	return e.startRunner(writeWorkerScript(t, workerScript), 100*time.Millisecond, adjust...)
}

// addDep records that task depends on dep (§6.1), as a plan's edge.
func (e *testEnv) addDep(task, dep int64) {
	e.t.Helper()
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO task_deps (plan_id, task_id, depends_on_task_id, is_base, is_continue)
			VALUES ((SELECT plan_id FROM tasks WHERE id = ?), ?, ?, 0, 0)`, task, task, dep)
		return err
	})
	if err != nil {
		e.t.Fatalf("add the dependency of task %d on %d: %v", task, dep, err)
	}
}

// remoteRefCommit returns the commit the remote holds for the full ref
// name, or "" when it has none.
func remoteRefCommit(t *testing.T, remote, ref string) string {
	t.Helper()
	fields := strings.Fields(git(t, "", "ls-remote", remote, ref))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// parentOf returns the first parent of commit in the remote.
func parentOf(t *testing.T, remote, commit string) string {
	t.Helper()
	return strings.TrimSpace(git(t, "", "--git-dir", strings.TrimPrefix(remote, "file://"), "rev-parse", commit+"^"))
}

// stackReviewWorker is the dual worker whose implementer writes a file
// named for its own branch — task-N.txt — so the tasks of one test
// change different files and their landings stack without a conflict.
var stackReviewWorker = strings.ReplaceAll(
	strings.Replace(reviewWorker, "echo hello > work.txt", `echo hello > "$(basename "$(git rev-parse --abbrev-ref HEAD)").txt"`, 1),
	"@RESULT@", reviewApproveResult)

// stackSpec and stackCheck are the stack tests' task: the file named
// for the task, holding hello. A task's worktree holds its own file
// alone, so the glob names one.
const (
	stackSpec  = "Add a file named for the task with hello in it."
	stackCheck = `test "$(cat task-*.txt)" = hello`
)

// assertStackPushed checks the two waiting landings bottom and top, of
// tasks bottomTask and topTask — top squashed onto bottom, bottom onto
// the seed, the waiting ref at top, main unmoved — then releases the
// push gate and checks that both push, each its recorded commit: main
// ends at top, the seed and the two squashes alone, each task's file
// there.
func (e *testEnv) assertStackPushed(t *testing.T, remote, seed string, bottomTask, topTask int64, bottom, top string) {
	t.Helper()
	if got := parentOf(t, remote, top); got != bottom {
		t.Fatalf("the later landing %s sits on %s, want the earlier landing %s", top, got, bottom)
	}
	if got := parentOf(t, remote, bottom); got != seed {
		t.Fatalf("the earlier landing %s sits on %s, want the seed %s", bottom, got, seed)
	}
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != top {
		t.Errorf("the waiting ref = %q, want the top of the stack %s", got, top)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Fatalf("main moved while the pushes waited: %s, want the seed %s", got, seed)
	}
	ures, err := e.eng.PushApprove(e.ctx, remote)
	if err != nil || ures.Released != 2 {
		t.Fatalf("PushApprove = %+v, %v; want 2 released", ures, err)
	}
	if got := e.waitLanding(bottomTask, "pushed"); got != bottom {
		t.Errorf("task %d pushed %s, want its recorded commit %s", bottomTask, got, bottom)
	}
	if got := e.waitLanding(topTask, "pushed"); got != top {
		t.Errorf("task %d pushed %s, want its recorded commit %s", topTask, got, top)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != top {
		t.Errorf("main after the pushes = %s, want the top of the stack %s (no new squash)", got, top)
	}
	bare := strings.TrimPrefix(remote, "file://")
	if out := strings.TrimSpace(git(t, "", "--git-dir", bare, "rev-list", "--count", "main")); out != "3" {
		t.Errorf("commits on main = %s, want the seed and the two squashes alone", out)
	}
	for _, task := range []int64{bottomTask, topTask} {
		file := "main:task-" + strconv.FormatInt(task, 10) + ".txt"
		if got := git(t, "", "--git-dir", bare, "show", file); got != "hello\n" {
			t.Errorf("%s = %q, want hello", file, got)
		}
	}
}

// pushToMain pushes one commit on the remote's main from a scratch
// clone, as another engineer's landing would: dir/file is written with
// content before the commit. It returns main's new head.
func pushToMain(t *testing.T, remote, file, content string) string {
	t.Helper()
	scratch := t.TempDir()
	git(t, "", "clone", "-q", "--branch", "main", remote, scratch)
	if err := os.WriteFile(filepath.Join(scratch, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, scratch, "add", "-A")
	git(t, scratch, "-c", "user.name=other", "-c", "user.email=other@brigada.invalid",
		"commit", "-qm", "another engineer's landing")
	git(t, scratch, "push", "-q", "origin", "main")
	return remoteBranchCommit(t, remote, "main")
}

// conflictReviewWorker is the review tests' dual worker with a slow
// reviewer: the test moves the remote's main in the second the
// reviewer takes, so the landing that follows the approval fetches a
// target that no longer takes the task's diff cleanly. (Built on the
// template by hand — reviewScript rebuilds from the template itself,
// so it cannot take an edited one.)
var conflictReviewWorker = strings.Replace(reviewWorker, "@RESULT@", "sleep 1\n\t"+reviewApproveResult, 1)

// infraReviewWorker is the dual worker whose implementer writes a file
// under infra/: the changed paths the runner records, which the land
// gate's protected-path rule then matches.
var infraReviewWorker = strings.ReplaceAll(
	strings.Replace(reviewWorker, "echo hello > work.txt", "mkdir -p infra && echo gateway > infra/gateway.txt", 1),
	"@RESULT@", reviewApproveResult)

// TestRunnerLandJobPushes: an approved task under gate_push auto lands
// as one squash commit on the target and is pushed: the record says
// pushed, the remote's main holds exactly the seed and the squash, and
// the message is the one the hand-driven land builds.
func TestRunnerLandJobPushes(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"auto"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	done := e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.workerResult(done)
	branch := taskBranch(task, 1)
	head := remoteBranchCommit(t, remote, branch)
	if head == "" {
		t.Fatal("the task's result branch was never pushed")
	}
	// The task's changed paths are the runner's to say (§6.3, §7.1) and
	// the engine's to keep with the task.
	if len(res.ChangedPaths) != 1 || res.ChangedPaths[0] != "work.txt" {
		t.Errorf("work result changed_paths = %v, want [work.txt]", res.ChangedPaths)
	}
	if cp := e.strOf("SELECT changed_paths FROM tasks WHERE id = ?", task); !strings.Contains(cp, "work.txt") {
		t.Errorf("task changed_paths = %q, want the recorded paths", cp)
	}

	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "pushed")
	bare := strings.TrimPrefix(remote, "file://")
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("remote main = %s, want the landing commit %s", got, commit)
	}
	if out := strings.TrimSpace(git(t, "", "--git-dir", bare, "rev-list", "--count", "main")); out != "2" {
		t.Errorf("commits on main = %s, want the seed and the squash alone", out)
	}
	if got := git(t, "", "--git-dir", bare, "show", "main:work.txt"); got != "hello\n" {
		t.Errorf("main's work.txt = %q, want hello", got)
	}
	msg := git(t, "", "--git-dir", bare, "show", "-s", "--format=%B", commit)
	for _, want := range []string{
		"Brigada-Task: " + strconv.FormatInt(task, 10),
		"Brigada-Branch: " + branch + "@" + head,
		"Co-Authored-By: fake-", // whichever agent authored; the testfam family has no address, so the model alone (§7.1)
		"    added work.txt",    // the worker's summary, indented under its heading
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the squash message lacks %q:\n%s", want, msg)
		}
	}
	e.assertNoWorktrees()
}

// TestRunnerDefaultLabelsLeaseLandJob: a runner built the normal way —
// the shape init and start pass when --labels names nothing, with no
// label offered at all — still leases and completes a land job, because
// the runner itself offers the git label the job needs (§7.1). No
// withGitLabel helper widens its labels here.
func TestRunnerDefaultLabelsLeaseLandJob(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"auto"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`,
		reviewScript(reviewApproveResult), func(c *Config) { c.Labels = nil })

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	e.waitLanding(task, "pushed")
}

// workerResultLine is the review worker template's fixed work-result
// line, replaced by the tests whose worker writes its own result.
const workerResultLine = `	printf '{"type":"result","text":"added work.txt: {\\"status\\":\\"done\\",\\"summary\\":\\"added work.txt\\",\\"branch\\":\\"%s\\"}"}\n' "$br"`

// TestRunnerLandJobUsesTheWorkersCommitMessage: a worker that writes
// the commit message in its result block (§5.4) has it as the squash
// commit's subject and body — indented, so its forged trailer is no
// trailer — with the land job's own trailers after it.
func TestRunnerLandJobUsesTheWorkersCommitMessage(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto"}`)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	branch := taskBranch(task, 1)
	result := fakeResultScript("added work.txt: ```json\n" +
		`{"status":"done","summary":"added work.txt","branch":"` + branch + `","commit_message":"feat(work): add work.txt\n\nWrites work.txt with hello, as the spec asks.\nCo-Authored-By: evil <e@x>"}` +
		"\n```")
	worker := strings.Replace(reviewScript(reviewApproveResult), workerResultLine, result, 1)
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", "", ""))
	e.startLandRunner(t, worker)

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "pushed")
	bare := strings.TrimPrefix(remote, "file://")
	msg := git(t, "", "--git-dir", bare, "show", "-s", "--format=%B", commit)
	if !strings.HasPrefix(msg, "feat(work): add work.txt\n") {
		t.Errorf("the worker's subject must lead the squash message:\n%s", msg)
	}
	for _, want := range []string{
		"\n    Writes work.txt with hello, as the spec asks.\n",
		"\n    Co-Authored-By: evil <e@x>\n",
		"\nBrigada-Task: " + strconv.FormatInt(task, 10) + "\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the squash message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Spec:") || strings.Contains(msg, "Worker summary:") {
		t.Errorf("the worker's message replaces the built parts:\n%s", msg)
	}
	if n := strings.Count(msg, "\nCo-Authored-By:"); n != 1 {
		t.Errorf("the worker's forged trailer is indented text; one real one, got %d:\n%s", n, msg)
	}
}

// TestRunnerLandGateAskWaitsThenPushes: gate_push ask records the
// landing as waiting and pushes nothing; `push approve` queues the push
// phase, which pushes the recorded commit — no new squash — and moves
// the record to pushed.
func TestRunnerLandGateAskWaitsThenPushes(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"ask"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "waiting")
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Fatalf("main moved while the push waited: %s, want the seed %s", got, seed)
	}
	// The commit waits on the remote, on no branch of the user's.
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != commit {
		t.Errorf("the waiting ref = %q, want the recorded commit %s", got, commit)
	}

	// The user releases the push gate (§6.3); the push phase pushes the
	// recorded commit.
	ures, err := e.eng.PushApprove(e.ctx, remote)
	if err != nil || ures.Released != 1 {
		t.Fatalf("PushApprove = %+v, %v; want 1 released", ures, err)
	}
	e.waitUntil("the landing to be pushed", func() bool {
		p, _, _, _, _ := e.landing(task)
		return p == "pushed"
	})
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("main after the push = %s, want the recorded commit %s (no new squash)", got, commit)
	}
	// One commit landed, once: the seed and the squash alone.
	if out := strings.TrimSpace(git(t, "", "--git-dir", strings.TrimPrefix(remote, "file://"), "rev-list", "--count", "main")); out != "2" {
		t.Errorf("commits on main = %s, want the seed and the squash alone", out)
	}
}

// TestRunnerLandGateAskStacksTwoLandings: two independent landings that
// wait at the push gate stack on the waiting ref — the later squashed
// onto the earlier, not onto the unmoved target — and one push approve
// releases both, each pushed as its recorded commit, no new squash.
func TestRunnerLandGateAskStacksTwoLandings(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	first, work := e.landTest(t, remote, `{"gate_push":"ask"}`, stackSpec, stackCheck, stackReviewWorker)
	second := e.seedTask("", stackCheck)
	e.submitWork(second, prompt.ComposeWorkerPrompt(stackSpec, "", ""))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	a := e.waitLanding(first, "waiting")
	b := e.waitLanding(second, "waiting")
	// Nothing orders the two: whichever landed first is the bottom.
	if parentOf(t, remote, a) == b {
		e.assertStackPushed(t, remote, seed, second, first, b, a)
		return
	}
	e.assertStackPushed(t, remote, seed, first, second, a, b)
}

// TestRunnerLandGateAskStackLandsInDependencyOrder: a dependent whose
// dependency waits at the push gate lands on top of the dependency's
// commit, not on a target that lacks it, and the release pushes the
// stack in dependency order.
func TestRunnerLandGateAskStackLandsInDependencyOrder(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	dep, work := e.landTest(t, remote, `{"gate_push":"ask"}`, stackSpec, stackCheck, stackReviewWorker)
	dependent := e.seedTask("", stackCheck)
	e.addDep(dependent, dep)
	e.submitWork(dependent, prompt.ComposeWorkerPrompt(stackSpec, "", ""))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	a := e.waitLanding(dep, "waiting")
	b := e.waitLanding(dependent, "waiting")
	e.assertStackPushed(t, remote, seed, dep, dependent, a, b)
}

// TestRunnerLandPushPhaseOnAnotherRunner: the push phase needs nothing
// of the runner that squashed. That runner stops and loses its state
// directory; a second runner leases the push, fetches the recorded
// commit from the waiting ref and pushes it.
func TestRunnerLandPushPhaseOnAnotherRunner(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"ask"}`)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", "", ""))
	t.Setenv("BRIGADA_TEST_BRANCH", taskBranch(task, 1))
	script := reviewScript(reviewApproveResult)
	squasher := e.startLandRunner(t, script)

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	commit := e.waitLanding(task, "waiting")
	squasher.stop(t)
	if err := os.RemoveAll(e.stateDir); err != nil {
		t.Fatal(err)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Fatalf("main moved while the push waited: %s, want the seed %s", got, seed)
	}

	other := e.startLandRunner(t, script, func(c *Config) {
		c.Token = e.secrets["r2"]
		c.StateDir = filepath.Join(t.TempDir(), "runner-state")
	})
	defer other.stop(t)
	ures, err := e.eng.PushApprove(e.ctx, remote)
	if err != nil || ures.Released != 1 {
		t.Fatalf("PushApprove = %+v, %v; want 1 released", ures, err)
	}
	if got := e.waitLanding(task, "pushed"); got != commit {
		t.Errorf("the pushed landing names %s, want the recorded commit %s", got, commit)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("main after the push = %s, want the recorded commit %s (no new squash)", got, commit)
	}
}

// TestRunnerLandGateNeverRecordsThePushCmd: gate_push never records the
// landing as local with the exact push command, pushes no branch of the
// user's — the commit waits on the remote's waiting ref, so the command
// runs from the user's own repository, which the test runs verbatim —
// and runs after_land after the landing.
func TestRunnerLandGateNeverRecordsThePushCmd(t *testing.T) {
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	markers := markerDir(t)
	task, work := e.landTest(t, remote, `{"gate_push":"never","after_land":"echo after-land >> \"$BRIGADA_TEST_MARKERS/after-land\""}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "local")
	cmd := "git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main"
	if got := e.strOf("SELECT push_cmd FROM landings WHERE task_id = ?", task); got != cmd {
		t.Errorf("landing push_cmd = %q, want %q", got, cmd)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Errorf("main moved under gate_push never: %s, want the seed %s", got, seed)
	}
	// The commit is durable on the waiting ref, and the recorded command
	// pushes it from a clone of the user's own, verbatim.
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != commit {
		t.Errorf("the waiting ref = %q, want the recorded commit %s", got, commit)
	}
	user := t.TempDir()
	git(t, "", "clone", "-q", "--branch", "main", remote, user)
	push := exec.Command("sh", "-c", cmd)
	push.Dir = user
	if err := push.Run(); err != nil {
		t.Fatalf("run the recorded push command in the user's clone: %v", err)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("main after the user's push = %s, want the recorded commit %s", got, commit)
	}
	e.waitUntil("after_land to run after the landing", func() bool {
		data, err := os.ReadFile(filepath.Join(markers, "after-land"))
		return err == nil && strings.Contains(string(data), "after-land")
	})
}

// TestRunnerLandJobConflict: a target that moved under the task's diff
// stops the landing — nothing pushed, no record, the conflicting files
// in the job's completion — and the task parks for the foreman.
func TestRunnerLandJobConflict(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"auto"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, conflictReviewWorker)

	done := e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	_ = done
	// While the reviewer takes its second, another engineer lands on
	// main a change that collides with the task's.
	moved := pushToMain(t, remote, "work.txt", "goodbye\n")
	if moved == seed {
		t.Fatal("the conflicting push never moved main")
	}
	e.waitTaskState(task, "needs_foreman")
	if _, _, _, _, ok := e.landing(task); ok {
		t.Error("a conflicting landing was recorded")
	}
	if got := remoteBranchCommit(t, remote, "main"); got != moved {
		t.Errorf("main moved by the failed landing: %s, want %s", got, moved)
	}
	rec := e.strOf("SELECT result FROM jobs WHERE id = ?", e.landJobID(task))
	if !strings.Contains(rec, "work.txt") {
		t.Errorf("the landing record does not name the conflicting file: %s", rec)
	}
}

// TestRunnerLandJobCheckFailure: a check that fails on the landed
// result parks the task with the log as the job's artifact, nothing
// pushed. The work round's own check passes — the project's default
// check is what the landing runs (§7.1).
func TestRunnerLandJobCheckFailure(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"auto","default_check":"test ! -f work.txt"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "needs_foreman")
	if _, _, _, _, ok := e.landing(task); ok {
		t.Error("a failed-check landing was recorded")
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Errorf("main moved by the failed check: %s, want the seed %s", got, seed)
	}
	land := e.landJobID(task)
	if land == 0 {
		t.Fatal("no land job ran")
	}
	arts := e.artifacts(land)
	if _, ok := arts["check_log"]; !ok {
		t.Errorf("the failed landing left no check log artifact (has %v)", arts)
	}
}

// TestRunnerLandAfterLandFailure: an after_land that fails rides the
// record and the history — reported, never undone — and the landing it
// followed stays pushed.
func TestRunnerLandAfterLandFailure(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"auto","after_land":"echo after-land boom; exit 3"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "pushed")
	if err := e.strOf("SELECT after_land_error FROM landings WHERE task_id = ?", task); !strings.Contains(err, "exit 3") {
		t.Errorf("landing after_land_error = %q, want the after_land failure", err)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("the after_land failure undid the push: main = %s, want %s", got, commit)
	}
}

// landHookRemote is newRemote with a pre-receive hook that counts the
// pushes it sees for main and rejects the first n of them, as a target
// that refuses the landing's push would. Pushes to other branches —
// the work round's own — pass untouched.
func landHookRemote(t *testing.T, reject int) string {
	t.Helper()
	remote := newRemote(t)
	hook := filepath.Join(strings.TrimPrefix(remote, "file://"), "hooks", "pre-receive")
	script := `#!/bin/sh
while read old new ref; do
	if [ "$ref" = "refs/heads/main" ]; then
		f="$BRIGADA_TEST_MARKERS/pushes"
		n=$(($(cat "$f" 2>/dev/null || echo 0)+1))
		echo $n >"$f"
		if [ "$n" -le ` + strconv.Itoa(reject) + ` ]; then exit 1; fi
	fi
done
exit 0
`
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return remote
}

// TestRunnerLandPushRejectedRetriesOnce: a push the target rejected is
// retried once from a fresh fetch and squash, and the retry's push
// lands.
func TestRunnerLandPushRejectedRetriesOnce(t *testing.T) {
	markers := markerDir(t)
	remote := landHookRemote(t, 1)
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"auto"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "pushed")
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("main = %s, want the retry's landing commit %s", got, commit)
	}
	e.waitUntil("two pushes to reach the hook", func() bool {
		data, err := os.ReadFile(filepath.Join(markers, "pushes"))
		return err == nil && strings.TrimSpace(string(data)) == "2"
	})
}

// TestRunnerLandPushRejectedTwiceFails: a push the target rejects again
// after the one retry fails the landing like a conflict — nothing
// pushed, no record, the task parked for the foreman.
func TestRunnerLandPushRejectedTwiceFails(t *testing.T) {
	markerDir(t)
	remote := landHookRemote(t, 2)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"auto"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "needs_foreman")
	if _, _, _, _, ok := e.landing(task); ok {
		t.Error("a landing whose push never took was recorded")
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Errorf("main moved by the rejected landing: %s, want the seed %s", got, seed)
	}
	rec := e.strOf("SELECT result FROM jobs WHERE id = ?", e.landJobID(task))
	if !strings.Contains(rec, "rejected") && !strings.Contains(rec, "push") {
		t.Errorf("the failed landing's record does not name the push: %s", rec)
	}
}

// TestRunnerRecordsChangedPathsMatchedByLandGate: the work round's
// recorded paths — not the spec's words — are what the land gate's
// protected-path rule matches: a task that touched infra/** holds at
// the gate though its spec never says so, and nothing is pushed.
func TestRunnerRecordsChangedPathsMatchedByLandGate(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"approval_protected_paths":"infra/**","gate_push":"auto"}`,
		"A quiet refactor.", `test "$(cat infra/gateway.txt)" = gateway`, infraReviewWorker)

	done := e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.workerResult(done)
	found := false
	for _, p := range res.ChangedPaths {
		if p == "infra/gateway.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("work result changed_paths = %v, want infra/gateway.txt among them", res.ChangedPaths)
	}
	e.waitTaskState(task, "done")
	e.waitUntil("the task to be held at the land gate", func() bool {
		return e.int64Of("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", task) == 1
	})
	if reason := e.strOf("SELECT reason FROM land_holds WHERE task_id = ?", task); !strings.Contains(reason, "touches infra/**") {
		t.Errorf("land hold reason = %q, want the protected path the work touched", reason)
	}
	if n := e.int64Of("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", task); n != 0 {
		t.Errorf("land jobs = %d, want none while held", n)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Errorf("main moved by a held landing: %s, want the seed %s", got, seed)
	}
}

// TestRunnerLandCheckRunsSetupAndSeesExtraRepos: the landing check runs
// after the task's recorded setup, in the landing clone, with the task's
// extra checkouts named (§5.7, §5.8): a default_check that needs either
// passes though the clone is another directory than the work's.
func TestRunnerLandCheckRunsSetupAndSeesExtraRepos(t *testing.T) {
	markers := markerDir(t)
	remote := newRemote(t)
	extra := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto","default_check":"grep -qx \"$PWD\" \"$BRIGADA_TEST_MARKERS/setup-dirs\" && test -f \"$BRIGADA_REPO_TOOLS/README.md\""}`)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	setup := `{"command":"echo \"$PWD\" >> \"$BRIGADA_TEST_MARKERS/setup-dirs\"","timeout_minutes":5}`
	extras := `{"repos":[{"name":"tools","repo":"example.invalid/tools","url":"` + extra + `"}],"source":"task"}`
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE tasks SET setup = ?, extra_repos = ? WHERE id = ?", setup, extras, task)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", "", ""))
	t.Setenv("BRIGADA_TEST_BRANCH", taskBranch(task, 1))
	e.startLandRunner(t, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "pushed")
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("remote main = %s, want the landing commit %s", got, commit)
	}
	data, _ := os.ReadFile(filepath.Join(markers, "setup-dirs"))
	if !strings.Contains(string(data), filepath.Join("lands", "task-"+strconv.FormatInt(task, 10))) {
		t.Errorf("setup never ran in the landing clone:\n%s", data)
	}
}

// TestRunnerLandFreshTargetCreatedFromDefault: a land_target the remote
// does not have yet is created from the default branch by the first
// landing, which pushes onto it; main is left alone.
func TestRunnerLandFreshTargetCreatedFromDefault(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, `{"gate_push":"auto","land_target":"brigada/landed"}`,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "pushed")
	if got := remoteBranchCommit(t, remote, "brigada/landed"); got != commit {
		t.Errorf("remote brigada/landed = %q, want the landing commit %s", got, commit)
	}
	if got := parentOf(t, remote, commit); got != seed {
		t.Errorf("the landing's parent = %s, want the default branch's head %s", got, seed)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Errorf("main moved: %s, want the seed %s", got, seed)
	}
}

// TestRunnerLandWithoutHostIdentity: on a host with no git identity at
// all, the landing clone commits under the runner's fallback identity
// (the mirror's), where it used to fail with "Author identity unknown".
func TestRunnerLandWithoutHostIdentity(t *testing.T) {
	markerDir(t)
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	e.setProjectSettings(`{"gate_push":"auto"}`)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", "", ""))
	t.Setenv("BRIGADA_TEST_BRANCH", taskBranch(task, 1))
	// No identity from anywhere: no environment, no global or system
	// config, and no guessing from the user and host names.
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.useConfigOnly")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	e.startRunner(writeWorkerScript(t, reviewScript(reviewApproveResult)), 100*time.Millisecond, withReviewerAgent)

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")
	commit := e.waitLanding(task, "pushed")
	bare := strings.TrimPrefix(remote, "file://")
	if got := strings.TrimSpace(git(t, "", "--git-dir", bare, "show", "-s", "--format=%an <%ae>", commit)); got != "brigada runner <runner@brigada.invalid>" {
		t.Errorf("landing commit author = %q, want the fallback identity", got)
	}
}
