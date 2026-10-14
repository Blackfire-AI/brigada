package runner

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// The review jobs on the runner (§7), end to end against the real
// engine: one runner advertising two fake agents of different families,
// so the work job runs on one and its review on the other, and one
// scripted worker that plays whichever the prompt asks for. The task's
// remote is the local bare repo; the work round pushes the result
// branch, and the review reads it.

// reviewFindings is substantive (§7's minimum length): what was checked
// and why it holds, long enough that the engine counts the approve. It
// carries no apostrophe — it rides a single-quoted shell argument.
const reviewFindings = "the diff adds work.txt holding hello, exactly what the spec asked; the check covers the content; the base is untouched and nothing else changed"

// reviewWorker plays both agents of the runner: the implementer for a
// work prompt, the reviewer for a review prompt, told apart by the
// review template's own first words. The reviewer is deliberately
// dull, for portability — the script must behave under any POSIX sh
// and any git a host brings (macOS included), so it parses nothing and
// pipes nowhere: it consumes the prompt, copies what it saw — the
// prompt itself, the commit under review, the diff the prompt names,
// the file's content — into marker files with plain redirects, and
// ends with @RESULT@. Every judgement is the Go test's, over those
// files; a git that fails writes its error into the file the test then
// shows. The branch under review arrives by environment
// (BRIGADA_TEST_BRANCH), never by parsing the prompt.
const reviewWorker = `
prompt=$(cat)
case "$prompt" in
*"brigada reviewer"*)
	echo '{"type":"system_init","session_id":"sess-rev"}'
	echo '{"type":"tool_start","name":"Read"}'
	M="$BRIGADA_TEST_MARKERS"
	printf '%s' "$prompt" > "$M/review-prompt"
	git rev-parse HEAD > "$M/review-head" 2>&1
	git --no-pager diff "main...$BRIGADA_TEST_BRANCH" --name-only > "$M/review-diff" 2>&1
	cat work.txt > "$M/review-work" 2>&1
	echo '{"type":"tool_end","name":"Read"}'
	@RESULT@
	;;
*)
	echo '{"type":"system_init","session_id":"sess-42"}'
	echo '{"type":"assistant","text":"reading the task"}'
	echo '{"type":"tool_start","name":"Edit"}'
	echo hello > work.txt
	git add -A
	git commit -qm 'fake work: add work.txt'
	echo '{"type":"tool_end","name":"Edit"}'
	br=$(git rev-parse --abbrev-ref HEAD)
	printf '{"type":"result","text":"added work.txt: {\\"status\\":\\"done\\",\\"summary\\":\\"added work.txt\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
	;;
esac
`

// reviewApproveResult is the reviewer's ending: the verdict block,
// fenced, as the review template tells it to (§7). fakeResultScript
// renders the whole event line in Go and emits it verbatim — a shell
// printf in its place reads the JSON's escapes differently on dash
// and on bash's POSIX mode, and the bash reading is not JSON at all.
var reviewApproveResult = fakeResultScript("reviewed the change. ```json\n{\"verdict\":\"approve\",\"findings\":\"" + reviewFindings + "\"}\n```")

// reviewNoVerdictResult is the reviewer that says it looked and never
// fences a verdict (§7): the runner completes with an empty verdict
// and the engine records review_no_verdict.
const reviewNoVerdictResult = `echo '{"type":"result","text":"I read the diff and it all looked fine to me"}'`

// reviewWedgedResult is the reviewer that goes silent without a
// verdict: whatever it was doing, it never reports. The run must not
// hang — the stall watch kills it and the job completes with the
// outcome, so the engine and the test both learn what happened.
const reviewWedgedResult = `sleep 30`

// reviewScript assembles the dual worker around one reviewer ending.
func reviewScript(result string) string {
	return strings.ReplaceAll(reviewWorker, "@RESULT@", result)
}

// withReviewerAgent widens the runner's advertisement to two families
// (§7): the work runs on the first agent, the review on the other. The
// same adjustment bounds the run for the test: a stall watch far under
// the test's own patience, so a reviewer that wedges on some host is
// killed and completes stalled — a fast, diagnosable failure — instead
// of holding its lease until the test times out with nothing to read.
// In production the same machinery, at its default, is the bound.
func withReviewerAgent(cfg *Config) {
	cfg.Agents = append(cfg.Agents, cfg.Agents[0])
	cfg.Agents[1].Model = "fake-2"
	cfg.Agents[1].Family = "otherfam"
	cfg.Agents[1].Account = "acct-2"
	cfg.StallAfter = 3 * time.Second
	cfg.ClassCaps = map[string]ClassCaps{"review": {TimeCap: 8 * time.Second}}
}

// startReviewRunner runs the dual-agent runner on the scripted worker,
// with the task's result branch named for the reviewer through the
// environment.
func (e *testEnv) startReviewRunner(t *testing.T, branch, script string) *runnerProc {
	e.t.Helper()
	t.Setenv("BRIGADA_TEST_BRANCH", branch)
	return e.startRunner(writeWorkerScript(t, script), 100*time.Millisecond, withReviewerAgent)
}

// waitTaskState polls the task's state until it holds.
func (e *testEnv) waitTaskState(task int64, state string) {
	e.t.Helper()
	e.waitUntil(fmt.Sprintf("task %d to become %s", task, state), func() bool {
		return e.taskState(task) == state
	})
}

// taskState and planStateOf read the states the engine drove.
func (e *testEnv) taskState(task int64) string {
	e.t.Helper()
	return e.strOf("SELECT state FROM tasks WHERE id = ?", task)
}

func (e *testEnv) planStateOf(task int64) string {
	e.t.Helper()
	return e.strOf("SELECT p.state FROM plans p JOIN tasks t ON t.plan_id = p.id WHERE t.id = ?", task)
}

// strOf reads one text column from the store.
func (e *testEnv) strOf(query string, args ...any) string {
	e.t.Helper()
	var v sql.NullString
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&v)
	})
	if err != nil {
		e.t.Fatalf("query %q %v: %v", query, args, err)
	}
	return v.String
}

// reviewResultOf unwraps the verdict a review job completed with: the
// job's result carries it verbatim (§7), not the worker result block.
func (e *testEnv) reviewResultOf(t *testing.T, job int64) string {
	t.Helper()
	info, err := e.eng.Job(e.ctx, job)
	if err != nil || info == nil {
		t.Fatalf("read review job %d: %v (%v)", job, info, err)
	}
	var outer struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(info.Result, &outer); err != nil {
		t.Fatalf("review job result %s: %v", info.Result, err)
	}
	return outer.Result
}

// checkRuns counts the check's own log lines: once for the work round,
// and never a second — a review runs no check (§7).
func checkRuns(t *testing.T, markers string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(markers, "checks"))
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(data)))
}

// reviewerMarker reads one marker the reviewer left, failing with a
// clear error when there is none: a review that never recorded is
// itself the finding, and the job's own result says what became of it.
func reviewerMarker(t *testing.T, markers, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(markers, name))
	if err != nil {
		t.Errorf("the reviewer left no %s marker; its run never got that far: %v", name, err)
		return ""
	}
	return string(data)
}

// assertReviewerSaw holds the reviewer's own record against §7: the
// prompt carried the spec and the check result verbatim, the diff
// command naming the base and the branch, and never the implementer's
// summary; the worktree sat at the pushed branch, holding its content,
// with the diff naming the work. What a marker shows on failure is
// what the reviewer actually saw — a git that failed names it itself.
func assertReviewerSaw(t *testing.T, markers, branch, pushed string) {
	t.Helper()
	saw := reviewerMarker(t, markers, "review-prompt")
	for _, want := range []string{
		"<task-spec>", "the task spec", "<check-result>",
		"git diff main..." + branch,
	} {
		if !strings.Contains(saw, want) {
			t.Errorf("the review prompt the reviewer saw lacks %q:\n%s", want, saw)
		}
	}
	if strings.Contains(saw, "added work.txt") {
		t.Errorf("the review prompt carried the implementer's summary (§7):\n%s", saw)
	}
	if head := strings.TrimSpace(reviewerMarker(t, markers, "review-head")); head != pushed {
		t.Errorf("the review worktree sat at %q, want the pushed commit %q", head, pushed)
	}
	if diff := reviewerMarker(t, markers, "review-diff"); !strings.Contains(diff, "work.txt") {
		t.Errorf("the diff the reviewer ran does not name the work:\n%s", diff)
	}
	if work := reviewerMarker(t, markers, "review-work"); work != "hello\n" {
		t.Errorf("the branch content the reviewer read = %q, want hello", work)
	}
}

// remoteRefs is the ref names the remote holds.
func remoteRefs(t *testing.T, remote string) []string {
	t.Helper()
	out := git(t, "", "ls-remote", "--refs", "--heads", remote)
	var refs []string
	for _, line := range strings.Fields(string(out)) {
		if strings.HasPrefix(line, "refs/heads/") {
			refs = append(refs, strings.TrimPrefix(line, "refs/heads/"))
		}
	}
	return refs
}

// TestRunnerRunsReviewJobToEnd is §7 on one runner: the work round done
// with its check passed queues the review on the other family; the
// review runs at the pushed branch with the composed prompt, runs no
// check and pushes nothing; its verdict reaches the engine, which
// finishes the task and the plan with it.
func TestRunnerRunsReviewJobToEnd(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	task := e.seedTask("", `echo check >> "$BRIGADA_TEST_MARKERS/checks"; test "$(cat work.txt)" = hello`)
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("the task spec", ""))

	p := e.startReviewRunner(t, taskBranch(task, 1), reviewScript(reviewApproveResult))
	defer p.stop(t)

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	branch := taskBranch(task, 1)
	pushed := remoteBranchCommit(t, remote, branch)

	// The review took the other family's agent and ran; its verdict —
	// the fenced block verbatim — finished the task and the plan (§7).
	review := e.int64Of("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", task)
	if review == 0 {
		t.Fatal("no review job was queued for the done work")
	}
	e.waitTaskState(task, "done")
	if state := e.planStateOf(task); state != "done" {
		t.Errorf("plan state = %q, want done with its task", state)
	}
	want := `{"verdict":"approve","findings":"` + reviewFindings + `"}`
	if got := e.reviewResultOf(t, review); got != want {
		t.Errorf("review result = %q\nwant the verdict block\n%q", got, want)
	}
	if agent := e.int64Of("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", review); agent == 0 {
		t.Error("the review job recorded no agent")
	}

	// The review ran no check (§7): the work round's is the only one.
	if n := checkRuns(t, markers); n != 1 {
		t.Errorf("the check ran %d times, want the work round's alone", n)
	}
	// And it pushed nothing (§7): the remote holds what the work round
	// pushed, and no branch more.
	if got := remoteBranchCommit(t, remote, branch); got != pushed {
		t.Errorf("the remote's %s moved during the review: %s -> %s", branch, pushed, got)
	}
	if refs := remoteRefs(t, remote); len(refs) != 2 {
		t.Errorf("the remote holds %v, want main and the task's branch alone", refs)
	}

	assertReviewerSaw(t, markers, branch, pushed)
	e.assertNoWorktrees()
}

// TestRunnerReviewWithoutAVerdictBlock: a reviewer that never fences a
// verdict completes with an empty one — the run is done, not failed —
// and the engine records review_no_verdict and parks the task for the
// foreman (§7; the retry with another reviewer is F1d3's).
func TestRunnerReviewWithoutAVerdictBlock(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	task := e.seedTask("", `echo check >> "$BRIGADA_TEST_MARKERS/checks"; test "$(cat work.txt)" = hello`)
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("the task spec", ""))

	p := e.startReviewRunner(t, taskBranch(task, 1), reviewScript(reviewNoVerdictResult))
	defer p.stop(t)

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	branch := taskBranch(task, 1)
	pushed := remoteBranchCommit(t, remote, branch)

	review := e.int64Of("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", task)
	if review == 0 {
		t.Fatal("no review job was queued for the done work")
	}
	e.waitTaskState(task, "needs_foreman")
	if n := e.int64Of("SELECT COUNT(*) FROM events WHERE kind = 'review_no_verdict'"); n != 1 {
		t.Errorf("review_no_verdict events = %d, want the engine to record one", n)
	}
	if got := e.reviewResultOf(t, review); got != emptyVerdictBlock {
		t.Errorf("review result = %q, want the empty verdict %q", got, emptyVerdictBlock)
	}
	if n := checkRuns(t, markers); n != 1 {
		t.Errorf("the check ran %d times, want the work round's alone", n)
	}
	if got := remoteBranchCommit(t, remote, branch); got != pushed {
		t.Errorf("the remote's %s moved during the review: %s -> %s", branch, pushed, got)
	}
	assertReviewerSaw(t, markers, branch, pushed)
	e.assertNoWorktrees()
}

// TestRunnerWedgedReviewFailsFast: a reviewer that goes silent mid-run
// is the case the macOS report named — leased, never completing, the
// test timing out with nothing to read. The stall watch (§5.6) is the
// bound: the worker's tree is killed, the review completes with the
// stalled outcome — its result block naming the silence, so the record
// says what happened — and the engine parks the task as no verdict
// (§7). Nothing is pushed, and the worktree comes down.
func TestRunnerWedgedReviewFailsFast(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	task := e.seedTask("", `echo check >> "$BRIGADA_TEST_MARKERS/checks"; test "$(cat work.txt)" = hello`)
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("the task spec", ""))

	p := e.startReviewRunner(t, taskBranch(task, 1), reviewScript(reviewWedgedResult))
	defer p.stop(t)

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	branch := taskBranch(task, 1)
	pushed := remoteBranchCommit(t, remote, branch)

	review := e.int64Of("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", task)
	if review == 0 {
		t.Fatal("no review job was queued for the done work")
	}
	// Well inside the env's ten-second patience, the wedged review ends:
	// the job completes stalled, its result names the silence, and the
	// task waits for the foreman on a no-verdict review (§7).
	e.waitJob(review, func(j *engine.JobInfo) bool { return j.Status == "stalled" })
	if got := e.reviewResultOf(t, review); !strings.Contains(got, "no activity") {
		t.Errorf("the wedged review's result = %q, want the stall named", got)
	}
	e.waitTaskState(task, "needs_foreman")
	if n := e.int64Of("SELECT COUNT(*) FROM events WHERE kind = 'review_no_verdict'"); n != 1 {
		t.Errorf("review_no_verdict events = %d, want the engine to record one", n)
	}
	if got := remoteBranchCommit(t, remote, branch); got != pushed {
		t.Errorf("the remote's %s moved during the review: %s -> %s", branch, pushed, got)
	}
	// It wedged after doing its reading: the markers it managed to
	// leave still show the run sat where §7 says it must.
	assertReviewerSaw(t, markers, branch, pushed)
	e.assertNoWorktrees()
}

// TestRunnerRunsTheAgentTheLeaseNamed is BPF1i on one runner (§5.1,
// §8): a runner offering two agents — a reviewer of another family and
// an implementer, each driving its own adapter — registers both, the
// work job runs the implementer's script even though the review-only
// agent sits first by id, and the review the family rule sends to the
// other family runs that agent's script. The grant names the agent the
// dispatcher picked, never the job's empty harness constraint, and
// each script leaves its own marker, so the test can tell which
// adapter actually ran.
func TestRunnerRunsTheAgentTheLeaseNamed(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("the task spec", ""))
	t.Setenv("BRIGADA_TEST_BRANCH", taskBranch(task, 1))

	workBody := "echo implement-ran > \"$BRIGADA_TEST_MARKERS/work-by\"\n" + happyWorker
	reviewBody := "echo review-ran > \"$BRIGADA_TEST_MARKERS/review-by\"\n" + reviewScript(reviewApproveResult)
	workAd := &fake.Adapter{Script: writeWorkerScript(t, workBody)}
	reviewAd := &fake.Adapter{Script: writeWorkerScript(t, reviewBody)}
	p := e.startRunner("", 100*time.Millisecond, func(cfg *Config) {
		cfg.Agents = []api.Agent{
			{Adapter: "fake", Harness: "revwh", Model: "fake-2", Family: "otherfam", Account: "acct-2", Classes: []string{"review"}},
			{Adapter: "fake", Harness: "implh", Model: "fake-1", Family: "workfam", Account: "acct-1", Classes: []string{"implement"}},
		}
		cfg.Adapters = []adapter.Adapter{reviewAd, workAd}
		cfg.StallAfter = 3 * time.Second
		cfg.ClassCaps = map[string]ClassCaps{"review": {TimeCap: 8 * time.Second}}
	})
	defer p.stop(t)

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")

	if got := reviewerMarker(t, markers, "work-by"); got != "implement-ran\n" {
		t.Errorf("the work ran %q, want the implementer's script", got)
	}
	if got := reviewerMarker(t, markers, "review-by"); got != "review-ran\n" {
		t.Errorf("the review ran %q, want the other family's script", got)
	}
	implAgent := e.int64Of("SELECT id FROM agents WHERE family = 'workfam'")
	reviewAgent := e.int64Of("SELECT id FROM agents WHERE family = 'otherfam'")
	if implAgent == 0 || reviewAgent == 0 {
		t.Fatalf("agents = impl %d, review %d; want both registered", implAgent, reviewAgent)
	}
	review := e.int64Of("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", task)
	if got := e.int64Of("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", work); got != implAgent {
		t.Errorf("the work job's agent = %d, want the implementer %d", got, implAgent)
	}
	if got := e.int64Of("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", review); got != reviewAgent {
		t.Errorf("the review job's agent = %d, want the other family's %d", got, reviewAgent)
	}
	e.assertNoWorktrees()
}
