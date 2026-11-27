package engine

import (
	"testing"
)

// TestHistoryFiltersByPlan covers the plan filter (§9): a plan's own
// events, which carry the plan in their payload alone, and its tasks'
// events are in; another plan's are out; the filter composes with the
// task filter and pages by the cursor.
func TestHistoryFiltersByPlan(t *testing.T) {
	e := newEnv(t)
	other := e.addPlanState(e.project, "the other plan", "active", e.clock.Now())
	otherTask := e.addTaskToPlan(other, "elsewhere", "ready")
	e.addTaskEvent(otherTask, e.project, "task_note", `{}`)
	e.exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
		VALUES ('plan_created', ?, NULL, NULL, ?, ?)`, e.project, `{"plan": `+itoa(other)+`}`, seedTS)
	e.exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
		VALUES ('plan_done', ?, NULL, NULL, ?, ?)`, e.project, `{"plan": `+itoa(e.plan)+`}`, seedTS)
	e.submit(e.work())

	page, err := e.eng.History(e.ctx, HistoryQuery{PlanID: e.plan})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, it := range page.Items {
		kinds = append(kinds, it.Kind)
		if it.TaskID == otherTask || (it.Kind == "plan_created") {
			t.Errorf("the plan's timeline holds another plan's item %s", it.Kind)
		}
	}
	has := func(k string) bool {
		for _, g := range kinds {
			if g == k {
				return true
			}
		}
		return false
	}
	if !has("plan_done") || !has(evJobCreated) || !has(msgKindTaskSpec) {
		t.Errorf("the plan's timeline = %v", kinds)
	}

	// The other plan sees its own plan event and task event.
	page, err = e.eng.History(e.ctx, HistoryQuery{PlanID: other})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Errorf("the other plan's timeline = %v", timeline(page.Items))
	}

	// A task of another plan composes to nothing.
	page, err = e.eng.History(e.ctx, HistoryQuery{PlanID: e.plan, TaskID: otherTask})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Errorf("plan and a foreign task = %v", timeline(page.Items))
	}

	// The cursor pages the plan's timeline.
	first, err := e.eng.History(e.ctx, HistoryQuery{PlanID: e.plan, Limit: 1})
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first page = %v %q %v", timeline(first.Items), first.NextCursor, err)
	}
	next, err := e.eng.History(e.ctx, HistoryQuery{PlanID: e.plan, Since: first.NextCursor})
	if err != nil || len(next.Items) != len(kinds)-1 {
		t.Errorf("next page = %v %v, want %d items", timeline(next.Items), err, len(kinds)-1)
	}
}

// TestStatusForPlanScopes reads the scoped status: only the plan, its
// spend and recent items; an unknown plan is ErrNoPlan.
func TestStatusForPlanScopes(t *testing.T) {
	e := newEnv(t)
	other := e.addPlanState(e.project, "the other plan", "active", e.clock.Now())
	e.addTaskToPlan(other, "elsewhere", "ready")
	e.submit(e.work())

	st, err := e.eng.StatusForPlan(e.ctx, 0, e.plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Plans) != 1 || st.Plans[0].ID != e.plan {
		t.Fatalf("plans = %+v", st.Plans)
	}
	if st.PlanScope == nil || st.PlanScope.PlanID != e.plan || len(st.PlanScope.Recent) == 0 {
		t.Errorf("plan scope = %+v", st.PlanScope)
	}
	if len(st.Jobs) != 0 || st.Report != nil || len(st.Skills) != 0 {
		t.Errorf("global sections leaked: %+v", st)
	}
	if _, err := e.eng.StatusForPlan(e.ctx, 0, 9999); err == nil {
		t.Error("an unknown plan was accepted")
	}
}
