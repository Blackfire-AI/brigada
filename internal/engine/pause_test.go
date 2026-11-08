package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
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
}

// TestResumeRunsTheQueuedTurnThenOneCoalescedTurn: at the resume, a
// foreman turn queued before the pause runs with the events it was
// given then; the judgment events that piled up behind the pause
// coalesce into one turn after it, so the freed project is charged one
// wake — not one per event and not a second one for the queued turn.
func TestResumeRunsTheQueuedTurnThenOneCoalescedTurn(t *testing.T) {
	e := newEnv(t)
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
}
