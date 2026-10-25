package runner

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The foreman turn tests run against the same machinery as the worker
// ones: the engine, its API and the fake adapter, with the project's
// foreman pointed at the fake agent — the columns `brigada project
// foreman` will set once it exists.

// setForeman points the project's foreman at a harness and model.
func (e *testEnv) setForeman(harness, model string) {
	e.t.Helper()
	// Older fixtures seed no URL; every foreman now needs code to read.
	repo := newRemote(e.t)
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE projects SET foreman_harness = ?, foreman_model = ?, repo_url = COALESCE(NULLIF(repo_url, ''), ?) WHERE id = ?",
			harness, model, repo, e.project)
		return err
	})
	if err != nil {
		e.t.Fatalf("set the project's foreman: %v", err)
	}
}

// submitForemanTurn wakes the project's foreman with the events, as
// the engine will for a judgment event (§4.2 item 2).
func (e *testEnv) submitForemanTurn(events ...string) engine.SubmitResult {
	e.t.Helper()
	res, err := e.eng.SubmitForemanTurn(e.ctx, e.project, events)
	if err != nil {
		e.t.Fatalf("submit foreman turn: %v", err)
	}
	return res
}

// foremanScratch is the project's scratch directory under the runner's
// state (§4), where the scripted foreman CLI leaves what it wants
// kept: its prompt, its resume id, its whereabouts.
func (e *testEnv) foremanScratch() string {
	e.t.Helper()
	return filepath.Join(e.stateDir, "foreman", strconv.FormatInt(e.project, 10))
}

// foremanCheckout is the reading checkout under the scratch directory,
// the working directory of the foreman's CLI.
func (e *testEnv) foremanCheckout() string {
	return filepath.Join(e.foremanScratch(), "repo")
}

// scratchFile reads one file the turn left in its working directory,
// the checkout (a refresh removes untracked files, so a file read is
// the latest turn's).
func (e *testEnv) scratchFile(name string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.foremanCheckout(), name))
	if err != nil {
		e.t.Fatalf("read %s from the turn's working directory: %v", name, err)
	}
	return string(data)
}

// The scripted foreman CLI: it keeps its prompt, names where it ran,
// reports the resume id it was handed, and ends with a plain final
// message — no result block, which a foreman turn owes no one (§5.4).
// turnWorker waits for the release marker before it ends, so a test
// can hold the turn open; a turnWorker that finds the marker already
// there runs straight through.
const turnWorker = `
cat > prompt.txt
pwd > where.txt
printf '%s\n' "$BRIGADA_FAKE_RESUME" > resume.txt
echo '{"type":"system_init","session_id":"foreman-sess-1"}'
echo '{"type":"assistant","text":"reading the events"}'
` + testsupport.AwaitRelease + `
echo '{"type":"result","text":"answered the worker; the plan stands"}'
`

// TestRunnerForemanTurnRunsInScratchDir is the turn's happy path (§4,
// §4.2 item 2, §5.4): the foreman wakes headless, runs in the project's
// scratch directory, with a reading repo but no task worktree or check.
// A clean exit without any result block completes the job as done with the final
// message as its summary.
func TestRunnerForemanTurnRunsInScratchDir(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	remote := newRemote(t)
	git(t, "", "--git-dir", strings.TrimPrefix(remote, "file://"), "branch", "project-default", "main")
	e.seedProject(remote)
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE projects SET default_branch = 'project-default' WHERE id = ?", e.project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.setForeman("fake", "fake-1")
	turn := e.submitForemanTurn("the worker of task 7 asked: should I split the CLI from the parser?")
	markers := markerDir(t)
	// The scripted turn ends at once: its wait marker is already there.
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatalf("release the turn's wait: %v", err)
	}

	p := e.startRunner(writeWorkerScript(t, turnWorker), 100*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })

	// The exemption held: no result block, yet done, with the final
	// message as the summary; and no check ran to run one against.
	res := e.workerResult(done)
	if res.Status != "done" || res.Summary != "answered the worker; the plan stands" {
		t.Errorf("turn result = %+v, want done with the final message as its summary", res)
	}
	if len(done.CheckResult) != 0 {
		t.Errorf("the turn carried a check result: %s", done.CheckResult)
	}

	// The turn ran in the project's scratch directory under the state
	// dir (§4), not in any worktree. The worker names its working
	// directory as the shell's pwd resolves it — on macOS /var is a
	// symlink to /private/var — so both sides are compared resolved.
	scratch, err := filepath.EvalSymlinks(e.foremanCheckout())
	if err != nil {
		t.Fatalf("resolve the checkout: %v", err)
	}
	where, err := filepath.EvalSymlinks(strings.TrimSpace(e.scratchFile("where.txt")))
	if err != nil {
		t.Fatalf("resolve the turn's working directory: %v", err)
	}
	if where != scratch {
		t.Errorf("the turn's working directory = %q, want the scratch %q", where, scratch)
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, "worktrees")); !os.IsNotExist(err) {
		t.Errorf("a foreman turn made a worktree: %v", err)
	}

	// Its prompt was the composed wake: the instructions prepended, the
	// event verbatim between its delimiters (§1.4).
	prompt := e.scratchFile("prompt.txt")
	if !strings.HasPrefix(prompt, foreman.Instructions()) {
		t.Errorf("the turn's prompt does not start with the foreman instructions")
	}
	if !strings.Contains(prompt, "<event>\nthe worker of task 7 asked: should I split the CLI from the parser?\n</event>") {
		t.Errorf("the wake event is not verbatim in the prompt:\n%s", prompt)
	}

	repoDir := filepath.Join(e.foremanScratch(), "repo")
	head := strings.TrimSpace(git(t, repoDir, "rev-parse", "HEAD"))
	wantHead := strings.TrimSpace(git(t, "", "--git-dir", strings.TrimPrefix(remote, "file://"), "rev-parse", "project-default"))
	if head != wantHead || strings.TrimSpace(git(t, repoDir, "branch", "--show-current")) != "project-default" {
		t.Fatalf("checkout is not at the project's default branch head: %s, want %s", head, wantHead)
	}
	for _, line := range []string{repoDir, `Branch: "project-default"`, head} {
		if !strings.Contains(prompt, line) {
			t.Errorf("prompt missing checkout metadata %q", line)
		}
	}

	// The key's first turn: nothing to resume.
	if resume := strings.TrimSpace(e.scratchFile("resume.txt")); resume != "" {
		t.Errorf("the first turn resumed %q; want no session id", resume)
	}

	// The transcript is the job's artifact, as for any job (§9).
	if _, ok := e.artifacts(turn.JobID)["transcript"]; !ok {
		t.Errorf("the turn left no transcript artifact: %v", e.artifacts(turn.JobID))
	}
}

// TestRunnerForemanTurnCoalescesAndResumes covers the session story of
// §4.2: while a turn runs, a second wake coalesces behind it — nothing
// is created, the live turn's follow-up flag is set — and the session
// id the first turn ended on is written back, so the next turn resumes
// the same conversation.
func TestRunnerForemanTurnCoalescesAndResumes(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	first := e.submitForemanTurn("first wake: a worker asked")
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, turnWorker), 100*time.Millisecond)
	defer p.stop(t)

	e.waitUntil("the turn to start", func() bool {
		_, err := os.Stat(filepath.Join(e.foremanCheckout(), "resume.txt"))
		return err == nil
	})

	// The second wake coalesces (§12.1): pending behind the live
	// turn, the flag set, and no second job.
	second := e.submitForemanTurn("second wake while the first runs")
	if second.Answer != engine.AnsPending || second.JobID != first.JobID {
		t.Fatalf("second wake = %+v; want pending behind job %d", second, first.JobID)
	}
	if n := e.int64Of("SELECT COUNT(*) FROM jobs WHERE kind = 'foreman'"); n != 1 {
		t.Errorf("foreman jobs = %d, want 1: the wake created nothing", n)
	}
	if n := e.int64Of("SELECT followup FROM jobs WHERE id = ?", first.JobID); n != 1 {
		t.Errorf("live turn followup = %d, want 1", n)
	}

	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatalf("release the turn: %v", err)
	}
	done := e.waitJob(first.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.workerResult(done); res.Status != "done" {
		t.Fatalf("first turn = %+v, want done", res)
	}

	// The session id was written back for the key (§4.2)…
	key := "foreman:" + strconv.FormatInt(e.project, 10)
	var session string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", key).Scan(&session)
	}); err != nil || session != "foreman-sess-1" {
		t.Errorf("session write-back = %q (%v), want foreman-sess-1", session, err)
	}

	// …and the next turn resumes it: a new job on the freed key, run 2,
	// handed the written-back id.
	third := e.submitForemanTurn("third wake after the first ended")
	if third.Answer != engine.AnsCreated || third.Run != 2 {
		t.Fatalf("wake after the key freed = %+v; want created run 2", third)
	}
	e.waitJob(third.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if resume := strings.TrimSpace(e.scratchFile("resume.txt")); resume != "foreman-sess-1" {
		t.Errorf("the second turn resumed %q, want foreman-sess-1", resume)
	}
}

// TestRunnerForemanTurnTimeCapped covers the policy paths a turn
// shares with any job (§5.6): the foreman class time cap applies, a
// capped turn is killed and completes stalled with the outcome in its
// result — and no branch to save on a stopped turn's way out. The
// worker speaks first thing, and the transcript is asserted only when
// it did: a turn the cap kills before its CLI said a word has an empty
// stream, and an empty stream is no transcript to upload — on a loaded
// machine the cap can win the race with the shell's own start, which
// is the runner's rule (nothing spoken, nothing recorded), not a lost
// transcript.
func TestRunnerForemanTurnTimeCapped(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	e.seedProject("") // setForeman supplies the reading repo
	e.setForeman("fake", "fake-1")
	turn := e.submitForemanTurn("a wake that outstays its welcome")
	markerDir(t)

	worker := `
echo '{"type":"system_init","session_id":"foreman-sess-4"}'
echo '{"type":"assistant","text":"thinking"}'
cat > /dev/null
` + testsupport.AwaitRelease + `
`
	p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond,
		func(c *Config) { c.ClassCaps = map[string]ClassCaps{"foreman": {TimeCap: 300 * time.Millisecond}} })
	defer p.stop(t)

	stalled := e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "stalled" })
	res := e.workerResult(stalled)
	if res.Status != statusTimeCap {
		t.Errorf("capped turn result = %+v, want status %q", res, statusTimeCap)
	}
	// The turn spoke — its progress events say so, and what the pipes
	// carried before the kill is always in the transcript — so the
	// transcript must be an artifact. A turn killed silent owes none.
	if len(res.Progress) > 0 {
		if _, ok := e.artifacts(turn.JobID)["transcript"]; !ok {
			t.Error("the capped turn left no transcript artifact")
		}
	}
}

// TestRunnerForemanTurnFails covers the other ending: a foreman CLI
// that exits non-zero fails the turn (§5.4's exemption cuts both ways —
// there is no result block to outweigh the exit), with the error kept.
func TestRunnerForemanTurnFails(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	turn := e.submitForemanTurn("a wake")
	markerDir(t)

	worker := `
cat > /dev/null
echo '{"type":"system_init","session_id":"foreman-sess-2"}'
echo "the CLI gave up mid-turn" >&2
exit 7
`
	p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond)
	defer p.stop(t)

	failed := e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "failed" })
	if msg := e.errorResult(failed); !strings.Contains(msg, "exit status 7") {
		t.Errorf("failure = %q, want the non-zero exit named", msg)
	}
	if _, ok := e.artifacts(turn.JobID)["transcript"]; !ok {
		t.Error("the failed turn left no transcript artifact")
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, "worktrees")); !os.IsNotExist(err) {
		t.Errorf("a failed turn made a worktree: %v", err)
	}
}

// TestRunnerForemanSpawnFailureIsInfrastructure covers the foreman's
// share of the spawn half of issue #39: a foreman CLI that cannot be
// started — a script that is not there — fails the turn as the infra
// kind before the CLI ran, and the agent is charged nothing.
func TestRunnerForemanSpawnFailureIsInfrastructure(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	remote := newRemote(t)
	e.seedProject(remote)
	e.setForeman("fake", "fake-1")

	p := e.startRunner(filepath.Join(t.TempDir(), "missing-turn.sh"), 100*time.Millisecond)
	defer p.stop(t)
	for i := 0; i < 2; i++ { // two failed turns would exhaust a streak of two
		turn := e.submitForemanTurn("a turn whose CLI is not there")
		failed := e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "failed" })
		if msg := e.errorResult(failed); !strings.Contains(msg, "start the foreman CLI") {
			t.Errorf("failure = %q, want the CLI's start named", msg)
		}
		if got := e.strOf("SELECT COALESCE(error_kind, '') FROM jobs WHERE id = ?", turn.JobID); got != engine.ErrorKindInfra {
			t.Errorf("error kind of turn %d = %q, want %q", turn.JobID, got, engine.ErrorKindInfra)
		}
	}
	if n := e.int64Of("SELECT COUNT(*) FROM agents WHERE health != 'ready'"); n != 0 {
		t.Errorf("%d agents were charged for a CLI that never started", n)
	}
}

// TestRunnerForemanLimitReachesTheEngine covers §4.4's first link: the
// kind of the CLI's error — and the reset time its event carried —
// rides the completion, so the engine marks the foreman limited.
func TestRunnerForemanLimitReachesTheEngine(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	turn := e.submitForemanTurn("a wake")
	markerDir(t)

	until := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	worker := `
cat > /dev/null
echo '{"type":"system_init","session_id":"foreman-sess-9"}'
echo '{"type":"error","kind":"limit","text":"You have hit your usage limit","until":"` + until.Format(time.RFC3339) + `"}'
exit 1
`
	p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond)
	defer p.stop(t)

	e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "failed" })
	var limited sql.NullString
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT foreman_limited FROM projects WHERE id = ?", e.project).Scan(&limited)
	}); err != nil {
		t.Fatalf("read the limited state: %v", err)
	}
	if !limited.Valid || limited.String == "unknown" {
		t.Fatalf("limited state = %+v, want the reset time the CLI named", limited)
	}
	got, err := time.Parse(engine.TimeLayout, limited.String)
	if err != nil || !got.Equal(until) {
		t.Errorf("limited until %q (%v), want %s", limited.String, err, until.Format(time.RFC3339))
	}
}

// fixedLeaseToken is the secret a test pins the engine's mint to, so
// the runner's handling of it can be checked byte for byte. Hex, the
// shape the default mint produces.
const fixedLeaseToken = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeed"

// mcpTurnWorker is turnWorker with the turn's plumbing recorded for
// the MCP config test: the config path the adapter was given, and the
// environment and command line the worker itself saw — where the
// lease-scoped token must not be.
//
// The test waits for mcpconfig.txt, then reads env.txt: so env.txt is
// written first, and each file appears whole (written aside, renamed
// into place), or a loaded run reads it missing or half written.
const mcpTurnWorker = `
cat > prompt.txt
env > env.tmp && mv env.tmp env.txt
printf '%s\n' "$BRIGADA_FAKE_MCP_CONFIG" > mcpconfig.tmp && mv mcpconfig.tmp mcpconfig.txt
echo '{"type":"system_init","session_id":"foreman-sess-5"}'
echo '{"type":"assistant","text":"checking my tools"}'
` + testsupport.AwaitRelease + `
echo '{"type":"result","text":"answered with the bridge"}'
`

// recordingAdapter wraps an adapter and keeps every command it built —
// argv and environment — so a test can prove what did and did not
// reach a command line the adapter built (§12.2).
type recordingAdapter struct {
	adapter.Adapter
	mu   sync.Mutex
	args [][]string
	envs [][]string
}

// Command implements adapter.Adapter by delegation, recording the build.
func (a *recordingAdapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	cmd, err := a.Adapter.Command(job, workdir, resumeSessionID)
	if err != nil {
		return cmd, err
	}
	a.mu.Lock()
	a.args = append(a.args, cmd.Args)
	a.envs = append(a.envs, cmd.Env)
	a.mu.Unlock()
	return cmd, nil
}

// carries reports whether any recorded argument or environment entry
// contains s — the shape of "the secret reached a command".
func (a *recordingAdapter) carries(s string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, args := range a.args {
		for _, arg := range args {
			if strings.Contains(arg, s) {
				return true
			}
		}
	}
	for _, env := range a.envs {
		for _, kv := range env {
			if strings.Contains(kv, s) {
				return true
			}
		}
	}
	return false
}

// TestRunnerForemanMCPConfig covers the bridge's file on the runner
// (§12.2): a foreman turn's MCP config is written before the worker
// starts — mode 0600, in a private 0700 directory under the runner's
// state, never in the scratch directory the worker runs in — names the
// runner's own binary as the bridge with the socket, and carries the
// lease-scoped token in the bridge's env alone: not in the command or
// its arguments, not on any command line the adapter built, not in
// anything the worker itself sees. Whatever ends the turn, the file is
// gone when it ends.
func TestRunnerForemanMCPConfig(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{NewLeaseToken: func() (string, error) { return fixedLeaseToken, nil }})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	turn := e.submitForemanTurn("a wake with tools")
	markers := markerDir(t)

	// The adapter the runner drives is wrapped, so the command it built
	// for the turn is kept for the token check below.
	rec := &recordingAdapter{}
	p := e.startRunner(writeWorkerScript(t, mcpTurnWorker), 100*time.Millisecond, func(c *Config) {
		rec.Adapter = c.Adapters[0]
		c.Adapters = []adapter.Adapter{rec}
	})
	defer p.stop(t)

	// The config exists while the turn runs; the worker names the path
	// the adapter was handed, under the runner's mcp root and in no
	// directory the worker runs in.
	e.waitUntil("the turn to record its MCP config", func() bool {
		_, err := os.Stat(filepath.Join(e.foremanCheckout(), "mcpconfig.txt"))
		return err == nil
	})
	config := strings.TrimSpace(e.scratchFile("mcpconfig.txt"))
	root := filepath.Join(e.stateDir, "mcp")
	if !strings.HasPrefix(config, root+string(filepath.Separator)) {
		t.Fatalf("the MCP config = %q, want it under %s", config, root)
	}
	if strings.HasPrefix(config, e.foremanScratch()) {
		t.Errorf("the MCP config lives under the scratch directory: %s", config)
	}

	// The file is 0600, and every directory it sits in is private.
	fi, err := os.Stat(config)
	if err != nil {
		t.Fatalf("stat the MCP config: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the MCP config's mode = %v, want 0600", fi.Mode().Perm())
	}
	for _, dir := range []string{filepath.Dir(config), root} {
		if fi, err = os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v (%v), want 0700", dir, fi.Mode().Perm(), err)
		}
	}

	// The config names the runner's own binary as the bridge over the
	// engine's socket, with the token in the bridge's env alone: the
	// command and every argument stay clean of it, so no command line
	// the config builds carries the secret.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test's own binary: %v", err)
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("read the MCP config: %v", err)
	}
	var cfg struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode the MCP config %s: %v", data, err)
	}
	srv, ok := cfg.Servers["brigada"]
	if !ok {
		t.Fatalf("config servers = %v, want one named brigada", cfg.Servers)
	}
	if srv.Command != self {
		t.Errorf("the bridge's command = %q, want the runner's own binary %q", srv.Command, self)
	}
	if want := []string{"mcp", "--socket", e.socket}; !slices.Equal(srv.Args, want) {
		t.Errorf("the bridge's args = %v, want %v", srv.Args, want)
	}
	if len(srv.Env) != 1 || srv.Env["BRIGADA_TOKEN"] != fixedLeaseToken {
		t.Errorf("the bridge's env = %v, want the lease token under BRIGADA_TOKEN alone", srv.Env)
	}
	ridesArgv := strings.Contains(srv.Command, fixedLeaseToken) ||
		slices.ContainsFunc(srv.Args, func(a string) bool { return strings.Contains(a, fixedLeaseToken) })
	if ridesArgv {
		t.Errorf("the token rides the bridge's command line: %s", data)
	}

	// No command the adapter built for the turn carries the token —
	// neither its arguments nor its environment (§12.2: the token is
	// the bridge's, passed by the file alone).
	if rec.carries(fixedLeaseToken) {
		t.Error("the lease token reached a command the adapter built")
	}

	// The worker itself saw neither the token nor any other brigada
	// secret: the token is the bridge's, passed by the file alone.
	if env := e.scratchFile("env.txt"); strings.Contains(env, fixedLeaseToken) {
		t.Errorf("the lease token reached the worker's environment:\n%s", env)
	}

	// The turn ends, and the file dies with it — the directory too.
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatalf("release the turn: %v", err)
	}
	e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Errorf("the MCP config outlived the turn: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(config)); !os.IsNotExist(err) {
		t.Errorf("the config directory outlived the turn: %v", err)
	}
}

// TestRunnerForemanMCPConfigFailsTheTurn covers the hook's own failure:
// a config that cannot be written is a turn without brigada's tools,
// and the engine hears why instead of a silent tool-less turn — with
// no debris of the failed attempt left behind.
func TestRunnerForemanMCPConfigFailsTheTurn(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root; this failure path needs a non-root user")
	}
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	turn := e.submitForemanTurn("a wake whose config cannot be written")
	markers := markerDir(t)
	// Should the turn ever start, it may end: the test wants the
	// failure, not a hang.
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatalf("release the turn's wait: %v", err)
	}

	// The configs' root, read-only and occupied: the startup sweep
	// cannot empty it (its child is unremovable without write
	// permission on it), and the hook cannot make a turn directory in
	// it — the write fails after the root exists, the branch a partial
	// failure takes. Writable again when the test ends, so the state
	// directory's own cleanup can remove it.
	root := filepath.Join(e.stateDir, "mcp")
	stale := filepath.Join(root, "stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("occupy the configs' root: %v", err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("read-only the configs' root: %v", err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o700) })

	p := e.startRunner(writeWorkerScript(t, mcpTurnWorker), 100*time.Millisecond)
	defer p.stop(t)

	failed := e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "failed" })
	if msg := e.errorResult(failed); !strings.Contains(msg, "MCP config") {
		t.Errorf("failure = %q, want the MCP config named", msg)
	}

	// The failed attempt left nothing of its own: the root holds what
	// the test planted and no turn directory beside it.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the configs' root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "stale" {
		var names []string
		for _, d := range entries {
			names = append(names, d.Name())
		}
		t.Errorf("the failed write left debris: %v", names)
	}
}

// TestRunnerSweepsStaleMCPConfigs covers the startup sweep (§12.2): a
// runner killed mid-turn never ran its cleanup, and the configs it left
// — each holding a token, revoked with its lease but a secret on disk
// all the same — are removed whole before any new turn adds one.
func TestRunnerSweepsStaleMCPConfigs(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	stale := filepath.Join(e.stateDir, "mcp", "turn-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("make the stale config's directory: %v", err)
	}
	body := `{"mcpServers":{"brigada":{"env":{"BRIGADA_TOKEN":"a-dead-turn-token"}}}}`
	if err := os.WriteFile(filepath.Join(stale, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("plant the stale config: %v", err)
	}

	p := e.startRunner(writeWorkerScript(t, mcpTurnWorker), 100*time.Millisecond)
	defer p.stop(t)

	e.waitUntil("the stale MCP configs to be swept", func() bool {
		_, err := os.Stat(filepath.Join(e.stateDir, "mcp"))
		return os.IsNotExist(err)
	})
}

// TestRunnerKeepsForemanTokenInMemory covers the token's one rule on
// the runner (§12.1): it arrives in the grant and stays in memory —
// never the scratch directory, a record, the transcript or any other
// file under the runner's or the engine's state.
func TestRunnerKeepsForemanTokenInMemory(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{NewLeaseToken: func() (string, error) { return fixedLeaseToken, nil }})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	turn := e.submitForemanTurn("a wake")
	markers := markerDir(t)
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatalf("release the turn's wait: %v", err)
	}

	p := e.startRunner(writeWorkerScript(t, turnWorker), 100*time.Millisecond)
	defer p.stop(t)

	e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })

	// The turn ran and spent its lease: the token was minted for it, and
	// died with the completion.
	if n := e.int64Of("SELECT COUNT(*) FROM lease_tokens"); n != 0 {
		t.Errorf("lease tokens after the turn = %d, want 0", n)
	}

	// No file the runner or the engine wrote carries the secret: not the
	// scratch directory's, not the records', not the transcript blob.
	var files []string
	err := filepath.WalkDir(e.stateDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), fixedLeaseToken) {
			t.Errorf("the lease token landed in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the runner's state: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the runner's state directory is empty; the walk proved nothing")
	}
	if a, ok := e.artifacts(turn.JobID)["transcript"]; ok {
		blob, err := os.ReadFile(a.Path)
		if err != nil {
			t.Fatalf("read the transcript blob: %v", err)
		}
		if strings.Contains(string(blob), fixedLeaseToken) {
			t.Error("the lease token landed in the turn's transcript")
		}
	} else {
		t.Error("the turn left no transcript artifact to check")
	}
}

func TestRunnerForemanCheckoutRefreshes(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	remote := newRemote(t)
	e.seedProject(remote)
	e.setForeman("fake", "fake-1")
	markers := markerDir(t)
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	first := e.submitForemanTurn("first read")
	p := e.startRunner(writeWorkerScript(t, turnWorker), 100*time.Millisecond)
	defer p.stop(t)
	e.waitJob(first.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	repo := filepath.Join(e.foremanScratch(), "repo")
	old := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	seed := t.TempDir()
	git(t, "", "clone", "--branch", "main", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "auth.txt"), []byte("updated code"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, seed, "add", ".")
	git(t, seed, "-c", "user.name=test", "-c", "user.email=test@brigada.invalid", "commit", "-m", "remote moved")
	git(t, seed, "push", "origin", "main")
	want := strings.TrimSpace(git(t, seed, "rev-parse", "HEAD"))
	// Preparation discards stale changes and files without touching the
	// session's own records in scratch.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	second := e.submitForemanTurn("read again")
	e.waitJob(second.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	got := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	if got != want || got == old {
		t.Fatalf("checkout = %s, want new head %s (old %s)", got, want, old)
	}
	if _, err := os.Stat(filepath.Join(repo, "untracked.txt")); !os.IsNotExist(err) {
		t.Errorf("stale file survived refresh: %v", err)
	}
	if !strings.Contains(e.scratchFile("prompt.txt"), want) {
		t.Error("second prompt did not name the refreshed commit")
	}
	if strings.TrimSpace(e.scratchFile("resume.txt")) != "foreman-sess-1" {
		t.Error("refresh lost the session")
	}
}

func TestRunnerForemanCheckoutGitFailure(t *testing.T) {
	t.Parallel()
	for _, fetch := range []bool{false, true} {
		name := "clone"
		if fetch {
			name = "fetch"
		}
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t, engine.Config{})
			remote := newRemote(t)
			e.seedProject(remote)
			e.setForeman("fake", "fake-1")
			if fetch {
				scratch, err := ForemanScratchDir(e.stateDir, e.project)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := PrepareForemanCheckout(e.ctx, scratch, remote, "main", nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(strings.TrimPrefix(remote, "file://"), strings.TrimPrefix(remote, "file://")+"-gone"); err != nil {
				t.Fatal(err)
			}
			turn := e.submitForemanTurn("cannot read the repo")
			p := e.startRunner(writeWorkerScript(t, turnWorker), 100*time.Millisecond)
			defer p.stop(t)
			failed := e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "failed" })
			reason := e.errorResult(failed)
			if !strings.Contains(reason, name) || !strings.Contains(reason, "does not appear to be a git repository") {
				t.Errorf("failure = %s, want %s and the git reason", reason, name)
			}
			if _, err := os.Stat(filepath.Join(e.foremanCheckout(), "where.txt")); !os.IsNotExist(err) {
				t.Errorf("CLI started without code: %v", err)
			}
		})
	}
}

// TestRunnerForemanTurnStartsFreshWhenResumeIsRejected: a key whose
// recorded session the CLI no longer knows (the migration clears the old
// ones, but a harness may also lose its own files) does not fail the
// turn: the runner runs it again without a resume, and the fresh
// session's id is written back.
func TestRunnerForemanTurnStartsFreshWhenResumeIsRejected(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	key := "foreman:" + strconv.FormatInt(e.project, 10)
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO sessions (key, harness_session_id, state, created_at)
			VALUES (?, 'stale-sess', 'active', '2026-01-01T00:00:00Z')`, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	turn := e.submitForemanTurn("a wake after the working directory moved")

	worker := `
cat > prompt.txt
printf '%s\n' "$BRIGADA_FAKE_RESUME" >> resumes.txt
if [ -n "$BRIGADA_FAKE_RESUME" ]; then
  echo "No conversation found with session ID: $BRIGADA_FAKE_RESUME" >&2
  exit 1
fi
echo '{"type":"system_init","session_id":"fresh-sess"}'
echo '{"type":"result","text":"started fresh"}'
`
	p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.workerResult(done); res.Status != "done" || res.Summary != "started fresh" {
		t.Errorf("turn result = %+v, want done with the fresh run's message", res)
	}
	if got := strings.Split(strings.TrimSuffix(e.scratchFile("resumes.txt"), "\n"), "\n"); !slices.Equal(got, []string{"stale-sess", ""}) {
		t.Errorf("resume ids tried = %q, want the stale one, then none", got)
	}
	var session string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", key).Scan(&session)
	}); err != nil || session != "fresh-sess" {
		t.Errorf("session write-back = %q (%v), want fresh-sess", session, err)
	}
}

// An init from before the rename saved its foreman as the file
// <state>/foreman: the scratch directory moves it aside, not fails.
func TestForemanScratchDirMovesOldForemanFile(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "foreman"), []byte("claude/opus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := ForemanScratchDir(state, 7)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("scratch dir: %v, %v", info, err)
	}
	if got, err := runnerstate.LoadForeman(state); got != "claude/opus" || err != nil {
		t.Fatalf("saved foreman after the move: %q, %v", got, err)
	}
}
