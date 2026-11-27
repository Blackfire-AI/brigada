package engine

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// TestToolGrantsHoldAPlan: a task that grants its worker a permission
// pattern outside approval_tool_grants is an approval trigger (§6.1,
// §6.3), the reason naming the pattern.
func TestToolGrantsHoldAPlan(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{{Spec: "x", ToolGrants: []string{"Bash(git merge:*)"}}})
	e.assertAwaits(res, "tool_grants: Bash(git merge:*)")
}

// TestApprovalToolGrantsSettingReleases: a setting that lists the granted
// pattern releases the plan, so it runs without approval (§6.3).
func TestApprovalToolGrantsSettingReleases(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalToolGrants: "Bash(git merge:*)+Bash(git checkout:*)"})
	res := e.createPlan([]PlanTask{{Spec: "x", ToolGrants: []string{"Bash(git merge:*)"}}})
	e.assertRuns(res, 1)
}

// TestToolGrantsStopAskingMovesSetting: an approval with --and-stop-asking
// on a tool_grants hold adds the task's patterns to approval_tool_grants,
// so the same grant runs without asking again (§6.3).
func TestToolGrantsStopAskingMovesSetting(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{{Spec: "x", ToolGrants: []string{"Bash(git merge:*)"}}})
	e.assertAwaits(res, "tool_grants: Bash(git merge:*)")

	dec, err := e.eng.ApprovePlanVersion(e.ctx, 0, res.PlanID, 0, true)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if dec.State != "active" {
		t.Fatalf("decision state = %q, want active", dec.State)
	}
	if !slices.Equal(dec.Settings, []string{"approval_tool_grants=Bash(git merge:*)"}) {
		t.Fatalf("settings changed = %v, want approval_tool_grants=Bash(git merge:*)", dec.Settings)
	}
	next := e.createPlan([]PlanTask{{Spec: "y", ToolGrants: []string{"Bash(git merge:*)"}}})
	if next.State != "active" || len(next.Reasons) != 0 {
		t.Errorf("the same grant again = %s %v, want active with no reasons", next.State, next.Reasons)
	}
}

// TestPlanReviewViewShowsToolGrants: the plan review's input renders each
// task's tool grants beside its tier, effort and skills (§6.1, §6.2).
func TestPlanReviewViewShowsToolGrants(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE tasks SET tool_grants = ? WHERE id = ?", `["Bash(git merge:*)","Bash(git checkout:*)"]`, e.task)
	var view planReviewPlanView
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		view, err = planReviewPlanTx(tx, e.plan)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(view.Tasks) != 1 || !slices.Equal(view.Tasks[0].ToolGrants, []string{"Bash(git merge:*)", "Bash(git checkout:*)"}) {
		t.Errorf("review view task grants = %+v", view.Tasks[0].ToolGrants)
	}
	got := composePlanReviewInput(1, view, nil, "", "")
	if !strings.Contains(got, "tool grants: Bash(git merge:*), Bash(git checkout:*)") {
		t.Errorf("review input lacks the tool grants:\n%s", got)
	}
}

// TestAmendAddToolGrantHolds: an amendment that adds a task with a tool
// grant outside approval_tool_grants holds the added task alone (§6.1,
// §6.3).
func TestAmendAddToolGrantHolds(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "a", Class: "answer"}})
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "b", Spec: "b", Class: "answer", ToolGrants: []string{"Bash(git merge:*)"}}}})
	if len(am.Held) != 1 || am.Held[0].Name != "b" || !strings.Contains(am.Held[0].Reason, "tool_grants: Bash(git merge:*)") {
		t.Errorf("held = %+v, want task b held for the grant", am.Held)
	}
}
