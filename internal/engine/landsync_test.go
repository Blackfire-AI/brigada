package engine

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// driveSync leases the project's pending sync and completes it current
// (§7.1) — the no-op fetch that opens the gate. A no-op when no sync is
// live, so a test without a land target off its default drives nothing.
func (e *env) driveSync(owner int64) {
	e.t.Helper()
	if e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'sync' AND status IN ('queued','leased','running')") == 0 {
		return
	}
	g := e.leaseGit(owner)
	if g == nil || g.Kind != KindSync {
		e.t.Fatalf("a sync is live but not leasable: %+v", g)
	}
	if ans := e.complete(g, Completion{Result: `{"outcome":"current"}`}); ans != AnsOK {
		e.t.Fatalf("complete the sync: %s", ans)
	}
}

// syncNeedsYou reads the project-level needs-you entries the sync writes
// (§7.1): a merge waiting at the gate, or a conflicting one.
func (e *env) syncNeedsYou() []NeedsYouItem {
	e.t.Helper()
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	var items []NeedsYouItem
	for _, it := range info.NeedsYou {
		if it.Kind == "project" {
			items = append(items, it)
		}
	}
	return items
}

// queuedSyncNotBefore reads the not-before of the project's queued sync,
// "" when it is leasable at once.
func (e *env) queuedSyncNotBefore() string {
	e.t.Helper()
	return e.str("SELECT COALESCE(not_before, '') FROM jobs WHERE kind = 'sync' AND status = 'queued'")
}

// TestSyncPrecedesFirstWave: a plan created while the land target is
// behind the default branch syncs first (§7.1). The first wave's work
// jobs hold behind the one sync; the sync's grant names both branches,
// and once it merges the default branch into the target and pushes, the
// wave leases on the target — whose head now carries the default
// branch's head (the runner materializes the synced target).
func TestSyncPrecedesFirstWave(t *testing.T) {
	e := landTargetEnv(t)
	res, err := e.eng.CreatePlan(e.ctx, e.project, "behind the default", []PlanTask{
		{Name: "root", Spec: "the work"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	root := res.Tasks[0].TaskID
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'sync' AND task_id IS NULL AND status IN ('queued','leased','running')"); n != 1 {
		t.Fatalf("task-less sync jobs = %d, want one queued ahead of the wave", n)
	}
	// The wave holds behind the sync: a plain poll leases nothing.
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("the first wave leased before the sync: %+v", g)
	}
	sg := e.leaseGit(e.runner1)
	if sg == nil || sg.Kind != KindSync || sg.TaskID != 0 {
		t.Fatalf("sync grant = %+v, want a task-less sync", sg)
	}
	if sg.LandTarget != "brigada/landed" || sg.DefaultBranch != "main" || sg.Push {
		t.Errorf("sync grant target/default/push = %q/%q/%v, want brigada/landed/main/false", sg.LandTarget, sg.DefaultBranch, sg.Push)
	}
	if sg.CheckCmd != "" || sg.AfterLand != "" || sg.LandReady != "" {
		t.Errorf("sync grant carries check/after_land/land_ready = %q/%q/%q, want none", sg.CheckCmd, sg.AfterLand, sg.LandReady)
	}
	// The target was behind; the sync merges the default head into it and
	// pushes under auto.
	if ans := e.complete(sg, Completion{Result: `{"outcome":"merged","behind":0,"merge_commit":"merge-1","pushed":"pushed","default_head":"def-head","target_head":"tgt-head"}`}); ans != AnsOK {
		t.Fatalf("complete the sync: %s", ans)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateCurrent {
		t.Errorf("sync state = %q, want current", state)
	}
	if merge := e.str("SELECT merge_commit FROM land_syncs WHERE project_id = ?", e.project); merge != "merge-1" {
		t.Errorf("sync merge commit = %q, want merge-1", merge)
	}
	// The wave leases now, on the target the sync brought current.
	g := e.leaseAny(e.runner1)
	if g == nil || g.TaskID != root || g.BaseRef != "brigada/landed" {
		t.Fatalf("root grant after the sync = %+v, want base brigada/landed", g)
	}
}

// TestSyncBehindStackOrdering: a waiting landing blocks the sync's merge
// (§7.1). The sync completes behind stack and re-queues on the backoff;
// while the stack waits, the wave leases on the standing record (the one
// exception); and when the last waiting landing is pushed, the queued
// sync wakes at once — its not-before clears.
func TestSyncBehindStackOrdering(t *testing.T) {
	e := landTargetEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	dep := seededDep(e, taskDone, "brigada/landed", "waiting")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "behind the stack", []PlanTask{
		{Name: "root", Spec: "the work", Class: "answer"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	root := res.Tasks[0].TaskID
	s1 := e.leaseGit(e.runner1)
	if s1 == nil || s1.Kind != KindSync {
		t.Fatalf("sync grant = %+v", s1)
	}
	if ans := e.complete(s1, Completion{Result: `{"outcome":"behind stack","behind":2}`}); ans != AnsOK {
		t.Fatalf("complete behind stack: %s", ans)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateBehindStack {
		t.Errorf("sync state = %q, want behind_stack", state)
	}
	if nb := e.queuedSyncNotBefore(); nb == "" {
		t.Error("the behind-stack sync re-queued with no not-before backoff")
	}
	// The exception: while the stack waits, the wave leases on the
	// standing record even though a sync is queued.
	g := e.leaseAny(e.runner1)
	if g == nil || g.TaskID != root {
		t.Fatalf("the wave did not lease on the standing record while the stack waits: %+v", g)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete root: %s", ans)
	}
	// Settle the last waiting landing: the approval queues its push, and
	// the push wakes the behind-stack sync at once.
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatalf("push approve: %v", err)
	}
	pg := e.leaseGit(e.runner1) // the sync still waits out its backoff; the push leases
	if pg == nil || pg.Kind != KindLand || pg.TaskID != dep || !pg.Push {
		t.Fatalf("push grant = %+v, want the waiting landing's push", pg)
	}
	if ans := e.complete(pg, Completion{Result: `{"target_branch":"brigada/landed","commit":"abc123","pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the push: %s", ans)
	}
	if nb := e.queuedSyncNotBefore(); nb != "" {
		t.Errorf("the behind-stack sync's not-before = %q after the stack settled, want it cleared (woken at once)", nb)
	}
	s2 := e.leaseGit(e.runner1)
	if s2 == nil || s2.Kind != KindSync {
		t.Fatalf("the woken sync did not lease at once: %+v", s2)
	}
	if ans := e.complete(s2, Completion{Result: `{"outcome":"current"}`}); ans != AnsOK {
		t.Fatalf("complete the woken sync: %s", ans)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateCurrent {
		t.Errorf("sync state = %q, want current after the stack settled", state)
	}
}

// TestSyncMirrorHoldsLeases: while a sync is pending — here conflicting
// at the gate — the mirror rule holds the wave (§7.1): no task leases the
// target as a base. One needs-you line names the project, the count
// behind and the conflicting files; the hand resolution (the next sync
// reads the target current) opens the gate and clears the line.
func TestSyncMirrorHoldsLeases(t *testing.T) {
	e := landTargetEnv(t)
	res, err := e.eng.CreatePlan(e.ctx, e.project, "conflicting sync", []PlanTask{
		{Name: "root", Spec: "the work"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	root := res.Tasks[0].TaskID
	s1 := e.leaseGit(e.runner1)
	if s1 == nil || s1.Kind != KindSync {
		t.Fatalf("sync grant = %+v", s1)
	}
	if ans := e.complete(s1, Completion{Result: `{"outcome":"conflict","behind":3,"conflict_files":["a.go","b.go"]}`}); ans != AnsOK {
		t.Fatalf("complete conflict: %s", ans)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateConflict {
		t.Errorf("sync state = %q, want conflict", state)
	}
	items := e.syncNeedsYou()
	if len(items) != 1 {
		t.Fatalf("sync needs-you = %+v, want one project line", items)
	}
	for _, want := range []string{"3 commits behind main", "a.go, b.go", "merge main into brigada/landed by hand", "clear land_target"} {
		if !strings.Contains(items[0].Reason, want) {
			t.Errorf("sync needs-you reason = %q, want it to contain %q", items[0].Reason, want)
		}
	}
	// The mirror holds the wave even once the re-queued sync is leasable.
	e.clock.Advance(time.Minute)
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("the wave leased while the sync conflicts: %+v", g)
	}
	// The hand resolution: the next attempt reads the target current.
	s2 := e.leaseGit(e.runner1)
	if s2 == nil || s2.Kind != KindSync {
		t.Fatalf("re-queued sync grant = %+v", s2)
	}
	if ans := e.complete(s2, Completion{Result: `{"outcome":"current"}`}); ans != AnsOK {
		t.Fatalf("complete the resolving sync: %s", ans)
	}
	if items := e.syncNeedsYou(); len(items) != 0 {
		t.Errorf("sync needs-you after resolution = %+v, want none", items)
	}
	g := e.leaseAny(e.runner1)
	if g == nil || g.TaskID != root {
		t.Fatalf("the wave did not lease once the sync cleared: %+v", g)
	}
}

// TestSyncOnePerProject: a plan's first wave of ready roots queues one
// sync, not one per task (§7.1); a repeat trigger coalesces behind the
// live sync, and the whole wave leases once it is current.
func TestSyncOnePerProject(t *testing.T) {
	e := landTargetEnv(t)
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "two roots", []PlanTask{
		{Name: "a", Spec: "one"}, {Name: "b", Spec: "two"},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'sync' AND status IN ('queued','leased','running')"); n != 1 {
		t.Fatalf("live sync jobs = %d, want one for the whole wave", n)
	}
	// A repeat trigger coalesces behind the live sync.
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		return e.eng.maybeQueueSyncTx(tx, e.project, e.clock.Now())
	}); err != nil {
		t.Fatalf("re-trigger the sync: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'sync' AND status IN ('queued','leased','running')"); n != 1 {
		t.Fatalf("live sync jobs after a repeat trigger = %d, want one", n)
	}
	e.driveSync(e.runner1)
	g1 := e.leaseAny(e.runner1)
	g2 := e.leaseAny(e.runner1)
	if g1 == nil || g2 == nil || g1.Kind != KindWork || g2.Kind != KindWork || g1.TaskID == g2.TaskID {
		t.Fatalf("wave grants = %+v / %+v, want two distinct work jobs once the sync is current", g1, g2)
	}
}

// TestSyncPushApproveReleasesWaitingMerge: a sync merge waiting at the
// ask gate is released by `brigada push approve` into a push phase that
// pushes the recorded merge commit, no new merge (§7.1); the needs-you
// line names both branches and the waiting commit.
func TestSyncPushApproveReleasesWaitingMerge(t *testing.T) {
	e := landTargetEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "waiting merge", []PlanTask{
		{Name: "root", Spec: "the work", Class: "answer"},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	s1 := e.leaseGit(e.runner1)
	if s1 == nil || s1.Kind != KindSync {
		t.Fatalf("sync grant = %+v", s1)
	}
	if ans := e.complete(s1, Completion{Result: `{"outcome":"merged","behind":2,"merge_commit":"m1","pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the waiting merge: %s", ans)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateWaiting {
		t.Errorf("sync state = %q, want waiting", state)
	}
	items := e.syncNeedsYou()
	if len(items) != 1 || !strings.Contains(items[0].Reason, "waits to push: merge main into brigada/landed at m1") {
		t.Fatalf("sync needs-you = %+v, want the waiting merge naming both branches and the commit", items)
	}
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatalf("push approve: %v", err)
	}
	s3 := e.leaseGit(e.runner1)
	if s3 == nil || s3.Kind != KindSync || !s3.Push || s3.LandCommit != "m1" {
		t.Fatalf("sync push grant = %+v, want the push phase carrying merge m1", s3)
	}
	if ans := e.complete(s3, Completion{Result: `{"outcome":"merged","merge_commit":"m1","pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the sync push: %s", ans)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateCurrent {
		t.Errorf("sync state after the push = %q, want current", state)
	}
	if items := e.syncNeedsYou(); len(items) != 0 {
		t.Errorf("sync needs-you after the push = %+v, want none", items)
	}
}

// TestSyncNeverTrailerClearsWait: under gate_push never a waiting merge
// carries the exact hand push, and the runner's scan of the merge's
// Brigada-Sync trailer on the target — reported through RecordSync —
// clears the wait (§7.1): the record reads current, the merge marked
// pushed, and the needs-you line goes.
func TestSyncNeverTrailerClearsWait(t *testing.T) {
	e := landTargetEnv(t)
	e.set(map[string]string{SettingGatePush: GateNever})
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "never merge", []PlanTask{
		{Name: "root", Spec: "the work", Class: "answer"},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	s1 := e.leaseGit(e.runner1)
	if s1 == nil || s1.Kind != KindSync {
		t.Fatalf("sync grant = %+v", s1)
	}
	if ans := e.complete(s1, Completion{Result: `{"outcome":"merged","behind":2,"merge_commit":"m1","pushed":"local","push_cmd":"git push","default_head":"def-head"}`}); ans != AnsOK {
		t.Fatalf("complete the local merge: %s", ans)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateWaiting {
		t.Errorf("sync state = %q, want waiting", state)
	}
	if items := e.syncNeedsYou(); len(items) != 1 || !strings.Contains(items[0].Reason, "push by hand: git push") {
		t.Fatalf("sync needs-you = %+v, want the hand push", items)
	}
	// The user pushes by hand; the runner's scan reads the merge's
	// Brigada-Sync trailer and reports it, clearing the wait.
	if err := e.eng.RecordSync(e.ctx, envRepo, "main", []SyncCommit{
		{Commit: "m1", Default: "main", Head: "def-head"},
	}); err != nil {
		t.Fatal(err)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateCurrent {
		t.Errorf("sync state after the trailer read = %q, want current", state)
	}
	if pushed := e.str("SELECT pushed FROM land_syncs WHERE project_id = ?", e.project); pushed != "pushed" {
		t.Errorf("sync pushed after the trailer read = %q, want pushed", pushed)
	}
	if p, ok := e.eventPayloadOf(evSyncPushed); !ok || p.Commit != "m1" || p.Pushed != "pushed" {
		t.Errorf("sync_pushed event = %+v (%t), want the pushed merge m1", p, ok)
	}
	if items := e.syncNeedsYou(); len(items) != 0 {
		t.Errorf("sync needs-you after the trailer read = %+v, want none", items)
	}
}

// TestSyncPushApproveRefusedForgetsMerge: a push approve whose recorded
// merge the target can no longer fast-forward to is refused (§7.1), as a
// landing's non-fast-forward push is — the waiting merge is forgotten,
// the sync re-queues on the backoff, and no task parks, for the sync has
// none.
func TestSyncPushApproveRefusedForgetsMerge(t *testing.T) {
	e := landTargetEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "refused push", []PlanTask{
		{Name: "root", Spec: "the work", Class: "answer"},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	s1 := e.leaseGit(e.runner1)
	if s1 == nil || s1.Kind != KindSync {
		t.Fatalf("sync grant = %+v", s1)
	}
	if ans := e.complete(s1, Completion{Result: `{"outcome":"merged","behind":2,"merge_commit":"m1","pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the waiting merge: %s", ans)
	}
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatalf("push approve: %v", err)
	}
	p1 := e.leaseGit(e.runner1)
	if p1 == nil || p1.Kind != KindSync || !p1.Push || p1.LandCommit != "m1" {
		t.Fatalf("sync push grant = %+v, want the push phase carrying m1", p1)
	}
	if ans := e.complete(p1, Completion{Error: "push the sync merge m1 to brigada/landed: ! [rejected] (non-fast-forward)"}); ans != AnsOK {
		t.Fatalf("complete the refused push: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM land_syncs WHERE project_id = ?", e.project); n != 0 {
		t.Errorf("land_syncs rows = %d, want the refused merge forgotten", n)
	}
	if n := e.int("SELECT COUNT(*) FROM tasks WHERE state = 'needs_foreman'"); n != 0 {
		t.Errorf("tasks parked = %d, want none: the sync has no task", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'sync' AND status = 'queued'"); n != 1 {
		t.Errorf("queued sync jobs = %d, want the refused push re-queued", n)
	}
}

// TestSyncHeldMergeWaitsAtAskAnew: a waiting merge is held by the ask
// gate as a waiting landing is (§7.1): a switch of gate_push to auto
// pushes nothing, and a sync re-made after its waiting ref went stale
// waits at ask anew rather than pushing under the new auto.
func TestSyncHeldMergeWaitsAtAskAnew(t *testing.T) {
	e := landTargetEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "held merge", []PlanTask{
		{Name: "root", Spec: "the work", Class: "answer"},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	s1 := e.leaseGit(e.runner1)
	if s1 == nil || s1.Kind != KindSync {
		t.Fatalf("sync grant = %+v", s1)
	}
	if ans := e.complete(s1, Completion{Result: `{"outcome":"merged","behind":2,"merge_commit":"m1","pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the waiting merge: %s", ans)
	}
	if nb := e.queuedSyncNotBefore(); nb == "" {
		t.Error("the waiting merge did not re-queue on the not-before backoff")
	}
	// The user switches to auto: the held merge still does not push
	// unasked — the re-made sync waits at ask anew.
	e.set(map[string]string{SettingGatePush: GateAuto})
	e.clock.Advance(time.Minute)
	s2 := e.leaseGit(e.runner1)
	if s2 == nil || s2.Kind != KindSync || s2.GatePush != GateAsk {
		t.Fatalf("re-made sync grant = %+v, want gate ask, not the new auto", s2)
	}
}

// TestSyncPushedMergeNotHeld: once a waiting merge is pushed by the push
// phase, the push mark clears the held state (§7.1): a sync re-made after
// the target moved past the ref does not wait at ask for a merge that
// already landed.
func TestSyncPushedMergeNotHeld(t *testing.T) {
	e := landTargetEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "pushed merge", []PlanTask{
		{Name: "root", Spec: "the work", Class: "answer"},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	s1 := e.leaseGit(e.runner1)
	if s1 == nil || s1.Kind != KindSync {
		t.Fatalf("sync grant = %+v", s1)
	}
	if ans := e.complete(s1, Completion{Result: `{"outcome":"merged","behind":2,"merge_commit":"m1","pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the waiting merge: %s", ans)
	}
	if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
		t.Fatalf("push approve: %v", err)
	}
	p1 := e.leaseGit(e.runner1)
	if p1 == nil || p1.Kind != KindSync || !p1.Push {
		t.Fatalf("sync push grant = %+v, want the push phase", p1)
	}
	if ans := e.complete(p1, Completion{Result: `{"outcome":"merged","merge_commit":"m1","pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the sync push: %s", ans)
	}
	if p, ok := e.eventPayloadOf(evSyncPushed); !ok || p.Commit != "m1" || p.Pushed != "pushed" {
		t.Fatalf("sync_pushed event = %+v (%t), want the pushed merge m1", p, ok)
	}
	var held bool
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		held, err = syncHeldPushTx(tx, e.project)
		return err
	}); err != nil {
		t.Fatalf("read the held push: %v", err)
	}
	if held {
		t.Error("the pushed merge is still held at ask")
	}
}

// TestSyncLeasesOnlyAfterInFlightLanding: a landing already in flight
// finishes first (§7.1) — the sync leases only once no land job of the
// project is leased or running.
func TestSyncLeasesOnlyAfterInFlightLanding(t *testing.T) {
	e := landTargetEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "to land", []PlanTask{{Spec: "the work"}})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	e.driveSync(author)
	g := e.leaseAny(author)
	if g == nil || g.TaskID != res.TaskID {
		t.Fatalf("work grant = %+v", g)
	}
	if ans := e.complete(g, doneOn(branchOf(res.TaskID), passCheck)); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	e.driveSync(author)
	lg := e.leaseGit(author)
	if lg == nil || lg.Kind != KindLand {
		t.Fatalf("land grant = %+v, want the landing in flight", lg)
	}
	// Another plan's sync queues, but holds behind the in-flight landing.
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "next wave", []PlanTask{
		{Name: "root", Spec: "more", Class: "answer"},
	}); err != nil {
		t.Fatalf("create the next plan: %v", err)
	}
	if g := e.leaseGit(author); g != nil {
		t.Fatalf("the sync leased while a landing is in flight: %+v", g)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"brigada/landed","commit":"c1","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the landing: %s", ans)
	}
	sg := e.leaseGit(author)
	if sg == nil || sg.Kind != KindSync {
		t.Fatalf("the sync did not lease once the landing finished: %+v", sg)
	}
}

// TestSyncStaleRefDropForgetsWaitingMerge: a sync attempt that drops the
// target's stale waiting ref forgets the waiting merge and the landings
// that sat on it (§7.1, the landing record's drop), and the sync runs
// again — here reading the target current.
func TestSyncStaleRefDropForgetsWaitingMerge(t *testing.T) {
	e := landTargetEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "stale merge", []PlanTask{
		{Name: "root", Spec: "the work", Class: "answer"},
	}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	s1 := e.leaseGit(e.runner1)
	if ans := e.complete(s1, Completion{Result: `{"outcome":"merged","behind":2,"merge_commit":"m1","pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the waiting merge: %s", ans)
	}
	// The engine marks the target's waiting ref stale (a restart forgot
	// the landings on it); the next sync attempt carries the drop mark.
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		return e.eng.markStaleLandRefTx(tx, e.project, "brigada/landed", e.clock.Now())
	}); err != nil {
		t.Fatalf("mark the ref stale: %v", err)
	}
	e.clock.Advance(time.Minute)
	s2 := e.leaseGit(e.runner1)
	if s2 == nil || s2.Kind != KindSync || !s2.DropWaitingRef {
		t.Fatalf("re-queued sync grant = %+v, want the stale-ref drop mark", s2)
	}
	if ans := e.complete(s2, Completion{Result: `{"outcome":"current"}`, StaleDropped: "brigada/landed"}); ans != AnsOK {
		t.Fatalf("complete the dropping sync: %s", ans)
	}
	if state := e.str("SELECT state FROM land_syncs WHERE project_id = ?", e.project); state != syncStateCurrent {
		t.Errorf("sync state = %q, want current after the drop", state)
	}
	if n := e.int("SELECT COUNT(*) FROM stale_land_refs WHERE project_id = ?", e.project); n != 0 {
		t.Errorf("stale-ref marks = %d, want the drop to clear them", n)
	}
}

// TestSyncLagStatusAndPrompt: the count a sync keeps (§7.1) shows on
// status while it is above zero and a land target is set; it is a
// needs-you entry, carrying the number, exactly for the merge waiting at
// the gate or conflicting — not while the sync queues behind a stack —
// and the foreman's opening prompt names both heads and the count.
func TestSyncLagStatusAndPrompt(t *testing.T) {
	e := landTargetEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "lag", []PlanTask{{Name: "root", Spec: "the work", Class: "answer"}}); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lags := func() []StatusLandSync {
		info, err := e.eng.Status(e.ctx, 0)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		return info.LandSyncs
	}
	s1 := e.leaseGit(e.runner1)
	if s1 == nil || s1.Kind != KindSync {
		t.Fatalf("sync grant = %+v", s1)
	}
	if ans := e.complete(s1, Completion{Result: `{"outcome":"merged","behind":2,"merge_commit":"m1","pushed":"waiting","default_head":"def-head","target_head":"tgt-head"}`}); ans != AnsOK {
		t.Fatalf("complete the waiting merge: %s", ans)
	}
	if l := lags(); len(l) != 1 || l[0].Behind != 2 || l[0].DefaultBranch != "main" {
		t.Errorf("status lag at the gate = %+v, want 2 behind main", l)
	}
	if items := e.syncNeedsYou(); len(items) != 1 || !strings.Contains(items[0].Reason, "land target is 2 commits behind main") {
		t.Errorf("gate-wait needs-you = %+v, want the count", items)
	}
	var prompt string
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		prompt, err = foremanPromptTx(tx, "", e.project, ForemanInbox{}, e.clock.Now())
		return err
	}); err != nil {
		t.Fatalf("foreman prompt: %v", err)
	}
	for _, want := range []string{`"main" at def-head`, `"brigada/landed" at tgt-head`, "2 commits behind"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("foreman prompt lacks %q", want)
		}
	}
	// Behind a waiting stack: the count is kept and shown, nothing needs the user.
	e.exec("UPDATE land_syncs SET state = ? WHERE project_id = ?", syncStateBehindStack, e.project)
	if l := lags(); len(l) != 1 {
		t.Errorf("status lag behind a stack = %+v, want the kept count", l)
	}
	if items := e.syncNeedsYou(); len(items) != 0 {
		t.Errorf("behind-stack needs-you = %+v, want none", items)
	}
	// Current: count zero, nothing shown.
	e.exec("UPDATE land_syncs SET state = ?, behind = 0 WHERE project_id = ?", syncStateCurrent, e.project)
	if l := lags(); len(l) != 0 {
		t.Errorf("status lag when current = %+v, want none", l)
	}
	// No land target set: a stale record shows nothing.
	e.exec("UPDATE land_syncs SET behind = 5 WHERE project_id = ?", e.project)
	e.set(map[string]string{SettingLandTarget: ""})
	if l := lags(); len(l) != 0 {
		t.Errorf("status lag with no land target = %+v, want none", l)
	}
}
