package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The land_ready gate's engine side (issue #87), part 2: the outcomes a
// land job completes with, their payloads, and the not-ready path's
// requeue on the not-before backoff. The gate's command itself is the
// runner's; these tests drive the completions it would deliver.

// landHeld completes g as the gate's hold (issue #87): exit 1, the
// command's first output line the reason.
func landHeld(line string) Completion {
	return Completion{
		Result:  fmt.Sprintf(`{"outcome":"held","line":%q}`, line),
		Outcome: OutcomeLandHeld,
	}
}

// landNotReady completes g as the gate's not-ready (issue #87): the
// command's last exit and a short reason.
func landNotReady(exit int, reason string) Completion {
	return Completion{
		Result:  fmt.Sprintf(`{"outcome":"not ready","exit":%d,"reason":%q}`, exit, reason),
		Outcome: OutcomeLandNotReady,
	}
}

// TestLandNotReadyBacksOffAndStaysLandable: a not-ready completion ends
// the land job done — no failure, no restart spent, the task keeps its
// done state — and requeues the landing on the not-before backoff
// (retryDelayFor): invisible to a lease before its time, leasable
// after, the delay grown by each consecutive refusal, and a later
// landing of the same job shape completes as any landing does. The next
// try time and the last exit are kept on the task for the status.
func TestLandNotReadyBacksOffAndStaysLandable(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		t.Fatalf("the land job was not leased: %+v", g)
	}
	if ans := e.complete(g, landNotReady(2, "")); ans != AnsOK {
		t.Fatalf("complete the not-ready land job: %s", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusDone {
		t.Errorf("land job status = %q, want done: not ready is the gate's answer, not a failure", status)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 0 {
		t.Errorf("restart jobs = %d, want none spent on a not-ready answer", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done: the task stays landable", state)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records = %d, want none: nothing landed", n)
	}
	// The retry waits out the backoff's first step (retryDelayFor).
	want := fmtTime(e.clock.Now().Add(retryDelayFor(1)))
	if got := e.str(`SELECT not_before FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task); got != want {
		t.Errorf("the retry's not_before = %q, want %q", got, want)
	}
	if got := e.str("SELECT land_retry_at FROM tasks WHERE id = ?", e.task); got != want {
		t.Errorf("task land_retry_at = %q, want %q", got, want)
	}
	if got := e.int("SELECT land_ready_exit FROM tasks WHERE id = ?", e.task); got != 2 {
		t.Errorf("task land_ready_exit = %d, want 2", got)
	}
	if p, ok := e.eventPayloadOf(evTaskLandNotReady); !ok || p.ExitCode != 2 || p.Until != want {
		t.Errorf("task_land_not_ready event = %+v (%t), want exit 2 and the next try %q", p, ok, want)
	}
	// The backoff holds the retry out of leasing until its time comes.
	if g := e.leaseGit(e.runner2); g != nil {
		t.Fatalf("the land retry was leased before its not-before: %+v", g)
	}
	e.clock.Advance(retryDelayFor(1) + time.Second)
	g2 := e.leaseGit(e.runner2)
	if g2 == nil || g2.Kind != KindLand {
		t.Fatalf("the land retry was not leasable after its not-before: %+v", g2)
	}
	// A second refusal grows the delay to the backoff's second step.
	if ans := e.complete(g2, landNotReady(3, "")); ans != AnsOK {
		t.Fatalf("complete the second not-ready: %s", ans)
	}
	want2 := fmtTime(e.clock.Now().Add(retryDelayFor(2)))
	if got := e.str(`SELECT not_before FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task); got != want2 {
		t.Errorf("the second retry's not_before = %q, want %q at the grown delay", got, want2)
	}
	if got := e.int("SELECT land_ready_exit FROM tasks WHERE id = ?", e.task); got != 3 {
		t.Errorf("task land_ready_exit = %d, want 3, the last exit", got)
	}
	// The gate's yes finally lands: the retry after the grown backoff
	// completes the landing like any land job.
	e.clock.Advance(retryDelayFor(2) + time.Second)
	g3 := e.leaseGit(e.runner2)
	if g3 == nil {
		t.Fatal("the land retry never became leasable")
	}
	if ans := e.complete(g3, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the landing: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "pushed" {
		t.Errorf("landing pushed = %q, want pushed: the task stayed landable through the backoff", pushed)
	}
}

// TestLandHeldHoldsAtTheGate: a held completion — the gate's exit 1 —
// holds the task at the land gate with the command's first output line
// as the reason, exactly as a land-gate rule holds it (§6.3): the task
// stays done, nothing landed or pushed, the job done, and the needs-you
// line names the rule and the command that releases it.
func TestLandHeldHoldsAtTheGate(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, landHeld("the release window is closed")); ans != AnsOK {
		t.Fatalf("complete the held land job: %s", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusDone {
		t.Errorf("land job status = %q, want done", status)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done: only the landing waits", state)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records = %d, want none: the gate held the landing", n)
	}
	reason := e.str("SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'", e.task)
	if reason != "held by rule land_ready: the release window is closed" {
		t.Errorf("land hold reason = %q, want the land_ready rule and the command's line", reason)
	}
	p, ok := e.eventPayloadOf(evTaskLandHeld)
	if !ok || !strings.Contains(p.Reason, "held by rule land_ready") || !strings.Contains(p.Reason, "the release window is closed") {
		t.Errorf("task_land_held event = %+v (%t), want the hold and the command's line", p, ok)
	}
	it, ok := e.needsYouTask(e.task)
	if !ok || !strings.Contains(it.Reason, "held by rule land_ready: the release window is closed") || !strings.Contains(it.Reason, "brigada land approve") {
		t.Errorf("needs-you entry = %+v (%t), want the hold with its release command", it, ok)
	}
}

// TestLandApproveThroughTheHold: the gate's exit 1 holds the landing,
// `brigada land approve` releases it, and the re-queued land job carries
// the gate as skipped — the runner runs no command and the landing
// completes — while a held task stays put until that approval: re-running
// the land rule queues nothing and no landing is recorded.
func TestLandApproveThroughTheHold(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingLandReady: ".brigada/land-ready.sh"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, landHeld("the release window is closed")); ans != AnsOK {
		t.Fatalf("complete the held land job: %s", ans)
	}
	if n := e.landJobs(); n != 1 {
		t.Fatalf("land jobs = %d, want only the held one", n)
	}
	// The hold stands on its own: re-running the rule queues nothing, and
	// nothing lands until the user approves.
	e.maybeLand(e.task)
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", e.task); n != 0 {
		t.Errorf("queued land jobs while held = %d, want none: the hold has no job", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records while held = %d, want none", n)
	}

	res, err := e.eng.LandApprove(e.ctx, e.project, e.task, false)
	if err != nil {
		t.Fatalf("LandApprove: %v", err)
	}
	if !res.Released || !res.Queued || res.JobID == 0 {
		t.Fatalf("LandApprove = %+v, want released with its land job queued", res)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); n != 0 {
		t.Errorf("held land_holds rows after approve = %d, want none", n)
	}

	// The re-queued landing carries the gate as skipped: the command does
	// not run again, and the landing completes on the skip.
	g2 := e.leaseGit(author)
	if g2 == nil || g2.Kind != KindLand {
		t.Fatalf("the re-queued land job was not leased: %+v", g2)
	}
	if g2.LandReady != ".brigada/land-ready.sh" || !g2.SkipLandReady {
		t.Errorf("re-queued grant land_ready/skip = %q/%v, want the command with skip set", g2.LandReady, g2.SkipLandReady)
	}
	if ans := e.complete(g2, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the released landing: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "pushed" {
		t.Errorf("landing pushed = %q, want pushed", pushed)
	}
	// The hold's needs-you line is gone once the landing is recorded.
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry = %+v, want none for a landed task", it)
	}
}

// statusTaskLandReady reads the task's land_ready state from the status.
func (e *env) statusTaskLandReady(task int64) string {
	e.t.Helper()
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	for _, p := range info.Plans {
		for _, t := range p.Tasks {
			if t.ID == task {
				return t.LandReady
			}
		}
	}
	return ""
}

// TestStatusShowsTheLandReadyState: the status carries the land_ready
// gate's target state per task (issue #87): "ready" while the land job
// is queued, "held by rule land_ready: <line>" once the gate's exit 1
// held it, and "waiting (retry at <time>, last exit <n>)" while the
// not-ready backoff waits out.
func TestStatusShowsTheLandReadyState(t *testing.T) {
	// ready: the land job queued at done reads ready.
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	if got := e.statusTaskLandReady(e.task); got != "ready" {
		t.Errorf("land_ready state while the land job is queued = %q, want ready", got)
	}

	// held: the gate's exit 1 holds, naming the rule and the line.
	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e.complete(g, landHeld("the release window is closed")); ans != AnsOK {
		t.Fatalf("complete the held land job: %s", ans)
	}
	if got := e.statusTaskLandReady(e.task); got != "held by rule land_ready: the release window is closed" {
		t.Errorf("land_ready state while held = %q, want the hold's rule and line", got)
	}

	// waiting: the gate's not-ready answer leaves the landing on its
	// backoff, the next try time and the last exit beside it.
	e2 := newEnv(t)
	author2 := e2.agented("author", "anthropic", "implement")
	reviewer2 := e2.agented("reviewer", "google", "implement", "review")
	e2.runToDone(author2, reviewer2)
	g2 := e2.leaseGit(author2)
	if g2 == nil {
		t.Fatal("the land job was not leased")
	}
	if ans := e2.complete(g2, landNotReady(2, "")); ans != AnsOK {
		t.Fatalf("complete the not-ready land job: %s", ans)
	}
	want := fmtTime(e2.clock.Now().Add(retryDelayFor(1)))
	if got := e2.statusTaskLandReady(e2.task); got != "waiting (retry at "+want+", last exit 2)" {
		t.Errorf("land_ready state while waiting = %q, want the retry and the exit", got)
	}
}

// TestLandApproveThroughThePushHold: the gate's exit 1 in the push phase
// — the landing already recorded waiting — holds the push, and
// `land approve` releases it by re-queueing the push with the gate
// skipped, so the recorded commit is pushed without re-running the
// command.
func TestLandApproveThroughThePushHold(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk, SettingLandReady: ".brigada/land-ready.sh"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	// The landing passes the gate and waits at the push gate.
	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		t.Fatalf("the land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}

	// The push phase re-runs the gate; its exit 1 holds the push.
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatalf("PushApprove: %v", err)
	}
	pg := e.leaseGit(author)
	if pg == nil || !pg.Push {
		t.Fatalf("the push job was not leased: %+v", pg)
	}
	if ans := e.complete(pg, landHeld("the release window is closed")); ans != AnsOK {
		t.Fatalf("complete the held push job: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "waiting" {
		t.Errorf("landing pushed = %q, want waiting: the gate held the push", pushed)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); reason != "held by rule land_ready: the release window is closed" {
		t.Errorf("land hold reason = %q, want the land_ready rule and line", reason)
	}
	it, ok := e.needsYouTask(e.task)
	if !ok || !strings.Contains(it.Reason, "held by rule land_ready") {
		t.Fatalf("needs-you entry = %+v (%t), want the push-phase hold", it, ok)
	}
	if strings.Contains(it.Reason, "waits to push") {
		t.Errorf("needs-you entry = %q, want the hold, not the plain push wait", it.Reason)
	}
	if got := e.statusTaskLandReady(e.task); got != "held by rule land_ready: the release window is closed" {
		t.Errorf("land_ready state while the push holds = %q, want the hold", got)
	}

	// The approval releases the hold by re-queueing the push, skipped.
	res, err := e.eng.LandApprove(e.ctx, e.project, e.task, false)
	if err != nil {
		t.Fatalf("LandApprove: %v", err)
	}
	if !res.Released || !res.Queued || res.JobID == 0 {
		t.Fatalf("LandApprove = %+v, want released with its push job queued", res)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); n != 0 {
		t.Errorf("held land_holds rows after approve = %d, want none", n)
	}
	pg2 := e.leaseGit(author)
	if pg2 == nil || !pg2.Push || !pg2.SkipLandReady {
		t.Fatalf("the re-queued push grant = %+v, want a push with the gate skipped", pg2)
	}
	if ans := e.complete(pg2, Completion{Result: `{"target_branch":"main","commit":"abc123","pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the released push: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "pushed" {
		t.Errorf("landing pushed = %q, want pushed", pushed)
	}
}

// TestLandOutcomeBelongsToALandJob: the gate's outcomes pass the
// completion contract's validation, but only a land job may carry them;
// a work job completing with one is a caller's mistake, refused without
// ending the job.
func TestLandOutcomeBelongsToALandJob(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("the work job was not leased")
	}
	if _, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, landNotReady(2, "")); err == nil || !strings.Contains(err.Error(), "land job") {
		t.Errorf("a work job completing not ready = %v, want a refusal naming the land job", err)
	}
}

// TestLandReadyRidesThePushGrant: the push phase's grant carries
// land_ready like the landing's (issue #87): the gate guards the push
// of a released commit as it guarded the landing.
func TestLandReadyRidesThePushGrant(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk, SettingLandReady: ".brigada/land-ready.sh"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil || g.LandReady != ".brigada/land-ready.sh" {
		t.Fatalf("the land grant = %+v, want the land_ready command", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatalf("PushApprove: %v", err)
	}
	pg := e.leaseGit(author)
	if pg == nil || !pg.Push {
		t.Fatalf("the push grant = %+v, want a push phase", pg)
	}
	if pg.LandReady != ".brigada/land-ready.sh" {
		t.Errorf("push grant land_ready = %q, want the command", pg.LandReady)
	}
}
