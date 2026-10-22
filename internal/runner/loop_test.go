package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// The loop-watch tests (§5.6): the watch's arithmetic on injected
// times, the call identity, and the signal's ride through the heartbeat
// — with nothing killed.

// TestLoopWatchArithmetic is the watch on fixed times: loopCalls
// near-identical calls inside the window trip it, varied calls and an
// aged window do not.
func TestLoopWatchArithmetic(t *testing.T) {
	start := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return start.Add(time.Duration(min) * time.Minute) }
	var w loopWatch

	key := loopKeyHash("Read\x00{\"file_path\":\"a.go\"}")
	// Varied calls never trip, however many.
	for i := 0; i < 3*loopCalls; i++ {
		if n := w.record(loopKeyHash(fmt.Sprintf("call-%d", i)), at(i)); n >= loopCalls {
			t.Fatalf("varied call %d counted %d, want no loop", i, n)
		}
	}
	// The same call, loopCalls times inside the window, trips at the
	// last of them.
	w = loopWatch{}
	for i := 1; i < loopCalls; i++ {
		if n := w.record(key, at(i)); n != i {
			t.Fatalf("call %d counted %d", i, n)
		}
	}
	if n := w.record(key, at(loopCalls)); n != loopCalls {
		t.Fatalf("the %dth identical call counted %d, want %d", loopCalls, n, loopCalls)
	}
	// A window that aged out holds only the calls still inside it:
	// eight calls at minutes 0..7, a ninth at minute 12 leaves the
	// five of them at minutes 3..7 inside, and the ninth does not trip.
	w = loopWatch{}
	for i := 0; i < loopCalls; i++ {
		w.record(key, at(i))
	}
	if n := w.record(key, at(12)); n != 6 {
		t.Errorf("the aged window counted %d, want 6 (minutes 3..7 and the new call)", n)
	}
}

// TestLoopKeyNamesTheCall: the identity is the tool and its normalised
// arguments — whitespace collapsed, nothing else, for the arguments are
// what tells two calls of one tool apart.
func TestLoopKeyNamesTheCall(t *testing.T) {
	read := adapter.Event{Kind: adapter.EvToolStart, Text: "Read", Args: `{"file_path":"a.go"}`}
	if key, want := loopKey(read), "Read\x00"+`{"file_path":"a.go"}`; key != want {
		t.Errorf("loopKey = %q, want %q", key, want)
	}
	// Whitespace runs collapse to one space; the JSON's own formatting
	// stays what the CLI printed — a run's identity is stable within
	// the run, which is what the window compares.
	spaced := adapter.Event{Kind: adapter.EvToolStart, Text: "Read", Args: "{\n\"file_path\":\t\"a.go\"\n}"}
	joined := adapter.Event{Kind: adapter.EvToolStart, Text: "Read", Args: `{ "file_path": "a.go" }`}
	if loopKey(spaced) != loopKey(joined) {
		t.Errorf("whitespace runs changed the key: %q vs %q", loopKey(spaced), loopKey(joined))
	}
	other := adapter.Event{Kind: adapter.EvToolStart, Text: "Read", Args: `{"file_path":"b.go"}`}
	if loopKey(other) == loopKey(read) {
		t.Error("two different files read share a key")
	}
	otherTool := adapter.Event{Kind: adapter.EvToolStart, Text: "Grep", Args: `{"file_path":"a.go"}`}
	if loopKey(otherTool) == loopKey(read) {
		t.Error("two different tools share a key")
	}
	// A start with no arguments carries nothing that tells its calls
	// apart, so it counts at all: eight different calls of one tool
	// would share a bare name, and a loop they do not make.
	noArgs := adapter.Event{Kind: adapter.EvToolStart, Text: "Bash"}
	if key := loopKey(noArgs); key != "" {
		t.Errorf("a call with no arguments keyed as %q, want nothing counted", key)
	}
	if line := loopCallText(noArgs); line != "Bash" {
		t.Errorf("loopCallText of a bare call = %q, want the tool named", line)
	}
	if line := loopCallText(spaced); line != `Read { "file_path": "a.go" }` {
		t.Errorf("loopCallText = %q", line)
	}
	if line := loopCallText(adapter.Event{Kind: adapter.EvToolStart, Text: "Bash", Args: strings.Repeat("x", 500)}); len(line) != loopCallTextLimit {
		t.Errorf("loopCallText keeps %d bytes, want %d", len(line), loopCallTextLimit)
	}
}

// loopingWorker repeats one call — the same tool, the same arguments —
// eight times, then carries on and finishes: a busy loop the stall
// watch never sees, and a run the loop signal must not stop.
const loopingWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"looking at the task"}'
i=0
while [ $i -lt 8 ]; do
  echo '{"type":"tool_start","name":"Read","args":"{\"file_path\":\"work.txt\"}"}'
  echo '{"type":"tool_end","name":"Read"}'
  i=$((i+1))
done
: > "$BRIGADA_TEST_MARKERS/started"
sleep 0.15
br=$(git rev-parse --abbrev-ref HEAD)
printf '{"type":"result","text":"done looping: {\\"status\\":\\"done\\",\\"summary\\":\\"looped then finished\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
: > "$BRIGADA_TEST_MARKERS/finished"
`

// variedWorker reads eight different files: as many calls, no loop.
const variedWorker = `
cat > /dev/null
echo '{"type":"assistant","text":"looking at the task"}'
i=0
while [ $i -lt 8 ]; do
  printf '{"type":"tool_start","name":"Read","args":"{\\"file_path\\":\\"file-%d.go\\"}"}\n' "$i"
  echo '{"type":"tool_end","name":"Read"}'
  i=$((i+1))
done
br=$(git rev-parse --abbrev-ref HEAD)
printf '{"type":"result","text":"done reading: {\\"status\\":\\"done\\",\\"summary\\":\\"read eight files\\",\\"branch\\":\\"%s\\"}"}\n' "$br"
`

// TestRunnerReportsToolLoopWithoutKilling covers the signal end to end
// (§5.6): the repeated call trips the watch, the heartbeat carries the
// report, the engine wakes the foreman with the task_loop event — and
// the worker runs on to its done, killed by nothing.
func TestRunnerReportsToolLoopWithoutKilling(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Loop politely.", "", ""))
	markers := markerDir(t)

	p := e.startRunner(writeWorkerScript(t, loopingWorker), 100*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.workerResult(done)
	if res.Status != statusDone || !strings.Contains(res.Summary, "looped then finished") {
		t.Fatalf("worker result = %+v, want a run that finished its loop", res)
	}
	if _, err := os.Stat(filepath.Join(markers, "finished")); err != nil {
		t.Fatal("the looping worker was killed: a loop stops nothing (§5.6)")
	}
	if n := e.int64Of("SELECT COUNT(*) FROM events WHERE kind = 'task_loop' AND job_id = ?", job); n != 1 {
		t.Errorf("task_loop events = %d, want the one report", n)
	}
	e.assertNoWorktrees()
}

// TestRunnerVariedCallsAreNoLoop: eight calls of one tool with different
// arguments are eight different calls (§5.6): no report, no event, a
// run that simply finishes.
func TestRunnerVariedCallsAreNoLoop(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Read eight files.", "", ""))

	p := e.startRunner(writeWorkerScript(t, variedWorker), 100*time.Millisecond)
	defer p.stop(t)

	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.workerResult(done); res.Status != statusDone {
		t.Fatalf("worker result = %+v, want a plain done", res)
	}
	if n := e.int64Of("SELECT COUNT(*) FROM events WHERE kind = 'task_loop'"); n != 0 {
		t.Errorf("task_loop events = %d, want none: varied calls are no loop", n)
	}
	e.assertNoWorktrees()
}
