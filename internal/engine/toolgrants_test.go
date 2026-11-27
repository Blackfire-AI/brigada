package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestToolGrantsRoundTrip: a grant list inside the allowlist round-trips
// through plan_create, plan_amend's add and respec, and the debug submit;
// a task without grants records none, a respec without the field keeps
// them, and [] clears them.
func TestToolGrantsRoundTrip(t *testing.T) {
	e := newEnv(t)
	grants := []string{"Bash(git merge:*)", "Bash(git cherry-pick:*)"}
	grantsJSON := `["Bash(git merge:*)","Bash(git cherry-pick:*)"]`
	res := e.createPlan([]PlanTask{
		{Name: "build", Spec: "build it", Class: "answer", ToolGrants: grants},
		{Name: "ship", Spec: "ship it", Class: "answer", Deps: []string{"build"}},
	})
	build, ship := res.Tasks[0], res.Tasks[1]
	if got := e.str("SELECT tool_grants FROM tasks WHERE id = ?", build.TaskID); got != grantsJSON {
		t.Errorf("created tool_grants = %q, want %q", got, grantsJSON)
	}
	info, err := e.eng.Task(e.ctx, build.TaskID)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if !slices.Equal(info.ToolGrants, grants) {
		t.Errorf("task read tool_grants = %v, want %v", info.ToolGrants, grants)
	}
	// A task without grants is unchanged: the column stays '', the read
	// names none.
	if got := e.str("SELECT tool_grants FROM tasks WHERE id = ?", ship.TaskID); got != "" {
		t.Errorf("grantless tool_grants = %q, want ''", got)
	}
	info, err = e.eng.Task(e.ctx, ship.TaskID)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if len(info.ToolGrants) != 0 {
		t.Errorf("grantless task read tool_grants = %v, want none", info.ToolGrants)
	}

	// amend-add records the added task's grants.
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{
		{Name: "extra", Spec: "extra work", Class: "answer", Deps: []string{"build"}, ToolGrants: []string{"Bash(git restore:*)"}},
	}})
	added := am.Added[0].TaskID
	if got := e.str("SELECT tool_grants FROM tasks WHERE id = ?", added); got != `["Bash(git restore:*)"]` {
		t.Errorf("added tool_grants = %q, want the restore grant", got)
	}

	// amend-respec replaces the grants; nil keeps them; [] clears them.
	e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "ship it again", ToolGrants: grants}}})
	if got := e.str("SELECT tool_grants FROM tasks WHERE id = ?", ship.TaskID); got != grantsJSON {
		t.Errorf("respecced tool_grants = %q, want %q", got, grantsJSON)
	}
	e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "ship it once more"}}})
	if got := e.str("SELECT tool_grants FROM tasks WHERE id = ?", ship.TaskID); got != grantsJSON {
		t.Errorf("respec without tool_grants = %q, want the recorded grants kept", got)
	}
	e.amend(res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "ship it grantless", ToolGrants: []string{}}}})
	if got := e.str("SELECT tool_grants FROM tasks WHERE id = ?", ship.TaskID); got != "" {
		t.Errorf("respec with [] = %q, want the grants cleared", got)
	}

	// The debug submit records its grants.
	sr := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "a change", ToolGrants: []string{"Bash(git checkout:*)"}})
	if got := e.str("SELECT tool_grants FROM tasks WHERE id = ?", sr.TaskID); got != `["Bash(git checkout:*)"]` {
		t.Errorf("submitted tool_grants = %q, want the checkout grant", got)
	}
}

// TestToolGrantsRefused: a pattern outside the allowlist is refused at
// every entry point — plan_create, plan_amend's add and respec, and the
// debug submit — the refusal naming the pattern, and nothing written.
func TestToolGrantsRefused(t *testing.T) {
	e := newEnv(t)
	bad := []string{"Bash(git merge:*)", "Bash(git push:*)"}
	const want = "Bash(git push:*)"

	_, err := e.eng.CreatePlan(e.ctx, e.project, "bad grants", []PlanTask{{Name: "a", Spec: "s", ToolGrants: bad}})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("create err = %v, want the refusal naming %s", err, want)
	}
	if got := e.int("SELECT COUNT(*) FROM plans WHERE title = 'bad grants'"); got != 0 {
		t.Errorf("plans after the refused create = %d, want 0", got)
	}

	res := e.twoStep()
	_, err = e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "extra", Spec: "x", ToolGrants: bad}}})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("amend add err = %v, want the refusal naming %s", err, want)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks WHERE name = 'extra'"); got != 0 {
		t.Errorf("added tasks after the refusal = %d, want 0", got)
	}
	before := e.str("SELECT spec FROM tasks WHERE id = ?", res.Tasks[1].TaskID)
	_, err = e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Respec: []PlanRespec{{Task: "ship", Spec: "x", ToolGrants: bad}}})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("amend respec err = %v, want the refusal naming %s", err, want)
	}
	if got := e.str("SELECT spec FROM tasks WHERE id = ?", res.Tasks[1].TaskID); got != before {
		t.Errorf("spec after the refused respec = %q, want %q untouched", got, before)
	}

	_, err = e.eng.SubmitTask(e.ctx, TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "refused submit", ToolGrants: bad})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("submit err = %v, want the refusal naming %s", err, want)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks WHERE spec = 'refused submit'"); got != 0 {
		t.Errorf("tasks after the refused submit = %d, want 0", got)
	}
}

// TestToolGrantsApprovalTrigger (§6.3): a task with tool grants holds
// its plan for approval, one reason naming the patterns outside
// approval_tool_grants; a setting that lists them releases the rule; an
// approval with --and-stop-asking adds the plan's patterns to the
// setting, so the same plan runs without asking again; an added task's
// grant in an active plan holds that task alone.
func TestToolGrantsApprovalTrigger(t *testing.T) {
	e := newEnv(t)
	grants := []string{"Bash(git merge:*)", "Bash(git add:*)"}
	tasks := []PlanTask{{Name: "merge", Spec: "merge the branches", ToolGrants: grants}}
	res := e.createPlan(tasks)
	e.assertAwaits(res, "tool_grants Bash(git merge:*), Bash(git add:*) outside approval_tool_grants")

	dec, err := e.eng.ApprovePlanVersion(e.ctx, 0, res.PlanID, 0, true)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if want := []string{"approval_tool_grants=Bash(git merge:*)+Bash(git add:*)"}; !slices.Equal(dec.Settings, want) {
		t.Fatalf("settings changed = %v, want %v", dec.Settings, want)
	}
	if settings := e.str("SELECT settings FROM projects WHERE id = ?", e.project); !strings.Contains(settings, `"approval_tool_grants":["Bash(git merge:*)","Bash(git add:*)"]`) {
		t.Errorf("settings %s lack the granted patterns", settings)
	}
	next := e.createPlan(tasks)
	if next.State != "active" || len(next.Reasons) != 0 {
		t.Errorf("the same plan again = %s %v, want active with no reasons", next.State, next.Reasons)
	}

	// A pattern the setting does not list still asks, naming that one.
	other := e.createPlan([]PlanTask{{Spec: "restore a file", ToolGrants: []string{"Bash(git merge:*)", "Bash(git restore:*)"}}})
	if reasons := strings.Join(other.Reasons, "; "); other.State != planAwaitingApproval || !strings.Contains(reasons, "tool_grants Bash(git restore:*) outside approval_tool_grants") || strings.Contains(reasons, "git merge") {
		t.Errorf("a plan with an unlisted pattern = %s %v, want it awaiting on that pattern alone", other.State, other.Reasons)
	}

	// An added task's grant in an active plan holds that task alone.
	am := e.amend(next.PlanID, PlanAmendment{Add: []PlanTask{{Name: "pick", Spec: "pick a commit", ToolGrants: []string{"Bash(git cherry-pick:*)"}}}})
	if am.State != "active" || len(am.Held) != 1 || !strings.Contains(am.Held[0].Reason, "tool_grants Bash(git cherry-pick:*)") {
		t.Errorf("amendment = %s, held %+v; want the plan active and the added task held for its grant", am.State, am.Held)
	}

	// A setting set by hand releases the rule: every pattern listed.
	f := newEnv(t)
	f.set(map[string]string{SettingApprovalToolGrants: strings.Join(ToolGrantAllowlist, "+")})
	f.assertRuns(f.createPlan(tasks), 1)
}

// TestApprovalToolGrantsSetting: the setting takes a +-separated subset
// of the allowlist, refuses a pattern outside it by name, and shows none
// when unset.
func TestApprovalToolGrantsSetting(t *testing.T) {
	e := newEnv(t)
	got := e.set(map[string]string{SettingApprovalToolGrants: ""})
	if s := settingOf(t, got, SettingApprovalToolGrants); s.Value != "none" || !s.Default {
		t.Errorf("unset approval_tool_grants = %+v, want none by default", s)
	}
	for _, g := range ToolGrantAllowlist {
		got = e.set(map[string]string{SettingApprovalToolGrants: g})
		if s := settingOf(t, got, SettingApprovalToolGrants); s.Value != g || s.Default {
			t.Errorf("approval_tool_grants=%s reads %+v", g, s)
		}
	}
	_, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingApprovalToolGrants: "Bash(git merge:*)+Bash(git push:*)"})
	if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), `"Bash(git push:*)" is not in the tool-grant allowlist`) {
		t.Errorf("a pattern outside the allowlist: err = %v, want the refusal naming it", err)
	}
}

// TestPlanReviewShowsToolGrants: the plan review's input names each
// task's tool grants beside its tier, effort and skills (§6.2); a task
// without grants names none.
func TestPlanReviewShowsToolGrants(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{
		{Name: "merge", Spec: "merge the release branch: its conflicts need git merge", ToolGrants: []string{"Bash(git merge:*)", "Bash(git add:*)"}},
		{Name: "plain", Spec: "no grants here"},
	})
	reviewPrompt := e.str("SELECT prompt FROM jobs WHERE key = ?", planReviewKey(res.PlanID))
	if !strings.Contains(reviewPrompt, "tool grants: Bash(git merge:*), Bash(git add:*)\nspec, verbatim:\nmerge the release branch") {
		t.Errorf("the plan review input lacks the task's tool grants:\n%s", reviewPrompt)
	}
	if strings.Count(reviewPrompt, "tool grants:") != 1 {
		t.Errorf("the plan review input names grants for a task without them:\n%s", reviewPrompt)
	}
}
