package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// park drives one work job on the fixture's task to a needs_help park
// (§3); the park is the judgment event the wake answers to.
func (e *env) park(question string) {
	e.t.Helper()
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		e.t.Fatal("the work job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"status":"needs_help","question":"` + question + `"}`}); ans != AnsOK {
		e.t.Fatalf("complete the work: %s", ans)
	}
}

// foremanJobs counts the project's foreman turns.
func (e *env) foremanJobs() int64 {
	return e.int("SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = 'foreman'", e.project)
}

// liveForeman reads the live foreman turn's id, 0 when none runs.
func (e *env) liveForeman() int64 {
	return e.int("SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = 'foreman' AND status IN ('queued', 'leased', 'running')", e.project)
}

// leaseForeman leases the project's live foreman turn, which waits for
// the fixture project's harness, claude.
func (e *env) leaseForeman() *LeaseGrant {
	e.t.Helper()
	g := e.lease(e.runner1, []string{"claude"}, nil)
	if g == nil {
		e.t.Fatal("the foreman turn was not leased")
	}
	return g
}

// inboxStates reads the project's inbox rows as state[:job] strings, in
// event order.
func (e *env) inboxStates() []string {
	e.t.Helper()
	var out []string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT state || ':' || COALESCE(job_id, 0) FROM foreman_inbox WHERE project_id = ? ORDER BY event_id", e.project)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatalf("read the foreman inbox: %v", err)
	}
	return out
}

// wakeLine is the wake prompt's rendering of one judgment event on the
// fixture's task (§4.2): the kind, the task, the reason verbatim.
func (e *env) wakeLine(kind, reason string) string {
	return fmt.Sprintf("%s task %d another task: %s", kind, e.task, reason)
}

// countEvents counts the events of kind for the fixture's project.
func (e *env) countEvents(kind string) int64 {
	return e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND project_id = ?", kind, e.project)
}

// TestParkWakesForemanWithTheEventVerbatim is the base flow (§4.2): a
// task parks, the event lands in the inbox, one foreman turn is queued
// whose prompt carries the event verbatim, and the row goes delivered
// to that job.
func TestParkWakesForemanWithTheEventVerbatim(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns after the park = %d, want 1", got)
	}
	job := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	if want := []string{"delivered:" + fmt.Sprint(job)}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("inbox after the wake = %v, want %v", e.inboxStates(), want)
	}
	want := "<event>\n" + e.wakeLine(evTaskNeedsHelp, "which file?") + "\n</event>"
	if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", job); !strings.Contains(prompt, want) {
		t.Errorf("wake prompt = %q, want it to carry the event verbatim:\n%s", prompt, want)
	}
	if p, ok := e.eventPayloadOf(evForemanWoken); !ok || p.Job != job || p.Count != 1 {
		t.Errorf("foreman_woken payload = %+v, want job %d counting one event", p, job)
	}
}

// TestParksWhileATurnRunsWaitForIt is the coalescing rule (§4.2): a
// park while a turn is live wakes nothing — the live turn's completion
// re-evaluates — and the next turn carries every pending event, the
// returned one and the arrival alike, verbatim.
func TestParksWhileATurnRunsWaitForIt(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	g := e.leaseForeman()
	// A second task parks while the turn runs: no second job appears.
	second := e.addTask("{}")
	e.submit(SubmitParams{
		Key: fmt.Sprintf("task:%d", second), ProjectID: e.project, TaskID: second,
		Kind: KindWork, Mode: ModeFresh, Prompt: "p",
	})
	g2 := e.lease(e.runner2, nil, nil)
	e.complete(g2, Completion{Result: `{"status":"needs_help","question":"what now?"}`})
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns while one runs = %d, want 1", got)
	}
	// The turn fails: its own event returns, and with the arrival waiting
	// the next turn carries both.
	e.complete(g, Completion{Error: "the harness is on fire"})
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the failure = %d, want 2", got)
	}
	job := e.int("SELECT id FROM jobs WHERE kind = 'foreman' AND status = 'queued'")
	for _, want := range []string{
		"<event>\n" + fmt.Sprintf("%s task %d another task: which file?", evTaskNeedsHelp, e.task) + "\n</event>",
		"<event>\n" + fmt.Sprintf("%s task %d another task: what now?", evTaskNeedsHelp, second) + "\n</event>",
	} {
		if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", job); !strings.Contains(prompt, want) {
			t.Errorf("second wake prompt = %q, want it to carry the event verbatim:\n%s", prompt, want)
		}
	}
}

// TestTurnCompletionSettlesTheInbox is the consumption rule (§4.2): a
// done turn consumes its events and wakes nothing; a failed one returns
// them to pending and wakes the next turn.
func TestTurnCompletionSettlesTheInbox(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	g := e.leaseForeman()
	e.complete(g, Completion{Result: "judged: answer the question", SessionID: "fs-1"})
	job := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	if want := []string{"consumed:" + fmt.Sprint(job)}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("inbox after a done turn = %v, want %v", e.inboxStates(), want)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after a done turn = %d, want 1: nothing is pending", got)
	}

	e.park("again?")
	g = e.leaseForeman()
	e.complete(g, Completion{Error: "boom"})
	if got := e.foremanJobs(); got != 3 {
		t.Errorf("foreman turns after a failed turn = %d, want 3: the event returned and woke one", got)
	}
	last := e.int("SELECT id FROM jobs WHERE kind = 'foreman' AND status = 'queued'")
	want := []string{"consumed:" + fmt.Sprint(job), "delivered:" + fmt.Sprint(last)}
	if got := e.inboxStates(); !equalStrings(got, want) {
		t.Errorf("inbox after a failed turn = %v, want %v: the done turn's stays consumed, the returned one redelivers", got, want)
	}
}

// TestThreeFailedTurnsStallTheWake is the loop guard (§4.2): after three
// consecutive failed turns on the same pending events, one
// foreman_wake_stalled event names the stop and no fourth turn is
// queued.
func TestThreeFailedTurnsStallTheWake(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	for i := 0; i < 3; i++ {
		g := e.leaseForeman()
		e.complete(g, Completion{Error: fmt.Sprintf("boom %d", i)})
		want := int64(i + 2) // the failure wakes the next turn…
		if i == 2 {
			want = 3 // …unless it was the third consecutive one
		}
		if got := e.foremanJobs(); got != want {
			t.Fatalf("foreman turns after failure %d = %d, want %d", i+1, got, want)
		}
	}
	if got := e.countEvents(evForemanWakeStalled); got != 1 {
		t.Errorf("foreman_wake_stalled events = %d, want 1", got)
	}
	e.mustSweep()
	if got := e.foremanJobs(); got != 3 {
		t.Errorf("foreman turns after the stall = %d, want 3: no fourth", got)
	}
	if want := []string{"pending:" + fmt.Sprint(e.int("SELECT id FROM jobs WHERE kind = 'foreman' ORDER BY id DESC LIMIT 1"))}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("inbox after the stall = %v, want the events pending", e.inboxStates())
	}
}

// TestWakeBudgetSpentStopsTheWake is the budget (§4.3): with the day's
// single wake spent, parks queue no turn, exactly one
// foreman_wake_budget_spent event says so, and a second park the same
// day adds no second event and still no turn.
func TestWakeBudgetSpentStopsTheWake(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 1 })
	e.park("which file?")
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns after the first park = %d, want 1: the budget had room", got)
	}
	// The turn fails with its event pending again: the re-evaluation has
	// budget for neither another turn nor silence — one event says so.
	e.complete(e.leaseForeman(), Completion{Error: "boom"})
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns with the budget spent = %d, want 1", got)
	}
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 1 {
		t.Errorf("foreman_wake_budget_spent events = %d, want 1", got)
	}
	e.park("again?")
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 1 {
		t.Errorf("foreman_wake_budget_spent events after a second park = %d, want still 1", got)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after the second park = %d, want still 1", got)
	}
}

// TestNoForemanHarnessLeavesEventsPending is the no-harness rule
// (§4.2): a project that never chose a foreman is never woken; the
// events wait pending and `brigada status` shows the parked tasks.
func TestNoForemanHarnessLeavesEventsPending(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE projects SET foreman_harness = '' WHERE id = ?", e.project)
	e.park("which file?")
	if got := e.foremanJobs(); got != 0 {
		t.Errorf("foreman turns without a harness = %d, want 0", got)
	}
	if want := []string{"pending:0"}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("inbox without a harness = %v, want the event pending", e.inboxStates())
	}
}

// TestPlanDoneWakesTheForeman is the report rule (§4.2): the plan's
// last task finishing appends plan_done, which lands in the inbox and
// wakes a turn like any judgment event.
func TestPlanDoneWakesTheForeman(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	review := e.runToReview(author)
	g := e.leaseAny(reviewer)
	if g == nil || g.JobID != review {
		t.Fatal("the review job was not leased by the reviewer")
	}
	e.complete(g, Completion{Result: `{"verdict":"approve","findings":"` + substantiveFindings + `"}`})
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns after plan_done = %d, want 1", got)
	}
	job := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	if want := []string{"delivered:" + fmt.Sprint(job)}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("inbox after plan_done = %v, want the plan_done row delivered", e.inboxStates())
	}
	want := "<event>\n" + e.wakeLine(evPlanDone, "the plan is done") + "\n</event>"
	if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", job); !strings.Contains(prompt, want) {
		t.Errorf("wake prompt = %q, want it to carry the plan_done event verbatim:\n%s", prompt, want)
	}
}

// TestRecoverForemanWakesOnStart is the recovery (§13): pending inbox
// rows with no live foreman turn get their wake when the engine starts,
// and a project whose turn is live is left alone.
func TestRecoverForemanWakesOnStart(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE projects SET foreman_harness = '' WHERE id = ?", e.project)
	e.park("which file?")
	e.exec("UPDATE projects SET foreman_harness = 'claude' WHERE id = ?", e.project)
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover foreman wakes: %v", err)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns after recovery = %d, want 1", got)
	}
	if got := e.liveForeman(); got != 1 {
		t.Fatalf("live foreman turns after recovery = %d, want 1", got)
	}
	// The turn is live now: a second recovery pass wakes nothing.
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover foreman wakes again: %v", err)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after a second recovery = %d, want still 1", got)
	}
}

// TestAFreshEventAfterAStallStallsAgainInItsOwnRight is the stall's
// sliding window (§4.2): the stall is per event, not per project. After a
// stall, a fresh event — attempts zero — wakes another turn; its own run
// of three failures is a new episode that stalls again with a second
// foreman_wake_stalled event, not a silent repeat of the first.
func TestAFreshEventAfterAStallStallsAgainInItsOwnRight(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	for i := 0; i < 3; i++ {
		e.complete(e.leaseForeman(), Completion{Error: fmt.Sprintf("boom %d", i)})
	}
	if got := e.countEvents(evForemanWakeStalled); got != 1 {
		t.Fatalf("foreman_wake_stalled events after three failures = %d, want 1", got)
	}
	if got := e.foremanJobs(); got != 3 {
		t.Fatalf("foreman turns after the stall = %d, want 3", got)
	}
	// A second task parks: its event is fresh, so the wake answers it —
	// the stalled event rides the same turn, its attempts still counting.
	second := e.addTask("{}")
	e.submit(SubmitParams{
		Key: fmt.Sprintf("task:%d", second), ProjectID: e.project, TaskID: second,
		Kind: KindWork, Mode: ModeFresh, Prompt: "p",
	})
	e.complete(e.lease(e.runner2, nil, nil), Completion{Result: `{"status":"needs_help","question":"and me?"}`})
	if got := e.foremanJobs(); got != 4 {
		t.Fatalf("foreman turns after the fresh park = %d, want 4", got)
	}
	// The fresh event needs its own three failures: the turns after the
	// first two wake again, and the third stalls the wake a second time.
	for i, want := range []int64{5, 6, 6} {
		e.complete(e.leaseForeman(), Completion{Error: "boom again"})
		if got := e.foremanJobs(); got != want {
			t.Fatalf("foreman turns after failure %d of the new episode = %d, want %d", i+1, got, want)
		}
	}
	if got := e.countEvents(evForemanWakeStalled); got != 2 {
		t.Errorf("foreman_wake_stalled events after the second episode = %d, want 2", got)
	}
	e.mustSweep()
	if got := e.foremanJobs(); got != 6 {
		t.Errorf("foreman turns after the second stall = %d, want 6: no seventh", got)
	}
}

// TestStalledTurnsCountTowardTheStall is §5.6 beside §4.2: a turn the
// runner stopped itself is a turn that never answered, so its returned
// events carry the attempt exactly a failed turn's.
func TestStalledTurnsCountTowardTheStall(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	for i := 0; i < 3; i++ {
		want := int64(i + 2)
		if i == 2 {
			want = 3
		}
		e.complete(e.leaseForeman(), Completion{Result: "a partial answer", Outcome: OutcomeStalled})
		if got := e.foremanJobs(); got != want {
			t.Fatalf("foreman turns after stalled turn %d = %d, want %d", i+1, got, want)
		}
	}
	if got := e.countEvents(evForemanWakeStalled); got != 1 {
		t.Errorf("foreman_wake_stalled events = %d, want 1", got)
	}
}

// TestACancelledTurnDoesNotRewakeAtOnce is the cancel rule (§4.2): a
// cancelled turn returns its events to pending without counting an
// attempt — the cancel is the user's own stop — and wakes nothing at
// once; the events wait for the next judgment event or the recovery.
func TestACancelledTurnDoesNotRewakeAtOnce(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	job := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	if ans := e.cancel(job); ans != AnsOK {
		t.Fatalf("cancel the foreman turn: %s", ans)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after the cancel = %d, want 1: an instant re-wake would defy the cancel", got)
	}
	if want := []string{"pending:" + fmt.Sprint(job)}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("inbox after the cancel = %v, want the event pending", e.inboxStates())
	}
	if got := e.int("SELECT attempts FROM foreman_inbox"); got != 0 {
		t.Errorf("attempts after the cancel = %d, want 0: a cancel is not a failure to answer", got)
	}
	// The recovery (§13) is what delivers them next.
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover foreman wakes: %v", err)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns after the recovery = %d, want 2", got)
	}
}

// TestAHeartbeatHardCapReturnsTheInbox is the heartbeat's own hard-cap
// rule (§14 beside §4.2): a foreman turn capped there — a job the sweep
// could not fail — fails with the cap named, its delivered events return
// to pending a carried attempt the poorer, and the wake re-evaluates.
// The sweep at the top of a real Heartbeat fails a capped job first (and
// settles it there), so the test drives heartbeatTx directly with an
// empty report to reach the backstop.
func TestAHeartbeatHardCapReturnsTheInbox(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	g := e.leaseForeman()
	e.clock.Advance(e.eng.cfg.HardCaps[KindForeman] + time.Minute)
	var ans Answer
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		a, _, err := e.eng.heartbeatTx(tx, g.LeaseOwner, g.JobID, g.LeaseID,
			Progress{}, SweepReport{}, e.clock.Now())
		ans = a
		return err
	}); err != nil {
		t.Fatalf("heartbeat the capped turn: %v", err)
	}
	if ans != AnsLost {
		t.Errorf("heartbeat = %s, want lost", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusFailed {
		t.Errorf("the capped turn = %s, want failed", status)
	}
	if got := e.int("SELECT attempts FROM foreman_inbox"); got != 1 {
		t.Errorf("attempts after the hard cap = %d, want 1", got)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns after the hard cap = %d, want 2: the re-evaluation woke one", got)
	}
	next := e.int("SELECT id FROM jobs WHERE kind = 'foreman' AND status = 'queued'")
	if want := []string{"delivered:" + fmt.Sprint(next)}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("inbox after the hard cap = %v, want the event delivered to the next turn", e.inboxStates())
	}
}

// TestRecoveryHealsAnOrphanDeliveredRow is the recovery's self-healing
// (§13): a delivered row whose job is no longer live — a turn that ended
// without its settle, as a crash between the two writes leaves — returns
// to pending and is woken; a row whose job is live keeps its delivery.
func TestRecoveryHealsAnOrphanDeliveredRow(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	first := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	// The orphan: the turn failed without the settle that would have
	// returned its row.
	e.exec("UPDATE jobs SET status = 'failed' WHERE id = ?", first)
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover foreman wakes: %v", err)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the healing recovery = %d, want 2", got)
	}
	second := e.int("SELECT id FROM jobs WHERE kind = 'foreman' AND status = 'queued'")
	if want := []string{"delivered:" + fmt.Sprint(second)}; !equalStrings(e.inboxStates(), want) {
		t.Fatalf("inbox after the healing recovery = %v, want the row delivered to the new turn", e.inboxStates())
	}
	// The second turn is live now, so a row delivered to it stays put:
	// a later park waits for that turn's completion, healing or not.
	e.park("again?")
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover foreman wakes again: %v", err)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns after the second recovery = %d, want 2: the live turn answers", got)
	}
	want := []string{"delivered:" + fmt.Sprint(second), "pending:0"}
	if got := e.inboxStates(); !equalStrings(got, want) {
		t.Errorf("inbox after the second recovery = %v, want %v: the live delivery kept, the arrival pending", got, want)
	}
}

// TestTheWakePromptCarriesTheEventsWhole is the prompt's verbatim rule
// (§4.2 item 2): the event's own words — the question a needs_help asked
// — reach the foreman as the log holds them, whole even past the
// needs-you queue's 160-byte line (§10); only the task title is
// flattened, so a newline in it cannot break the event's own line.
func TestTheWakePromptCarriesTheEventsWhole(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE tasks SET title = 'a title\nwith a newline' WHERE id = ?", e.task)
	long := strings.Repeat("which file should own the constant? ", 10)
	e.park(long)
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns after the park = %d, want 1", got)
	}
	job := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", job)
	want := fmt.Sprintf("<event>\n%s task %d a title with a newline: %s\n</event>", evTaskNeedsHelp, e.task, long)
	if !strings.Contains(prompt, want) {
		t.Errorf("wake prompt = %q,\nwant it to carry the question whole and the title on one line:\n%s", prompt, want)
	}
	if strings.Contains(prompt, "a title\nwith a newline") {
		t.Errorf("wake prompt carries the title's raw newline: %q", prompt)
	}
}
