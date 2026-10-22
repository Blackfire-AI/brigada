package engine

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// budgetUsage is a completion whose usage the engine records per job.
func budgetUsage(in, out int64, cost any) Completion {
	c := done()
	u := &adapter.Usage{InputTokens: &in, OutputTokens: &out}
	if cost != nil {
		d := cost.(float64)
		u.CostUSD = &d
	}
	c.Usage = u
	return c
}

// TestBudgetSettingRoundTrip: the value the CLI accepts is the shape the
// settings row stores (normalized, tokens first) and the view shows.
func TestBudgetSettingRoundTrip(t *testing.T) {
	e := newEnv(t)
	got := e.set(map[string]string{SettingApprovalBudget: "$5.50+2000000"})
	if s := settingOf(t, got, SettingApprovalBudget); s.Value != "2000000+$5.5" || s.Default || s.Source != SourceLocal {
		t.Errorf("approval_budget = %+v, want the normalized local value", s)
	}
	if raw := e.str("SELECT settings FROM projects WHERE id = ?", e.project); !strings.Contains(raw, `"approval_budget":"2000000+$5.5"`) {
		t.Errorf("stored settings %s hold the normalized budget", raw)
	}
	for _, v := range []string{"2000000", "$5", "2000000+$5", "$5+2000000"} {
		e.set(map[string]string{SettingApprovalBudget: v})
	}
	// A hand-edited row that does not parse counts as no budget.
	e.setSettings(`{"approval_budget": "cheap"}`)
	e.assertRuns(e.createPlan(nTasks(1)), 1)
}

// TestPlanBudgetHoldsDependent: the spend a completed job records is the
// plan's; passing the budget parks the dependent at the plan gate, its
// needs-you line naming the rule and the spend, and no job is queued.
func TestPlanBudgetHoldsDependent(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.setSettings(`{"approval_budget": "1000"}`)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer", Deps: []string{"a"}},
	})
	e.assertRuns(res, 1)

	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("no lease")
	}
	if ans := e.complete(g, budgetUsage(600, 500, nil)); ans != AnsOK {
		t.Fatal(ans)
	}
	if got := e.taskState(res.Tasks[0].TaskID); got != taskDone {
		t.Fatalf("a = %q, want done", got)
	}
	b := res.Tasks[1].TaskID
	if got := e.taskState(b); got != taskNeedsForeman {
		t.Fatalf("b = %q, want parked at needs_foreman over the budget", got)
	}
	if got := e.queuedWork(b); got != 0 {
		t.Errorf("b has %d queued work jobs, want none", got)
	}
	p, ok := e.eventPayloadOf(evTaskPlanHeld)
	if !ok || p.Reason != "plan spend 1100 tokens, over the approval_budget of 1000 tokens" {
		t.Fatalf("task_plan_held payload = %+v, want the rule and the spend named", p)
	}
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range st.NeedsYou {
		if it.Kind == "task" && it.ID == b {
			found = true
			if !strings.Contains(it.Reason, "over the approval_budget of 1000 tokens") {
				t.Errorf("needs-you entry = %+v, want the spend named", it)
			}
		}
	}
	if !found {
		t.Errorf("needs-you entries = %+v, want the held dependent", st.NeedsYou)
	}
}

// TestPlanBudgetDollars: a dollar budget trips only on dollars a harness
// reported; tokens alone never trip it, and dollars alone never trip a
// token budget.
func TestPlanBudgetDollars(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.setSettings(`{"approval_budget": "$5"}`)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer", Deps: []string{"a"}},
	})
	g := e.leaseAny(e.runner1)
	// Tokens reported, no price: a subscription harness counts, and never
	// trips the dollar side.
	if ans := e.complete(g, budgetUsage(900000, 900000, nil)); ans != AnsOK {
		t.Fatal(ans)
	}
	if got := e.taskState(res.Tasks[1].TaskID); got != taskReady {
		t.Fatalf("b = %q, want ready: tokens never trip a dollar budget", got)
	}

	e = newEnv(t)
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.setSettings(`{"approval_budget": "$5"}`)
	res = e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer", Deps: []string{"a"}},
	})
	g = e.leaseAny(e.runner1)
	if ans := e.complete(g, budgetUsage(10, 5, 5.5)); ans != AnsOK {
		t.Fatal(ans)
	}
	if got := e.taskState(res.Tasks[1].TaskID); got != taskNeedsForeman {
		t.Fatalf("b = %q, want parked over the dollar budget", got)
	}
	if p, ok := e.eventPayloadOf(evTaskPlanHeld); !ok || p.Reason != "plan spend $5.5, over the approval_budget of $5" {
		t.Fatalf("task_plan_held payload = %+v, want the dollar spend named", p)
	}
}

// TestPlanBudgetUnderRuns: a spend under the budget holds nothing, and
// unknown usage (an older runner) spends nothing.
func TestPlanBudgetUnderRuns(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.setSettings(`{"approval_budget": "2000+$10"}`)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer", Deps: []string{"a"}},
	})
	g := e.leaseAny(e.runner1)
	if ans := e.complete(g, budgetUsage(600, 500, 0.5)); ans != AnsOK {
		t.Fatal(ans)
	}
	if got := e.taskState(res.Tasks[1].TaskID); got != taskReady || e.queuedWork(res.Tasks[1].TaskID) == 0 {
		t.Errorf("b = %q, want ready with a job under the budget", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = 'task_plan_held'"); got != 0 {
		t.Errorf("task_plan_held events = %d, want none", got)
	}
}

// TestPlanBudgetHoldsAmendment: a plan whose spend passed its budget
// holds the tasks an amendment adds, at the plan gate like a rule-held
// addition, while the plan stays active.
func TestPlanBudgetHoldsAmendment(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.setSettings(`{"approval_budget": "1000"}`)
	res := e.createPlan([]PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer", Deps: []string{"a"}},
	})
	g := e.leaseAny(e.runner1)
	if ans := e.complete(g, budgetUsage(600, 500, nil)); ans != AnsOK {
		t.Fatal(ans)
	}
	if got := e.taskState(res.Tasks[1].TaskID); got != taskNeedsForeman {
		t.Fatalf("b = %q, want parked over the budget", got)
	}

	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: "c", Spec: "c", Class: "answer"},
	}})
	if am.State != "active" || len(am.Held) != 1 {
		t.Fatalf("amendment = %+v, want active with the added task held", am)
	}
	held := am.Held[0]
	if held.Name != "c" || held.Reason != "plan spend 1100 tokens, over the approval_budget of 1000 tokens" {
		t.Errorf("held = %+v, want c named with the rule and the spend", held)
	}
	if got := e.taskState(held.TaskID); got != taskNeedsForeman || e.queuedWork(held.TaskID) != 0 {
		t.Errorf("c = %q with %d queued jobs, want parked with none", got, e.queuedWork(held.TaskID))
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Errorf("plan state = %q, want active", got)
	}
}
