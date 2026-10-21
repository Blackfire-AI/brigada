package runner

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// setTaskSetup records a setup on the task, as the engine does at the
// task's creation (§5.7).
func (e *testEnv) setTaskSetup(task int64, spec engine.SetupSpec) {
	e.t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		e.t.Fatal(err)
	}
	err = e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE tasks SET setup = ? WHERE id = ?", string(b), task)
		return err
	})
	if err != nil {
		e.t.Fatalf("record the task's setup: %v", err)
	}
}

// markerLines reads a marker file one line per entry.
func markerLines(t *testing.T, dir, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// setupWorker leaves work.txt behind, and edits the key file too when the
// test asked it to (the marker "mutate").
var setupWorker = `
cat > /dev/null
echo "worker cache=[${BRIGADA_CACHE_DIR-unset}]" >> "$BRIGADA_TEST_MARKERS/order.log"
: > "$BRIGADA_TEST_MARKERS/worker-started"
echo '{"type":"tool_start","name":"Edit"}'
echo hello > work.txt
if [ -f "$BRIGADA_TEST_MARKERS/mutate" ]; then echo changed >> README.md; fi
echo '{"type":"tool_end","name":"Edit"}'
` + fakeResultScript(`{"status":"done","summary":"edited"}`)

// cacheProbe is a setup command that notes whether its cache directory
// already held what a previous run left there.
const cacheProbe = `if [ -f "$BRIGADA_CACHE_DIR/stamp" ]; then echo hit; else echo miss; : > "$BRIGADA_CACHE_DIR/stamp"; fi >> "$BRIGADA_TEST_MARKERS/cache.log"`

func TestRunnerSetupRunsBeforeWorkerAndCheck(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", `echo check >> "$BRIGADA_TEST_MARKERS/order.log"`)
	e.setTaskSetup(task, engine.SetupSpec{
		Command:        `echo "setup: $(ls | tr '\n' ' ')" >> "$BRIGADA_TEST_MARKERS/order.log"`,
		TimeoutMinutes: 5,
	})
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, setupWorker), 100*time.Millisecond)
	defer p.stop(t)
	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })

	want := []string{
		"setup: README.md ",
		"worker cache=[unset]", // the worker never sees the cache directory
		"setup: README.md work.txt ",
		"check",
	}
	got := markerLines(t, markers, "order.log")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("order = %q, want %q", got, want)
	}
	if check := e.checkResult(done); check.ExitCode != 0 {
		t.Errorf("check = %+v, want a pass", check)
	}
	if _, ok := e.artifacts(job)["setup_log"]; !ok {
		t.Error("no setup log artifact for a job that ran a setup")
	}
	if info, _ := e.eng.Job(e.ctx, job); info == nil || info.SetupLogID == 0 {
		t.Errorf("the job does not link its setup log: %+v", info)
	}
	e.assertNoWorktrees()
	if entries, _ := os.ReadDir(filepath.Join(e.stateDir, "setup-jobs")); len(entries) != 0 {
		t.Errorf("the job's private cache copy was left behind: %v", entries)
	}
}

func TestRunnerSetupCacheReusedAndRebuilt(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	p := e.startRunner(writeWorkerScript(t, setupWorker), 100*time.Millisecond)
	defer p.stop(t)

	run := func() {
		t.Helper()
		task := e.seedTask("", "true")
		e.setTaskSetup(task, engine.SetupSpec{Command: cacheProbe, CacheKeyFiles: []string{"README.md"}, TimeoutMinutes: 5})
		job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
		e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	}

	// First job: the cache is cold before the worker and warm for the
	// second run, since the lock file is unchanged and the first run stored
	// its copy.
	run()
	// Second job, same lock file: both runs find the entry.
	run()
	// Third job, whose worker changes the key file: the first run still
	// hits, the run before the check has another key and is cold.
	if err := os.WriteFile(filepath.Join(markers, "mutate"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run()
	// Fourth job: the cold run before the check stored nothing, so the
	// original key's entry is still the one, and the lock file is back.
	if err := os.Remove(filepath.Join(markers, "mutate")); err != nil {
		t.Fatal(err)
	}
	run()

	want := []string{"miss", "hit", "hit", "hit", "hit", "miss", "hit", "hit"}
	if got := markerLines(t, markers, "cache.log"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cache log = %v, want %v", got, want)
	}
}

func TestRunnerSetupCacheNeverSharedAcrossRepositories(t *testing.T) {
	remoteA, remoteB := newRemote(t), newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remoteA)
	markers := markerDir(t)
	p := e.startRunner(writeWorkerScript(t, setupWorker), 100*time.Millisecond)
	defer p.stop(t)

	for _, repo := range []string{"", remoteB} { // the project's repo, then another
		task := e.seedTask(repo, "true")
		e.setTaskSetup(task, engine.SetupSpec{Command: cacheProbe, CacheKeyFiles: []string{"README.md"}, TimeoutMinutes: 5})
		job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
		e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	}

	// The second repository's seed has the same README, hence the same
	// key: it must still start cold.
	want := []string{"miss", "hit", "miss", "hit"}
	if got := markerLines(t, markers, "cache.log"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cache log = %v, want %v", got, want)
	}
	entries, err := os.ReadDir(filepath.Join(e.stateDir, "setup-cache"))
	if err != nil || len(entries) != 2 {
		t.Errorf("setup-cache holds %v (%v), want one directory per repository", entries, err)
	}
}

func TestRunnerFailedSetupIsAnInfrastructureFailure(t *testing.T) {
	remote := newRemote(t)
	clk := newManualClock()
	e := newTestEnv(t, engine.Config{Now: clk.Now})
	e.seedProject(remote)
	task := e.seedTask("", `echo check >> "$BRIGADA_TEST_MARKERS/order.log"`)
	e.setTaskSetup(task, engine.SetupSpec{Command: "echo setup-boom; exit 7", TimeoutMinutes: 5})
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, setupWorker), 100*time.Millisecond)
	defer p.stop(t)
	failed := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "failed" })

	if msg := e.errorResult(failed); msg != "setup failed: exit code 7" {
		t.Errorf("error = %q, want the setup's exit code and none of its output", msg)
	}
	arts := e.artifacts(job)
	a, ok := arts["setup_log"]
	if !ok {
		t.Fatalf("a failed setup left no log artifact (have %v)", arts)
	}
	if data, _ := os.ReadFile(a.Path); !strings.Contains(string(data), "setup-boom") {
		t.Errorf("the setup log does not hold the output:\n%s", data)
	}
	if _, ok := arts["check_log"]; ok {
		t.Error("a check ran after a failed setup")
	}
	if _, err := os.Stat(filepath.Join(markers, "worker-started")); err == nil {
		t.Error("the worker started after a failed setup")
	}

	// Not a failed check, not a worker failure: the task is retried like
	// a transient error, capped, and then parked for the foreman; the
	// agent is not charged with any of it. The retries wait a growing
	// engine-clock delay (§3, issue #46): step the clock forward so the
	// two retries become leasable without sleeping the test for the real
	// 5s + 15s.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if e.strOf("SELECT state FROM tasks WHERE id = ?", task) == "needs_foreman" {
			break
		}
		clk.Advance(5 * time.Second)
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the task to park after its retries")
		}
		time.Sleep(15 * time.Millisecond)
	}
	if n := e.int64Of("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND status = 'failed'", task); n != 3 {
		t.Errorf("failed jobs = %d, want the first run and two retries", n)
	}
	if n := e.int64Of("SELECT COUNT(*) FROM agents WHERE health != 'ready'"); n != 0 {
		t.Errorf("%d agents were marked unhealthy by setup failures", n)
	}
	if _, err := os.Stat(filepath.Join(markers, "worker-started")); err == nil {
		t.Error("the worker started during the retries")
	}
	e.assertNoWorktrees()
}

func TestRunnerSetupTimeoutKillsTheTree(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	e.setTaskSetup(task, engine.SetupSpec{Command: `(sleep 30; : > "$BRIGADA_TEST_MARKERS/straggler") & sleep 30`, TimeoutMinutes: 5})
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, setupWorker), 100*time.Millisecond, func(c *Config) {
		c.SetupTimeout = 300 * time.Millisecond
	})
	defer p.stop(t)
	start := time.Now()
	failed := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "failed" })
	if msg := e.errorResult(failed); !strings.HasPrefix(msg, "setup failed: timed out") {
		t.Errorf("error = %q, want a timeout", msg)
	}
	if time.Since(start) > 8*time.Second {
		t.Errorf("the timeout took %v to bite", time.Since(start))
	}
}

func TestSetupCacheKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	key := func(spec engine.SetupSpec) (string, bool) {
		t.Helper()
		k, ok, err := setupCacheKey(dir, &spec)
		if err != nil {
			t.Fatal(err)
		}
		return k, ok
	}
	spec := engine.SetupSpec{Command: "npm ci", CacheKeyFiles: []string{"lock.json"}}
	absent, cacheable := key(spec)
	if !cacheable {
		t.Fatal("a setup with key files is not cacheable")
	}
	write("lock.json", "v1")
	v1, _ := key(spec)
	write("lock.json", "v2")
	v2, _ := key(spec)
	if absent == v1 || v1 == v2 {
		t.Errorf("keys do not follow the file: absent=%s v1=%s v2=%s", absent, v1, v2)
	}
	other := spec
	other.Command = "npm install"
	if k, _ := key(other); k == v2 {
		t.Error("the key ignores the command text")
	}
	if _, cacheable := key(engine.SetupSpec{Command: "npm ci"}); cacheable {
		t.Error("a setup that names no key file got a cache")
	}
	if _, _, err := setupCacheKey(dir, &engine.SetupSpec{Command: "x", CacheKeyFiles: []string{"../escape"}}); err == nil {
		t.Error("a key file outside the repository was accepted")
	}
}

func TestPruneSetupCacheBounds(t *testing.T) {
	repoDir := t.TempDir()
	now := time.Now()
	for i, age := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 40 * 24 * time.Hour} {
		p := filepath.Join(repoDir, string(rune('a'+i)))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	pruneSetupCache(repoDir, now)
	left, _ := os.ReadDir(repoDir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Errorf("entries left = %v, want the three most recent", names)
	}
}
