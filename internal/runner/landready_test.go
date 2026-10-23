package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The land_ready gate on the runner (issue #87): the repo-relative
// command a land job runs before landing, one test per answer it can
// give — exit 0, exit 1 (held), exit 2, another exit, a timeout — and
// the grant's skip flag, plus the push phase the gate also guards. The
// command lives on the remote's main, so the landing clone's fresh
// checkout holds it; its exit is steered through a file in the test's
// markers directory, whose path is baked into the script (the gate runs
// under the scrubbed environment of a check, §5.8, which carries none
// of the test's variables).

// landReadyRemote is newRemote with .brigada/land-ready.sh on main: the
// gate's command, seeded as one commit before the runner starts.
func landReadyRemote(t *testing.T, body string) string {
	t.Helper()
	remote := newRemote(t)
	scratch := t.TempDir()
	git(t, "", "clone", "-q", "--branch", "main", remote, scratch)
	if err := os.MkdirAll(filepath.Join(scratch, ".brigada"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(filepath.Join(scratch, ".brigada", "land-ready.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, scratch, "add", "-A")
	git(t, scratch, "-c", "user.name=seed", "-c", "user.email=seed@brigada.invalid",
		"commit", "-qm", "seed the land_ready gate")
	git(t, scratch, "push", "-q", "origin", "main")
	return remote
}

// landReadyBody is the gate's steered script: it always leaves its ran
// mark, echoes its hold line on exit 1, and exits with the number the
// exit file holds (0, the ready answer, when it holds none).
func landReadyBody(markers string) string {
	return `: > '` + markers + `/ran'
n=$(cat '` + markers + `/exit' 2>/dev/null || echo 0)
if [ "$n" = 1 ]; then echo "the release window is closed"; fi
exit "$n"
`
}

// setLandReadyExit steers the gate's answer.
func setLandReadyExit(t *testing.T, markers string, exit int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(markers, "exit"), []byte(strconv.Itoa(exit)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// landReadySettings is the project settings of every gate test: the
// command's path and a pushing gate, unless a test says otherwise.
const landReadySettings = `{"gate_push":"auto","land_ready":".brigada/land-ready.sh"}`

// waitForLandJob waits for the task's first land job to exist — the
// review's approval queues it — and returns its id.
func (e *testEnv) waitForLandJob(task int64) int64 {
	e.t.Helper()
	e.waitUntil("the land job to be queued", func() bool {
		return e.landJobID(task) != 0
	})
	return e.landJobID(task)
}

// TestRunnerLandReadyPassLands: exit 0 is the gate's yes — the command
// runs in the landing clone before anything lands, and the landing
// proceeds to the push.
func TestRunnerLandReadyPassLands(t *testing.T) {
	t.Parallel()
	markers := markerDir(t)
	setLandReadyExit(t, markers, 0)
	remote := landReadyRemote(t, landReadyBody(markers))
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, landReadySettings,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	commit := e.waitLanding(task, "pushed")
	// The gate really ran: its mark is there, though the landing alone
	// would also follow a skipped gate.
	if _, err := os.Stat(filepath.Join(markers, "ran")); err != nil {
		t.Errorf("the land_ready command never ran: %v", err)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != commit {
		t.Errorf("remote main = %s, want the landing commit %s", got, commit)
	}
}

// TestRunnerLandReadyHeldDoesNotLand: exit 1 is the gate's own hold —
// nothing is squashed, pushed or recorded — and the job completes with
// the outcome held and the command's first output line.
func TestRunnerLandReadyHeldDoesNotLand(t *testing.T) {
	t.Parallel()
	markers := markerDir(t)
	setLandReadyExit(t, markers, 1)
	remote := landReadyRemote(t, landReadyBody(markers))
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, landReadySettings,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	land := e.waitForLandJob(task)
	e.waitJob(land, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.strOf("SELECT result FROM jobs WHERE id = ?", land); !strings.Contains(res, "held") {
		t.Errorf("the land job's result = %s, want the held outcome", res)
	}
	if _, _, _, _, ok := e.landing(task); ok {
		t.Error("a landing was recorded past the gate's hold")
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Errorf("main moved past the gate's hold: %s, want the seed %s", got, seed)
	}
	// The hold's line — the command's first output — rides the payload.
	if res := e.strOf("SELECT result FROM jobs WHERE id = ?", land); !strings.Contains(res, "the release window is closed") {
		t.Errorf("the held record lacks the command's first line: %s", res)
	}
}

// TestRunnerLandReadyNotReadyRetriesAndLands: exit 2 is not ready —
// the job completes done with the outcome and the exit, the task parks
// nowhere, and the requeued landing runs again once the gate says yes.
func TestRunnerLandReadyNotReadyRetriesAndLands(t *testing.T) {
	t.Parallel()
	markers := markerDir(t)
	setLandReadyExit(t, markers, 2)
	remote := landReadyRemote(t, landReadyBody(markers))
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, landReadySettings,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	land := e.waitForLandJob(task)
	e.waitJob(land, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.strOf("SELECT result FROM jobs WHERE id = ?", land)
	if !strings.Contains(res, "not ready") || !strings.Contains(res, `exit\":2`) {
		t.Errorf("the land job's result = %s, want the not-ready outcome with exit 2", res)
	}
	if state := e.strOf("SELECT state FROM tasks WHERE id = ?", task); state != "done" {
		t.Errorf("task state = %q, want done: not ready fails nothing", state)
	}
	if n := e.int64Of("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", task); n != 0 {
		t.Errorf("restart jobs = %d, want none spent", n)
	}
	// The gate turns ready; the requeued landing lands.
	setLandReadyExit(t, markers, 0)
	e.waitLanding(task, "pushed")
}

// TestRunnerLandReadyOtherExitIsNotReady: any exit but 0, 1 and 2 — 3
// here — is not ready like exit 2 is, the exit carried in the record.
func TestRunnerLandReadyOtherExitIsNotReady(t *testing.T) {
	t.Parallel()
	markers := markerDir(t)
	setLandReadyExit(t, markers, 3)
	remote := landReadyRemote(t, landReadyBody(markers))
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, landReadySettings,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	land := e.waitForLandJob(task)
	e.waitJob(land, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.strOf("SELECT result FROM jobs WHERE id = ?", land); !strings.Contains(res, "not ready") || !strings.Contains(res, `exit\":3`) {
		t.Errorf("the land job's result = %s, want the not-ready outcome with exit 3", res)
	}
	if _, _, _, _, ok := e.landing(task); ok {
		t.Error("a landing was recorded past a not-ready gate")
	}
	setLandReadyExit(t, markers, 0)
	e.waitLanding(task, "pushed")
}

// TestRunnerLandReadyTimeoutIsNotReady: a gate that outruns the check
// timeout — the after_land convention (§6.3) — is not ready, the record
// carrying the timeout as its reason; nothing lands.
func TestRunnerLandReadyTimeoutIsNotReady(t *testing.T) {
	t.Parallel()
	remote := landReadyRemote(t, "sleep 30\n")
	e := newTestEnv(t, engine.Config{})
	task, work := e.landTest(t, remote, landReadySettings,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult),
		func(c *Config) { c.CheckTimeout = 300 * time.Millisecond })

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	land := e.waitForLandJob(task)
	e.waitJob(land, func(j *engine.JobInfo) bool { return j.Status == "done" })
	res := e.strOf("SELECT result FROM jobs WHERE id = ?", land)
	if !strings.Contains(res, "not ready") || !strings.Contains(res, "timed out") {
		t.Errorf("the land job's result = %s, want the not-ready outcome with the timeout", res)
	}
	if _, _, _, _, ok := e.landing(task); ok {
		t.Error("a landing was recorded past a timed-out gate")
	}
}

// TestRunnerLandReadyGuardsThePushPhase: under gate_push ask the
// landing passes a ready gate and waits; the push phase — the released
// commit — runs the same gate, and its hold leaves the landing waiting
// and the target unmoved.
func TestRunnerLandReadyGuardsThePushPhase(t *testing.T) {
	t.Parallel()
	markers := markerDir(t)
	setLandReadyExit(t, markers, 0)
	remote := landReadyRemote(t, landReadyBody(markers))
	seed := remoteBranchCommit(t, remote, "main")
	e := newTestEnv(t, engine.Config{})
	settings := `{"gate_push":"ask","land_ready":".brigada/land-ready.sh"}`
	task, work := e.landTest(t, remote, settings,
		"Add work.txt with hello in it.", `test "$(cat work.txt)" = hello`, reviewScript(reviewApproveResult))

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	commit := e.waitLanding(task, "waiting")
	// The gate goes red before the push is released.
	setLandReadyExit(t, markers, 1)
	if _, err := e.eng.PushApprove(e.ctx, remote); err != nil {
		t.Fatalf("PushApprove: %v", err)
	}
	push := e.int64Of("SELECT MAX(id) FROM jobs WHERE task_id = ? AND kind = 'land'", task)
	e.waitJob(push, func(j *engine.JobInfo) bool { return j.Status == "done" })
	if res := e.strOf("SELECT result FROM jobs WHERE id = ?", push); !strings.Contains(res, "held") {
		t.Errorf("the push job's result = %s, want the held outcome", res)
	}
	if pushed, _, _, _, _ := e.landing(task); pushed != "waiting" {
		t.Errorf("landing pushed = %q, want waiting: the gate held the push", pushed)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Errorf("main moved past the gate's hold: %s, want the seed %s", got, seed)
	}
	if got := remoteRefCommit(t, remote, landWaitRef("main")); got != commit {
		t.Errorf("the waiting ref = %q, want the recorded commit %s", got, commit)
	}
}

// TestLandReadySkipFlagSkipsTheCommand: a grant flagged
// skip_land_ready runs no gate — the engine's mark that an approval
// already released the hold (issue #87, part 3) — and the landing
// proceeds whatever the command would say.
func TestLandReadySkipFlagSkipsTheCommand(t *testing.T) {
	t.Parallel()
	markers := markerDir(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gate.sh"), []byte(": > '"+markers+"/ran'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &jobRun{
		cfg:   &Config{CheckTimeout: time.Second},
		grant: &api.JobGrant{LandReady: "./gate.sh", SkipLandReady: true},
	}
	if !r.runLandReady(context.Background(), dir) {
		t.Fatal("the gate held the landing though the grant skips it")
	}
	if _, err := os.Stat(filepath.Join(markers, "ran")); err == nil {
		t.Error("the skipped gate command ran")
	}
}
