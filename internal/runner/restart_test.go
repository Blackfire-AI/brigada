package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The restart request's runner side (§5.1): a drain takes no new lease
// and waits out a running job before the runner exits; `now` exits at
// once and cuts the job it is running. Both run against the real loop,
// with a scripted worker the test releases only by its marker.

// gatingWorker runs until the test writes the release marker, then
// reports done: the run's end is the test's to time.
var gatingWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"starting"}'
: > "$BRIGADA_TEST_MARKERS/started"
` + testsupport.AwaitRelease + `
echo '{"type":"tool_start","name":"Edit"}'
echo hello > work.txt
echo '{"type":"tool_end","name":"Edit"}'
` + fakeResultScript(`{"status":"done","summary":"released"}`)

// TestRunnerRestartDrainWaitsForJobs covers the drain: with a job
// running, the runner does not exit until the job finished — and the
// job finishes on its own terms, done, never cut. The engine learns of
// the drain while it waits, so status can say the runner is
// restarting.
func TestRunnerRestartDrainWaitsForJobs(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, gatingWorker), 100*time.Millisecond)
	stopped := false
	defer func() {
		if !stopped {
			p.stop(t)
		}
	}()

	e.waitUntil("the worker to start", func() bool {
		_, err := os.Stat(filepath.Join(markers, "started"))
		return err == nil
	})

	if err := runnerstate.RequestRestart(e.stateDir, runnerstate.RestartDrain); err != nil {
		t.Fatalf("request the drain: %v", err)
	}

	// The engine hears of the drain while the job still runs (§10's
	// restarting line rides on it).
	e.waitUntil("the engine to see the drain", func() bool {
		st, err := e.eng.Status(e.ctx, 0)
		return err == nil && st.RunnerDraining
	})

	// The running job is not cut: still live, its worker untouched, and
	// the runner has not exited.
	e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "leased" || j.Status == "running" })
	select {
	case err := <-p.done:
		t.Fatalf("the runner exited while its job still runs: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Released, the job finishes — done, not failed — and only then
	// does the runner exit, cleanly, its request consumed.
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.workerResult(done); res.Status != "done" {
		t.Errorf("worker result = %+v, want done: the drain cut a running job", res)
	}
	select {
	case err := <-p.done:
		stopped = true
		if err != nil {
			t.Fatalf("the drained runner exited with %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not exit after its last job finished")
	}
	if _, ok := runnerstate.RestartRequested(e.stateDir); ok {
		t.Error("the honoured restart request was left behind")
	}
}

// TestRunnerRestartNowCutsJobs covers `now`: the runner exits at once,
// its running job cut mid-run — the worker never reached its end.
func TestRunnerRestartNowCutsJobs(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, gatingWorker), 100*time.Millisecond)
	stopped := false
	defer func() {
		if !stopped {
			p.stop(t)
		}
	}()

	e.waitUntil("the worker to start", func() bool {
		_, err := os.Stat(filepath.Join(markers, "started"))
		return err == nil
	})

	if err := runnerstate.RequestRestart(e.stateDir, runnerstate.RestartNow); err != nil {
		t.Fatalf("request the immediate restart: %v", err)
	}

	select {
	case err := <-p.done:
		stopped = true
		if err != nil {
			t.Fatalf("the restarted runner exited with %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not exit on the --now request")
	}
	// No release marker was ever written, so a done job would mean the
	// run was waited out, not cut.
	if j, err := e.eng.Job(e.ctx, job); err != nil {
		t.Fatal(err)
	} else if j.Status == "done" {
		t.Error("the cut job completed; --now did not cut it")
	}
	if _, ok := runnerstate.RestartRequested(e.stateDir); ok {
		t.Error("the honoured restart request was left behind")
	}
}
