package lifecycle_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The BP8a exit test: a task submitted through the CLI runs the worker
// in a worktree, the runner runs its check, the branch is pushed, the
// history shows the whole timeline — and an engine killed mid-job and
// restarted recovers through lease expiry and fencing with no double
// work: exactly one completion, one pushed result, one worker run per
// task. The fake adapter drives a scripted worker; the remote is a local
// bare git repo; no real agent CLI is involved.

// exitWorkerScript is the scripted worker both tasks run: it reports
// progress, holds still long enough for the test to kill the engine
// mid-run, then commits work.txt and ends with the done result block.
// Every run appends one line to the markers file, so the test can count
// runs.
func exitWorkerScript(t *testing.T, markers string) string {
	t.Helper()
	script := `#!/bin/sh
cat > /dev/null
echo "fake-cli — starting"
echo '{"type":"system_init","session_id":"sess-exit"}'
echo '{"type":"assistant","text":"reading the task"}'
echo run >> "$BRIGADA_EXIT_MARKERS/runs"
sleep 3
echo '{"type":"tool_start","name":"Edit"}'
echo hello > work.txt
git add -A
git commit -qm 'exit-test work'
echo '{"type":"tool_end","name":"Edit"}'
br=$(git rev-parse --abbrev-ref HEAD)
printf '{"type":"result","text":"added work.txt: {\\"status\\":\\"done\\",\\"summary\\":\\"added work.txt\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
`
	path := filepath.Join(t.TempDir(), "worker.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the worker script: %v", err)
	}
	return path
}

// startRunnerProc runs `brigada runner` with the fake adapter, logging
// into a file the cleanup quotes on failure.
func startRunnerProc(t *testing.T, bin, state, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "runner",
		"--state-dir", state, "--harness", "fake", "--worker-script", script)
	logPath := filepath.Join(t.TempDir(), "runner.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("open the runner log: %v", err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Its own process group: the cleanup waits for every git child the
	// runner started (a push's receive-pack and index-pack write into the
	// remote's objects) to be gone before TempDir removal runs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the runner: %v", err)
	}
	t.Cleanup(func() {
		pgid := cmd.Process.Pid
		if cmd.ProcessState == nil {
			syscall.Kill(-pgid, syscall.SIGTERM)
			done := make(chan struct{})
			go func() { cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				syscall.Kill(-pgid, syscall.SIGKILL)
				<-done
			}
		}
		waitGroupGone(pgid)
		logFile.Close()
		if data, err := os.ReadFile(logPath); err == nil {
			t.Logf("runner log:\n%s", data)
		}
	})
	return cmd
}

// waitGroupGone returns once no process of group pgid is left, so no
// git child of the runner can still write into the remote when TempDir
// cleanup runs. Stragglers get a SIGKILL after five seconds.
func waitGroupGone(pgid int) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(-pgid, syscall.SIGKILL)
			deadline = time.Now().Add(5 * time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lsRemoteCount runs `git ls-remote --heads` with one ref pattern and
// returns how many refs matched. Git does the matching — the test never
// filters ref names itself. Every pattern carries a `?` wildcard next
// to the namespace: against this git (2.39.5), a pattern with no
// wildcard matches nothing under refs/heads/brigada/ while the same ref
// matches through one (`refs/heads/brig?da/task-2`), so the patterns
// ride the matching path that works. A pattern without a trailing `*`
// must match the whole ref name, which is how the test tells
// task-2 from task-2-a2 apart.
func lsRemoteCount(t *testing.T, remote, pattern string) int {
	t.Helper()
	out, err := exec.Command("git", "ls-remote", "--heads", remote, pattern).Output()
	if err != nil {
		t.Fatalf("ls-remote %s: %v", pattern, err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return 0
	}
	return len(strings.Fields(string(out))) / 2
}

// historyKinds runs `brigada history --task N` and returns the timeline
// line kinds, in order.
func historyKinds(t *testing.T, bin, state string, task int) []string {
	t.Helper()
	stdout, stderr, code := runBrigada(t, bin, "history", "--state-dir", state, "--task", strconv.Itoa(task), "--no-start")
	if code != 0 {
		t.Fatalf("history --task %d exit = %d, stderr = %q", task, code, stderr)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			t.Fatalf("history line = %q, want <time> <kind> <subject>: <summary>", line)
		}
		kinds = append(kinds, fields[1])
	}
	return kinds
}

// markerCount counts the worker runs the markers file recorded.
func markerCount(t *testing.T, markers string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(markers, "runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read the run markers: %v", err)
	}
	return len(strings.Fields(string(data)))
}

func TestBP8aExit(t *testing.T) {
	bin := testsupport.BuildBrigada(t)
	remote := testsupport.NewGitRemote(t)
	state := testsupport.ShortStateDir(t)
	killEngineOnCleanup(t, state)

	// The markers directory reaches the worker through its environment
	// (the runner passes the ambient set through, minus brigada's own
	// share).
	markers := t.TempDir()
	t.Setenv("BRIGADA_EXIT_MARKERS", markers)
	script := exitWorkerScript(t, markers)

	// The runner starts before any engine exists: it starts one on
	// demand (§13) and would retry through a gap besides (§5.1).
	startRunnerProc(t, bin, state, script)
	waitHealthy(t, state)

	// 1. A task submitted through the CLI runs the worker in a worktree.
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("Add a work.txt with hello in it.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runBrigada(t, bin, "task", "submit",
		"--state-dir", state, "--repo", remote, "--base", "main",
		"--spec", spec, "--check", `test "$(cat work.txt)" = hello`)
	if code != 0 {
		t.Fatalf("task submit exit = %d, stderr = %q", code, stderr)
	}
	if want := "task 1 job 1: created (run 1)\n"; stdout != want {
		t.Errorf("task submit output = %q, want %q", stdout, want)
	}

	// 2-3. The runner runs the check and pushes the branch (§5.5, §5.4).
	waitFor(t, "the first task's branch on the remote", func() bool {
		return lsRemoteCount(t, remote, "refs/heads/brig?da/*") > 0
	}, func() {
		out, _ := exec.Command("git", "ls-remote", remote).CombinedOutput()
		t.Logf("the remote's refs:\n%s", out)
		st, _, _ := runBrigada(t, bin, "status", "--state-dir", state)
		t.Logf("status:\n%s", st)
		h, _, _ := runBrigada(t, bin, "history", "--state-dir", state, "--task", "1", "--no-start")
		t.Logf("history task 1:\n%s", h)
	})
	waitFor(t, "the first task to complete", func() bool {
		out, _, code := runBrigada(t, bin, "status", "--state-dir", state)
		return code == 0 && strings.Contains(out, "done 1")
	})
	if runs := markerCount(t, markers); runs != 1 {
		t.Fatalf("worker runs after the first task = %d, want 1", runs)
	}

	// 4. `brigada history --task` shows the whole timeline (§9): the run
	// itself, then the review job its completion queued (§7) — which
	// this one-family world never leases, so its story ends at creation
	// and the note that no reviewer exists.
	if kinds := historyKinds(t, bin, state, 1); strings.Join(kinds, " ") !=
		"job_created task_spec job_leased job_agent_picked task_running job_completed task_reviewing job_created review_unservable worker_result task_spec" {
		t.Errorf("the first task's timeline = %v, want the whole story", kinds)
	}

	// 5. SIGKILL the engine mid-job, restart it, and the job recovers
	// with no double work: never both a completion and a re-lease.
	if err := os.WriteFile(spec, []byte("Add work.txt again, the same way.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runBrigada(t, bin, "task", "submit",
		"--state-dir", state, "--repo", remote, "--base", "main",
		"--spec", spec, "--check", `test "$(cat work.txt)" = hello`)
	if code != 0 {
		t.Fatalf("second task submit exit = %d, stderr = %q", code, stderr)
	}
	// The first task's completion queued its review job (§7), which took
	// the next job id; the second task's work job follows it.
	if want := "task 2 job 3: created (run 1)\n"; stdout != want {
		t.Errorf("second task submit output = %q, want %q", stdout, want)
	}
	waitFor(t, "the second worker to be running", func() bool {
		return markerCount(t, markers) >= 2
	})

	lock := lifecycle.PathsOf(state).Lock
	enginePid := lifecycle.EnginePid(lock)
	if enginePid <= 0 {
		t.Fatalf("no engine pid to kill (lock %s)", lock)
	}
	if err := syscall.Kill(enginePid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL the engine pid %d: %v", enginePid, err)
	}
	waitFor(t, "the killed engine's lock to come free", func() bool {
		return lifecycle.EnginePid(lock) == 0
	})

	// The restart rides the same on-demand path a client takes (§13).
	_, stderr, code = runBrigada(t, bin, "status", "--state-dir", state)
	if code != 0 {
		t.Fatalf("status after the restart exit = %d, stderr = %q", code, stderr)
	}
	waitFor(t, "the second task's branch on the remote", func() bool {
		return lsRemoteCount(t, remote, "refs/heads/brig?da/task-2*") > 0
	}, func() {
		out, _, _ := runBrigada(t, bin, "history", "--state-dir", state, "--task", "2", "--no-start")
		t.Logf("history task 2 while waiting:\n%s", out)
	})
	waitFor(t, "the second task to complete", func() bool {
		out, _, code := runBrigada(t, bin, "status", "--state-dir", state)
		return code == 0 && strings.Contains(out, "done 2")
	})

	// Exactly one completion and one pushed result: the branch name
	// carries the attempt suffix on any re-lease (§6.4), so one plain
	// brigada/task-2 — matched whole, with no trailing wildcard — is
	// the proof the lease never changed hands.
	if n := lsRemoteCount(t, remote, "refs/heads/brig?da/task-2*"); n != 1 {
		t.Errorf("the remote holds %d task-2 branches, want exactly one", n)
	}
	if lsRemoteCount(t, remote, "refs/heads/brig?da/task-2") != 1 {
		t.Error("the remote's task-2 branch is not exactly brigada/task-2; the lease changed hands")
	}
	kinds := historyKinds(t, bin, state, 2)
	if count(kinds, "job_completed") != 1 || count(kinds, "worker_result") != 1 {
		t.Errorf("the second task's timeline = %v, want exactly one completion and one result", kinds)
	}
	if count(kinds, "job_leased") != 1 || count(kinds, "lease_requeued") != 0 {
		t.Errorf("the second task's timeline = %v, want one lease and no requeue", kinds)
	}
	if runs := markerCount(t, markers); runs != 2 {
		t.Errorf("worker runs after the recovery = %d, want 2: the worker never ran twice", runs)
	}
}

func count(haystack []string, needle string) int {
	n := 0
	for _, s := range haystack {
		if s == needle {
			n++
		}
	}
	return n
}
