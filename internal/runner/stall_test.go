package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The stalled-worker tests (§5.6, BP8a-4b). The timings are real but
// small — the policy's arithmetic is covered by its table test below,
// and the default minutes-long windows by the manual clock, so nothing
// here sleeps longer than a heartbeat interval needs.

// TestStallPolicyCheck is the policy's arithmetic on fixed times: the
// silence window, the suspension an open tool call buys and its own
// bound, and the class time cap over them all.
func TestStallPolicyCheck(t *testing.T) {
	start := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return start.Add(time.Duration(min) * time.Minute) }
	// The worker reported once at its start; a tool call opened at
	// minute 2 and is still open.
	idle := runTiming{LastActivity: start}
	inTool := func(openedMin int) runTiming {
		return runTiming{LastActivity: at(openedMin), ToolOpenedAt: at(openedMin), ToolInFlight: true}
	}
	quiet := stallPolicy{StallAfter: 10 * time.Minute, ToolCap: 30 * time.Minute, StartedAt: start}

	cases := []struct {
		name string
		p    stallPolicy
		t    runTiming
		now  time.Time
		want string // outcome, "" for none
		note string // what the reason must name
	}{
		{"quiet inside the window", quiet, idle, at(9), "", ""},
		{"quiet past the window", quiet, idle, at(11), statusStalled, "no activity"},
		{"an open tool call suspends the clock", quiet, inTool(2), at(25), "", ""},
		{"an open tool call past its own cap", quiet, inTool(2), at(33), statusStalled, "tool call"},
		{"an open call with no cap suspends unbounded",
			stallPolicy{StallAfter: 10 * time.Minute, ToolCap: 0, StartedAt: start},
			inTool(2), at(120), "", ""},
		{"the class time cap over silence",
			stallPolicy{StallAfter: 10 * time.Minute, TimeCap: 40 * time.Minute, StartedAt: start},
			idle, at(41), statusTimeCap, "class time cap"},
		{"the class time cap over an open tool call",
			stallPolicy{StallAfter: 10 * time.Minute, ToolCap: 30 * time.Minute, TimeCap: 40 * time.Minute, StartedAt: start},
			inTool(2), at(41), statusTimeCap, "class time cap"},
		{"a worker with no word yet is measured from its start", quiet,
			runTiming{LastActivity: start}, at(9), "", ""},
		{"every watch off",
			stallPolicy{StartedAt: start}, idle, at(600), "", ""},
		{"the silence watch off, the cap on",
			stallPolicy{TimeCap: 40 * time.Minute, StartedAt: start}, idle, at(41), statusTimeCap, "class time cap"},
	}
	for _, tc := range cases {
		got := tc.p.check(tc.now, tc.t)
		if tc.want == "" {
			if got != nil {
				t.Errorf("%s: check = %+v, want none", tc.name, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: check = nil, want %q", tc.name, tc.want)
			continue
		}
		if got.Outcome != tc.want || !strings.Contains(got.Reason, tc.note) {
			t.Errorf("%s: check = %+v, want %q naming %q", tc.name, got, tc.want, tc.note)
		}
	}
}

// TestStopOutcomesAreTheProtocolOnes pins the runner's stop statuses to
// the engine's protocol outcomes (§5.6): finishStopped sends the same
// word as the result block's status, and the engine refuses any other.
func TestStopOutcomesAreTheProtocolOnes(t *testing.T) {
	if statusStalled != engine.OutcomeStalled || statusTimeCap != engine.OutcomeTimeCap {
		t.Fatalf("the runner's stop statuses (%q, %q) must equal the engine's outcomes (%q, %q)",
			statusStalled, statusTimeCap, engine.OutcomeStalled, engine.OutcomeTimeCap)
	}
}

// TestMergeClassCaps pins what a class's caps start from and how an
// entry changes that: a set field overrides, a zero field keeps the
// default, a negative one switches the bound off.
func TestMergeClassCaps(t *testing.T) {
	merged := mergeClassCaps(map[string]ClassCaps{
		"review": {TimeCap: 15 * time.Minute},
		"work":   {ToolCap: -1},
		"audit":  {TimeCap: 5 * time.Minute},
	})
	for kind, want := range map[string]ClassCaps{
		"work":    {ToolCap: -1, TimeCap: 40 * time.Minute},
		"fix":     {ToolCap: 30 * time.Minute, TimeCap: 40 * time.Minute},
		"review":  {ToolCap: 30 * time.Minute, TimeCap: 15 * time.Minute},
		"foreman": {ToolCap: 30 * time.Minute, TimeCap: 25 * time.Minute},
		// a class the defaults do not know: the tool cap still applies,
		// the wall cap only because the entry set one.
		"audit": {ToolCap: 30 * time.Minute, TimeCap: 5 * time.Minute},
	} {
		if got := merged[kind]; got != want {
			t.Errorf("caps[%s] = %+v, want %+v", kind, got, want)
		}
	}
	// A nil configuration keeps every default.
	if got := mergeClassCaps(nil); !reflect.DeepEqual(got, defaultClassCaps()) {
		t.Errorf("mergeClassCaps(nil) = %+v, want the defaults", got)
	}
}

// The scripted workers for the stall tests. Each reports a burst of
// progress and then plays out its scenario; the markers say whether it
// ran to the end — a killed worker leaves no finished marker.

// silentWorker goes quiet mid-run: alive, but making no progress.
const silentWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"looking at the task"}'
echo '{"type":"tool_start","name":"Edit"}'
echo '{"type":"tool_end","name":"Edit"}'
: > "$BRIGADA_TEST_MARKERS/started"
sleep 30
: > "$BRIGADA_TEST_MARKERS/finished"
`

// longToolWorker opens a tool call and works inside it, silent for
// longer than the stall window: the open call suspends the clock.
const longToolWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"starting"}'
echo '{"type":"tool_start","name":"Bash"}'
: > "$BRIGADA_TEST_MARKERS/started"
sleep 0.6
echo '{"type":"tool_end","name":"Bash"}'
echo hello > work.txt
git add -A
git commit -qm 'fake work: add work.txt'
br=$(git rev-parse --abbrev-ref HEAD)
printf '{"type":"result","text":"done inside the tool: {\\"status\\":\\"done\\",\\"summary\\":\\"added work.txt\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
: > "$BRIGADA_TEST_MARKERS/finished"
`

// stalledCommitter does real work, commits it, and then goes quiet:
// work a stall must not throw away.
const stalledCommitter = `
cat > /dev/null
echo '{"type":"assistant","text":"starting"}'
echo stalled work > work.txt
git add -A
git commit -qm 'stalled work'
: > "$BRIGADA_TEST_MARKERS/started"
sleep 30
: > "$BRIGADA_TEST_MARKERS/finished"
`

// termProofWorker spawns a child that ignores SIGTERM and ignores it
// itself: only the group SIGKILL, after the grace, can end either.
const termProofWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"starting"}'
trap "" TERM
echo $$ > "$BRIGADA_TEST_MARKERS/parent"
sh -c 'trap "" TERM; echo $$ > "$BRIGADA_TEST_MARKERS/child"; sleep 60' &
: > "$BRIGADA_TEST_MARKERS/started"
sleep 60
: > "$BRIGADA_TEST_MARKERS/finished"
`

// termLoyalWorker honors the TERM itself but leaves behind a child
// that ignores it and holds no pipe of the worker's: the leader is dead
// and reaped well before the grace runs out, the run completes, and
// only the group kill can still reach the child.
const termLoyalWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"starting"}'
sh -c 'trap "" TERM; echo $$ > "$BRIGADA_TEST_MARKERS/child"; sleep 60' >/dev/null 2>&1 &
: > "$BRIGADA_TEST_MARKERS/started"
sleep 60
: > "$BRIGADA_TEST_MARKERS/finished"
`

// TestRunnerStalledWorker covers the silence watch end to end (§5.6):
// a worker that reports and then goes quiet past the window is killed
// and its job completes with outcome stalled — not dropped, not left
// running — carrying the reason and the last progress events.
func TestRunnerStalledWorker(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, silentWorker), 100*time.Millisecond,
		func(c *Config) { c.StallAfter = 400 * time.Millisecond })
	defer p.stop(t)

	stopped := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == statusStalled })
	res := e.workerResult(stopped)
	if res.Status != statusStalled {
		t.Errorf("worker result = %+v, want status %q", res, statusStalled)
	}
	if !strings.Contains(res.Reason, "no activity") {
		t.Errorf("reason = %q, want the silence named", res.Reason)
	}
	// The last progress events ride with the outcome (§5.6).
	want := []string{"message looking at the task", "tool_start Edit", "tool_end Edit"}
	if !reflect.DeepEqual(res.Progress, want) {
		t.Errorf("progress events = %v, want %v", res.Progress, want)
	}
	// The killed run's transcript is still an artifact, and the
	// worktree came down as usual.
	if _, ok := e.artifacts(job)["transcript"]; !ok {
		t.Error("a stalled run delivered no transcript artifact")
	}
	e.assertNoWorktrees()
	if _, err := os.Stat(filepath.Join(markers, "finished")); err == nil {
		t.Fatal("the stalled worker ran to the end; it was not killed")
	}
	// No work happened, so no branch was pushed: an empty result branch
	// is noise, not a result.
	if remoteBranchCommit(t, remote, taskBranch(task, 1)) != "" {
		t.Error("a stalled run with no work pushed a result branch")
	}
}

// TestRunnerStallDefaultWindowWithManualClock runs the stall watch at
// its shipped ten minutes (§5.6) on an injected clock: the window plays
// out in a beat of real time, which is the point of the clock — no
// test sleeps the stall out.
func TestRunnerStallDefaultWindowWithManualClock(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	clk := newManualClock()
	p := e.startRunner(writeWorkerScript(t, silentWorker), 50*time.Millisecond,
		func(c *Config) { c.Clock = clk.Clock() }) // StallAfter keeps its 10-minute default
	defer p.stop(t)

	e.waitUntil("the worker to start", func() bool {
		_, err := os.Stat(filepath.Join(markers, "started"))
		return err == nil
	})
	// The advance is repeated until the job stalls: the worker's marker
	// can land before the run counts the worker as watched, so a single
	// advance may fire the one beat too early (it finds no worker to
	// judge) or move the clock before the activity clock starts from it.
	// Each advance leaves the silence longer than the window again.
	stopped := e.waitJob(job, func(j *engine.JobInfo) bool {
		if j.Status == statusStalled {
			return true
		}
		clk.Advance(defaultStallAfter + time.Minute)
		return false
	})
	res := e.workerResult(stopped)
	if res.Status != statusStalled {
		t.Errorf("worker result = %+v, want status %q", res, statusStalled)
	}
	if !strings.Contains(res.Reason, durText(defaultStallAfter)) {
		t.Errorf("reason = %q, want the default window named", res.Reason)
	}
	e.assertNoWorktrees()
}

// TestRunnerToolCallSuspendsStallClock covers the suspension (§5.6): a
// worker silent inside an open tool call — a long build or test — is
// working, not stalled, and finishes its run fine.
func TestRunnerToolCallSuspendsStallClock(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", `test "$(cat work.txt)" = hello`)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt with hello in it.", "", ""))
	markerDir(t)

	// The stall window is a third of the call: without the suspension
	// the run would be stopped mid-call.
	p := e.startRunner(writeWorkerScript(t, longToolWorker), 100*time.Millisecond,
		func(c *Config) { c.StallAfter = 400 * time.Millisecond })
	defer p.stop(t)

	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.workerResult(done)
	if res.Status != statusDone || res.Branch != taskBranch(task, 1) {
		t.Errorf("worker result = %+v, want a run that finished fine", res)
	}
	if remoteBranchCommit(t, remote, taskBranch(task, 1)) == "" {
		t.Error("the suspended run's branch was never pushed")
	}
	e.assertNoWorktrees()
}

// TestRunnerToolCallOverCap covers the bound on the suspension (§5.6):
// a tool call open past the class's tool cap counts as stalled even
// though the clock is suspended.
func TestRunnerToolCallOverCap(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, longToolWorker), 100*time.Millisecond,
		func(c *Config) {
			c.StallAfter = 10 * time.Second // the call, not the silence, stops this run
			c.ClassCaps = map[string]ClassCaps{"work": {ToolCap: 300 * time.Millisecond}}
		})
	defer p.stop(t)

	stopped := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == statusStalled })
	res := e.workerResult(stopped)
	if res.Status != statusStalled {
		t.Fatalf("worker result = %+v, want status %q", res, statusStalled)
	}
	if !strings.Contains(res.Reason, "tool call") {
		t.Errorf("reason = %q, want the open tool call named", res.Reason)
	}
	if want := []string{"message starting", "tool_start Bash"}; !reflect.DeepEqual(res.Progress, want) {
		t.Errorf("progress events = %v, want %v", res.Progress, want)
	}
	e.assertNoWorktrees()
	if _, err := os.Stat(filepath.Join(markers, "finished")); err == nil {
		t.Fatal("the worker over the tool cap ran to the end; it was not killed")
	}
}

// TestRunnerClassTimeCap covers the class's wall-time cap (§5.6): a
// worker still inside its stall window is stopped when the run itself
// passes the cap, and the job completes with outcome time_cap.
func TestRunnerClassTimeCap(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, silentWorker), 100*time.Millisecond,
		func(c *Config) {
			c.StallAfter = 30 * time.Second // the cap, not the silence, stops this run
			c.ClassCaps = map[string]ClassCaps{"work": {TimeCap: 400 * time.Millisecond}}
		})
	defer p.stop(t)

	stopped := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == statusStalled })
	res := e.workerResult(stopped)
	if res.Status != statusTimeCap {
		t.Errorf("worker result = %+v, want status %q (the job's own status is stalled for either stop)", res, statusTimeCap)
	}
	if !strings.Contains(res.Reason, "class time cap") {
		t.Errorf("reason = %q, want the cap named", res.Reason)
	}
	e.assertNoWorktrees()
	if _, err := os.Stat(filepath.Join(markers, "finished")); err == nil {
		t.Fatal("the time-capped worker ran to the end; it was not killed")
	}
}

// TestRunnerStoppedRunPushesCommittedWork covers the no-work-lost rule
// (§5.6): a stopped run's committed work still reaches the result
// branch, so a worker that was killed mid-run loses nothing it did.
func TestRunnerStoppedRunPushesCommittedWork(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markerDir(t)

	p := e.startRunner(writeWorkerScript(t, stalledCommitter), 100*time.Millisecond,
		func(c *Config) { c.StallAfter = 400 * time.Millisecond })
	defer p.stop(t)

	stopped := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == statusStalled })
	res := e.workerResult(stopped)
	if res.Status != statusStalled {
		t.Fatalf("worker result = %+v, want status %q", res, statusStalled)
	}
	if res.Branch != taskBranch(task, 1) {
		t.Errorf("result branch = %q, want %s named in the outcome", res.Branch, taskBranch(task, 1))
	}
	if remoteBranchCommit(t, remote, taskBranch(task, 1)) == "" {
		t.Fatalf("the stalled run's committed work was never pushed")
	}
	clone := t.TempDir()
	git(t, "", "clone", "-q", "--branch", taskBranch(task, 1), remote, clone)
	if data, err := os.ReadFile(filepath.Join(clone, "work.txt")); err != nil || string(data) != "stalled work\n" {
		t.Errorf("the pushed branch does not hold the work: %q (%v)", data, err)
	}
	e.assertNoWorktrees()
}

// TestRunnerStallKillsProcessTree covers the kill itself (§5.6): the
// worker's whole process tree goes down — a child that ignored the
// SIGTERM dies to the SIGKILL after the grace — and no orphan is left
// holding the worktree.
func TestRunnerStallKillsProcessTree(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	// The activity clock is the manual one: it moves only on Advance, so
	// the silence window cannot expire before the markers below exist,
	// however long the shell takes under load. KillGrace stays on real
	// time, so the group kill itself is unchanged.
	clk := newManualClock()
	p := e.startRunner(writeWorkerScript(t, termProofWorker), 100*time.Millisecond,
		func(c *Config) {
			c.Clock = clk.Clock()
			c.StallAfter = 300 * time.Millisecond
			c.KillGrace = 400 * time.Millisecond
		})
	defer p.stop(t)

	e.waitUntil("the worker and its child to start", func() bool {
		_, errParent := os.Stat(filepath.Join(markers, "parent"))
		_, errChild := os.Stat(filepath.Join(markers, "child"))
		return errParent == nil && errChild == nil
	})
	parent := readMarkerPID(t, markers, "parent")
	child := readMarkerPID(t, markers, "child")

	// Advanced until the job stalls, as the manual-clock test does: the
	// first advance may land before the run counts the worker as watched.
	stopped := e.waitJob(job, func(j *engine.JobInfo) bool {
		if j.Status == statusStalled {
			return true
		}
		clk.Advance(time.Minute)
		return false
	})
	if res := e.workerResult(stopped); res.Status != statusStalled {
		t.Fatalf("worker result = %+v, want status %q", res, statusStalled)
	}

	// The whole tree is gone: not the worker alone — its child, which
	// trapped the SIGTERM, died to the post-grace SIGKILL.
	for name, pid := range map[string]int{"worker": parent, "child": child} {
		deadline := time.Now().Add(10 * time.Second)
		for !testsupport.ProcGone(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("the %s (pid %d) survived the group kill", name, pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if _, err := os.Stat(filepath.Join(markers, "finished")); err == nil {
		t.Fatal("the killed worker ran to the end")
	}
	e.assertNoWorktrees()
}

// TestRunnerStallKillsChildAfterLeaderExits covers the orphan the
// group kill exists for (§5.6): a worker that honors the SIGTERM and a
// child that ignores it and holds none of the worker's pipes. The
// leader dies and is reaped before the grace runs out — the run
// completes in between — and the child is still dead on schedule: the
// kill probes the group, not the reaped leader's pid.
func TestRunnerStallKillsChildAfterLeaderExits(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Take your time.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, termLoyalWorker), 100*time.Millisecond,
		func(c *Config) {
			c.StallAfter = 300 * time.Millisecond
			c.KillGrace = 400 * time.Millisecond
		})
	defer p.stop(t)

	e.waitUntil("the worker and its child to start", func() bool {
		_, errChild := os.Stat(filepath.Join(markers, "child"))
		_, errStarted := os.Stat(filepath.Join(markers, "started"))
		return errChild == nil && errStarted == nil
	})
	child := readMarkerPID(t, markers, "child")

	stopped := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == statusStalled })
	if res := e.workerResult(stopped); res.Status != statusStalled {
		t.Fatalf("worker result = %+v, want status %q", res, statusStalled)
	}

	// The child — alive in the group after its leader was reaped — dies
	// to the post-grace SIGKILL, not before it: it ignored the term.
	deadline := time.Now().Add(10 * time.Second)
	for !testsupport.ProcGone(child) {
		if time.Now().After(deadline) {
			t.Fatalf("the orphaned child (pid %d) survived the group kill", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.assertNoWorktrees()
}

// readMarkerPID reads a pid the worker wrote to its markers directory.
func readMarkerPID(t *testing.T, markers, name string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(markers, name))
	if err != nil {
		t.Fatalf("read the %s marker: %v", name, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("the %s marker holds no pid: %q", name, data)
	}
	return pid
}

// manualClock is a Clock whose time moves only when the test advances
// it (§5.6): minutes of stall window play out in microseconds of real
// time, and the heartbeats fire when the test says.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*manualTicker
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Now()}
}

// Clock returns the manual clock as the runner's Clock.
func (c *manualClock) Clock() Clock {
	return Clock{Now: c.Now, NewTicker: c.NewTicker}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTicker(time.Duration) Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	tk := &manualTicker{ch: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, tk)
	return tk
}

// Advance moves the clock by d and fires every ticker at the new time.
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	tickers := append([]*manualTicker(nil), c.tickers...)
	c.mu.Unlock()
	for _, tk := range tickers {
		select {
		case tk.ch <- now:
		default: // a beat still pending: the next advance carries it
		}
	}
}

type manualTicker struct {
	ch chan time.Time
}

func (t *manualTicker) Chan() <-chan time.Time { return t.ch }
func (t *manualTicker) Stop()                  {}
