package engine

import (
	"database/sql"
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

// TestPlanActualsTx: the done report's actuals read straight off the
// jobs' records — tokens, dollars and wall time only where a job
// reported them, and the restarts and fix/review rounds the engine
// recorded itself. A setup retry is no restart, and a plain work job
// spends no round.
func TestPlanActualsTx(t *testing.T) {
	e := newEnv(t)
	seed := func(kind, key string, restart, retry int, round string, in, out, cost, wall any) {
		e.exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, restart, retry, round, input_tokens, output_tokens, cost_usd, wall_seconds, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'fresh', 'p', 'done', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.project, e.task, kind, key, restart, retry, round, in, out, cost, wall, seedTS, seedTS)
	}
	seed("work", "a", 0, 0, "", 10, 5, 0.5, 30.0)
	seed("fix", "b", 0, 0, roundFix, nil, nil, nil, nil)
	seed("fix", "c", 0, 0, roundReview, nil, nil, nil, nil)
	seed("work", "d", 1, 0, "", nil, nil, nil, nil) // a restart
	seed("work", "e", 1, 1, "", nil, nil, nil, nil) // a setup retry, no restart

	var a planActuals
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		a, err = planActualsTx(tx, e.plan)
		return err
	}); err != nil {
		t.Fatalf("planActualsTx: %v", err)
	}
	if a.tokens == nil || *a.tokens != 15 || a.usd == nil || *a.usd != 0.5 || a.wall == nil || *a.wall != 30 {
		t.Errorf("actuals = %+v, want tokens 15, $0.5, wall 30", a)
	}
	if a.restarts != 1 || a.rounds != 2 {
		t.Errorf("actuals restarts/rounds = %d/%d, want 1/2", a.restarts, a.rounds)
	}
}

// TestPlanActualText: the done report renders each known counter and
// leaves an unknown one out — never zero — while the engine's own
// restarts and rounds always show.
func TestPlanActualText(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    planActuals
		want string
	}{
		{"all known", planActuals{tokens: usageInt(168), usd: usageFloat(2.5), wall: usageFloat(90), restarts: 1, rounds: 2},
			"168 tokens and $2.5, 1m30s wall, 1 restart, 2 rounds"},
		{"no dollars", planActuals{tokens: usageInt(168), wall: usageFloat(90), restarts: 1, rounds: 2},
			"168 tokens, 1m30s wall, 1 restart, 2 rounds"},
		{"no tokens", planActuals{usd: usageFloat(2.5), wall: usageFloat(90)},
			"$2.5, 1m30s wall, 0 restarts, 0 rounds"},
		{"no wall", planActuals{tokens: usageInt(168), usd: usageFloat(2.5)},
			"168 tokens and $2.5, 0 restarts, 0 rounds"},
		{"only rounds", planActuals{}, "0 restarts, 0 rounds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planActualText(tc.a); got != tc.want {
				t.Errorf("planActualText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWallText: the done report's wall time reads as a duration, "45m"
// not "45m0s".
func TestWallText(t *testing.T) {
	for _, tc := range []struct {
		seconds float64
		want    string
	}{
		{0, "0s"},
		{30, "30s"},
		{90, "1m30s"},
		{45 * 60, "45m"},
		{60 * 60, "1h"},
		{90 * 60, "1h30m"},
		{3661, "1h1m1s"},
	} {
		if got := wallText(tc.seconds); got != tc.want {
			t.Errorf("wallText(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}
