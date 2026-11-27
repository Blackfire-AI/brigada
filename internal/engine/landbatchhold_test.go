package engine

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// Batch gate holds (§7.1 "Deciding a batch hold", §6.3 "Releasing
// holds", acceptance at "Gate and setting acceptance").

// heldBatch leases the four-member batch and completes it with the
// land_ready gate's exit 1 before the first squash: every member held,
// each with its own re-check.
func heldBatch(t *testing.T) (*env, []int64) {
	t.Helper()
	e, tasks, g := leasedBatch(t)
	if ans := e.complete(g, Completion{Outcome: OutcomeLandHeld, Result: `{"outcome":"held","line":"CI is red"}`}); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	for _, id := range tasks {
		if r := e.str("SELECT COALESCE(reason, '') FROM land_holds WHERE task_id = ? AND state = 'held'", id); !strings.Contains(r, "CI is red") {
			t.Fatalf("task %d hold = %q, want the command's line", id, r)
		}
	}
	return e, tasks
}

// liveBatchJob reads the project's live batch job, 0 when none.
func (e *env) liveBatchJob() int64 {
	e.t.Helper()
	return e.int(`SELECT COALESCE(MAX(id), 0) FROM jobs WHERE key = ? AND status IN ('queued', 'leased', 'running')`, landBatchKey(e.project))
}

// TestLandBatchHoldApproveReleasesOneMember: approving one member of a
// batch hold deletes only its hold, cancels its re-check and queues it
// alone, once, into one next batch whose grant skips its gate; the other
// members stay held, a second approval is refused, and the batch's
// completion spends the release.
func TestLandBatchHoldApproveReleasesOneMember(t *testing.T) {
	e, tasks := heldBatch(t)
	member := tasks[1]
	d, err := e.eng.LandApprove(e.ctx, e.project, member, false)
	if err != nil {
		t.Fatal(err)
	}
	batch := e.liveBatchJob()
	if !d.Released || !d.Queued || d.JobID != batch || batch == 0 {
		t.Fatalf("approval = %+v, want the member queued into the new batch %d", d, batch)
	}
	if got := e.reservedTasks(batch); !slices.Equal(got, []int64{member}) {
		t.Errorf("the next batch reserves %v, want only the approved member %d", got, member)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", keyOf(member)); n != 0 {
		t.Errorf("the approved member's live re-checks = %d, want it cancelled", n)
	}
	for _, id := range []int64{tasks[0], tasks[2], tasks[3]} {
		if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", id); n != 1 {
			t.Errorf("task %d holds = %d, want it still held", id, n)
		}
	}
	if _, err := e.eng.LandApprove(e.ctx, e.project, member, false); err == nil {
		t.Error("a second approval of the released member was accepted; want it refused, released once")
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status <> 'cancelled'", landBatchKey(e.project)); n != 2 {
		t.Errorf("batch jobs = %d, want the held batch and the one next batch", n)
	}

	g := e.leaseGit(e.runner1)
	if g == nil || !g.LandBatch || g.JobID != batch {
		t.Fatalf("leased %+v, want the next batch %d", g, batch)
	}
	if len(g.LandMembers) != 1 || !g.LandMembers[0].SkipLandReady || !g.SkipLandReady {
		t.Fatalf("grant members %+v skip %v, want the one member with its gate skipped", g.LandMembers, g.SkipLandReady)
	}
	if ans := e.complete(g, batchResult(t, "", landedResult(member, "c1"))); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	if n := e.int("SELECT land_skip_ready FROM tasks WHERE id = ?", member); n != 0 {
		t.Errorf("land_skip_ready after its batch = %d, want the release spent", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", member); n != 1 {
		t.Errorf("landings of the released member = %d, want 1", n)
	}
}

// TestLandBatchHoldMixedBatch: a released member joins a batch with an
// unreleased one; the command's hold in that batch holds only the
// unreleased member, the released member lands, and a released member's
// dependent on the newly held one drops to wait, its release kept.
func TestLandBatchHoldMixedBatch(t *testing.T) {
	e, tasks := heldBatch(t)
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, tasks[3], tasks[2])
	// Two released members; the dependent's approval releases no held
	// dependency, so its landing waits for it.
	for _, id := range []int64{tasks[0], tasks[3]} {
		if _, err := e.eng.LandApprove(e.ctx, e.project, id, false); err != nil {
			t.Fatal(err)
		}
	}
	batch := e.liveBatchJob()
	if got := e.reservedTasks(batch); !slices.Equal(got, []int64{tasks[0]}) {
		t.Fatalf("the next batch reserves %v, want only %d: its dependent waits on the held dependency", got, tasks[0])
	}
	// The dependency's hold is lifted under another rule — its gate still
	// runs — and both join the batch, as a later formation would gather
	// them.
	e.exec("DELETE FROM land_holds WHERE task_id = ?", tasks[2])
	e.exec("UPDATE jobs SET status = 'cancelled' WHERE key = ? AND status = 'queued'", keyOf(tasks[2]))
	e.exec("UPDATE tasks SET land_batch_job = ? WHERE id IN (?, ?)", batch, tasks[2], tasks[3])
	g := e.leaseGit(e.runner1)
	if g == nil || g.JobID != batch {
		t.Fatalf("leased %+v, want batch %d", g, batch)
	}
	skips := map[int64]bool{}
	for _, m := range g.LandMembers {
		skips[m.TaskID] = m.SkipLandReady
	}
	if !skips[tasks[0]] || skips[tasks[2]] || !skips[tasks[3]] || g.SkipLandReady {
		t.Fatalf("member skips = %v (grant %v), want the released members only", skips, g.SkipLandReady)
	}
	c := batchResult(t, "",
		landedResult(tasks[0], "c0"),
		landMemberResult{TaskID: tasks[2], Outcome: landMemberHeld, Line: "CI is red", Suspects: "suspects unknown"},
		landMemberResult{TaskID: tasks[3], Outcome: landMemberDropped, BlockedBy: tasks[2]})
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	if r := e.str("SELECT COALESCE(reason, '') FROM land_holds WHERE task_id = ? AND state = 'held'", tasks[2]); r != "held by rule land_ready: CI is red; suspects unknown" {
		t.Errorf("the unreleased member's hold = %q, want the line then the suspect line", r)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", tasks[0]); n != 1 {
		t.Errorf("the released member's landings = %d, want 1", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", tasks[3]); n != 0 {
		t.Errorf("the dropped dependent's landings = %d, want none", n)
	}
	if n := e.int("SELECT land_skip_ready FROM tasks WHERE id = ?", tasks[3]); n != 1 {
		t.Errorf("the dropped dependent's release = %d, want it kept for the batch it lands in", n)
	}
	if n := e.int("SELECT COUNT(*) FROM tasks WHERE land_batch_job IS NOT NULL"); n != 0 {
		t.Errorf("reserved tasks = %d, want none: the dependent waits on the held member", n)
	}
}

// TestLandBatchHoldRejectDeclinesOne: rejecting one member declines only
// its landing; the others stay held and the plan stays open over them.
func TestLandBatchHoldRejectDeclinesOne(t *testing.T) {
	e, tasks := heldBatch(t)
	d, err := e.eng.LandReject(e.ctx, e.project, tasks[0])
	if err != nil {
		t.Fatal(err)
	}
	if d.Released {
		t.Errorf("rejection = %+v, want declined", d)
	}
	if s := e.str("SELECT state FROM land_holds WHERE task_id = ?", tasks[0]); s != "rejected" {
		t.Errorf("the rejected member's hold = %q, want rejected", s)
	}
	for _, id := range tasks[1:] {
		if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", id); n != 1 {
			t.Errorf("task %d holds = %d, want it still held", id, n)
		}
	}
	if s := e.str("SELECT state FROM plans WHERE id = ?", e.plan); s == planDone {
		t.Error("the plan closed over a decline while other landings remain to settle")
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status IN ('queued', 'leased', 'running')", landBatchKey(e.project)); n != 0 {
		t.Errorf("live batches after the rejection = %d, want none queued", n)
	}
}

// TestLandBatchHoldApproveBehindALiveBatch: an approval while another
// batch is live reports that job and queues nothing; the member's release
// carries into the next batch the live one's end forms. --and-stop-asking
// keeps its one-setting rule: a land_ready hold maps to no setting, and
// the batch adds none.
func TestLandBatchHoldApproveBehindALiveBatch(t *testing.T) {
	e, tasks := heldBatch(t)
	if _, err := e.eng.LandApprove(e.ctx, e.project, tasks[0], false); err != nil {
		t.Fatal(err)
	}
	live := e.liveBatchJob()
	settings := e.str("SELECT settings FROM projects WHERE id = ?", e.project)
	d, err := e.eng.LandApprove(e.ctx, e.project, tasks[1], true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Queued || d.JobID != live || len(d.Settings) != 0 {
		t.Errorf("approval behind the live batch = %+v, want job %d, nothing queued, no setting changed", d, live)
	}
	if got := e.str("SELECT settings FROM projects WHERE id = ?", e.project); got != settings {
		t.Errorf("settings = %s, want unchanged %s", got, settings)
	}
	for _, id := range tasks[2:] {
		if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", id); n != 1 {
			t.Errorf("task %d holds = %d, want it still held", id, n)
		}
	}
	g := e.leaseGit(e.runner1)
	if g == nil || g.JobID != live {
		t.Fatalf("leased %+v, want the live batch %d", g, live)
	}
	if ans := e.complete(g, batchResult(t, "", landedResult(tasks[0], "c0"))); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	next := e.liveBatchJob()
	if next == live || !slices.Equal(e.reservedTasks(next), []int64{tasks[1]}) {
		t.Fatalf("next batch %d reserves %v, want the approved member %d alone", next, e.reservedTasks(next), tasks[1])
	}
	g2 := e.leaseGit(e.runner1)
	if g2 == nil || len(g2.LandMembers) != 1 || !g2.LandMembers[0].SkipLandReady {
		t.Errorf("the next batch's grant = %+v, want the member with its gate skipped", g2)
	}
}

// TestLandBatchHoldRecheckReleasesOneMember: a member's re-check runs
// the command and lands nothing; its pass releases that member alone,
// once, into one next batch with its gate skipped, the other holds
// waiting for their own re-checks.
func TestLandBatchHoldRecheckReleasesOneMember(t *testing.T) {
	e, tasks := heldBatch(t)
	e.clock.Advance(landReadyRecheckDelay + 1)
	g := e.leaseGit(e.runner1)
	if g == nil || g.LandBatch || !g.LandRecheck || g.SkipLandReady {
		t.Fatalf("leased %+v, want a member's re-check", g)
	}
	member := g.TaskID
	if ans := e.complete(g, Completion{Outcome: OutcomeLandReady, Result: `{"outcome":"ready"}`}); ans != AnsOK {
		t.Fatalf("complete the re-check: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", member); n != 0 {
		t.Errorf("the passing member's holds = %d, want released", n)
	}
	if _, ok := e.taskEvent(member, evTaskLandRecheckReleased); !ok {
		t.Error("no engine release event for the passing re-check")
	}
	batch := e.liveBatchJob()
	if !slices.Equal(e.reservedTasks(batch), []int64{member}) {
		t.Errorf("the next batch reserves %v, want the passing member %d alone", e.reservedTasks(batch), member)
	}
	if n := e.int("SELECT land_skip_ready FROM tasks WHERE id = ?", member); n != 1 {
		t.Errorf("the passing member's release = %d, want its gate skipped in the next batch", n)
	}
	if n := e.int("SELECT COUNT(*) FROM landings"); n != 0 {
		t.Errorf("landings = %d, want none: a re-check lands nothing", n)
	}
	for _, id := range tasks {
		if id == member {
			continue
		}
		if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", id); n != 1 {
			t.Errorf("task %d holds = %d, want it waiting for its own re-check", id, n)
		}
	}
}

// waitingStack records three of the batch's members as waiting landings,
// in landing order, under gate_push ask — the stack one batch published
// to the waiting ref.
func waitingStack(t *testing.T) (*env, []int64) {
	t.Helper()
	e, tasks, g := leasedBatch(t)
	waiting := func(task int64, commit string) landMemberResult {
		return landMemberResult{TaskID: task, Outcome: landMemberLanded,
			Record: &landRecord{TargetBranch: "main", Commit: commit, CheckPassed: true, Pushed: "waiting"}}
	}
	c := batchResult(t, "", waiting(tasks[0], "c0"), waiting(tasks[1], "c1"), waiting(tasks[2], "c2"),
		landMemberResult{TaskID: tasks[3], Outcome: landMemberParked, Reason: "moved head"})
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	return e, tasks[:3]
}

// TestLandBatchPushHoldKeepsTheStackWaiting: the push phase's command
// runs once for the proposed push; its exit 1 holds that one push and
// every waiting landing it covers waits with the same reason on its own
// needs-you line — nothing published, re-squashed or newly recorded. A
// release of one held member pushes its recorded prefix alone: the
// landings beneath it are carried, later ones stay held and waiting.
func TestLandBatchPushHoldKeepsTheStackWaiting(t *testing.T) {
	e, stack := waitingStack(t)
	tip := stack[2]
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := e.eng.queuePushJobTx(tx, e.project, tip, false, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatal(err)
	}
	pg := e.leaseGit(e.runner1)
	if pg == nil || !pg.Push || pg.TaskID != tip {
		t.Fatalf("leased %+v, want the tip's push", pg)
	}
	if ans := e.complete(pg, Completion{Outcome: OutcomeLandHeld, Result: `{"outcome":"held","line":"CI is red"}`}); ans != AnsOK {
		t.Fatalf("complete the held push: %s", ans)
	}
	for _, id := range stack {
		if p := e.str("SELECT pushed FROM landings WHERE task_id = ?", id); p != "waiting" {
			t.Errorf("task %d pushed = %q, want waiting", id, p)
		}
		if r := e.str("SELECT COALESCE(reason, '') FROM land_holds WHERE task_id = ? AND state = 'held'", id); !strings.Contains(r, "CI is red") {
			t.Errorf("task %d hold = %q, want the push's reason", id, r)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM landings"); n != 3 {
		t.Errorf("landings = %d, want the three recorded, none new", n)
	}
	// One push, one command: the covered landings' own pushes are
	// cancelled, and only the held push re-checks.
	live := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'land' AND status IN ('queued', 'leased', 'running')")
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", keyOf(tip)); live != 1 || n != 1 {
		t.Errorf("live land jobs = %d (tip re-checks %d), want only the tip's re-check", live, n)
	}
	var lines []NeedsYouItem
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		lines, err = statusLandHoldsTx(tx, e.project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Errorf("needs-you hold lines = %+v, want one per covered member", lines)
	}

	// Releasing the middle member pushes its prefix alone.
	d, err := e.eng.LandApprove(e.ctx, e.project, stack[1], false)
	if err != nil || !d.Queued {
		t.Fatalf("approve the middle member = %+v, %v; want its push queued", d, err)
	}
	pg2 := e.leaseGit(e.runner1)
	if pg2 == nil || !pg2.Push || pg2.TaskID != stack[1] || !pg2.SkipLandReady || pg2.LandCommit != "c1" {
		t.Fatalf("leased %+v, want the middle member's recorded commit with its gate skipped", pg2)
	}
	if ans := e.complete(pg2, Completion{Result: `{"target_branch":"main","commit":"c1","pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the released push: %s", ans)
	}
	for id, want := range map[int64]string{stack[0]: "pushed", stack[1]: "pushed", tip: "waiting"} {
		if p := e.str("SELECT pushed FROM landings WHERE task_id = ?", id); p != want {
			t.Errorf("task %d pushed = %q, want %s", id, p, want)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", stack[0]); n != 0 {
		t.Errorf("the carried landing's holds = %d, want released with the push", n)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", tip); n != 1 {
		t.Errorf("the later landing's holds = %d, want it still held", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", keyOf(tip)); n != 1 {
		t.Errorf("the later landing's re-checks = %d, want its one re-check still waiting", n)
	}
}
