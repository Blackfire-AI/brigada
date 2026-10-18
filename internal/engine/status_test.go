package engine

import (
	"database/sql"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// addPlan seeds one plan in the named project — the tests' shortcut for
// the states plan_create cannot reach yet (done, pending_approval, a
// second project); updated is the plan's updated_at, what the status
// reads as the moment a done plan went done.
func (e *env) addPlanState(project int64, title, state string, updated time.Time) int64 {
	e.t.Helper()
	var id int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`, project, title, state, seedTS, fmtTime(updated))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		e.t.Fatalf("add plan %q: %v", title, err)
	}
	return id
}

// addProject seeds one more project, for the scope tests.
func (e *env) addProject(repo string) int64 {
	e.t.Helper()
	var id int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (?, 'main', '{}', 'claude', 'sonnet', ?, ?)`, repo, seedTS, seedTS)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		e.t.Fatalf("add project %q: %v", repo, err)
	}
	return id
}

// addTaskToPlan seeds one task on a plan other than the fixture's.
func (e *env) addTaskToPlan(plan int64, title, state string) int64 {
	e.t.Helper()
	var id int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (?, ?, 'the spec', 'go test ./...', 'implement', ?, 'main', '{}', ?, ?)`,
			plan, title, state, seedTS, seedTS)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		e.t.Fatalf("add task %q: %v", title, err)
	}
	return id
}

// addTaskEvent seeds one event for a task — the shortcut for states the
// current rules cannot reach again (a second parking, another project).
func (e *env) addTaskEvent(task, project int64, kind, payload string) {
	e.t.Helper()
	e.exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
		VALUES (?, ?, ?, NULL, ?, ?)`, kind, project, task, payload, fmtTime(e.clock.Now()))
}

func TestStatusListsPlansAndTasks(t *testing.T) {
	e := newEnv(t)
	now := e.clock.Now()

	// The seeded plan is active with its ready task; a second task in
	// another state, its branch set, still belongs to it.
	reviewing := e.addTask("{}")
	e.exec(`UPDATE tasks SET state = ?, branch = ? WHERE id = ?`, taskReviewing, "task/2-1", reviewing)

	// A task parked for the user with no parking event: it queues with no
	// reason, and it is still one of the plan's tasks.
	userTask := e.addTask("{}")
	e.exec(`UPDATE tasks SET state = ? WHERE id = ?`, taskNeedsUser, userTask)

	// The done window: one plan finished an hour ago, one a day and an
	// hour ago; a plan waiting for approval and a cancelled one.
	recent := e.addPlanState(e.project, "freshly done", planDone, now.Add(-time.Hour))
	e.addTaskToPlan(recent, "the finished task", taskDone)
	e.addPlanState(e.project, "stale done", planDone, now.Add(-statusDoneWindow-time.Hour))
	approval := e.addPlanState(e.project, "a big plan", "pending_approval", now)
	e.addPlanState(e.project, "a cancelled plan", "cancelled", now)

	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	// The plans: the active one and the recently done one, in id order;
	// the stale, pending and cancelled plans stay out.
	if len(st.Plans) != 2 {
		t.Fatalf("plans = %d, want the active and the recent done: %+v", len(st.Plans), st.Plans)
	}
	active, done := st.Plans[0], st.Plans[1]
	if active.ID != e.plan || active.State != "active" || active.Title != "a plan" {
		t.Errorf("first plan = %+v, want the seeded active plan", active)
	}
	if active.RepoURL != "https://example.invalid/brigada.git" {
		t.Errorf("first plan repo = %q, want the project's URL", active.RepoURL)
	}
	wantTasks := []StatusTask{
		{ID: e.task, Title: "another task", Class: "implement", State: taskReady},
		{ID: reviewing, Title: "another task", Class: "implement", State: taskReviewing, Branch: "task/2-1"},
		{ID: userTask, Title: "another task", Class: "implement", State: taskNeedsUser},
	}
	if len(active.Tasks) != len(wantTasks) {
		t.Fatalf("the active plan's tasks = %+v, want %d", active.Tasks, len(wantTasks))
	}
	for i, want := range wantTasks {
		if got := active.Tasks[i]; !reflect.DeepEqual(got, want) {
			t.Errorf("task %d = %+v, want %+v", i, got, want)
		}
	}
	if done.ID != recent || done.State != planDone || done.Title != "freshly done" {
		t.Errorf("second plan = %+v, want the recently done one", done)
	}
	if len(done.Tasks) != 1 || done.Tasks[0].State != taskDone {
		t.Errorf("the done plan's tasks = %+v, want its one done task", done.Tasks)
	}

	// The needs-you queue: the user's task with no reason, then the plan
	// awaiting approval — in id order.
	if len(st.NeedsYou) != 2 {
		t.Fatalf("needs you = %+v, want the user's task and the pending plan", st.NeedsYou)
	}
	if got := st.NeedsYou[0]; got.Kind != "task" || got.ID != userTask || got.State != taskNeedsUser || got.Reason != "" || got.PlanID != e.plan {
		t.Errorf("the task entry = %+v, want the needs_user task, unreasoned, on its plan", got)
	}
	if got := st.NeedsYou[1]; got.Kind != "plan" || got.ID != approval || got.State != "pending_approval" || got.Reason != "" {
		t.Errorf("the plan entry = %+v, want the pending_approval plan, unreasoned", got)
	}
}

func TestStatusNeedsYouReasonsFromTheLatestParkingEvent(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	spare := e.agented("spare", "meta", "review")

	// begin creates a plan through the real path — active, its one task
	// ready, the task's work job queued — and leases the work job with
	// the author, so each case below drives only its completion.
	begin := func() (task int64, work *LeaseGrant) {
		t.Helper()
		res, err := e.eng.CreatePlan(e.ctx, e.project, "a plan", []PlanTask{{Title: "a task", Spec: "do the thing"}})
		if err != nil {
			t.Fatalf("create plan: %v", err)
		}
		g := e.leaseAny(author)
		if g == nil || g.TaskID != res.TaskID {
			t.Fatalf("the work job was not leased: %+v, want task %d's", g, res.TaskID)
		}
		return res.TaskID, g
	}

	// A failed run: the failure's words.
	failedTask, work := begin()
	if ans := e.complete(work, Completion{Error: "the adapter died"}); ans != AnsOK {
		t.Fatalf("complete the failed run: %s", ans)
	}

	// A worker's question: the question, verbatim.
	helpTask, work := begin()
	if ans := e.complete(work, Completion{Result: `{"status":"needs_help","question":"which API should the client call?"}`}); ans != AnsOK {
		t.Fatalf("complete the needs_help run: %s", ans)
	}

	// A failed check of a task with no fix rounds behind it (a class
	// without them): the check's output tail, flattened to one line.
	checkTask, work := begin()
	e.exec(`UPDATE tasks SET class = 'answer' WHERE id = ?`, checkTask)
	if ans := e.complete(work, doneOn("task/3-1", failCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check run: %s", ans)
	}

	// Review changes past the task's review rounds: the out-of-rounds
	// words, with the findings that asked for the round beside them —
	// the foreman reads what the review said, not only that the budget
	// ran out. No verdict twice over: the no-verdict's words. Both park
	// from the review job their done work queued (§7).
	reviewVerdict := func(runner int64, task int64, result string) {
		t.Helper()
		rg := e.leaseAny(runner)
		if rg == nil || rg.TaskID != task || rg.Kind != KindReview {
			t.Fatalf("the review job of task %d was not leased: %+v", task, rg)
		}
		if ans := e.complete(rg, Completion{Result: result}); ans != AnsOK {
			t.Fatalf("complete the review of task %d: %s", task, ans)
		}
	}
	changesTask, work := begin()
	e.exec(`UPDATE tasks SET caps = '{"review_rounds":0}' WHERE id = ?`, changesTask)
	if ans := e.complete(work, doneOn("task/4-1", passCheck)); ans != AnsOK {
		t.Fatalf("complete the work before the changes review: %s", ans)
	}
	reviewVerdict(reviewer, changesTask, `{"verdict":"changes","findings":"the frob went boom"}`)
	noVerdictTask, work := begin()
	if ans := e.complete(work, doneOn("task/5-1", passCheck)); ans != AnsOK {
		t.Fatalf("complete the work before the empty review: %s", ans)
	}
	reviewVerdict(reviewer, noVerdictTask, `{"verdict":"approve","findings":"ok"}`)
	reviewVerdict(spare, noVerdictTask, `{}`)

	// A question longer than one line flattens and cuts to about the
	// limit; a task parked twice reads its latest event.
	longTask := e.addTask("{}")
	e.exec(`UPDATE tasks SET state = ? WHERE id = ?`, taskNeedsForeman, longTask)
	e.addTaskEvent(longTask, e.project, evTaskNeedsHelp,
		`{"question":"`+strings.Repeat("very ", 40)+`long question?"}`)
	twiceTask := e.addTask("{}")
	e.exec(`UPDATE tasks SET state = ? WHERE id = ?`, taskNeedsForeman, twiceTask)
	e.addTaskEvent(twiceTask, e.project, evTaskFailed, `{"reason":"the first word"}`)
	e.clock.Advance(time.Second)
	e.addTaskEvent(twiceTask, e.project, evTaskNeedsHelp, `{"question":"the latest word"}`)

	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	reasons := map[int64]string{}
	for _, it := range st.NeedsYou {
		if it.Kind != "task" {
			t.Fatalf("needs you holds %+v, want tasks alone", it)
		}
		reasons[it.ID] = it.Reason
	}
	want := map[int64]string{
		failedTask:  "the adapter died",
		helpTask:    "which API should the client call?",
		checkTask:   "FAIL TestBoom the frobnicator was off",
		changesTask: "the task's review_rounds are spent: the frob went boom",
		twiceTask:   "the latest word",
	}
	for id, wantReason := range want {
		if got := reasons[id]; got != wantReason {
			t.Errorf("reason of task %d = %q, want %q", id, got, wantReason)
		}
	}
	// The spent review retry says plainly that the task is parked and
	// waits for the foreman, and names both reviews of the cycle.
	wantParked := "task " + strconv.FormatInt(noVerdictTask, 10) + " is parked (needs_foreman): its review retry is spent"
	if got := reasons[noVerdictTask]; !strings.HasPrefix(got, wantParked) ||
		!strings.Contains(got, "reviewer/google-1: thin approve") || !strings.Contains(got, "spare/meta-1: no verdict") {
		t.Errorf("reason of the spent-retry task = %q, want it to start %q and name both reviews", got, wantParked)
	}
	if len(reasons) != len(want)+2 {
		t.Errorf("needs you holds %d entries, want %d: %v", len(reasons), len(want)+2, reasons)
	}
	if got := reasons[longTask]; len(got) > needsYouReasonLimit || strings.Contains(got, "\n") {
		t.Errorf("the long reason = %q (%d bytes), want one line within %d bytes", got, len(got), needsYouReasonLimit)
	}
}

// TestStatusShowsTheLatestForemanReport covers the report the status
// carries (§3, §10): the scope's latest foreman_report — the moment it
// landed and the first lines of its text, the rest left to the history
// — or nothing at all when no turn has reported yet.
func TestStatusShowsTheLatestForemanReport(t *testing.T) {
	e := newEnv(t)
	addReport := func(project int64, text string) {
		e.t.Helper()
		e.exec(`INSERT INTO events (kind, project_id, payload, created_at)
			VALUES (?, ?, ?, ?)`, evForemanReport, project,
			`{"report":`+mustJSON(text)+`}`, fmtTime(e.clock.Now()))
	}

	// Two reports of the fixture's project and an older one of another:
	// a scoped read shows the project's latest, an unscoped one the
	// latest of them all, and a scope without reports shows none.
	other := e.addProject("https://example.invalid/other.git")
	addReport(other, "their first turn ended quietly")
	e.clock.Advance(time.Minute)
	addReport(e.project, "woke for the question of task 4; answered it")
	e.clock.Advance(time.Minute)
	at := fmtTime(e.clock.Now())
	addReport(e.project, "plan 2 done: every task approved\nmerge task/6-1\nthen task/7-1\nthen task/8-1\nthen task/9-1\nthen task/10-1")

	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Report == nil {
		t.Fatal("the scoped status carries no report, want the project's latest")
	}
	if st.Report.ProjectID != e.project || st.Report.CreatedAt != at {
		t.Errorf("the report = project %d at %s, want project %d at %s (the latest, not the first)",
			st.Report.ProjectID, st.Report.CreatedAt, e.project, at)
	}
	// The first few lines show; the sixth stays for the history.
	want := "plan 2 done: every task approved\nmerge task/6-1\nthen task/7-1\nthen task/8-1\nthen task/9-1"
	if st.Report.Text != want {
		t.Errorf("the report text = %q, want the first %d lines:\n%q", st.Report.Text, statusReportLines, want)
	}

	all, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status of every project: %v", err)
	}
	if all.Report == nil || all.Report.ProjectID != e.project || all.Report.CreatedAt != at {
		t.Errorf("the unscoped report = %+v, want the latest of all the projects", all.Report)
	}

	theirs, err := e.eng.Status(e.ctx, other)
	if err != nil {
		t.Fatalf("status of the other project: %v", err)
	}
	if theirs.Report == nil || theirs.Report.ProjectID != other || theirs.Report.Text != "their first turn ended quietly" {
		t.Errorf("the other project's report = %+v, want its own", theirs.Report)
	}

	// A project whose foreman never reported shows nothing, not an
	// empty report.
	fresh := e.addProject("https://example.invalid/fresh.git")
	st, err = e.eng.Status(e.ctx, fresh)
	if err != nil {
		t.Fatalf("status of the fresh project: %v", err)
	}
	if st.Report != nil {
		t.Errorf("the fresh project's report = %+v, want none", st.Report)
	}
}

func TestStatusScopesToTheCallersProject(t *testing.T) {
	e := newEnv(t)

	// A second project with its own active plan and a parked task.
	other := e.addProject("https://example.invalid/other.git")
	otherPlan := e.addPlanState(other, "the other plan", "active", e.clock.Now())
	otherTask := e.addTaskToPlan(otherPlan, "the other task", taskNeedsForeman)
	e.addTaskEvent(otherTask, other, evTaskFailed, `{"reason":"their adapter died"}`)
	e.addTaskEvent(e.task, e.project, evTaskNeedsHelp, `{"question":"the seeded task's question"}`)
	e.exec(`UPDATE tasks SET state = ? WHERE id = ?`, taskNeedsForeman, e.task)

	// Every project: both plans, both parked tasks.
	all, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status of every project: %v", err)
	}
	if len(all.Plans) != 2 || len(all.NeedsYou) != 2 {
		t.Fatalf("unscoped status = %d plans %+v, want both projects' plans and queues", len(all.Plans), all.NeedsYou)
	}

	// The foreman's scope: its own project alone.
	own, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatalf("status of the foreman's project: %v", err)
	}
	if len(own.Plans) != 1 || own.Plans[0].ID != e.plan {
		t.Errorf("scoped plans = %+v, want the foreman's own plan alone", own.Plans)
	}
	if len(own.NeedsYou) != 1 || own.NeedsYou[0].ID != e.task {
		t.Errorf("scoped needs you = %+v, want the foreman's own task alone", own.NeedsYou)
	}
	if own.NeedsYou[0].Reason != "the seeded task's question" {
		t.Errorf("scoped reason = %q, want the question", own.NeedsYou[0].Reason)
	}

	// The other project scoped the same way.
	theirs, err := e.eng.Status(e.ctx, other)
	if err != nil {
		t.Fatalf("status of the other project: %v", err)
	}
	if len(theirs.Plans) != 1 || theirs.Plans[0].ID != otherPlan ||
		len(theirs.NeedsYou) != 1 || theirs.NeedsYou[0].ID != otherTask {
		t.Errorf("the other project's status = %+v / %+v, want its plan and task alone", theirs.Plans, theirs.NeedsYou)
	}
}

// TestStatusListsRunningJobsWithTheirLastEvent: a heartbeat's newest
// progress event lands on the job, flattened to one line, and the
// status lists the held job with it (§10); a heartbeat without events
// keeps the last one, and a finished job leaves the list.
func TestStatusListsRunningJobsWithTheirLastEvent(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	prog := Progress{LastProgressAt: e.clock.Now(), Events: []string{"tool_start Read", "tool_start Edit\n  internal/runner/review.go"}}
	if ans, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, prog); err != nil || ans != AnsOK {
		t.Fatalf("heartbeat = %s, %v; want ok", ans, err)
	}
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("an eventless heartbeat = %s; want ok", ans)
	}

	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Running) != 1 {
		t.Fatalf("running = %+v, want the one held job", st.Running)
	}
	j := st.Running[0]
	if j.ID != g.JobID || j.Kind != string(KindWork) || j.LeasedAt == "" {
		t.Errorf("running job = %+v, want job %d, kind work, with its lease start", j, g.JobID)
	}
	if want := "tool_start Edit internal/runner/review.go"; j.LastEvent != want {
		t.Errorf("last event = %q, want the newest event on one line %q", j.LastEvent, want)
	}

	if ans := e.complete(g, Completion{Result: `{"status":"done","summary":"ok"}`}); ans != AnsOK {
		t.Fatalf("complete = %s; want ok", ans)
	}
	if st, err = e.eng.Status(e.ctx, 0); err != nil || len(st.Running) != 0 {
		t.Errorf("running after the job finished = %+v (%v), want none", st.Running, err)
	}
}

// TestHeartbeatLastEventSkipsToolEnds: a tool end carries a call id,
// not a tool name, so the last event stays the newest event that is not
// one (§10); a heartbeat of tool ends alone keeps it.
func TestHeartbeatLastEventSkipsToolEnds(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	for _, events := range [][]string{{"tool_start Edit", "tool_end toolu_x"}, {"tool_end toolu_y"}} {
		prog := Progress{LastProgressAt: e.clock.Now(), Events: events}
		if ans, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, prog); err != nil || ans != AnsOK {
			t.Fatalf("heartbeat = %s, %v; want ok", ans, err)
		}
		if got, want := e.str("SELECT COALESCE(last_event, '') FROM jobs WHERE id = ?", g.JobID), "tool_start Edit"; got != want {
			t.Errorf("last_event after %q = %q, want %q", events, got, want)
		}
	}
}

// TestNewLeaseClearsTheLastEvent: a job re-leased after its lease
// expired starts with no last
// event, so the status never shows the previous attempt's last tool
// call beside the new attempt's fresh elapsed time.
func TestNewLeaseClearsTheLastEvent(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	prog := Progress{LastProgressAt: e.clock.Now(), Events: []string{"tool_start Edit"}}
	if ans, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, prog); err != nil || ans != AnsOK {
		t.Fatalf("heartbeat = %s, %v; want ok", ans, err)
	}
	e.expireLease()
	g2 := e.leaseAny(e.runner1)
	if g2 == nil || g2.JobID != g.JobID {
		t.Fatalf("re-lease = %+v, want job %d again", g2, g.JobID)
	}
	if got := e.str("SELECT COALESCE(last_event, '') FROM jobs WHERE id = ?", g.JobID); got != "" {
		t.Errorf("last_event after a new lease = %q, want it cleared", got)
	}
}
