package runner

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
)

// The foreman turn tests run against the same machinery as the worker
// ones: the engine, its API and the fake adapter, with the project's
// foreman pointed at the fake agent — the columns `brigada project
// foreman` will set once it exists.

// setForeman points the project's foreman at a harness and model.
func (e *testEnv) setForeman(harness, model string) {
	e.t.Helper()
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE projects SET foreman_harness = ?, foreman_model = ? WHERE id = ?",
			harness, model, e.project)
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

// scratchFile reads one file the turn left in the scratch directory.
func (e *testEnv) scratchFile(name string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.foremanScratch(), name))
	if err != nil {
		e.t.Fatalf("read %s from the scratch directory: %v", name, err)
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
while [ ! -f "$BRIGADA_TEST_MARKERS/release" ]; do sleep 0.05; done
echo '{"type":"result","text":"answered the worker; the plan stands"}'
`

// TestRunnerForemanTurnRunsInScratchDir is the turn's happy path (§4,
// §4.2 item 2, §5.4): the foreman wakes headless, runs in the project's
// scratch directory — no repo, no worktree, no check — and a clean exit
// without any result block completes the job as done with the final
// message as its summary.
func TestRunnerForemanTurnRunsInScratchDir(t *testing.T) {
	e := newTestEnv(t, engine.Config{})
	e.seedProject("") // a project with no repo: a foreman turn never needs one
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
	scratch, err := filepath.EvalSymlinks(e.foremanScratch())
	if err != nil {
		t.Fatalf("resolve the scratch directory: %v", err)
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
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	first := e.submitForemanTurn("first wake: a worker asked")
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, turnWorker), 100*time.Millisecond)
	defer p.stop(t)

	e.waitUntil("the turn to start", func() bool {
		_, err := os.Stat(filepath.Join(e.foremanScratch(), "resume.txt"))
		return err == nil
	})

	// The second wake coalesces (§12.1, §14): pending behind the live
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
// result — and no branch to save on the way out.
func TestRunnerForemanTurnTimeCapped(t *testing.T) {
	e := newTestEnv(t, engine.Config{})
	e.seedProject("")
	e.setForeman("fake", "fake-1")
	turn := e.submitForemanTurn("a wake that outstays its welcome")
	markerDir(t)

	worker := `
cat > /dev/null
echo '{"type":"system_init","session_id":"foreman-sess-4"}'
echo '{"type":"assistant","text":"thinking"}'
while [ ! -f "$BRIGADA_TEST_MARKERS/release" ]; do sleep 0.1; done
`
	p := e.startRunner(writeWorkerScript(t, worker), 100*time.Millisecond,
		func(c *Config) { c.ClassCaps = map[string]ClassCaps{"foreman": {TimeCap: 300 * time.Millisecond}} })
	defer p.stop(t)

	stalled := e.waitJob(turn.JobID, func(j *engine.JobInfo) bool { return j.Status == "stalled" })
	if res := e.workerResult(stalled); res.Status != statusTimeCap {
		t.Errorf("capped turn result = %+v, want status %q", res, statusTimeCap)
	}
	if _, ok := e.artifacts(turn.JobID)["transcript"]; !ok {
		t.Error("the capped turn left no transcript artifact")
	}
}

// TestRunnerForemanTurnFails covers the other ending: a foreman CLI
// that exits non-zero fails the turn (§5.4's exemption cuts both ways —
// there is no result block to outweigh the exit), with the error kept.
func TestRunnerForemanTurnFails(t *testing.T) {
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

// fixedLeaseToken is the secret a test pins the engine's mint to, so
// the runner's handling of it can be checked byte for byte. Hex, the
// shape the default mint produces.
const fixedLeaseToken = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeed"

// TestRunnerKeepsForemanTokenInMemory covers the token's one rule on
// the runner (§12.1): it arrives in the grant and stays in memory —
// never the scratch directory, a record, the transcript or any other
// file under the runner's or the engine's state.
func TestRunnerKeepsForemanTokenInMemory(t *testing.T) {
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
