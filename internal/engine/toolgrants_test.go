package engine

import (
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

// TestToolGrantAllowlistPinned pins the allowlist's whole set: git add
// stages a merge resolution (§6.1), and the runner alone commits.
func TestToolGrantAllowlistPinned(t *testing.T) {
	want := []string{"Bash(git merge:*)", "Bash(git checkout:*)", "Bash(git restore:*)", "Bash(git cherry-pick:*)", "Bash(git add:*)"}
	if !slices.Equal(ToolGrantAllowlist, want) {
		t.Errorf("ToolGrantAllowlist = %v, want %v", ToolGrantAllowlist, want)
	}
	if err := validateToolGrants([]string{"Bash(git add:*)"}); err != nil {
		t.Errorf("git add refused: %v", err)
	}
}

// TestToolGrantsInLeaseEvent: a worker job's lease event carries the
// task's tool grants, so history shows them (§6.1, §9).
func TestToolGrantsInLeaseEvent(t *testing.T) {
	e := newEnv(t)
	sr := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "a change", ToolGrants: []string{"Bash(git checkout:*)"}})
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != sr.JobID {
		t.Fatalf("lease = %+v, want job %d", g, sr.JobID)
	}
	payload := e.str("SELECT payload FROM events WHERE kind = ? AND job_id = ?", evJobLeased, sr.JobID)
	if !strings.Contains(payload, `"tool_grants":["Bash(git checkout:*)"]`) {
		t.Errorf("job_leased payload = %s, want the tool grants", payload)
	}
}

// TestLeaseGrantCarriesToolGrants: a task's tool grants ride its lease
// grant, so the runner can hand them to the adapter (§6.1, §12.3); a task
// without grants carries none.
func TestLeaseGrantCarriesToolGrants(t *testing.T) {
	e := newEnv(t)
	sr := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "a change", ToolGrants: []string{"Bash(git checkout:*)"}})
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != sr.JobID {
		t.Fatalf("lease = %+v, want job %d", g, sr.JobID)
	}
	if !slices.Equal(g.ToolGrants, []string{"Bash(git checkout:*)"}) {
		t.Errorf("lease grant tool_grants = %v, want the checkout grant", g.ToolGrants)
	}
	// A task with no grants leases none.
	plain := e.submitTask(TaskParams{RepoURL: envRepo, BaseRef: "main", Spec: "plain"})
	pg := e.leaseAny(e.runner1)
	if pg == nil || pg.JobID != plain.JobID {
		t.Fatalf("lease = %+v, want job %d", pg, plain.JobID)
	}
	if len(pg.ToolGrants) != 0 {
		t.Errorf("grantless lease tool_grants = %v, want none", pg.ToolGrants)
	}
}
