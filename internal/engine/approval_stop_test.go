package engine

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// TestEveryHoldReasonHasASetting (§6.3): every hold reason the gates
// give maps to exactly one setting, and that setting is one the project
// can set.
func TestEveryHoldReasonHasASetting(t *testing.T) {
	for _, kind := range []string{
		holdGatePlan, holdGateLand, holdMaxTasks, holdBudget,
		holdClass, holdPath, holdTier, holdRestart, holdExtras, holdCheck,
	} {
		key, ok := holdSettingOf[kind]
		if !ok {
			t.Errorf("hold reason %q has no setting", kind)
			continue
		}
		if !slices.Contains(SettingKeys, key) {
			t.Errorf("hold reason %q maps to %q, which no project set takes", kind, key)
		}
	}
}

// TestStopAskingValues: each hold reason moves its own setting just far
// enough that the hold no longer trips (§6.3).
func TestStopAskingValues(t *testing.T) {
	one := 1
	s := approvalSettings{
		MaxTasks:          &one,
		Classes:           []string{"implement", "review", "answer"},
		ProtectedPaths:    []string{"go.mod", "infra/**"},
		ApprovalTiers:     []string{"light", "heavy"},
		ApprovalRestarted: true,
		ExtraRepos:        []ExtraRepo{{Name: "docs", Repo: "example.com/other/docs"}},
	}
	got := stopAskingValues(s, []holdAsk{
		{kind: holdClass, item: "deploy"},
		{kind: holdPath, item: "go.mod"},
		{kind: holdTier, item: "heavy"},
		{kind: holdRestart},
		{kind: holdMaxTasks, count: 4},
		{kind: holdBudget, toks: 2_000_000, usd: 5},
	})
	want := map[string]string{
		SettingApprovalClasses:        "implement+review+answer+deploy",
		SettingApprovalProtectedPaths: "infra/**",
		SettingApprovalTiers:          "light",
		SettingApprovalRestarted:      "false",
		SettingApprovalMaxTasks:       "4",
		SettingApprovalBudget:         "2000000+$5",
	}
	if len(got) != len(want) {
		t.Fatalf("values = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// The last tier and path go back to the default, not to empty lists.
	got = stopAskingValues(s, []holdAsk{{kind: holdTier, item: "light"}, {kind: holdTier, item: "heavy"}})
	if got[SettingApprovalTiers] != "" {
		t.Errorf("the last tier removed leaves %q, want the empty default", got[SettingApprovalTiers])
	}
	if _, ok := got[SettingApprovalClasses]; ok {
		t.Error("an ask that changes nothing wrote a setting")
	}

	// An extra repository joins the project's default list.
	got = stopAskingValues(s, []holdAsk{{kind: holdExtras, extra: &ExtraRepo{Name: "web", Repo: "example.com/other/web"}}})
	if got[SettingExtraRepos] != "docs example.com/other/docs+web example.com/other/web" {
		t.Errorf("extra_repos = %q", got[SettingExtraRepos])
	}
}

// TestApprovePlanStopAskingChangesTheRules: an approval with
// --and-stop-asking releases the hold and changes the rules that held
// the plan through the project set path, recorded in the history, so
// the same plan shape runs without asking again (§6.3).
func TestApprovePlanStopAskingChangesTheRules(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_max_tasks": 1, "approval_protected_paths": ["go.mod"], "gate_plan": "ask", "default_check": "git diff go.mod"}`)
	tasks := []PlanTask{
		{Spec: "first"},
		{Spec: "second", Class: "deploy", Check: "git diff go.mod"},
	}
	res := e.createPlan(tasks)
	e.assertAwaits(res, "gate_plan is ask")

	dec, err := e.eng.ApprovePlanVersion(e.ctx, 0, res.PlanID, 0, true)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if dec.State != "active" {
		t.Fatalf("decision state = %q", dec.State)
	}
	want := []string{
		"approval_classes=implement+review+answer+deploy",
		"approval_max_tasks=2",
		"approval_protected_paths=",
		"gate_plan=auto",
	}
	if !slices.Equal(dec.Settings, want) {
		t.Fatalf("settings changed = %v, want %v", dec.Settings, want)
	}
	settings := e.str("SELECT settings FROM projects WHERE id = ?", e.project)
	for _, w := range []string{`"approval_classes":["implement","review","answer","deploy"]`, `"approval_max_tasks":2`, `"gate_plan":"auto"`} {
		if !strings.Contains(settings, w) {
			t.Errorf("settings %s lack %s", settings, w)
		}
	}
	if strings.Contains(settings, "approval_protected_paths") {
		t.Errorf("settings %s still hold the protected path", settings)
	}
	if p, ok := e.eventPayloadOf(evProjectSettingSet); !ok || p.Reason != strings.Join(want, "; ") {
		t.Errorf("project_setting_set event = %+v (%t), want the key=value pairs", p, ok)
	}

	// The same question is not asked twice: the same shape runs.
	next := e.createPlan(tasks)
	if next.State != "active" || len(next.Reasons) != 0 {
		t.Errorf("the same plan shape again = %s %v, want active with no reasons", next.State, next.Reasons)
	}
}

// TestApprovePlanWithoutStopAskingChangesNothing: without the option the
// approval behaves as before — the rules stand and history records no
// setting change (§6.3).
func TestApprovePlanWithoutStopAskingChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"gate_plan": "ask"}`)
	res := e.createPlan(nTasks(1))
	e.assertAwaits(res, "gate_plan is ask")

	dec, err := e.eng.ApprovePlan(e.ctx, 0, res.PlanID)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(dec.Settings) != 0 {
		t.Errorf("decision changed settings: %v", dec.Settings)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evProjectSettingSet); n != 0 {
		t.Errorf("project_setting_set events = %d, want none", n)
	}
	if got := e.str("SELECT settings FROM projects WHERE id = ?", e.project); !strings.Contains(got, "gate_plan") {
		t.Errorf("settings = %s, want gate_plan still ask", got)
	}
}

// TestPlanGateNeverHoldsForLandRules (§6.3): the tier, restart and
// budget rules hold landings and mid-plan starts, never an awaiting
// plan, so no ask for them can reach the plan approve's stop-asking.
func TestPlanGateNeverHoldsForLandRules(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_tiers": ["heavy"], "approval_restarted": true, "approval_budget": "100"}`)
	res := e.createPlan([]PlanTask{{Spec: "heavy work", Class: "implement"}})
	if res.State != "active" || len(res.Reasons) != 0 {
		t.Fatalf("the plan = %s %v, want active with no reasons", res.State, res.Reasons)
	}
	var asks []holdAsk
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		_, got, err := planHoldAsksTx(tx, e.project, res.PlanID)
		asks = got
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, a := range asks {
		if a.kind == holdTier || a.kind == holdRestart || a.kind == holdBudget {
			t.Errorf("a plan hold ask of kind %s reached the plan gate", a.kind)
		}
	}
}

// TestLandApproveStopAskingChangesTheRules: a land approval with
// --and-stop-asking releases the hold, changes the rules that held the
// landing and queues the land job (§6.3).
func TestLandApproveStopAskingChangesTheRules(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.set(map[string]string{SettingGateLand: GateAsk, SettingApprovalTiers: "heavy"})
	e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", e.task)
	e.runToDone(author, reviewer)

	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d, want none while held", n)
	}
	res, err := e.eng.LandApprove(e.ctx, e.project, e.task, true)
	if err != nil {
		t.Fatalf("LandApprove: %v", err)
	}
	if !res.Released || !slices.Equal(res.Settings, []string{"approval_tiers=", "gate_land=auto"}) {
		t.Fatalf("LandApprove = %+v, want released with the rules changed", res)
	}
	if n := e.landJobs(); n != 1 {
		t.Errorf("land jobs after approve = %d, want 1", n)
	}
	settings := e.str("SELECT settings FROM projects WHERE id = ?", e.project)
	if strings.Contains(settings, "approval_tiers") {
		t.Errorf("settings %s still hold the tier", settings)
	}
	if !strings.Contains(settings, `"gate_land":"auto"`) {
		t.Errorf("settings %s lack gate_land auto", settings)
	}
	if p, ok := e.eventPayloadOf(evProjectSettingSet); !ok || !strings.Contains(p.Reason, "gate_land=auto") {
		t.Errorf("project_setting_set event = %+v (%t), want the key=value pairs", p, ok)
	}
}

// TestLandApproveStopAskingRestartedRule: a spent restart's hold maps to
// approval_restarted, which the stop-asking approval turns off (§6.3).
func TestLandApproveStopAskingRestartedRule(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.set(map[string]string{SettingApprovalRestarted: "true"})
	e.exec("INSERT INTO events (kind, project_id, task_id, payload, created_at) VALUES (?, ?, ?, '{}', '2026-01-01 00:00:00')",
		evTaskTierBumped, e.project, e.task)
	e.runToDone(author, reviewer)

	if got := e.str("SELECT reason FROM land_holds WHERE task_id = ?", e.task); !strings.Contains(got, "restarted") {
		t.Fatalf("hold reason = %q, want the restart rule", got)
	}
	res, err := e.eng.LandApprove(e.ctx, e.project, e.task, true)
	if err != nil {
		t.Fatalf("LandApprove: %v", err)
	}
	if !res.Released || !slices.Equal(res.Settings, []string{"approval_restarted=false"}) {
		t.Fatalf("LandApprove = %+v, want released with approval_restarted=false", res)
	}
	if got := e.str("SELECT settings FROM projects WHERE id = ?", e.project); !strings.Contains(got, `"approval_restarted":false`) {
		t.Errorf("settings = %s, want approval_restarted off", got)
	}
	if n := e.landJobs(); n != 1 {
		t.Errorf("land jobs after approve = %d, want 1", n)
	}
}

// TestProjectSetRecordsHistory: the project set path records its change
// in the history, as the approvals' stop-asking does (§6.3).
func TestProjectSetRecordsHistory(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePlan: GateAsk})
	if p, ok := e.eventPayloadOf(evProjectSettingSet); !ok || p.Reason != "gate_plan=ask" {
		t.Errorf("project_setting_set event = %+v (%t), want gate_plan=ask", p, ok)
	}
}
