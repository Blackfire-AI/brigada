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
		{"a cancelled turn", false, 0, func(e *env) {
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
