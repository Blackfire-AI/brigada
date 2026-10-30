package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The retire (§6.2): a task that will not land — done work that landed
// elsewhere or is superseded, a task parked past repair, one held at
// land gate — settles by retirement. Its live jobs stop, its land hold
// drops, its branch stays, nothing ever lands, and the plan closes over
// it, the done report naming it retired with the reason. The dependents
// rule is deliberately narrower than cancel's: a failed dependent blocks
// too, for its branch was still built on the retired task's.

// seedRetireChain builds the shape the retire answers: parked, a task
// parked at needs_foreman, and top, its done dependent stacked on it —
// both implement, top approved — so the plan cannot close over top's
// landing still to come, and cancel refuses top because it is done.
func seedRetireChain(t *testing.T) (e *env, author, plan, parked, top int64) {
	t.Helper()
	e = newEnv(t)
	author = e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{
		{Name: "parked", Spec: "the parked work"},
		{Name: "top", Spec: "the stacked work", Deps: []string{"parked"}},
	})
	parked, top = res.Tasks[0].TaskID, res.Tasks[1].TaskID
	plan = res.PlanID
	// Stop the work createPlan queued for parked and park it, the shape
	// a failed run leaves: its dependent still runs and finishes.
	if job := e.int("SELECT MIN(id) FROM jobs WHERE task_id = ?", parked); job != 0 {
		e.cancel(job)
	}
	e.exec("UPDATE tasks SET state = 'needs_foreman' WHERE id = ?", parked)
	e.exec(`INSERT INTO events (kind, project_id, task_id, payload, created_at)
		VALUES (?, ?, ?, '{}', ?)`, evTaskFailed, e.project, parked, seedTS)
	e.runTaskToDone(top, author, reviewer)
	return e, author, plan, parked, top
}

// TestRetireDoneUnlandedClosesPlan: the retire of a done task whose
// landing never comes — the shape that kept its plan open forever —
// closes the plan, the done report naming each retired task with its
// reason verbatim, and the start-up reopen pass leaves the closed plan
// closed.
func TestRetireDoneUnlandedClosesPlan(t *testing.T) {
	e, _, plan, parked, top := seedRetireChain(t)
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state before the retire = %q, want active: top's landing is still to come", got)
	}

	res, err := e.eng.Retire(e.ctx, e.project, []int64{top}, "superseded by task 274", "foreman")
	if err != nil {
		t.Fatalf("retire top: %v", err)
	}
	if len(res.Retired) != 1 || res.Retired[0] != top {
		t.Fatalf("retire result = %+v, want top", res)
	}
	if got := e.taskState(top); got != taskRetired {
		t.Fatalf("top state = %q, want retired", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != "active" {
		t.Fatalf("plan state after retiring top = %q, want active: parked still holds it open", got)
	}

	// The parked task goes the same way — the plan closes, and the
	// report names both retired with the reasons verbatim.
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{parked}, "landed by hand on main", "user"); err != nil {
		t.Fatalf("retire parked: %v", err)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Fatalf("plan state after retiring both = %q, want done", got)
	}
	p, ok := e.eventPayloadOf(evPlanDone)
	if !ok {
		t.Fatal("no plan_done event after the retire")
	}
	want := fmt.Sprintf("retired: task %d parked (landed by hand on main), task %d top (superseded by task 274)", parked, top)
	if !strings.Contains(p.Reason, want) {
		t.Errorf("plan_done report = %q, want it to carry %q", p.Reason, want)
	}

	// The start-up reopen pass (#137) leaves the closed plan closed: a
	// retired task is settled, not live work.
	reopened, err := e.eng.ReopenStrandedPlans(e.ctx)
	if err != nil {
		t.Fatalf("reopen pass: %v", err)
	}
	if len(reopened) != 0 {
		t.Errorf("reopen pass = %v, want none over a retired task", reopened)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Errorf("plan state after the reopen pass = %q, want done", got)
	}

	// A second retire of the same task is refused as already retired.
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{top}, "again", "user"); err == nil || !strings.Contains(err.Error(), "retired already") {
		t.Errorf("retire a retired task: err = %v, want the refusal", err)
	}
}

// TestRetireEventsCarryCallerAndReason: each task_retired event records
// who asked — the foreman or the user — beside the reason verbatim.
func TestRetireEventsCarryCallerAndReason(t *testing.T) {
	e, _, _, _, top := seedRetireChain(t)
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{top}, "superseded by task 274", "foreman"); err != nil {
		t.Fatalf("retire top: %v", err)
	}
	var rows []eventPayload
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rowsel, err := tx.Query(`SELECT payload FROM events WHERE kind = ? ORDER BY id`, evTaskRetired)
		if err != nil {
			return err
		}
		defer rowsel.Close()
		for rowsel.Next() {
			var payload string
			if err := rowsel.Scan(&payload); err != nil {
				return err
			}
			var p eventPayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				rows = append(rows, p)
			}
		}
		return rowsel.Err()
	})
	if err != nil {
		t.Fatalf("read the task_retired events: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("task_retired events = %d, want one per task", len(rows))
	}
	if rows[0].Caller != "foreman" || rows[0].Reason != "superseded by task 274" || rows[0].Status != taskRetired {
		t.Errorf("task_retired payload = %+v, want the caller, the reason verbatim and the state", rows[0])
	}
}

// TestRetireParkedWithDoneDependent: the parked task alone cannot be
// retired — its done dependent, built on its branch, blocks the call and
// the refusal names it with its state — while retiring both in one call
// closes the plan.
func TestRetireParkedWithDoneDependent(t *testing.T) {
	e, _, plan, parked, top := seedRetireChain(t)

	_, err := e.eng.Retire(e.ctx, e.project, []int64{parked}, "park it", "foreman")
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("task %d top (done)", top)) {
		t.Fatalf("retire parked alone: err = %v, want the done dependent named", err)
	}
	if got := e.taskState(parked); got != taskNeedsForeman {
		t.Fatalf("parked state after the refused call = %q, want needs_foreman: the refusal wrote nothing", got)
	}

	res, err := e.eng.Retire(e.ctx, e.project, []int64{parked, top}, "superseded elsewhere", "foreman")
	if err != nil {
		t.Fatalf("retire both: %v", err)
	}
	if len(res.Retired) != 2 {
		t.Fatalf("retire result = %+v, want both", res)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Fatalf("plan state after retiring both = %q, want done", got)
	}
}

// TestRetireDependentStates: the dependents rule apart from cancel's —
// a failed dependent blocks the retirement (cancel exempts it), while a
// dependent already cancelled never does.
func TestRetireDependentStates(t *testing.T) {
	e, _, _, parked, top := seedRetireChain(t)
	e.exec("UPDATE tasks SET state = 'failed' WHERE id = ?", top)
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{parked}, "go", "foreman"); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("task %d top (failed)", top)) {
		t.Fatalf("retire under a failed dependent: err = %v, want the failure to block", err)
	}

	e.exec("UPDATE tasks SET state = 'cancelled' WHERE id = ?", top)
	res, err := e.eng.Retire(e.ctx, e.project, []int64{parked}, "a cancelled dependent never blocks", "foreman")
	if err != nil {
		t.Fatalf("retire under a cancelled dependent: %v", err)
	}
	if len(res.Retired) != 1 || res.Retired[0] != parked {
		t.Fatalf("retire result = %+v, want parked", res)
	}
}

// TestRetireLandGateHeldTask: a task held at the land gate retires —
// the hold is gone, nothing lands, and the land decision on it refuses
// naming the retirement instead of the gateless "not held".
func TestRetireLandGateHeldTask(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalTiers: "heavy"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{{Name: "solo", Spec: "the only work", Tier: "heavy"}})
	solo := res.Tasks[0].TaskID
	e.runTaskToDone(solo, author, reviewer)
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", solo); n != 1 {
		t.Fatalf("solo land holds = %d, want 1: the heavy tier holds its landing", n)
	}

	if _, err := e.eng.Retire(e.ctx, e.project, []int64{solo}, "the change ships by hand", "user"); err != nil {
		t.Fatalf("retire the held task: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", solo); n != 0 {
		t.Errorf("land_holds rows after the retire = %d, want none", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND status IN ('queued', 'leased', 'running')", solo); n != 0 {
		t.Errorf("live jobs after the retire = %d, want none", n)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planDone {
		t.Errorf("plan state = %q, want done over the retired task", got)
	}
	if _, err := e.eng.LandApprove(e.ctx, e.project, solo, false); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("task %d is retired: the change ships by hand", solo)) {
		t.Errorf("land approve after the retire: err = %v, want the retirement named", err)
	}
	if _, err := e.eng.LandReject(e.ctx, e.project, solo); err == nil || !strings.Contains(err.Error(), "is retired") {
		t.Errorf("land reject after the retire: err = %v, want the retirement named", err)
	}
	// No release path lands it: the land rule, run as a dependency's
	// landing or a hold's release would, leaves it alone.
	e.maybeLand(solo)
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'", solo); n != 0 {
		t.Errorf("land jobs after the retire = %d, want none", n)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", solo); n != 0 {
		t.Errorf("land holds after the land rule = %d, want none: a retired task is never re-held", n)
	}
	// The trailer path records nothing either: its commit on the branch
	// is a fact of the repo, not a landing (§6.2).
	if err := e.eng.RecordLanded(e.ctx, envRepo, "main", []LandedCommit{{TaskID: solo, Commit: "tr41l3r"}}); err != nil {
		t.Fatalf("record the trailers: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", solo); n != 0 {
		t.Errorf("landings after the trailer detection = %d, want none", n)
	}
}

// TestRetireLandedTaskRefused: a task with a landing record — pushed,
// waiting or local — is never retired.
func TestRetireLandedTaskRefused(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res := e.createPlan([]PlanTask{{Name: "solo", Spec: "the only work"}})
	solo := res.Tasks[0].TaskID
	e.runTaskToDone(solo, author, reviewer)
	g := e.leaseGit(author)
	if g == nil || g.TaskID != solo {
		t.Fatalf("lease = %+v, want solo's land job", g)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land solo: %s", ans)
	}
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{solo}, "too late", "user"); err == nil ||
		!strings.Contains(err.Error(), "is landed") {
		t.Errorf("retire a landed task: err = %v, want the refusal", err)
	}
	if got := e.taskState(solo); got != taskDone {
		t.Errorf("solo state = %q, want done: nothing was written", got)
	}
}

// TestRetireStopsLiveJobs: the retire stops the task's live jobs and
// leaves its result branch untouched.
func TestRetireStopsLiveJobs(t *testing.T) {
	e, _, plan, parked, top := seedRetireChain(t)
	e.exec("UPDATE tasks SET branch = 'brigada/task-N' WHERE id = ?", parked)

	// A live work job for the parked task, cancelled by the retire.
	e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", parked), ProjectID: e.project, TaskID: parked, Kind: KindWork, Mode: ModeFresh, Prompt: "again"})
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{parked, top}, "done for good", "user"); err != nil {
		t.Fatalf("retire both: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND status IN ('queued', 'leased', 'running')", parked); n != 0 {
		t.Errorf("parked's live jobs after the retire = %d, want none", n)
	}
	if got := e.str("SELECT COALESCE(branch, '') FROM tasks WHERE id = ?", parked); got != "brigada/task-N" {
		t.Errorf("parked branch after the retire = %q, want it untouched", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", plan); got != planDone {
		t.Errorf("plan state = %q, want done", got)
	}
}

// TestRetireCancelledTaskRefused: a cancelled task is settled its own
// way and is not retired.
func TestRetireCancelledTaskRefused(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE tasks SET state = 'cancelled' WHERE id = ?", e.task)
	if _, err := e.eng.Retire(e.ctx, e.project, []int64{e.task}, "go", "user"); err == nil ||
		!strings.Contains(err.Error(), "is cancelled") {
		t.Errorf("retire a cancelled task: err = %v, want the refusal", err)
	}
}
