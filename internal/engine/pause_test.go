package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// pauseProject pauses the fixture project's dispatch and resumeProject
// lifts it; pauseEngine and resumeEngine do the same for the engine's
// own scope (§8.3). Wrappers so each test reads as the design does.
func (e *env) pauseProject(origin, caller string) PauseResult {
	e.t.Helper()
	res, err := e.eng.Pause(e.ctx, e.project, origin, caller)
	if err != nil {
		e.t.Fatalf("pause the project: %v", err)
	}
	return res
}

func (e *env) pauseEngine(origin, caller string) PauseResult {
	e.t.Helper()
	res, err := e.eng.Pause(e.ctx, 0, origin, caller)
	if err != nil {
		e.t.Fatalf("pause the engine: %v", err)
	}
	return res
}

func (e *env) resumeProject(caller string) ResumeResult {
	e.t.Helper()
	res, err := e.eng.Resume(e.ctx, e.project, caller)
	if err != nil {
		e.t.Fatalf("resume the project: %v", err)
	}
	return res
}

func (e *env) resumeEngine(caller string) ResumeResult {
	e.t.Helper()
	res, err := e.eng.Resume(e.ctx, 0, caller)
	if err != nil {
		e.t.Fatalf("resume the engine: %v", err)
	}
	return res
}

// livePauses counts the stored pauses still standing for one scope
// (project 0 the engine's, §8.3).
func (e *env) livePauses(scope int64) int64 {
	return e.int("SELECT COUNT(*) FROM pauses WHERE ended_at IS NULL AND COALESCE(project_id, 0) = ?", scope)
}

// foremanWakesToday counts the day's foreman turns of the fixture
// project — the count the wake budget charges (§4.3, wakeOutcomeTx) —
// so a test can assert what a resume spent: one wake, not one per
// event and not a second one for a queued turn.
func (e *env) foremanWakesToday() int64 {
	dayStart, _ := wakeDayBounds(e.clock.Now())
	return e.int("SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = 'foreman' AND created_at >= ?",
		e.project, fmtTime(dayStart))
}

// eventField reads one payload field of every event of kind, in order.
func (e *env) eventField(kind, field string) []string {
	e.t.Helper()
	var out []string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT json_extract(payload, '$."+field+"') FROM events WHERE kind = ? ORDER BY id", kind)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s sql.NullString
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s.String)
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatalf("read the %s events: %v", kind, err)
	}
	return out
}

// cancelForeman cancels the project's live foreman turn: its inbox rows
// return pending with no wake (§4.2), the staging a paused scope's
// tests need — a judgment event waiting with no live turn to fold
// behind. It returns the cancelled turn's id, which the returned rows
// keep as the last turn that carried them.
func (e *env) cancelForeman() int64 {
	e.t.Helper()
	job := e.int("SELECT id FROM jobs WHERE project_id = ? AND kind = 'foreman' AND status IN ('queued', 'leased', 'running')", e.project)
	if job == 0 {
		e.t.Fatal("no live foreman turn to cancel")
	}
	e.cancel(job)
	return job
}

// TestPauseOriginPrecedence: a user's origin outranks a drain's, both
// ways, on the one live pause a scope keeps; pausing an already paused
// scope starts nothing, resuming an unpaused scope says so, the events
// record the standing origin and the caller, and a lifted pause keeps
// its interval.
func TestPauseOriginPrecedence(t *testing.T) {
	e := newEnv(t)
	if res := e.pauseProject(PauseOriginDrain, "cli"); !res.Started || res.Origin != PauseOriginDrain {
		t.Fatalf("the drain pause = %+v, want it started with origin drain", res)
	}
	// A user pause over a drain pause makes it the user's (§8.3).
	if res := e.pauseProject(PauseOriginUser, "cli"); res.Started || res.Origin != PauseOriginUser {
		t.Fatalf("the user pause over the drain = %+v, want the same pause made the user's", res)
	}
	// A drain over a user's pause leaves the origin user (§8.3).
	if res := e.pauseProject(PauseOriginDrain, "stop --engine --drain"); res.Started || res.Origin != PauseOriginUser {
		t.Fatalf("the drain pause over the user's = %+v, want the user's pause untouched", res)
	}
	if got := e.livePauses(e.project); got != 1 {
		t.Fatalf("live pauses = %d, want the one pause the scope keeps", got)
	}
	if got, want := e.eventField(evDispatchPaused, "origin"), []string{PauseOriginDrain, PauseOriginUser, PauseOriginUser}; !equalStrings(got, want) {
		t.Errorf("dispatch_paused origins = %v, want %v", got, want)
	}
	if got, want := e.eventField(evDispatchPaused, "caller"), []string{"cli", "cli", "stop --engine --drain"}; !equalStrings(got, want) {
		t.Errorf("dispatch_paused callers = %v, want %v", got, want)
	}
	if got := e.countEvents(evDispatchResumed); got != 0 {
		t.Errorf("dispatch_resumed events = %d, want none yet", got)
	}
	// Resume lifts that scope's pause and no other, and resuming an
	// unpaused scope says so.
	if res := e.resumeProject("cli"); !res.Resumed || res.Origin != PauseOriginUser {
		t.Fatalf("the resume = %+v, want the user's pause lifted", res)
	}
	if res := e.resumeProject("cli"); res.Resumed {
		t.Fatalf("the resume of an unpaused scope = %+v, want it to say so", res)
	}
	if got := e.livePauses(e.project); got != 0 {
		t.Fatalf("live pauses after the lift = %d, want none", got)
	}
	// A lifted pause keeps its interval (§8.3 "Clocks").
	started := e.str("SELECT started_at FROM pauses WHERE COALESCE(project_id, 0) = ?", e.project)
	ended := e.str("SELECT ended_at FROM pauses WHERE COALESCE(project_id, 0) = ?", e.project)
	if started == "" || ended == "" || ended < started {
		t.Fatalf("the stored interval = (%q, %q), want a start and its end", started, ended)
	}
	if got := e.countEvents(evDispatchResumed); got != 1 {
		t.Errorf("dispatch_resumed events = %d, want the one lift", got)
	}
}

// TestPauseEngineScopeKeepsEveryInterval: the engine's own pause
// follows the same origin rules on its own scope row, and every pause
// interval is kept: a second pause after a lift is a second row.
func TestPauseEngineScopeKeepsEveryInterval(t *testing.T) {
	e := newEnv(t)
	e.pauseEngine(PauseOriginDrain, "stop --engine --drain")
	if res := e.pauseEngine(PauseOriginUser, "cli"); res.Started || res.Origin != PauseOriginUser {
		t.Fatalf("the user pause over the engine's drain = %+v, want the same pause made the user's", res)
	}
	if got := e.livePauses(0); got != 1 {
		t.Fatalf("live engine pauses = %d, want one", got)
	}
	if res := e.resumeEngine("cli"); !res.Resumed || res.Origin != PauseOriginUser {
		t.Fatalf("the engine's resume = %+v, want the user's pause lifted", res)
	}
	e.pauseEngine(PauseOriginUser, "cli")
	e.resumeEngine("cli")
	if got := e.int("SELECT COUNT(*) FROM pauses WHERE COALESCE(project_id, 0) = 0"); got != 2 {
		t.Fatalf("stored engine intervals = %d, want both kept", got)
	}
	if got := e.livePauses(0); got != 0 {
		t.Fatalf("live engine pauses after the lifts = %d, want none", got)
	}
}

// TestPausedScopeOffersNoJobOfAnyKind: while the fixture project is
// paused, none of its queued jobs leases — work, fix, review, plan
// review, land or a foreman turn woken by judgment events, a turn
// already queued when the pause began included — while another
// project's job leases as usual, and the paused jobs lease in their
// usual order once the pause lifts.
func TestPausedScopeOffersNoJobOfAnyKind(t *testing.T) {
	e := newEnv(t)
	// One job of every kind on the fixture project, each on its own key.
	fix, review, land := e.addTask("{}"), e.addTask("{}"), e.addTask("{}")
	e.submit(e.work())
	e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", fix), ProjectID: e.project, TaskID: fix,
		Kind: KindFix, Mode: ModeSession, Prompt: "fix it"})
	e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", review), ProjectID: e.project, TaskID: review,
		Kind: KindReview, Mode: ModeFresh, Prompt: "review it"})
	e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", land), ProjectID: e.project, TaskID: land,
		Kind: KindLand, Mode: ModeFresh, Prompt: "land it", Labels: []string{"git"}})
	e.submit(SubmitParams{Key: planReviewKey(e.plan), ProjectID: e.project,
		Kind: KindPlanReview, Mode: ModeFresh, Prompt: "review the plan"})
	e.submit(e.foremanTurn())
	// An unpaused project with one queued job of its own.
	other := e.addProject("https://example.invalid/second.git")
	otherPlan := e.addPlanState(other, "the other plan", "active", e.clock.Now())
	otherTask := e.addTaskToPlan(otherPlan, "the other task", "ready")
	otherJob := e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", otherTask), ProjectID: other, TaskID: otherTask,
		Kind: KindWork, Mode: ModeFresh, Prompt: "the other work"})

	e.pauseProject(PauseOriginUser, "cli")
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != otherJob.JobID {
		t.Fatalf("the poll = %+v, want the unpaused project's job %d", g, otherJob.JobID)
	}
	if again := e.leaseAny(e.runner2); again != nil {
		t.Fatalf("a paused scope's job leased: %+v", again)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE project_id = ? AND status = 'queued'", e.project); got != 6 {
		t.Fatalf("the paused project's queued jobs = %d, want all six waiting", got)
	}
	// The pause lifted, the same jobs lease in their usual order.
	e.resumeProject("cli")
	if g3 := e.leaseAny(e.runner2); g3 == nil || g3.Kind != KindWork || g3.TaskID != e.task {
		t.Fatalf("after the resume the poll = %+v, want the fixture project's oldest queued job", g3)
	}
}

// TestRunningJobCompletesIntoAPausedScope: running jobs are untouched
// by the pause; the completion records and queues its follow-on — the
// review — which waits with everything else and leases at the resume.
func TestRunningJobCompletesIntoAPausedScope(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("the work job was not leased")
	}
	e.pauseProject(PauseOriginUser, "cli")
	if ans := e.complete(g, doneOn(branchOf(e.task), passCheck)); ans != AnsOK {
		t.Fatalf("complete the running work under the pause: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state = %q, want reviewing", state)
	}
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", e.task)
	if review == 0 || e.str("SELECT status FROM jobs WHERE id = ?", review) != statusQueued {
		t.Fatalf("the follow-on review = %d, want it queued under the pause", review)
	}
	if again := e.leaseAny(reviewer); again != nil {
		t.Fatalf("the follow-on leased under the pause: %+v", again)
	}
	e.resumeProject("cli")
	if g2 := e.leaseAny(reviewer); g2 == nil || g2.JobID != review {
		t.Fatalf("after the resume the poll = %+v, want the queued review %d", g2, review)
	}
}

// TestUserMessageTurnLeasesWhilePaused: a foreman turn that carries a
// user message still leases under the pause (§8.3), for the user asking
// in person is not what a pause holds; the message reaches the turn and
// is delivered to it as any wake's input is.
func TestUserMessageTurnLeasesWhilePaused(t *testing.T) {
	e := newEnv(t)
	e.pauseProject(PauseOriginUser, "cli")
	res, err := e.eng.AskForeman(e.ctx, "", "go on anyway", "c1")
	if err != nil || res.Outcome != WakeWoken {
		t.Fatalf("ask under the pause = %+v, %v; want woken", res, err)
	}
	g := e.leaseForeman()
	if g == nil || g.Kind != KindForeman {
		t.Fatalf("the turn the message woke = %+v, want it leased under the pause", g)
	}
	if !strings.Contains(g.Prompt, "go on anyway") {
		t.Errorf("the turn's prompt does not carry the message:\n%s", g.Prompt)
	}
	if want := []string{fmt.Sprintf("delivered:%d", g.JobID)}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("the inbox = %v, want the message delivered to the turn", e.inboxStates())
	}
}

// TestUserMessagePastAQueuedTurnWhilePaused: a judgment turn queued
// before the pause holds the foreman key, and the one-live-job-per-key
// rule would fold the user's message behind it — trapped, for the pause
// keeps the turn from ever running. The queued turn leases instead (the
// user is present), runs with the events it was given then, and its
// completion wakes the turn that carries the message, which leases
// under the pause too.
func TestUserMessagePastAQueuedTurnWhilePaused(t *testing.T) {
	e := newEnv(t)
	e.park("the first question") // the judgment turn, queued with its event
	e.pauseProject(PauseOriginUser, "cli")
	res, err := e.eng.AskForeman(e.ctx, "", "and my message", "c1")
	if err != nil || res.Outcome != WakeTurnRunning {
		t.Fatalf("ask behind the queued turn = %+v, %v; want the fold", res, err)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns = %d, want the queued judgment turn alone", got)
	}
	g := e.leaseForeman()
	if g == nil || g.Kind != KindForeman {
		t.Fatalf("the queued turn = %+v, want it leased under the pause", g)
	}
	if !strings.Contains(g.Prompt, "the first question") || strings.Contains(g.Prompt, "and my message") {
		t.Fatalf("the queued turn's prompt does not carry the events it was given then:\n%s", g.Prompt)
	}
	e.complete(g, Completion{Result: `{"status":"done","summary":"answered"}`})
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the queued one = %d, want the one turn carrying the message", got)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC LIMIT 1")
	if !strings.Contains(prompt, "and my message") {
		t.Fatalf("the turn after the queued one does not carry the message:\n%s", prompt)
	}
	g2 := e.leaseForeman()
	if g2 == nil || g2.JobID == g.JobID {
		t.Fatalf("the message's turn = %+v, want it leased under the pause", g2)
	}
}

// TestPauseDoesNotCountTheTurnTheLeaseGateLetsThrough: a queued foreman
// turn a pending user message waits on leases under the pause, so the
// status counts it neither under the pause nor as a held wait; a queued
// judgment turn with no message waiting is the pause's.
func TestPauseDoesNotCountTheTurnTheLeaseGateLetsThrough(t *testing.T) {
	e := newEnv(t)
	e.park("the first question") // the judgment turn, queued with its event
	e.pauseProject(PauseOriginUser, "cli")
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pauses) != 1 || st.Pauses[0].Queued != 1 {
		t.Fatalf("pauses = %+v, want the queued judgment turn counted under the pause", st.Pauses)
	}
	if _, err := e.eng.AskForeman(e.ctx, "", "and my message", "c1"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if st, err = e.eng.Status(e.ctx, 0); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pauses) != 1 || st.Pauses[0].Queued != 0 {
		t.Errorf("pauses = %+v, want the turn the user's message waits on left out of the count", st.Pauses)
	}
	// The turn leases through the pause, so whatever holds it is a real
	// wait (here no runner offers its agent), not the pause's.
	for _, h := range st.Held {
		if h.Kind != string(KindForeman) {
			t.Errorf("held = %+v, want at most the foreman turn", st.Held)
		}
	}
}

// TestPauseKeepsJudgmentEventsPending: while the scope is paused, the
// engine submits no headless turn for a judgment event — it stays
// pending in the inbox, the wake's outcome says paused — and the resume
// evaluation is what frees it.
func TestPauseKeepsJudgmentEventsPending(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	turn := e.cancelForeman() // the row returns pending, and a cancel wakes nothing
	e.pauseProject(PauseOriginUser, "cli")
	var out WakeOutcome
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		out, err = e.eng.wakeOutcomeTx(tx, e.project, e.clock.Now())
		return err
	}); err != nil {
		t.Fatalf("evaluate the wake: %v", err)
	}
	if out != WakePaused {
		t.Fatalf("the wake's outcome = %s, want paused", out)
	}
	if want := []string{fmt.Sprintf("pending:%d", turn)}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("the inbox = %v, want the event pending", e.inboxStates())
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns = %d, want the cancelled one alone", got)
	}
	e.resumeProject("cli")
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the resume = %d, want the held wake submitted", got)
	}
	// One wake charged: the day counts the cancelled turn and the
	// resume's one, and nothing was refused (§4.3).
	if got := e.foremanWakesToday(); got != 2 {
		t.Fatalf("the day's foreman turns = %d, want the cancelled one and the resume's one", got)
	}
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 0 {
		t.Errorf("budget-spent events = %d, want none", got)
	}
}

// TestResumeUnderARemainingScope: resuming one project under an engine
// pause frees nothing of that project, and resuming the engine over a
// project's own pause frees nothing of the project either (§8.3); the
// second lift of each pair is what submits.
func TestResumeUnderARemainingScope(t *testing.T) {
	// The engine's pause outlasts the project's own lift.
	e := newEnv(t)
	e.park("which file?")
	e.cancelForeman()
	e.pauseProject(PauseOriginUser, "cli")
	e.pauseEngine(PauseOriginUser, "cli")
	if res := e.resumeProject("cli"); !res.Resumed {
		t.Fatalf("the project's resume = %+v, want its own pause lifted", res)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns under the engine's pause = %d, want the cancelled one alone", got)
	}
	if res := e.resumeEngine("cli"); !res.Resumed {
		t.Fatalf("the engine's resume = %+v, want the lift", res)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the engine's lift = %d, want the held wake submitted", got)
	}
	if got := e.foremanWakesToday(); got != 2 {
		t.Fatalf("the day's foreman turns = %d, want the cancelled one and the one the lift woke", got)
	}
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 0 {
		t.Errorf("budget-spent events = %d, want none", got)
	}

	// The project's own pause outlasts the engine's lift.
	e2 := newEnv(t)
	e2.park("which file?")
	e2.cancelForeman()
	e2.pauseProject(PauseOriginUser, "cli")
	e2.pauseEngine(PauseOriginUser, "cli")
	if res := e2.resumeEngine("cli"); !res.Resumed {
		t.Fatalf("the engine's resume = %+v, want the lift", res)
	}
	if got := e2.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns under the project's own pause = %d, want the cancelled one alone", got)
	}
	e2.resumeProject("cli")
	if got := e2.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the project's lift = %d, want the held wake submitted", got)
	}
	if got := e2.foremanWakesToday(); got != 2 {
		t.Fatalf("the day's foreman turns = %d, want the cancelled one and the one the lift woke", got)
	}
	if got := e2.countEvents(evForemanWakeBudgetSpent); got != 0 {
		t.Errorf("budget-spent events = %d, want none", got)
	}
}

// TestResumeRunsTheQueuedTurnThenOneCoalescedTurn: at the resume, a
// foreman turn queued before the pause runs with the events it was
// given then; the judgment events that piled up behind the pause
// coalesce into one turn after it, so the freed project is charged one
// wake — not one per event and not a second one for the queued turn.
// The budget is two: the queued turn spent one charge when it was
// submitted, so the resume has exactly one wake left — enough for the
// coalesced turn, and a charge more would be refused and named.
func TestResumeRunsTheQueuedTurnThenOneCoalescedTurn(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 2 })
	e.park("the first question")  // the turn, queued with its event
	e.park("the second question") // parks again: the event waits behind the live turn
	e.park("the third question")
	e.pauseProject(PauseOriginUser, "cli")
	e.resumeProject("cli")
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("the resume submitted %d new turns, want the queued turn to run first", got-1)
	}
	g := e.leaseForeman()
	if !strings.Contains(g.Prompt, "the first question") || strings.Contains(g.Prompt, "the second question") {
		t.Fatalf("the queued turn's prompt does not carry the events it was given then:\n%s", g.Prompt)
	}
	e.complete(g, Completion{Result: `{"status":"done","summary":"answered"}`})
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the queued one = %d, want one coalesced turn for both events", got)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC LIMIT 1")
	if !strings.Contains(prompt, "the second question") || !strings.Contains(prompt, "the third question") {
		t.Fatalf("the coalesced turn does not carry both pending events:\n%s", prompt)
	}
	// The charge (§4.3): the day counts the queued turn and the one
	// coalesced turn — two of the budget's two, nothing refused — so the
	// resume spent one wake for both events, and the queued turn's run
	// cost no second charge.
	if got := e.foremanWakesToday(); got != 2 {
		t.Fatalf("the day's foreman turns = %d, want the queued one and the coalesced one", got)
	}
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 0 {
		t.Fatalf("budget-spent events = %d, want the coalesced wake to have fit the budget", got)
	}
	// The budget is now exactly spent: a later judgment event waits for
	// the day to reset — the proof that the resume used its one wake and
	// left the budget at exactly the two charges there were.
	g2 := e.leaseForeman()
	e.complete(g2, Completion{Result: `{"status":"done","summary":"answered"}`})
	e.park("a fourth question")
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the spent budget = %d, want the two charges only", got)
	}
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 1 {
		t.Fatalf("budget-spent events after the later event = %d, want the day's budget exactly spent", got)
	}
}

// TestStartupRecoverySkipsAPausedScope: the start-up re-evaluation of
// pending foreman wake-ups (§13) submits nothing for a project a stored
// pause covers — its own or the engine's — and the resume evaluation is
// what frees them.
func TestStartupRecoverySkipsAPausedScope(t *testing.T) {
	t.Run("the project's own pause", func(t *testing.T) {
		e := newEnv(t)
		e.park("which file?")
		turn := e.cancelForeman()
		e.pauseProject(PauseOriginUser, "cli")
		if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if got := e.foremanJobs(); got != 1 {
			t.Fatalf("foreman turns after the recovery = %d, want none submitted", got)
		}
		if want := []string{fmt.Sprintf("pending:%d", turn)}; !equalStrings(e.inboxStates(), want) {
			t.Errorf("the inbox = %v, want the event pending", e.inboxStates())
		}
		e.resumeProject("cli")
		if got := e.foremanJobs(); got != 2 {
			t.Fatalf("foreman turns after the resume = %d, want the held wake submitted", got)
		}
		if got := e.foremanWakesToday(); got != 2 {
			t.Fatalf("the day's foreman turns = %d, want the cancelled one and the resume's one", got)
		}
		if got := e.countEvents(evForemanWakeBudgetSpent); got != 0 {
			t.Errorf("budget-spent events = %d, want none", got)
		}
	})
	t.Run("the engine's pause", func(t *testing.T) {
		e := newEnv(t)
		e.park("which file?")
		e.cancelForeman()
		e.pauseEngine(PauseOriginUser, "cli")
		if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if got := e.foremanJobs(); got != 1 {
			t.Fatalf("foreman turns after the recovery = %d, want none submitted", got)
		}
	})
}

// TestResumeDrainLiftsOnlyADrainPause: `brigada start` lifts the
// engine's pause when a drain set it, with the resume evaluation of the
// held wakes, and leaves a user's pause standing.
func TestResumeDrainLiftsOnlyADrainPause(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	e.cancelForeman()
	e.pauseEngine(PauseOriginDrain, "stop --engine --drain")
	res, err := e.eng.ResumeDrain(e.ctx, "start")
	if err != nil || !res.Resumed || res.Origin != PauseOriginDrain {
		t.Fatalf("the drain lift = %+v, %v; want the drain pause lifted", res, err)
	}
	if got := e.livePauses(0); got != 0 {
		t.Fatalf("live engine pauses after the drain lift = %d, want none", got)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the drain lift = %d, want the held wake submitted", got)
	}
	if got := e.foremanWakesToday(); got != 2 {
		t.Fatalf("the day's foreman turns = %d, want the cancelled one and the one the lift woke", got)
	}
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 0 {
		t.Errorf("budget-spent events = %d, want none", got)
	}

	e2 := newEnv(t)
	e2.park("which file?")
	e2.cancelForeman()
	e2.pauseEngine(PauseOriginUser, "cli")
	if res, err := e2.eng.ResumeDrain(e2.ctx, "start"); err != nil || res.Resumed {
		t.Fatalf("the drain lift over a user's pause = %+v, %v; want the pause left alone", res, err)
	}
	if got := e2.livePauses(0); got != 1 {
		t.Fatalf("live engine pauses after the drain lift = %d, want the user's standing", got)
	}
	if got := e2.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns after the drain lift = %d, want none submitted", got)
	}
	if res := e2.resumeEngine("cli"); !res.Resumed || res.Origin != PauseOriginUser {
		t.Fatalf("the engine's resume = %+v, want the user's pause lifted", res)
	}
	if got := e2.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the resume = %d, want the held wake submitted", got)
	}
	if got := e2.foremanWakesToday(); got != 2 {
		t.Fatalf("the day's foreman turns = %d, want the cancelled one and the resume's one", got)
	}
	if got := e2.countEvents(evForemanWakeBudgetSpent); got != 0 {
		t.Errorf("budget-spent events = %d, want none", got)
	}
}

// queuedAt reads a job's queue-TTL clock.
func (e *env) queuedAt(job int64) string {
	return e.str("SELECT queued_at FROM jobs WHERE id = ?", job)
}

// TestPauseHeldTimeCountsOverlappingPausesOnce: an engine pause and a
// project pause that overlap count once (§8.3 "Clocks"), and a resume
// under a remaining scope shifts nothing yet — the engine's lift leaves
// the project's own pause holding the job, so the clock moves only at
// the project's lift, by the whole union. The wall clock ran past the
// queue TTL meanwhile, and the paused job never timed out.
func TestPauseHeldTimeCountsOverlappingPausesOnce(t *testing.T) {
	e := newEnv(t) // the 10m queue TTL
	t0 := e.clock.Now()
	job := e.submit(e.work()).JobID
	queued := e.queuedAt(job)
	e.elapse(2 * time.Minute)
	e.pauseEngine(PauseOriginUser, "cli") // t0+2m
	e.elapse(3 * time.Minute)
	e.pauseProject(PauseOriginUser, "cli") // t0+5m, inside the engine's
	e.elapse(3 * time.Minute)
	e.resumeEngine("cli") // t0+8m: the project's own pause still holds the job
	if got := e.queuedAt(job); got != queued {
		t.Fatalf("queued_at after the engine's lift = %s, want %s unshifted under the project's pause", got, queued)
	}
	e.elapse(3 * time.Minute)
	e.resumeProject("cli") // t0+11m: the union [t0+2m, t0+11m] is 9m
	if got, want := e.queuedAt(job), fmtTime(t0.Add(9*time.Minute)); got != want {
		t.Fatalf("queued_at after the last lift = %s, want %s: the overlapping pauses counted once", got, want)
	}
	if got := e.str("SELECT status FROM jobs WHERE id = ?", job); got != statusQueued {
		t.Fatalf("job status = %s, want queued: 11m of wall clock, 2m of it unpaused", got)
	}
}

// TestPauseHeldTimeOfAJobQueuedMidPause: each job counts only its own
// wait (§8.3 "Clocks") — a job queued before the pause is held from the
// pause's start, one queued mid-pause from its own queuing.
func TestPauseHeldTimeOfAJobQueuedMidPause(t *testing.T) {
	e := newEnv(t)
	t0 := e.clock.Now()
	before := e.submit(e.work()).JobID
	e.elapse(time.Minute)
	e.pauseProject(PauseOriginUser, "cli") // t0+1m
	e.elapse(4 * time.Minute)
	other := e.addTask("{}")
	mid := e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", other), ProjectID: e.project, TaskID: other,
		Kind: KindWork, Mode: ModeFresh, Prompt: "the other work"}).JobID // t0+5m
	e.elapse(3 * time.Minute)
	e.resumeProject("cli") // t0+8m
	if got, want := e.queuedAt(before), fmtTime(t0.Add(7*time.Minute)); got != want {
		t.Errorf("queued_at of the job queued before the pause = %s, want %s (7m held)", got, want)
	}
	if got, want := e.queuedAt(mid), fmtTime(t0.Add(8*time.Minute)); got != want {
		t.Errorf("queued_at of the job queued mid-pause = %s, want %s (its own 3m)", got, want)
	}
}

// TestPauseLongerThanTheQueueTTLFailsNothing: while the pause lasts the
// queue TTL never fails a paused job, however long; after the resume
// the TTL is shifted by the held time, so the job fails only once its
// unpaused wait passes the TTL.
func TestPauseLongerThanTheQueueTTLFailsNothing(t *testing.T) {
	e := newEnv(t) // the 10m queue TTL
	job := e.submit(e.work()).JobID
	e.elapse(5 * time.Minute)
	e.pauseProject(PauseOriginUser, "cli")
	e.elapse(30 * time.Minute) // three TTLs
	if got := e.str("SELECT status FROM jobs WHERE id = ?", job); got != statusQueued {
		t.Fatalf("job status under the pause = %s, want queued", got)
	}
	if got := e.countEvents(evJobQueueTimedOut); got != 0 {
		t.Fatalf("queue-TTL events under the pause = %d, want none", got)
	}
	e.resumeProject("cli") // 5m of unpaused wait so far
	e.elapse(4 * time.Minute)
	if got := e.str("SELECT status FROM jobs WHERE id = ?", job); got != statusQueued {
		t.Fatalf("job status 9m unpaused = %s, want queued: the TTL was shifted by the pause", got)
	}
	e.elapse(2 * time.Minute)
	if got := e.str("SELECT status FROM jobs WHERE id = ?", job); got != statusFailed {
		t.Fatalf("job status 11m unpaused = %s, want failed on the shifted TTL", got)
	}
	if got := e.countEvents(evJobQueueTimedOut); got != 1 {
		t.Errorf("queue-TTL events = %d, want the one past the shifted TTL", got)
	}
}

// TestPauseShiftsARetrysNotBefore: a retry's not-before is shifted by
// the held time at the lift, so the backoff that remained at the pause
// remains after it.
func TestPauseShiftsARetrysNotBefore(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	t0 := e.clock.Now()
	infraFail(e, author) // the retry waits for t0+1m
	retry := e.int("SELECT id FROM jobs WHERE key = ? AND status = 'queued'", e.work().Key)
	e.elapse(30 * time.Second)
	e.pauseProject(PauseOriginUser, "cli")
	e.elapse(10 * time.Minute)
	if g := e.leaseAny(author); g != nil {
		t.Fatalf("the paused retry leased: %+v", g)
	}
	e.resumeProject("cli") // 10m held
	if got, want := e.str("SELECT not_before FROM jobs WHERE id = ?", retry), fmtTime(t0.Add(infraDelayFor(1)+10*time.Minute)); got != want {
		t.Fatalf("not_before after the lift = %s, want %s", got, want)
	}
	if g := e.leaseAny(author); g != nil {
		t.Fatalf("the retry leased before its shifted not-before: %+v", g)
	}
	e.elapse(30 * time.Second)
	if g := e.leaseAny(author); g == nil || g.JobID != retry {
		t.Fatalf("the poll after the shifted not-before = %+v, want the retry %d", g, retry)
	}
}

// TestPauseShiftsTheInfraWindowOfATaskRetry: a task job's infra streak
// that spans a pause longer than the retry window (§5.1) is not spent on
// account of the pause — without the shift the failure after the resume
// would be past the window and park the task.
func TestPauseShiftsTheInfraWindowOfATaskRetry(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.SweepInterval = 5 * time.Minute })
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	key := e.work().Key
	infraFail(e, author) // the streak begins
	e.pauseProject(PauseOriginUser, "cli")
	e.elapse(infraRetryWindow + 10*time.Minute)
	e.resumeProject("cli")
	e.waitOutInfraRetry(key)
	infraFail(e, author) // past the window by the wall clock, not by the unpaused time
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Fatalf("task state = %s, want running behind another retry", state)
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none: the pause spent no window", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", key); n != 1 {
		t.Errorf("queued retries = %d, want 1", n)
	}
}

// TestPauseShiftsTheInfraWindowOfAForemanWake: a foreman turn that
// failed on the infrastructure as the pause began has its re-wake held
// by the pause; at the resume the wake's streak is measured without the
// paused time, so the wake answers instead of naming a spent outage.
func TestPauseShiftsTheInfraWindowOfAForemanWake(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.SweepInterval = 5 * time.Minute })
	e.park("which file?")
	g := e.leaseForemanRaw()
	if g == nil {
		t.Fatal("no foreman turn to lease")
	}
	e.pauseProject(PauseOriginUser, "cli")
	if ans := e.complete(g, Completion{Error: "prepare the foreman checkout: could not resolve host", ErrorKind: ErrorKindInfra}); ans != AnsOK {
		t.Fatalf("complete the infra turn: %s", ans)
	}
	if got := e.liveForeman(); got != 0 {
		t.Fatalf("live foreman turns under the pause = %d, want the re-wake held", got)
	}
	e.elapse(infraRetryWindow + 10*time.Minute)
	e.resumeProject("cli")
	if n := e.countEvents(evForemanOutageSpent); n != 0 {
		t.Errorf("foreman_outage_spent events = %d, want none: the pause spent no window", n)
	}
	if got := e.liveForeman(); got != 1 {
		t.Fatalf("live foreman turns after the resume = %d, want the re-wake", got)
	}
}

// TestPauseShiftsTheInfraWindowOfAPlanReview: the plan review's infra
// retry (§5.1, §6.2) shares the window check, so a streak that spans a
// pause is not held for the foreman on account of the paused time.
func TestPauseShiftsTheInfraWindowOfAPlanReview(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.PlanReview = nil; c.SweepInterval = 5 * time.Minute })
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	key := planReviewKey(res.PlanID)
	leaseRound := func() *LeaseGrant {
		t.Helper()
		g := e.lease(e.runner2, nil, nil)
		if g == nil || g.Kind != KindPlanReview {
			t.Fatalf("plan review lease = %+v", g)
		}
		return g
	}
	g := leaseRound()
	e.pauseProject(PauseOriginUser, "cli")
	if ans := e.complete(g, Completion{Error: "Cannot connect to API", ErrorKind: ErrorKindInfra}); ans != AnsOK {
		t.Fatalf("complete the infra failure: %s", ans)
	}
	e.elapse(infraRetryWindow + 10*time.Minute)
	e.resumeProject("cli")
	e.waitOutInfraRetry(key)
	if ans := e.complete(leaseRound(), Completion{Error: "Cannot connect to API", ErrorKind: ErrorKindInfra}); ans != AnsOK {
		t.Fatalf("complete the second infra failure: %s", ans)
	}
	if got := e.str("SELECT COALESCE(review_verdict, '') FROM plans WHERE id = ?", res.PlanID); got == planReviewNoRun {
		t.Fatalf("recorded verdict = %q, want no hold: the pause spent no window", got)
	}
	if got := e.countEvents(evPlanReviewed); got != 0 {
		t.Errorf("plan_reviewed events = %d, want none", got)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", key); n != 1 {
		t.Errorf("queued plan review retries = %d, want 1", n)
	}
}

// TestRunningJobWallTimeCapFiresDuringAPause: running jobs are not
// touched by the pause, and neither is their wall-time cap (§8.3).
func TestRunningJobWallTimeCapFiresDuringAPause(t *testing.T) {
	e := newEnv(t)
	job := e.submit(e.work()).JobID
	if g := e.leaseAny(e.runner1); g == nil {
		t.Fatal("the work job was not leased")
	}
	e.pauseProject(PauseOriginUser, "cli")
	limit := e.eng.cfg.HardCaps[KindWork]
	// The lease outlives the cap, so the cap and not its expiry ends it.
	e.exec("UPDATE jobs SET lease_expires_at = ? WHERE id = ?", fmtTime(e.clock.Now().Add(limit+time.Hour)), job)
	e.elapse(limit + time.Minute)
	if got := e.str("SELECT status FROM jobs WHERE id = ?", job); got != statusFailed {
		t.Fatalf("job status past its cap under the pause = %s, want failed", got)
	}
	if got := e.countEvents(evJobHardCapped); got != 1 {
		t.Errorf("hard-cap events = %d, want 1", got)
	}
}

// TestStatusListsThePauseWithItsDrainingJobs: the status and the
// capacity view carry each live pause with its scope, start and origin
// and the scope's jobs still leased or running; the paused scope's
// queued jobs count under the pause and leave the held: list and the
// layers' queue depths; once nothing runs, the scope is drained.
func TestStatusListsThePauseWithItsDrainingJobs(t *testing.T) {
	e := newEnv(t)
	running := e.submit(e.work()).JobID
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != running {
		t.Fatalf("the poll = %+v, want job %d", g, running)
	}
	other := e.addTask("{}")
	queued := e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", other), ProjectID: e.project, TaskID: other,
		Kind: KindWork, Mode: ModeFresh, Prompt: "the other work"}).JobID
	// The stamp a full runner layer leaves on the job it holds (§8.1).
	e.exec("UPDATE jobs SET cap_held_layer = 'runner', cap_held_key = ?, cap_held_at = ? WHERE id = ?",
		fmt.Sprint(e.runner1), fmtTime(e.clock.Now()), queued)
	heldIDs := func(hs []HeldJob) []int64 {
		var ids []int64
		for _, h := range hs {
			ids = append(ids, h.ID)
		}
		return ids
	}
	runnerQueue := func(snap CapacitySnapshot) int {
		for _, l := range snap.Layers {
			if l.Layer == "runner" && l.RunnerID == e.runner1 {
				return l.Queue
			}
		}
		return -1
	}
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pauses) != 0 || len(st.Held) != 1 || st.Held[0].ID != queued {
		t.Fatalf("status before the pause: pauses %+v, held %v; want no pause and the held job %d", st.Pauses, heldIDs(st.Held), queued)
	}
	snap, err := e.eng.Capacity(e.ctx, 0)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if got := runnerQueue(snap); got != 1 {
		t.Fatalf("runner queue before the pause = %d, want the held job", got)
	}

	started := fmtTime(e.clock.Now())
	e.pauseProject(PauseOriginUser, "cli")
	if st, err = e.eng.Status(e.ctx, 0); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pauses) != 1 {
		t.Fatalf("pauses = %+v, want the project's", st.Pauses)
	}
	p := st.Pauses[0]
	if p.ProjectID != e.project || p.RepoURL != "https://example.invalid/brigada.git" || p.Origin != PauseOriginUser ||
		p.StartedAt != started || p.Drained || p.Queued != 1 {
		t.Errorf("the pause = %+v, want the project's user pause from %s, draining, one queued", p, started)
	}
	if len(p.Draining) != 1 || p.Draining[0].ID != running || p.Draining[0].TaskID != e.task ||
		p.Draining[0].Kind != string(KindWork) || (p.Draining[0].Status != statusLeased && p.Draining[0].Status != statusRunning) {
		t.Errorf("draining = %+v, want the leased job %d", p.Draining, running)
	}
	if len(st.Held) != 0 {
		t.Errorf("held = %v, want the paused job counted under the pause, not as a wait for a slot", heldIDs(st.Held))
	}
	if snap, err = e.eng.Capacity(e.ctx, 0); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if got := runnerQueue(snap); got != 0 {
		t.Errorf("runner queue under the pause = %d, want the paused job left out", got)
	}
	if len(snap.Pauses) != 1 || snap.Pauses[0].ProjectID != e.project || len(snap.Pauses[0].Draining) != 1 {
		t.Errorf("capacity pauses = %+v, want the project's with its draining job", snap.Pauses)
	}

	// The engine's pause lists first; a foreman's view scoped to its
	// project carries both.
	e.pauseEngine(PauseOriginDrain, "stop --engine --drain")
	if st, err = e.eng.Status(e.ctx, e.project); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pauses) != 2 || st.Pauses[0].ProjectID != 0 || st.Pauses[0].Origin != PauseOriginDrain ||
		st.Pauses[0].RepoURL != "" || len(st.Pauses[0].Draining) != 1 || st.Pauses[1].ProjectID != e.project {
		t.Fatalf("pauses = %+v, want the engine's drain then the project's", st.Pauses)
	}

	// The running job ends: the scope is drained.
	if ans := e.complete(g, doneOn(branchOf(e.task), passCheck)); ans != AnsOK {
		t.Fatalf("complete the running job: %s", ans)
	}
	if st, err = e.eng.Status(e.ctx, 0); err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, p := range st.Pauses {
		if !p.Drained || len(p.Draining) != 0 {
			t.Errorf("pause of scope %d after the job ended = %+v, want drained", p.ProjectID, p)
		}
	}
}

// TestPauseSurvivesAnEngineRestart: the pause is stored, not
// remembered (§8.3 "Storage and stopping") — a new engine over the same
// store keeps the scope paused, lists the pause, and its resume shifts
// the held time across the restart.
func TestPauseSurvivesAnEngineRestart(t *testing.T) {
	e := newEnv(t)
	t0 := e.clock.Now()
	job := e.submit(e.work()).JobID
	e.elapse(time.Minute)
	e.pauseProject(PauseOriginUser, "cli") // t0+1m
	e.elapse(5 * time.Minute)
	e.eng = New(e.st, e.eng.cfg) // the engine restarts over the same store
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("the restarted engine leased a paused job: %+v", g)
	}
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pauses) != 1 || st.Pauses[0].ProjectID != e.project || st.Pauses[0].Queued != 1 {
		t.Fatalf("pauses after the restart = %+v, want the project's holding its job", st.Pauses)
	}
	e.elapse(4 * time.Minute)
	if res := e.resumeProject("cli"); !res.Resumed {
		t.Fatalf("the resume after the restart = %+v, want the stored pause lifted", res)
	}
	if got, want := e.queuedAt(job), fmtTime(t0.Add(9*time.Minute)); got != want {
		t.Errorf("queued_at after the resume = %s, want %s (9m held across the restart)", got, want)
	}
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job {
		t.Fatalf("the poll after the resume = %+v, want job %d", g, job)
	}
}

// TestHeldWithinUnionsTheSpans pins the held-time arithmetic: the
// union of the spans within the window, a span cut to the window and
// overlapping or touching spans counted once.
func TestHeldWithinUnionsTheSpans(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	spans := []pauseSpan{
		{start: at(10), end: at(20)},
		{start: at(15), end: at(30)}, // overlaps the first
		{start: at(30), end: at(35)}, // touches the second
		{start: at(50), end: at(60)},
	}
	cases := []struct {
		from, to int
		want     time.Duration
	}{
		{0, 100, 35 * time.Minute},
		{25, 100, 20 * time.Minute}, // queued mid-pause: from 25 only
		{0, 12, 2 * time.Minute},
		{36, 49, 0},
	}
	for _, c := range cases {
		if got := heldWithin(spans, at(c.from), at(c.to)); got != c.want {
			t.Errorf("heldWithin([%d, %d]) = %s, want %s", c.from, c.to, got, c.want)
		}
	}
}
