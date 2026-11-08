package engine

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// taskItem reads the fixture task's entry in the engine's whole
// needs-you list; ok is false when the list has none.
func (e *env) taskItem() (NeedsYouItem, bool) {
	e.t.Helper()
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	for _, it := range info.NeedsYou {
		if it.Kind == "task" && it.ID == e.task {
			return it, true
		}
	}
	return NeedsYouItem{}, false
}

// rewake evaluates one more wake of the project, as a later park or a
// turn's end does.
func (e *env) rewake() {
	e.t.Helper()
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		return e.eng.wakeForemanTx(tx, e.project, e.clock.Now())
	}); err != nil {
		e.t.Fatalf("wake the foreman: %v", err)
	}
}

// TestForemanHoldsRule is the rule of §10: a task parked at needs_foreman
// is the foreman's while a live turn or an attached chat will handle it,
// and the user's — listed, and announced once by task_unattended — when
// nothing will.
func TestForemanHoldsRule(t *testing.T) {
	cases := []struct {
		name       string
		held       bool
		unattended int64
		run        func(e *env)
	}{
		{"a live turn holds", true, 0, func(e *env) {
			e.park("which file?")
		}},
		{"a done turn that left the task parked does not", false, 1, func(e *env) {
			e.park("which file?")
			e.complete(e.leaseForeman(), Completion{Result: `{"status":"done"}`})
			// A later wake neither holds it nor announces it again.
			e.rewake()
		}},
		{"no foreman harness", false, 1, func(e *env) {
			e.exec(`UPDATE projects SET foreman_harness = '', foreman_model = '' WHERE id = ?`, e.project)
			e.park("which file?")
		}},
		{"a limited foreman", false, 1, func(e *env) {
			e.limitForeman(e.clock.Now().Add(2 * time.Hour).Format(time.RFC3339))
		}},
		{"a limit with a live turn still does not hold", false, 1, func(e *env) {
			e.park("which file?")
			e.exec(`UPDATE projects SET foreman_limited = 'unknown' WHERE id = ?`, e.project)
			e.rewake()
		}},
		{"a spent budget", false, 1, func(e *env) {
			e.park("which file?")
			e.complete(e.leaseForeman(), Completion{Error: "boom"})
		}},
		{"a stalled wake", false, 1, func(e *env) {
			e.park("which file?")
			for i := 0; i < 3; i++ {
				e.complete(e.leaseForeman(), Completion{Error: fmt.Sprintf("boom %d", i)})
				if i < 2 {
					e.clock.Advance(retryDelayFor(i + 1))
				}
			}
		}},
		{"a cancelled turn", false, 1, func(e *env) {
			e.park("which file?")
			job := e.int(`SELECT id FROM jobs WHERE kind = 'foreman'`)
			if ans := e.cancel(job); ans != AnsOK {
				e.t.Fatalf("cancel the turn: %s", ans)
			}
		}},
		{"a live attach holds", true, 0, func(e *env) {
			e.attach(false)
			e.park("which file?")
		}},
		{"a live attach holds a row its events tool consumed", true, 0, func(e *env) {
			e.attach(false)
			e.park("which file?")
			if _, err := e.eng.ForemanEvents(e.ctx, e.project); err != nil {
				e.t.Fatalf("read the events: %v", err)
			}
		}},
		{"a park with no inbox row", false, 0, func(e *env) {
			e.exec(`UPDATE tasks SET state = ? WHERE id = ?`, taskNeedsForeman, e.task)
			e.addTaskEvent(e.task, e.project, evTaskPlanHeld, `{"status":"needs_foreman","reason":"over the budget"}`)
			e.rewake()
		}},
		{"needs_user", false, 0, func(e *env) {
			e.exec(`UPDATE tasks SET state = ? WHERE id = ?`, taskNeedsUser, e.task)
			e.addTaskEvent(e.task, e.project, evTaskNeedsHelp, `{"status":"needs_user","question":"which file?"}`)
			e.rewake()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The budget case runs on a day's budget of one turn.
			e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 1 })
			if tc.name != "a spent budget" {
				e = newEnv(t)
			}
			tc.run(e)
			it, ok := e.taskItem()
			if !ok {
				t.Fatalf("the engine's whole list lacks task %d", e.task)
			}
			if it.ForemanHolds != tc.held {
				t.Errorf("ForemanHolds = %v, want %v", it.ForemanHolds, tc.held)
			}
			if got := e.countEvents(evTaskUnattended); got != tc.unattended {
				t.Errorf("task_unattended events = %d, want %d", got, tc.unattended)
			}
			if tc.unattended == 1 {
				p, ok := e.eventPayloadOf(evTaskUnattended)
				if !ok || p.ParkKind != evTaskNeedsHelp || p.Reason != "which file?" {
					t.Errorf("task_unattended payload = %+v, want the park's kind and words", p)
				}
				if n := e.int(`SELECT COUNT(*) FROM foreman_inbox i JOIN events ev ON ev.id = i.event_id WHERE ev.kind = ?`, evTaskUnattended); n != 0 {
					t.Errorf("task_unattended rows in the inbox = %d, want none", n)
				}
			}
		})
	}
}

// TestSplitForemanHoldsAndProjectCount: the split moves a held task out
// of needs_you and into foreman_holds, and the project list counts only
// what is not held.
func TestSplitForemanHoldsAndProjectCount(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	info.SplitForemanHolds()
	if len(info.NeedsYou) != 0 || len(info.ForemanHolds) != 1 || info.ForemanHolds[0].ID != e.task {
		t.Fatalf("needs_you = %+v, foreman_holds = %+v; want the held task apart", info.NeedsYou, info.ForemanHolds)
	}
	projects, err := e.eng.Projects(e.ctx)
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if len(projects) != 1 || projects[0].NeedsYou != 0 {
		t.Errorf("projects = %+v, want needs_you 0 for a held task", projects)
	}
	// Once the foreman will not handle it, both count it.
	e.complete(e.leaseForeman(), Completion{Result: `{"status":"done"}`})
	if projects, err = e.eng.Projects(e.ctx); err != nil || projects[0].NeedsYou != 1 {
		t.Errorf("projects = %+v, %v; want needs_you 1 for an unheld task", projects, err)
	}
}

func TestParkedPlanHeldUntilTurnCancelled(t *testing.T) {
	e := newPlanReviewEnv(t)
	res, _ := parkForChanges(t, e, nTasks(2))
	entry := needsYouPlan(t, e, res.PlanID)
	if entry == nil || !entry.ForemanHolds {
		t.Fatalf("whole queue entry = %+v, want held plan", entry)
	}
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	st.SplitForemanHolds()
	if len(st.NeedsYou) != 0 || len(st.ForemanHolds) != 1 {
		t.Fatalf("split = %+v / %+v", st.NeedsYou, st.ForemanHolds)
	}
	projects, err := e.eng.Projects(e.ctx)
	if err != nil || len(projects) != 1 || projects[0].NeedsYou != 0 {
		t.Fatalf("projects = %+v, %v", projects, err)
	}
	g := e.leaseForeman()
	if ans := e.cancel(g.JobID); ans != AnsOK {
		t.Fatal(ans)
	}
	entry = needsYouPlan(t, e, res.PlanID)
	if entry == nil || entry.ForemanHolds {
		t.Fatalf("after cancel = %+v", entry)
	}
	if got := e.countEvents("plan_unattended"); got != 1 {
		t.Fatalf("announcements = %d", got)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("turns = %d", got)
	}
}

func TestParkedPlanHoldAndAnnouncements(t *testing.T) {
	for _, tc := range []struct {
		name  string
		held  bool
		setup func(*env)
	}{
		{"attach", true, func(e *env) { e.attach(false) }},
		{"limited", false, func(e *env) { e.exec(`UPDATE projects SET foreman_limited = 'unknown' WHERE id = ?`, e.project) }},
		{"no harness", false, func(e *env) {
			e.exec(`UPDATE projects SET foreman_harness = '', foreman_model = '' WHERE id = ?`, e.project)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlanReviewEnv(t)
			e.offerForeman(e.runner1)
			e.offerReviewer()
			tc.setup(e)
			res := e.createPlan(nTasks(2))
			e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictChanges, theFindings)
			it := needsYouPlan(t, e, res.PlanID)
			if it == nil || it.ForemanHolds != tc.held {
				t.Fatalf("entry = %+v", it)
			}
			e.rewake()
			want := int64(1)
			if tc.held {
				want = 0
			}
			if got := e.countEvents(evPlanUnattended); got != want {
				t.Fatalf("announcements = %d, want %d", got, want)
			}
		})
	}
}

func TestCancelTurnWithAttachDoesNotAnnounce(t *testing.T) {
	e := newPlanReviewEnv(t)
	res, _ := parkForChanges(t, e, nTasks(2))
	g := e.leaseForeman()
	e.exec(`INSERT INTO attach_holds (project_id, state, expires_at, created_at) VALUES (?, 'live', ?, ?)`, e.project, fmtTime(e.clock.Now().Add(time.Hour)), fmtTime(e.clock.Now()))
	if ans := e.cancel(g.JobID); ans != AnsOK {
		t.Fatal(ans)
	}
	if it := needsYouPlan(t, e, res.PlanID); it == nil || !it.ForemanHolds {
		t.Fatalf("entry = %+v", it)
	}
	if got := e.countEvents(evPlanUnattended); got != 0 {
		t.Fatalf("announcements = %d", got)
	}
}

func TestCancelledDeliveredTaskAnnouncesWithoutWake(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	g := e.leaseForeman()
	if it, ok := e.taskItem(); !ok || !it.ForemanHolds {
		t.Fatalf("before cancel = %+v", it)
	}
	if ans := e.cancel(g.JobID); ans != AnsOK {
		t.Fatal(ans)
	}
	if it, ok := e.taskItem(); !ok || it.ForemanHolds {
		t.Fatalf("after cancel = %+v", it)
	}
	if got := e.countEvents(evTaskUnattended); got != 1 {
		t.Fatalf("announcements = %d", got)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("turns = %d", got)
	}
}

func TestParkedPlanSpentBudgetAnnouncesOnce(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.PlanReview = nil; c.ForemanWakeBudget = 1 })
	res, _ := parkForChanges(t, e, nTasks(2))
	e.complete(e.leaseForeman(), Completion{Error: "boom"})
	if it := needsYouPlan(t, e, res.PlanID); it == nil || it.ForemanHolds {
		t.Fatalf("entry = %+v", it)
	}
	e.rewake()
	if got := e.countEvents(evPlanUnattended); got != 1 {
		t.Fatalf("announcements = %d", got)
	}
}

func TestPendingApprovalPlanRemainsInNeedsYou(t *testing.T) {
	e := newPlanReviewEnv(t)
	res, _ := parkForChanges(t, e, nTasks(2))
	e.exec(`UPDATE plans SET state = 'pending_approval' WHERE id = ?`, res.PlanID)
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	st.SplitForemanHolds()
	found := false
	for _, it := range st.NeedsYou {
		if it.Kind == "plan" && it.ID == res.PlanID && !it.ForemanHolds {
			found = true
		}
	}
	if !found {
		t.Fatalf("queue = %+v, holds = %+v", st.NeedsYou, st.ForemanHolds)
	}
}
