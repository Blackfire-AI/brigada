package engine

import (
	"testing"
	"time"
)

// The engine's re-check of a land_ready hold (issue #142): the gate's
// exit 1 is a red CI run, and GitHub keeps a failed run rerunnable for
// weeks, so the hold is never final. The engine queues the landing's
// next land job not before landReadyRecheckDelay, running the command
// again exactly as the landing ran it; these tests drive the
// completions the re-check would deliver, as landready_test.go drives
// the gate's own.

// holdAtTheGate drives the fixture's task to done and holds its landing
// at the land_ready gate with the given first line, returning the author
// runner's id. The engine's re-check is queued and waiting out its
// interval when this returns.
func holdAtTheGate(t *testing.T, line string) (*env, int64) {
	t.Helper()
	e := newEnv(t)
	e.set(map[string]string{SettingLandReady: ".brigada/land-ready.sh"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		t.Fatalf("the land job was not leased: %+v", g)
	}
	if ans := e.complete(g, landHeld(line)); ans != AnsOK {
		t.Fatalf("complete the held land job: %s", ans)
	}
	return e, author
}

// theRecheck leases the task's queued re-check once its interval has
// passed, failing the test when there is none to lease; the clock is
// advanced past the interval first, so a re-check the engine never
// queued reads as the missing grant it is.
func theRecheck(e *env, owner int64) *LeaseGrant {
	e.t.Helper()
	e.clock.Advance(landReadyRecheckDelay + time.Second)
	g := e.leaseGit(owner)
	if g == nil || g.Kind != KindLand {
		e.t.Fatalf("the re-check was not leased: %+v", g)
	}
	return g
}

// TestRecheckReleasesTheHoldWhenTheGatePasses: a held landing whose gate
// exits 1 then 0 — the run turned green on attempt 2 — lands with no
// approve: the re-check runs the command again exactly as a land job
// does, and its exit 0 releases the hold itself, with a history event
// distinct from the user's release, while the landing keeps every other
// gate.
func TestRecheckReleasesTheHoldWhenTheGatePasses(t *testing.T) {
	e, _ := holdAtTheGate(t, "the release window is closed")

	// The re-check waits out its interval, invisible to a lease before
	// it, and its grant runs the command — nothing skipped.
	if g := e.leaseGit(e.runner2); g != nil {
		t.Fatalf("the re-check was leased before its interval: %+v", g)
	}
	g2 := theRecheck(e, e.runner2)
	if g2.LandReady != ".brigada/land-ready.sh" || g2.SkipLandReady {
		t.Errorf("re-check grant land_ready/skip = %q/%v, want the command run, not skipped", g2.LandReady, g2.SkipLandReady)
	}
	if ans := e.complete(g2, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the re-check's landing: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "pushed" {
		t.Errorf("landing pushed = %q, want pushed: the re-check released the hold and landed", pushed)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("land holds after the re-check passed = %d, want none", n)
	}
	// The engine's release is its own event, not the user's.
	if n := e.countEvents(evTaskLandRecheckReleased); n != 1 {
		t.Errorf("task_land_recheck_released events = %d, want 1", n)
	}
	if n := e.countEvents(evTaskLandReleased); n != 0 {
		t.Errorf("task_land_released events = %d, want none: no user released anything", n)
	}
	// The landing settled: no re-check is left waiting.
	if n := e.int(`SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task); n != 0 {
		t.Errorf("queued land jobs after the landing = %d, want none", n)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != planDone {
		t.Errorf("plan state = %q, want done: the landing finished it", state)
	}
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry = %+v, want none for a landed task", it)
	}
}

// TestRecheckKeepsTheHoldWhenTheGateRefusesAgain: the re-check's exit 1
// keeps the hold — the task still done, nothing landed — moves its
// reason to the command's new first line only when the line moved, and
// queues the next re-check after the interval.
func TestRecheckKeepsTheHoldWhenTheGateRefusesAgain(t *testing.T) {
	e, _ := holdAtTheGate(t, "the release window is closed")

	// The same line again: the hold is unchanged and records nothing.
	g2 := theRecheck(e, e.runner2)
	if ans := e.complete(g2, landHeld("the release window is closed")); ans != AnsOK {
		t.Fatalf("complete the re-check's refusal: %s", ans)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); reason != "held by rule land_ready: the release window is closed" {
		t.Errorf("land hold reason = %q, want the unchanged line", reason)
	}
	if n := e.countEvents(evTaskLandHeld); n != 1 {
		t.Errorf("task_land_held events = %d, want the one from the first hold: an unchanged line records nothing", n)
	}
	want := fmtTime(e.clock.Now().Add(landReadyRecheckDelay))
	if got := e.str(`SELECT not_before FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task); got != want {
		t.Errorf("the next re-check's not_before = %q, want %q, a full interval later", got, want)
	}

	// A new line: the reason moves and the move is recorded.
	g3 := theRecheck(e, e.runner2)
	if ans := e.complete(g3, landHeld("the CI run for the head failed")); ans != AnsOK {
		t.Fatalf("complete the second refusal: %s", ans)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); reason != "held by rule land_ready: the CI run for the head failed" {
		t.Errorf("land hold reason = %q, want the command's new first line", reason)
	}
	if n := e.countEvents(evTaskLandHeld); n != 2 {
		t.Errorf("task_land_held events = %d, want 2: the moved line is recorded", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done: the hold keeps only the landing waiting", state)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records = %d, want none", n)
	}
	if n := e.int(`SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task); n != 1 {
		t.Errorf("queued land jobs = %d, want the one next re-check", n)
	}
}

// TestRecheckQueuesAgainOnAnyOtherExit: a re-check the gate answers not
// ready keeps the hold and requeues the landing on the not-ready
// backoff, whose retry is the next re-check of the same cycle.
func TestRecheckQueuesAgainOnAnyOtherExit(t *testing.T) {
	e, _ := holdAtTheGate(t, "the release window is closed")

	g2 := theRecheck(e, e.runner2)
	if ans := e.complete(g2, landNotReady(2, "")); ans != AnsOK {
		t.Fatalf("complete the not-ready re-check: %s", ans)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); reason != "held by rule land_ready: the release window is closed" {
		t.Errorf("land hold reason = %q, want the hold kept as it was", reason)
	}
	// The backoff's retry is the next re-check, leasable after its
	// shorter delay, and the hold still outranks it in the status.
	want := fmtTime(e.clock.Now().Add(retryDelayFor(1)))
	if got := e.str(`SELECT not_before FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task); got != want {
		t.Errorf("the retry's not_before = %q, want %q on the backoff", got, want)
	}
	e.clock.Advance(retryDelayFor(1) + time.Second)
	g3 := e.leaseGit(e.runner2)
	if g3 == nil || g3.Kind != KindLand || g3.SkipLandReady {
		t.Fatalf("the backoff's re-check = %+v, want a land job that runs the command", g3)
	}
	if got := e.statusTaskLandReady(e.task); got != "held by rule land_ready: the release window is closed" {
		t.Errorf("land_ready state under the hold = %q, want the hold, not the backoff", got)
	}
}

// TestApproveDuringAPendingRecheckLandsOnce: `brigada land approve`
// while a re-check is pending cancels the re-check and lands exactly
// once, with the gate skipped — the user's yes replaces the command.
func TestApproveDuringAPendingRecheckLandsOnce(t *testing.T) {
	e, author := holdAtTheGate(t, "the release window is closed")
	recheck := e.int(`SELECT id FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task)

	res, err := e.eng.LandApprove(e.ctx, e.project, e.task, false)
	if err != nil {
		t.Fatalf("LandApprove: %v", err)
	}
	if !res.Released || !res.Queued || res.JobID == 0 || res.JobID == recheck {
		t.Fatalf("LandApprove = %+v, want released with its own skip job queued, not the re-check", res)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", recheck); status != statusCancelled {
		t.Errorf("the pending re-check = %s, want cancelled by the approval", status)
	}
	g2 := e.leaseGit(author)
	if g2 == nil || g2.Kind != KindLand || !g2.SkipLandReady || g2.JobID != res.JobID {
		t.Fatalf("the approved landing's job = %+v, want the skip job", g2)
	}
	if ans := e.complete(g2, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the approved landing: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 1 {
		t.Errorf("landing records = %d, want exactly one", n)
	}
	if n := e.int(`SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status IN ('queued', 'leased', 'running')`, e.task); n != 0 {
		t.Errorf("live land jobs after the landing = %d, want none: the landing happened once", n)
	}
}

// TestRejectCancelsThePendingRecheck: `brigada land reject` while a
// re-check is pending declines the landing and cancels the re-check: no
// job of the engine's may land a task the user refused.
func TestRejectCancelsThePendingRecheck(t *testing.T) {
	e, _ := holdAtTheGate(t, "the release window is closed")

	res, err := e.eng.LandReject(e.ctx, e.project, e.task)
	if err != nil {
		t.Fatalf("LandReject: %v", err)
	}
	if res.Released {
		t.Fatalf("LandReject = %+v, want the landing declined", res)
	}
	if state := e.str("SELECT state FROM land_holds WHERE task_id = ?", e.task); state != "rejected" {
		t.Errorf("land hold state = %q, want rejected", state)
	}
	if n := e.int(`SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status IN ('queued', 'leased', 'running')`, e.task); n != 0 {
		t.Errorf("live land jobs after the rejection = %d, want none: the re-check is cancelled", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records = %d, want none: the branch stays", n)
	}
}

// TestRecheckReleasesThePushPhaseHold: the push phase's land_ready hold
// — the landing already recorded waiting — is re-checked the same way,
// and the re-check's exit 0 pushes the recorded commit, releases the
// hold with the engine's own event, and leaves no re-check behind.
func TestRecheckReleasesThePushPhaseHold(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk, SettingLandReady: ".brigada/land-ready.sh"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		t.Fatalf("the land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatalf("PushApprove: %v", err)
	}
	pg := e.leaseGit(author)
	if pg == nil || !pg.Push {
		t.Fatalf("the push job was not leased: %+v", pg)
	}
	if ans := e.complete(pg, landHeld("the release window is closed")); ans != AnsOK {
		t.Fatalf("hold the push: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "waiting" {
		t.Fatalf("landing pushed under the push hold = %q, want waiting", pushed)
	}

	// The re-check queues for the push as for the landing, and its
	// grant is the push phase's: the recorded commit, the gate not
	// skipped.
	want := fmtTime(e.clock.Now().Add(landReadyRecheckDelay))
	if got := e.str(`SELECT not_before FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task); got != want {
		t.Fatalf("the push re-check's not_before = %q, want %q", got, want)
	}
	e.clock.Advance(landReadyRecheckDelay + time.Second)
	pg2 := e.leaseGit(author)
	if pg2 == nil || !pg2.Push || pg2.SkipLandReady || pg2.LandReady != ".brigada/land-ready.sh" {
		t.Fatalf("the push re-check = %+v, want a push phase that runs the command", pg2)
	}
	if ans := e.complete(pg2, Completion{Result: `{"target_branch":"main","commit":"abc123","pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the re-check's push: %s", ans)
	}
	if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", e.task); pushed != "pushed" {
		t.Errorf("landing pushed = %q, want pushed: the re-check released the push", pushed)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("land holds after the push = %d, want none", n)
	}
	if n := e.countEvents(evTaskLandRecheckReleased); n != 1 {
		t.Errorf("task_land_recheck_released events = %d, want 1", n)
	}
	if n := e.int(`SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'`, e.task); n != 0 {
		t.Errorf("queued land jobs after the push = %d, want none", n)
	}
}

// TestALandRuleHoldIsNeverRechecked: only a land_ready hold is
// re-checked; a hold a land-gate rule made — gate_land at ask — queues
// no job of the engine's and waits for the user alone.
func TestALandRuleHoldIsNeverRechecked(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)

	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d, want none while held", n)
	}
	e.maybeLand(e.task)
	e.clock.Advance(landReadyRecheckDelay * 2)
	e.mustSweep()
	if n := e.landJobs(); n != 0 {
		t.Errorf("land jobs after the interval = %d, want none: a rule hold is never re-checked", n)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); reason == "" || landReadyHold(reason) {
		t.Errorf("land hold reason = %q, want the gate_land rule's own", reason)
	}
}
