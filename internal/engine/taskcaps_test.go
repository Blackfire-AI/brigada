package engine

import (
	"fmt"
	"strings"
	"testing"
)

func TestCreatePlanWithCaps(t *testing.T) {
	e := newEnv(t)
	res, err := e.eng.CreatePlan(e.ctx, e.project, "capped plan", []PlanTask{
		{Name: "tight", Spec: "a", Caps: map[string]int{"restart": 4, "fix_rounds": 0}},
		{Name: "plain", Spec: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.str("SELECT caps FROM tasks WHERE id = ?", res.Tasks[0].TaskID); got != `{"fix_rounds":0,"restart":4}` {
		t.Errorf("caps = %s", got)
	}
	if got := e.str("SELECT caps FROM tasks WHERE id = ?", res.Tasks[1].TaskID); got != "{}" {
		t.Errorf("task without caps recorded %s; want {}", got)
	}
}

func TestCreatePlanRefusesBadCaps(t *testing.T) {
	for name, tc := range map[string]struct {
		caps  map[string]int
		words string
	}{
		"unknown key": {map[string]int{"restarts": 2}, `unknown key "restarts"`},
		"too high":    {map[string]int{"restart": 11}, "restart is 11"},
		"negative":    {map[string]int{"review_rounds": -1}, "review_rounds is -1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			_, err := e.eng.CreatePlan(e.ctx, e.project, "bad", []PlanTask{{Spec: "valid"}, {Name: "bad", Spec: "x", Caps: tc.caps}})
			if err == nil || !strings.Contains(err.Error(), tc.words) || !strings.Contains(err.Error(), "bad") {
				t.Fatalf("refusal = %v; want it to name %q", err, tc.words)
			}
			if e.int("SELECT COUNT(*) FROM tasks") != 1 || e.int("SELECT COUNT(*) FROM plans") != 1 {
				t.Fatal("refused plan was partially inserted")
			}
		})
	}
}

func TestAmendAddWithCaps(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "extra", Spec: "more", Caps: map[string]int{"review_rounds": 5}}}})
	if got := e.str("SELECT caps FROM tasks WHERE id = ?", am.Added[0].TaskID); got != `{"review_rounds":5}` {
		t.Errorf("added task caps = %s", got)
	}

	before := e.int("SELECT COUNT(*) FROM tasks")
	_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{{Spec: "ok"}, {Name: "bad", Spec: "x", Caps: map[string]int{"restart": 99}}}})
	if err == nil || !strings.Contains(err.Error(), "restart is 99") {
		t.Fatalf("refusal = %v", err)
	}
	if e.int("SELECT COUNT(*) FROM tasks") != before {
		t.Error("refused amendment partially applied")
	}
}

func TestAmendRetierCaps(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build := res.Tasks[0].TaskID
	e.exec("UPDATE tasks SET state = 'needs_foreman', caps = '{\"restart\":3,\"fix_rounds\":1}' WHERE id = ?", build)

	e.amend(res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "build", Caps: map[string]int{"restart": 5}}}})
	if got := e.str("SELECT caps FROM tasks WHERE id = ?", build); got != `{"fix_rounds":1,"restart":5}` {
		t.Errorf("caps after merge = %s; want restart replaced and fix_rounds kept", got)
	}

	for _, bad := range []map[string]int{{"nope": 1}, {"restart": 11}} {
		_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Retier: []PlanRetier{{Task: "build", Caps: bad}}})
		if err == nil || !strings.Contains(err.Error(), "caps") {
			t.Errorf("retier with caps %v: err = %v; want a caps refusal", bad, err)
		}
	}
	if got := e.str("SELECT caps FROM tasks WHERE id = ?", build); got != `{"fix_rounds":1,"restart":5}` {
		t.Errorf("refused retier changed caps to %s", got)
	}
}

func TestRoundCapReachedHonoursTaskRestartCap(t *testing.T) {
	e := newEnv(t)
	task := e.addTask(`{"restart": 3}`)
	e.exec("UPDATE tasks SET class = 'answer' WHERE id = ?", task)
	p := SubmitParams{
		Key:       fmt.Sprintf("task:%d", task),
		ProjectID: e.project,
		TaskID:    task,
		Kind:      KindWork,
		Mode:      ModeFresh,
		Restart:   true,
		Prompt:    "again",
	}
	// The engine default is 2; the task's own cap of 3 lets a third in
	// and refuses the fourth.
	for i := 1; i <= 3; i++ {
		if r := e.submit(p); r.Answer != AnsCreated {
			t.Fatalf("restart %d = %s; want created under a restart cap of 3", i, r.Answer)
		}
	}
	if r := e.submit(p); r.Answer != AnsCapped {
		t.Errorf("restart 4 = %s; want capped", r.Answer)
	}
}

func TestTaskCapsLabel(t *testing.T) {
	for want, caps := range map[string]map[string]int{
		"":                             {},
		"restarts 4":                   {"restart": 4},
		"fix 0, review 3, restarts 10": {"restart": 10, "review_rounds": 3, "fix_rounds": 0},
	} {
		if got := TaskCapsLabel(caps); got != want {
			t.Errorf("TaskCapsLabel(%v) = %q; want %q", caps, got, want)
		}
	}
}
