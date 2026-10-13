package runner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The runner's tests run against the real thing: an engine, its store
// and its API in-process on a temp Unix socket, a local bare repo as
// the task's remote, and the fake adapter driving a scripted worker.
// The engine's clock is real — the runner's heartbeats are — so the
// timings that matter (lease TTL against heartbeat cadence) are set
// per test instead.

const e2eSeedTS = "2026-10-12T12:00:00.000000000Z"

const e2eTokens = `root admin root-secret-9
r1 runner r1-secret-9
r2 runner r2-secret-9
`

type testEnv struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	eng      *engine.Engine
	socket   string
	secrets  map[string]string
	stateDir string
	project  int64
	client   *http.Client

	handler http.Handler
	srv     *httptest.Server
}

func newTestEnv(t *testing.T, engCfg engine.Config) *testEnv {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tmp, "store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tokenFile := filepath.Join(tmp, "tokens")
	if err := os.WriteFile(tokenFile, []byte(e2eTokens), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	eng := engine.New(st, engCfg)
	srv, err := api.New(ctx, st, eng, nil, tokenFile)
	if err != nil {
		t.Fatalf("build api server: %v", err)
	}
	e := &testEnv{
		t: t, ctx: ctx, st: st, eng: eng,
		secrets:  map[string]string{"root": "root-secret-9", "r1": "r1-secret-9", "r2": "r2-secret-9"},
		stateDir: filepath.Join(tmp, "runner-state"),
	}
	// Not under t.TempDir(): its name carries the test's, and on the
	// platforms with long temporary directories that pushes a socket
	// path past the kernel's sun_path cap.
	e.socket = testsupport.ShortSocketPath(t)
	e.handler = srv.Handler()
	e.startAPI()
	t.Cleanup(e.stopAPI)
	e.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.socket)
		},
	}}
	t.Cleanup(e.client.CloseIdleConnections)
	return e
}

// startAPI serves the API on the env's socket. It is separate from
// newTestEnv so a test can blink the socket away and back: the engine
// and its store live on, only the listener dies, which is what a
// restarting engine looks like to a runner.
func (e *testEnv) startAPI() {
	e.t.Helper()
	_ = os.Remove(e.socket)
	l, err := net.Listen("unix", e.socket)
	if err != nil {
		e.t.Fatalf("listen: %v", err)
	}
	hs := httptest.NewUnstartedServer(e.handler)
	hs.Listener = l
	hs.Start()
	e.srv = hs
}

// stopAPI takes the listener down; the socket file it leaves is
// removed by the next startAPI.
func (e *testEnv) stopAPI() {
	if e.srv != nil {
		e.srv.Close()
		e.srv = nil
	}
}

// seedProject makes a project over the given repo URL, the task's
// remote, plus its plan.
func (e *testEnv) seedProject(repoURL string) {
	e.t.Helper()
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (?, 'main', '{}', 'claude', 'sonnet', ?, ?)`, repoURL, e2eSeedTS, e2eSeedTS)
		if err != nil {
			return err
		}
		e.project, err = res.LastInsertId()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (?, 'a plan', 'active', ?, ?)`, e.project, e2eSeedTS, e2eSeedTS)
		return err
	})
	if err != nil {
		e.t.Fatalf("seed project: %v", err)
	}
}

// seedTask adds one ready task to the project with its check command.
// An empty repoURL means the project's repo (§6.1).
func (e *testEnv) seedTask(repoURL, checkCmd string) int64 {
	e.t.Helper()
	var task int64
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, repo_url, base_ref, caps, created_at, updated_at)
			VALUES ((SELECT id FROM plans WHERE project_id = ?), 'a task', 'the task spec', ?, 'implement', 'ready', NULLIF(?, ''), 'main', '{}', ?, ?)`,
			e.project, checkCmd, repoURL, e2eSeedTS, e2eSeedTS)
		if err != nil {
			return err
		}
		task, err = res.LastInsertId()
		return err
	})
	if err != nil {
		e.t.Fatalf("seed task: %v", err)
	}
	return task
}

// submitWork queues a work job for the task, as the admin submit does
// (§12.1), with the worker prompt the engine composed.
func (e *testEnv) submitWork(task int64, prompt string) int64 {
	e.t.Helper()
	body := fmt.Sprintf(`{"key":"task:%d","project_id":%d,"task_id":%d,"kind":"work","mode":"fresh","prompt":%q}`,
		task, e.project, task, prompt)
	var out struct {
		Answer string `json:"answer"`
		JobID  int64  `json:"job_id"`
	}
	e.doJSON(http.MethodPost, "/v1/jobs", "root", body, &out)
	if out.Answer != "created" {
		e.t.Fatalf("submit work: answered %q", out.Answer)
	}
	return out.JobID
}

// cancelJob stops a live job through the client API (§12.1).
func (e *testEnv) cancelJob(job int64) {
	e.t.Helper()
	var out struct {
		Answer string `json:"answer"`
	}
	e.doJSON(http.MethodPost, fmt.Sprintf("/v1/jobs/%d/cancel", job), "root", "", &out)
	if out.Answer != "ok" && out.Answer != "already" {
		e.t.Fatalf("cancel job %d: answered %q", job, out.Answer)
	}
}

func (e *testEnv) doJSON(method, path, token, body string, out any) {
	e.t.Helper()
	req, err := http.NewRequest(method, "http://brigada"+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+e.secrets[token])
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
}

// runnerProc is one Run loop in flight.
type runnerProc struct {
	cancel context.CancelFunc
	done   chan error
}

func (p *runnerProc) stop(t *testing.T) {
	t.Helper()
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner loop did not stop")
	}
}

// startRunner runs Run against the env with the fake adapter driving
// script, at the given heartbeat cadence. Each adjust func, if any, is
// applied to the Config before the loop starts.
func (e *testEnv) startRunner(script string, heartbeatEvery time.Duration, adjust ...func(*Config)) *runnerProc {
	e.t.Helper()
	ad := &fake.Adapter{Script: script}
	ctx, cancel := context.WithCancel(context.Background())
	p := &runnerProc{cancel: cancel, done: make(chan error, 1)}
	go func() {
		cfg := Config{
			Socket:         e.socket,
			Token:          e.secrets["r1"],
			StateDir:       e.stateDir,
			Agents:         []api.Agent{{Adapter: "fake", Harness: "fake", Model: "fake-1", Family: "testfam", Account: "acct-1", Classes: []string{"implement", "review"}}},
			Adapters:       []adapter.Adapter{ad},
			Labels:         []string{"test"},
			HeartbeatEvery: heartbeatEvery,
			LeaseWait:      300 * time.Millisecond,
			CheckTimeout:   10 * time.Second,
			Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		for _, fn := range adjust {
			fn(&cfg)
		}
		p.done <- Run(ctx, cfg)
	}()
	return p
}

// waitJob polls the job until cond holds, failing with the last state.
func (e *testEnv) waitJob(job int64, cond func(*engine.JobInfo) bool) *engine.JobInfo {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobInfo, err := e.eng.Job(e.ctx, job)
		if err != nil {
			e.t.Fatalf("read job %d: %v", job, err)
		}
		if jobInfo != nil && cond(jobInfo) {
			return jobInfo
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %d never reached the expected state; last: %+v result=%s check=%s",
				job, jobInfo, jobInfo.Result, jobInfo.CheckResult)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// waitUntil polls cond until it holds or the deadline passes.
func (e *testEnv) waitUntil(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// int64Of reads one integer column from the store.
func (e *testEnv) int64Of(query string, args ...any) int64 {
	e.t.Helper()
	var v int64
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&v)
	})
	if err != nil {
		e.t.Fatalf("query %q %v: %v", query, args, err)
	}
	return v
}

// get performs one GET with a token and returns the body, failing on
// anything but 200.
func (e *testEnv) get(path, token string) []byte {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://brigada"+path, nil)
	if err != nil {
		e.t.Fatalf("get %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+e.secrets[token])
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("get %s: read body: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("get %s: %d %s", path, resp.StatusCode, data)
	}
	return data
}

// workerResult unwraps the WorkerResult the runner completed with,
// which the engine stores as a JSON string inside the job's result.
func (e *testEnv) workerResult(job *engine.JobInfo) WorkerResult {
	e.t.Helper()
	var outer struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(job.Result, &outer); err != nil {
		e.t.Fatalf("job result %s: %v", job.Result, err)
	}
	var res WorkerResult
	if err := json.Unmarshal([]byte(outer.Result), &res); err != nil {
		e.t.Fatalf("worker result %q: %v", outer.Result, err)
	}
	return res
}

// errorResult reads a completion delivered as an error (§12.3): the
// job's result is {"error": "..."}.
func (e *testEnv) errorResult(job *engine.JobInfo) string {
	e.t.Helper()
	var outer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(job.Result, &outer); err != nil {
		e.t.Fatalf("job result %s: %v", job.Result, err)
	}
	return outer.Error
}

func (e *testEnv) checkResult(job *engine.JobInfo) CheckResult {
	e.t.Helper()
	var res CheckResult
	if err := json.Unmarshal(job.CheckResult, &res); err != nil {
		e.t.Fatalf("check result %s: %v", job.CheckResult, err)
	}
	return res
}

// testArtifact is one artifact row as the runner's tests read it.
type testArtifact struct {
	Kind   string
	Path   string
	Size   int64
	SHA256 string
}

// artifacts reads the job's artifact rows from the store.
func (e *testEnv) artifacts(job int64) map[string]testArtifact {
	e.t.Helper()
	out := map[string]testArtifact{}
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT kind, path, size, sha256 FROM artifacts WHERE job_id = ?", job)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a testArtifact
			if err := rows.Scan(&a.Kind, &a.Path, &a.Size, &a.SHA256); err != nil {
				return err
			}
			out[a.Kind] = a
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatalf("read artifacts: %v", err)
	}
	return out
}

// assertNoWorktrees verifies the workspace came down (§5.1): no
// worktree directory left, or none was ever needed.
func (e *testEnv) assertNoWorktrees() {
	e.t.Helper()
	e.waitUntil("the worktrees to be cleaned up", func() bool {
		entries, err := os.ReadDir(filepath.Join(e.stateDir, "worktrees"))
		return os.IsNotExist(err) || (err == nil && len(entries) == 0)
	})
}

// writeWorkerScript writes one scripted fake CLI.
func writeWorkerScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write worker script: %v", err)
	}
	return path
}

// remoteBranchCommit returns the commit the remote holds for branch, or
// "" when it does not have it.
func remoteBranchCommit(t *testing.T, remote, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "ls-remote", remote, "refs/heads/"+branch).Output()
	if err != nil {
		t.Fatalf("ls-remote %s: %v", remote, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// The scripted workers. They run in the worktree, emit the fake
// adapter's event stream, and end with a final message. The result
// block rides unfenced at the end of the message — the parser takes
// both forms, and printf keeps the shell honest about quotes.
const happyWorker = `
cat > /dev/null
# a banner the adapter must shrug off: not an event, not an error
echo "fake-cli 1.2.3 — starting session"
echo '{"type":"system_init","session_id":"sess-42"}'
# a word of progress before the pause, so the runner's heartbeats
# carry a last_progress_at that is real, not just the start
echo '{"type":"assistant","text":"reading the task"}'
sleep 0.4
echo '{"type":"tool_start","name":"Edit"}'
echo hello > work.txt
git add -A
git commit -qm 'fake work: add work.txt'
echo '{"type":"tool_end","name":"Edit"}'
br=$(git rev-parse --abbrev-ref HEAD)
printf '{"type":"result","text":"added work.txt: {\\"status\\":\\"done\\",\\"summary\\":\\"added work.txt\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
`

const pushingWorker = `
cat > /dev/null
echo '{"type":"tool_start","name":"Edit"}'
echo pushed > work.txt
git add -A
git commit -qm 'fake work: add work.txt'
git push -q origin HEAD
echo '{"type":"tool_end","name":"Edit"}'
br=$(git rev-parse --abbrev-ref HEAD)
printf '{"type":"result","text":"done and pushed: {\\"status\\":\\"done\\",\\"summary\\":\\"pushed work.txt\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
`

const noResultWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"thinking hard about it"}'
echo '{"type":"result","text":"I finished everything, trust me"}'
`

// slowWorker runs for ~20 s and only then reports done; the markers say
// whether it ever started and whether it ran to the end — a killed
// worker leaves no finished marker.
const slowWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"starting"}'
: > "$BRIGADA_TEST_MARKERS/started"
i=0
while [ $i -lt 200 ]; do sleep 0.1; i=$((i+1)); done
echo '{"type":"result","text":"{\\"status\\":\\"done\\",\\"summary\\":\\"finally done\\"}"}'
: > "$BRIGADA_TEST_MARKERS/finished"
`

func markerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIGADA_TEST_MARKERS", dir)
	return dir
}

func TestRunnerHappyPath(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", ""))
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, happyWorker), 100*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })

	// The result contract came through: done, with the branch named.
	res := e.workerResult(done)
	if res.Status != "done" || res.Summary != "added work.txt" || res.Branch != taskBranch(task, 1) {
		t.Errorf("worker result = %+v, want done with the worktree's branch", res)
	}

	// The branch is on the remote and holds the worker's commit (§5.4) —
	// the runner pushed it, the worker did not.
	commit := remoteBranchCommit(t, remote, taskBranch(task, 1))
	if commit == "" {
		t.Fatalf("branch %s was never pushed to the remote", taskBranch(task, 1))
	}
	clone := t.TempDir()
	git(t, "", "clone", "-q", "--branch", taskBranch(task, 1), remote, clone)
	if data, err := os.ReadFile(filepath.Join(clone, "work.txt")); err != nil || string(data) != "hello\n" {
		t.Errorf("the pushed branch does not hold the work: %q (%v)", data, err)
	}

	// The check ran and passed (§5.5), and its result is the job's.
	check := e.checkResult(done)
	if check.ExitCode != 0 || check.TimedOut {
		t.Errorf("check result = %+v, want a pass", check)
	}

	// The artifacts: the transcript with the session id, the check log.
	arts := e.artifacts(job)
	for _, kind := range []string{"transcript", "check_log"} {
		a, ok := arts[kind]
		if !ok {
			t.Fatalf("no %s artifact for job %d (have %v)", kind, job, arts)
		}
		data, err := os.ReadFile(a.Path)
		if err != nil {
			t.Fatalf("read the %s artifact: %v", kind, err)
		}
		if int64(len(data)) != a.Size {
			t.Errorf("%s artifact size = %d, file is %d", kind, a.Size, len(data))
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != a.SHA256 {
			t.Errorf("%s artifact digest does not match its file", kind)
		}
	}
	if data, _ := os.ReadFile(arts["transcript"].Path); !strings.Contains(string(data), "sess-42") {
		t.Errorf("the transcript does not carry the session event:\n%s", data)
	}

	// The session id was written back for the next round (§4.2).
	var session string
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", fmt.Sprintf("task:%d", task)).Scan(&session)
	})
	if err != nil || session != "sess-42" {
		t.Errorf("session write-back = %q (%v), want sess-42", session, err)
	}

	// The heartbeats carried progress (§5.6).
	var progress string
	err = e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COALESCE(last_progress_at, '') FROM jobs WHERE id = ?", job).Scan(&progress)
	})
	if err != nil || progress == "" {
		t.Errorf("last_progress_at = %q (%v), want the heartbeats recorded", progress, err)
	}

	e.assertRegistration()
	e.assertNoWorktrees()
}

// TestRunnerWorkerPushesItself is the happy path where the worker does
// the pushing (§5.3: the template tells it to) and the runner's
// ls-remote check finds the branch already there.
func TestRunnerWorkerPushesItself(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", `test -f work.txt`)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", ""))
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, pushingWorker), 100*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.workerResult(done)
	if res.Status != "done" || res.Branch != taskBranch(task, 1) {
		t.Errorf("worker result = %+v, want done on %s", res, taskBranch(task, 1))
	}
	if remoteBranchCommit(t, remote, taskBranch(task, 1)) == "" {
		t.Error("the worker's push is not on the remote")
	}
	if check := e.checkResult(done); check.ExitCode != 0 {
		t.Errorf("check = %+v, want a pass", check)
	}
	e.assertNoWorktrees()
}

func TestRunnerNoResultBlock(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Do something.", ""))
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, noResultWorker), 100*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.workerResult(done)
	if res.Status != "failed" || res.Reason != reasonNoResult {
		t.Errorf("worker result = %+v, want failed/no-result (§5.4)", res)
	}
	if !strings.Contains(res.Summary, "trust me") {
		t.Errorf("the no-result summary should name what the worker said: %+v", res)
	}
	e.assertNoWorktrees()
}

func TestRunnerCheckFailed(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "echo check-boom; exit 3")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", ""))
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, happyWorker), 100*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	// A failed check makes the job's outcome check_failed, not done
	// (§5.5).
	res := e.workerResult(done)
	if res.Status != statusCheckFail {
		t.Errorf("worker result = %+v, want status %q", res, statusCheckFail)
	}
	check := e.checkResult(done)
	if check.ExitCode != 3 || !strings.Contains(check.Output, "check-boom") {
		t.Errorf("check result = %+v, want exit 3 with the output kept", check)
	}
	if _, ok := e.artifacts(job)["check_log"]; !ok {
		t.Error("a failed check left no check log artifact")
	}
	e.assertNoWorktrees()
}

func TestRunnerHeartbeatCancelledKillsWorker(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, slowWorker), 100*time.Millisecond)
	defer p.stop(t)

	e.waitUntil("the worker to start", func() bool {
		_, err := os.Stat(filepath.Join(markers, "started"))
		return err == nil
	})
	e.cancelJob(job)
	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "cancelled" })

	// The runner gave the job up — its worktree came down — and with it
	// the worker's process group (§12.3): the run never reached its end.
	e.assertNoWorktrees()
	if _, err := os.Stat(filepath.Join(markers, "finished")); err == nil {
		t.Fatal("the cancelled worker ran to the end; it was not killed")
	}
	// And nothing was completed for it.
	if len(e.artifacts(job)) != 0 {
		t.Errorf("a cancelled run delivered artifacts: %v", e.artifacts(job))
	}
}

func TestRunnerLeaseLostMidRun(t *testing.T) {
	remote := newRemote(t)
	// A lease shorter than the runner's heartbeat cadence: the lease
	// lapses between beats, a second runner takes the job, and our
	// runner's next heartbeat answers lost (§12.3).
	e := newTestEnv(t, engine.Config{LeaseTTL: 300 * time.Millisecond})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, slowWorker), 600*time.Millisecond)
	defer p.stop(t)

	e.waitUntil("the worker to start", func() bool {
		_, err := os.Stat(filepath.Join(markers, "started"))
		return err == nil
	})

	// The second runner long-polls until the lapse hands it the job.
	r2 := api.NewClient(e.socket, e.secrets["r2"])
	if err := r2.Register(e.ctx, nil, 0); err != nil {
		t.Fatalf("register r2: %v", err)
	}
	grant, err := r2.Lease(e.ctx, 5*time.Second, nil, nil)
	if err != nil || grant == nil {
		t.Fatalf("r2 never took over the job: grant=%v err=%v", grant, err)
	}
	if grant.JobID != job {
		t.Fatalf("r2 leased job %d, want %d", grant.JobID, job)
	}

	// Our runner kills its worker, cleans up, and never completes.
	e.assertNoWorktrees()
	if _, err := os.Stat(filepath.Join(markers, "finished")); err == nil {
		t.Fatal("the worker whose lease was lost ran to the end; it was not killed")
	}
	e.cancelJob(job)
	final := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "cancelled" })
	if len(final.Result) != 0 {
		t.Errorf("the runner that lost the lease delivered a result: %s", final.Result)
	}
	if len(e.artifacts(job)) != 0 {
		t.Errorf("the run that lost its lease delivered artifacts: %v", e.artifacts(job))
	}
}

func TestRunnerRejectsRepoWithoutURL(t *testing.T) {
	e := newTestEnv(t, engine.Config{})
	e.seedProject("") // a project with no repo URL
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Anything.", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, slowWorker), 100*time.Millisecond)
	defer p.stop(t)

	failed := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "failed" })
	if msg := e.errorResult(failed); !strings.Contains(msg, "no repo URL") {
		t.Errorf("failure = %q, want it to name the missing repo URL", msg)
	}
	// The worker never ran (product §5 rule 6).
	if _, err := os.Stat(filepath.Join(markers, "started")); err == nil {
		t.Error("the worker ran for a task with no repo URL")
	}
	e.assertNoWorktrees()
}

// assertRegistration verifies the advertisement was persisted (§5.1):
// one agents row for the runner, keyed by runner, adapter and model.
func (e *testEnv) assertRegistration() {
	e.t.Helper()
	var adapterName, harness, model, family, account, classes string
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT a.adapter, a.harness, a.model, a.family, a.account, a.classes
			FROM agents a JOIN runners r ON r.id = a.runner_id WHERE r.name = 'r1'`).Scan(
			&adapterName, &harness, &model, &family, &account, &classes)
	})
	if err != nil {
		e.t.Fatalf("read the registered agent: %v", err)
	}
	if adapterName != "fake" || harness != "fake" || model != "fake-1" ||
		family != "testfam" || account != "acct-1" || !strings.Contains(classes, "implement") {
		e.t.Errorf("registered agent = %s/%s/%s %s %s %s, want the fake advertisement",
			adapterName, harness, model, family, account, classes)
	}
}

// TestRunnerStderrOnlyFailure covers the stderr pipeline (§5.2): a CLI
// that reports its failure only on stderr — here a resume that cannot
// be found, printed before any JSON — still fails the run as that
// error, and the line rides the uploaded transcript.
func TestRunnerStderrOnlyFailure(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Do something.", ""))
	markerDir(t)

	worker := `
cat > /dev/null
echo "error: resume session sess-gone not found" >&2
exit 1
`
	p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond)
	defer p.stop(t)

	failed := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "failed" })
	msg := e.errorResult(failed)
	if !strings.Contains(msg, "resume_failed") || !strings.Contains(msg, "not found") {
		t.Errorf("failure = %q, want the stderr-only resume failure named", msg)
	}

	// The transcript carries the stderr line, tagged as such (§9).
	arts := e.artifacts(job)
	a, ok := arts["transcript"]
	if !ok {
		t.Fatalf("no transcript artifact for job %d (have %v)", job, arts)
	}
	data, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatalf("read the transcript artifact: %v", err)
	}
	if !strings.Contains(string(data), "stderr: error: resume session sess-gone not found") {
		t.Errorf("the transcript does not carry the stderr line:\n%s", data)
	}
	e.assertNoWorktrees()
}

// TestRunnerStderrErrorWeighsAgainstTheExit covers the weighing (§5.2,
// §5.6): stderr carries the CLI's warnings beside its failures, so an
// error sorted from it fails the run only when the CLI did not end
// clean — a warning beside a good result never does.
func TestRunnerStderrErrorWeighsAgainstTheExit(t *testing.T) {
	t.Run("an error beside a clean exit and a good result", func(t *testing.T) {
		remote := newRemote(t)
		e := newTestEnv(t, engine.Config{})
		e.seedProject(remote)
		task := e.seedTask("", `test "$(cat work.txt)" = hello`)
		job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", ""))
		markerDir(t)

		worker := `
cat > /dev/null
echo '{"type":"error","kind":"auth","message":"MCP server github requires authentication"}' >&2
echo hello > work.txt
git add -A
git commit -qm 'fake work: add work.txt'
printf '{"type":"result","text":"done amid warnings: {\\"status\\":\\"done\\",\\"summary\\":\\"added work.txt\\"}"}\n'
`
		p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond)
		defer p.stop(t)

		done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
		res := e.workerResult(done)
		if res.Status != "done" {
			t.Errorf("worker result = %+v, want done: the stderr warning must not fail a clean run", res)
		}
		e.assertNoWorktrees()
	})
	t.Run("an error against a non-zero exit", func(t *testing.T) {
		remote := newRemote(t)
		e := newTestEnv(t, engine.Config{})
		e.seedProject(remote)
		task := e.seedTask("", "true")
		job := e.submitWork(task, prompt.ComposeWorkerPrompt("Do something.", ""))
		markerDir(t)

		worker := `
cat > /dev/null
echo '{"type":"error","kind":"auth","message":"invalid API key provided"}' >&2
exit 1
`
		p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond)
		defer p.stop(t)

		failed := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "failed" })
		msg := e.errorResult(failed)
		if !strings.Contains(msg, "auth") || !strings.Contains(msg, "invalid API key") {
			t.Errorf("failure = %q, want the stderr error against the non-clean exit", msg)
		}
		e.assertNoWorktrees()
	})
}

// TestRunnerHistoryEndToEnd is the whole story on one page (§9): a fake
// worker run uploads its transcript and check log as artifacts —
// content-addressed on the engine's own disk — and the history reads
// the job's timeline: events and messages merged, oldest first.
func TestRunnerHistoryEndToEnd(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", ""))
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, happyWorker), 100*time.Millisecond)
	defer p.stop(t)

	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })

	// The artifacts live in the engine's data directory, addressed by
	// their content.
	arts := e.artifacts(job)
	for _, kind := range []string{"transcript", "check_log"} {
		a, ok := arts[kind]
		if !ok {
			t.Fatalf("no %s artifact for job %d (have %v)", kind, job, arts)
		}
		if !strings.Contains(a.Path, filepath.Join("artifacts", a.SHA256[:2], a.SHA256)) {
			t.Errorf("the %s artifact's path = %q, want the content-addressed layout", kind, a.Path)
		}
		if _, err := os.Stat(a.Path); err != nil {
			t.Errorf("the %s blob is not on the engine's disk: %v", kind, err)
		}
	}
	transcriptID := e.int64Of("SELECT id FROM artifacts WHERE job_id = ? AND kind = 'transcript'", job)
	if got := e.int64Of("SELECT COALESCE(transcript_id, 0) FROM jobs WHERE id = ?", job); got != transcriptID {
		t.Errorf("the job's transcript ref = %d, want the uploaded artifact %d", got, transcriptID)
	}

	// The history shows the job's whole timeline, oldest first: the run
	// itself, then the review the completion queued (§7) — its created
	// event names the review job, not the work job's id.
	var page struct {
		Items []struct {
			Type    string `json:"type"`
			Kind    string `json:"kind"`
			JobID   int64  `json:"job_id"`
			Author  string `json:"author"`
			Content string `json:"content"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	e.doJSON(http.MethodGet, fmt.Sprintf("/v1/history?task=%d&limit=50", task), "root", "", &page)
	var story []string
	for _, it := range page.Items {
		story = append(story, it.Type+":"+it.Kind)
	}
	want := []string{
		"event:job_created", "message:task_spec",
		"event:job_leased", "event:task_running",
		"event:job_completed", "event:task_reviewing", "event:job_created",
		"message:worker_result", "message:task_spec",
	}
	if strings.Join(story, " ") != strings.Join(want, " ") {
		t.Errorf("the task's timeline:\n got %v\nwant %v", story, want)
	}
	reviewJob := e.int64Of("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", task)
	for _, it := range page.Items {
		if it.JobID != 0 && it.JobID != job && it.JobID != reviewJob {
			t.Errorf("item %s names job %d, want %d or its review %d", it.Kind, it.JobID, job, reviewJob)
		}
	}
	if len(page.Items) < 2 || page.Items[1].Author != "engine" || page.Items[1].Content != "the task spec" {
		t.Errorf("the task_spec message = %+v, want the seeded spec from the engine", page.Items[1])
	}
	// The worker's own words are in the story: the done block of §5.4.
	var workerResult string
	var haveWorkerResult bool
	for _, it := range page.Items {
		if it.Kind == "worker_result" {
			workerResult, haveWorkerResult = it.Content, it.Author == "worker"
		}
	}
	if !haveWorkerResult || !strings.Contains(workerResult, `"status":"done"`) {
		t.Errorf("the worker_result message = %q (%t), want the worker's done block", workerResult, haveWorkerResult)
	}

	// The artifacts endpoint streams the blob the path names.
	transcript := e.get("/v1/artifacts/"+fmt.Sprint(transcriptID), "root")
	if !strings.Contains(string(transcript), "sess-42") {
		t.Errorf("the streamed transcript does not carry the session event:\n%s", transcript)
	}
}

// TestRunnerLongCheckKeepsLease covers the fix for a check that
// outlives several lease TTLs: the heartbeats span the push, the check
// and the delivery, so a slow check does not lapse the lease and turn
// finished work into a lost answer (§12.3). The lease here is shorter
// than the check by design.
func TestRunnerLongCheckKeepsLease(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{LeaseTTL: 800 * time.Millisecond})
	e.seedProject(remote)
	// A check that crosses several lease windows, then passes.
	task := e.seedTask("", "sleep 1.5 && echo check-ok")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", ""))
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, happyWorker), 150*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.workerResult(done)
	if res.Status != "done" {
		t.Errorf("worker result = %+v, want done: the lease lapsed during the check", res)
	}
	if check := e.checkResult(done); check.ExitCode != 0 || !strings.Contains(check.Output, "check-ok") {
		t.Errorf("check result = %+v, want the slow check to have run to its end", check)
	}
	e.assertNoWorktrees()
}

// TestRunnerCancelDuringCheckKillsCheck covers a cancel that lands
// while the runner itself is running the check (§5.5): the heartbeat
// answers cancelled, the check's process group is killed — not left to
// finish — and nothing is completed.
func TestRunnerCancelDuringCheckKillsCheck(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	check := `touch "$BRIGADA_TEST_MARKERS/check-started" && sleep 5 && touch "$BRIGADA_TEST_MARKERS/check-finished" && exit 0`
	task := e.seedTask("", check)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", ""))

	p := e.startRunner(writeWorkerScript(t, happyWorker), 100*time.Millisecond)
	defer p.stop(t)

	// The worker finished; the runner is inside the check now.
	e.waitUntil("the check to start", func() bool {
		_, err := os.Stat(filepath.Join(markers, "check-started"))
		return err == nil
	})
	e.cancelJob(job)
	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "cancelled" })

	// The check was killed with the run: it never reached its end, and
	// nothing was completed for the job.
	e.assertNoWorktrees()
	if _, err := os.Stat(filepath.Join(markers, "check-finished")); err == nil {
		t.Fatal("the cancelled check ran to the end; it was not killed")
	}
	final, err := e.eng.Job(e.ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Result) != 0 {
		t.Errorf("a run cancelled during its check delivered a result: %s", final.Result)
	}
	// The transcript the worker's session already earned stays (§9: it
	// is uploaded after every job); the check log of a killed check —
	// and any completion — must not join it.
	arts := e.artifacts(job)
	if _, ok := arts["transcript"]; !ok {
		t.Errorf("the cancelled run's transcript was not kept: %v", arts)
	}
	if _, ok := arts["check_log"]; ok {
		t.Errorf("the cancelled check delivered a log: %v", arts)
	}
}

// TestRunnerSurvivesSocketBlink covers the loop's answer to an engine
// that goes away and comes back: a registration or lease failure costs
// a backoff and a re-register, not the runner's life. The engine and
// its store live on; only the socket blinks, which is what a restart
// looks like from outside.
func TestRunnerSurvivesSocketBlink(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	markerDir(t)

	// The runner starts with the socket already down: its first
	// registration fails and must be retried, not returned.
	e.stopAPI()
	p := e.startRunner(writeWorkerScript(t, happyWorker), 100*time.Millisecond)
	defer p.stop(t)

	e.startAPI() // back within the first backoff window
	// Let the runner register and settle into an empty long-poll…
	time.Sleep(300 * time.Millisecond)
	// …then blink the socket mid-poll: the in-flight lease fails, the
	// next round re-registers (the engine may have a fresh store) and
	// polls again.
	e.stopAPI()
	time.Sleep(150 * time.Millisecond)
	e.startAPI()

	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", ""))
	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.workerResult(done); res.Status != "done" {
		t.Errorf("worker result = %+v, want done after the blink", res)
	}
	e.assertNoWorktrees()
	// The runner loop is still alive: p.done has nothing to say.
	select {
	case err := <-p.done:
		t.Fatalf("the runner died on the blink: %v", err)
	default:
	}
}

// TestRunnerScrubsGitEnvFromWorker covers the credential boundary
// (§5.1): the variables the runner's own git credentials ride
// (GitEnv) reach the runner's pushes but never the worker's
// environment.
func TestRunnerScrubsGitEnvFromWorker(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	markers := markerDir(t)

	// The worker fails loudly if it ever sees the credential variable.
	worker := `
if [ "$BRIGADA_TEST_GITCRED" != "" ]; then
	echo leak > "$BRIGADA_TEST_MARKERS/leaked"
	exit 9
fi
cat > /dev/null
echo '{"type":"tool_start","name":"Edit"}'
echo scrubbed > work.txt
git add -A
git commit -qm 'fake work'
echo '{"type":"tool_end","name":"Edit"}'
br=$(git rev-parse --abbrev-ref HEAD)
printf '{"type":"result","text":"done: {\\"status\\":\\"done\\",\\"summary\\":\\"edited\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
`
	p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond,
		func(c *Config) { c.GitEnv = []string{"BRIGADA_TEST_GITCRED=hush-hush"} })
	defer p.stop(t)

	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", ""))
	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.workerResult(done); res.Status != "done" {
		t.Errorf("worker result = %+v, want done: the credential scrub broke the run", res)
	}
	if _, err := os.Stat(filepath.Join(markers, "leaked")); err == nil {
		t.Fatal("the runner's git credential variable reached the worker")
	}
	// The branch made it to the remote, so the runner's own git — with
	// the credential in its environment — kept working.
	if remoteBranchCommit(t, remote, taskBranch(task, 1)) == "" {
		t.Error("the branch was never pushed")
	}
	e.assertNoWorktrees()
}
