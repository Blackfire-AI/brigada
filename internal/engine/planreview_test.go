package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// newPlanReviewEnv is an env with the plan review on (§6.2): the
// shared fixture turns it off, these tests turn it back to the default.
func newPlanReviewEnv(t *testing.T) *env {
	t.Helper()
	return newEnvCfg(t, func(c *Config) { c.PlanReview = nil })
}

// offerReviewer seeds, on runner 2, a codex-family agent offering the
// review class: the foreman's family (claude, offerForeman's) is the one
// a plan review must differ from.
func (e *env) offerReviewer() {
	e.t.Helper()
	e.exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
		VALUES (?, 'codex', 'codex', 'gpt-5', 'codex', 'codex', '["review"]', ?, ?)`, e.runner2, seedTS, seedTS)
}

// reviewVerdict completes a plan review's lease with a verdict block.
func (e *env) reviewVerdict(g *LeaseGrant, verdict, findings string) Answer {
	e.t.Helper()
	return e.complete(g, Completion{Result: fmt.Sprintf(`{"verdict":%q,"findings":%q}`, verdict, findings)})
}

// theFindings is long enough to read as a verdict's real findings.
const theFindings = "The second task duplicates the first one's work; split the storage layer out before any of this runs."

// TestPlanReviewCreation: a multi-task plan of a project with the review
// on is created reviewing — every task pending, no work job, one
// plan_review job excluding the foreman's family — while a single
// review-class task, or the setting off, skips the review whole.
func TestPlanReviewCreation(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Spec: "first thing"}, {Spec: "second thing"}})
	if res.State != planReviewing {
		t.Fatalf("plan state = %q, want %s", res.State, planReviewing)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Errorf("stored plan state = %q", got)
	}
	if got := e.int("SELECT review_round FROM plans WHERE id = ?", res.PlanID); got != 1 {
		t.Errorf("review round = %d, want 1", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("work jobs = %d, want none queued", got)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks WHERE plan_id = ? AND state <> 'pending'", res.PlanID); got != 0 {
		t.Errorf("%d tasks left pending, want all held", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status = 'queued'"); got != 1 {
		t.Fatalf("plan_review jobs = %d, want one queued", got)
	}
	if key := e.str("SELECT key FROM jobs WHERE kind = 'plan_review'"); key != planReviewKey(res.PlanID) {
		t.Errorf("plan review key = %q, want %s", key, planReviewKey(res.PlanID))
	}
	if fam := e.str("SELECT exclude_family FROM jobs WHERE kind = 'plan_review'"); fam != "claude" {
		t.Errorf("plan review excludes family %q, want claude (the foreman's)", fam)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evPlanReviewQueued); got != 1 {
		t.Errorf("plan_review_queued events = %d, want one", got)
	}
	// The input carries every spec verbatim (§1.4) and the round.
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'plan_review'")
	for _, want := range []string{"round 1 of 2", "first thing", "second thing", `plan 2 "a plan to judge"`} {
		// The plan under review is the second plan of the fixture's store.
		if !strings.Contains(prompt, want) {
			t.Errorf("plan review input lacks %q:\n%s", want, prompt)
		}
	}
	// The foreman's own family takes nothing: a runner with only the
	// claude agent does not lease the review.
	if g := e.lease(e.runner1, nil, nil); g != nil {
		t.Fatalf("claude runner leased %s job %d, want nothing", g.Kind, g.JobID)
	}
	g := e.lease(e.runner2, nil, nil)
	if g == nil || g.Kind != KindPlanReview {
		t.Fatalf("lease = %+v, want the plan_review job", g)
	}
	if g.AgentAdapter != "codex" || g.Agent != "codex" || g.AgentModel != "gpt-5" {
		t.Errorf("review agent = %s %s %s, want the codex reviewer", g.AgentAdapter, g.Agent, g.AgentModel)
	}

	// A single review-class task needs no review of its own.
	solo := e.createPlan([]PlanTask{{Spec: "review the branch", Class: "review"}})
	if solo.State != "active" || e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review'") != 1 {
		t.Errorf("review-class task: state %s, plan_review jobs %d; want active, the one above", solo.State,
			e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review'"))
	}

	// The project can turn the review off.
	e.setSettings(`{"plan_review": false}`)
	off := e.createPlan(nTasks(2))
	if off.State != "active" || e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'") != 3 {
		t.Errorf("plan_review off: state %s, work jobs %d; want active with the review-class plan's one and two more",
			off.State, e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"))
	}
}

// TestPlanReviewApproveActivatesAndQueues: an approve verdict runs the
// approval-rule evaluation as a no-review creation does — an untripped
// plan activates and its first wave queues, a tripped one awaits the
// user with the existing reasons.
func TestPlanReviewApproveActivatesAndQueues(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}, {Name: "b", Spec: "do b", Deps: []string{"a"}}})
	g := e.lease(e.runner2, nil, nil)
	e.reviewVerdict(g, verdictApprove, theFindings)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state after approve = %q, want active", got)
	}
	if got := e.str("SELECT review_verdict FROM plans WHERE id = ?", res.PlanID); got != verdictApprove {
		t.Errorf("recorded verdict = %q, want approve", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 1 {
		t.Errorf("work jobs = %d, want the first wave's one (b waits on a)", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE name = 'b' AND plan_id = ?", res.PlanID); got != taskPending {
		t.Errorf("task b = %s, want pending behind a", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evPlanReviewed); got != 1 {
		t.Errorf("plan_reviewed events = %d, want one", got)
	}

	// A rule the plan trips holds it for the user after the verdict.
	e2 := newPlanReviewEnv(t)
	e2.offerForeman(e2.runner1)
	e2.offerReviewer()
	held := e2.createPlan([]PlanTask{{Spec: "deploy it", Class: "deploy"}})
	g2 := e2.lease(e2.runner2, nil, nil)
	e2.reviewVerdict(g2, verdictApprove, theFindings)
	if got := e2.str("SELECT state FROM plans WHERE id = ?", held.PlanID); got != planAwaitingApproval {
		t.Fatalf("tripped plan state after approve = %q, want %s", got, planAwaitingApproval)
	}
	if got := e2.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("tripped plan queued %d work jobs, want none", got)
	}
	if got := e2.int("SELECT COUNT(*) FROM events WHERE kind = ?", evPlanAwaitingApproval); got != 1 {
		t.Errorf("plan_awaiting_approval events = %d, want one", got)
	}
	// The verdict reached the foreman: a turn was woken for it.
	if got := e2.int("SELECT COUNT(*) FROM jobs WHERE kind = 'foreman'"); got != 1 {
		t.Errorf("foreman turns = %d, want one woken with the verdict", got)
	}
}

// TestPlanReviewChangesWakesForeman: a changes verdict with a round left
// parks the plan at needs_foreman and carries the findings to the
// foreman verbatim, its turn woken.
func TestPlanReviewChangesWakesForeman(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	g := e.lease(e.runner2, nil, nil)
	e.reviewVerdict(g, verdictChanges, theFindings)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planNeedsForeman {
		t.Fatalf("plan state after changes = %q, want %s (the foreman amends or accepts)", got, planNeedsForeman)
	}
	if got := e.str("SELECT review_verdict FROM plans WHERE id = ?", res.PlanID); got != verdictChanges {
		t.Errorf("recorded verdict = %q, want changes", got)
	}
	if got := e.str("SELECT review_findings FROM plans WHERE id = ?", res.PlanID); got != theFindings {
		t.Errorf("recorded findings = %q, want them verbatim", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("work jobs = %d, want none", got)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	if !strings.Contains(prompt, theFindings) {
		t.Errorf("the woken turn's prompt lacks the findings verbatim:\n%s", prompt)
	}
	if !strings.Contains(prompt, "changes") {
		t.Errorf("the woken turn's prompt lacks the verdict:\n%s", prompt)
	}
}

// TestPlanReviewRoundTwoForcedProceed: the foreman's amendment answers a
// changes verdict with round 2 — whose input carries the previous
// findings — and the second changes verdict proceeds through the same
// evaluation as approve, the findings recorded and shown.
func TestPlanReviewRoundTwoForcedProceed(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}})
	g := e.lease(e.runner2, nil, nil)
	e.reviewVerdict(g, verdictChanges, theFindings)

	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Respec: []PlanRespec{{Task: "a", Spec: "do a, and only a"}},
	}); err != nil {
		t.Fatalf("amend the reviewing plan: %v", err)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Fatalf("plan state after the answer = %q, want reviewing for round 2", got)
	}
	if got := e.int("SELECT review_round FROM plans WHERE id = ?", res.PlanID); got != 2 {
		t.Fatalf("review round = %d, want 2", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status = 'queued'"); got != 1 {
		t.Fatalf("queued plan_review jobs = %d, want round 2's", got)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'plan_review' ORDER BY id DESC")
	if !strings.Contains(prompt, "round 2 of 2") || !strings.Contains(prompt, theFindings) {
		t.Errorf("round 2 input lacks the round or the previous findings:\n%s", prompt)
	}

	g2 := e.lease(e.runner2, nil, nil)
	e.reviewVerdict(g2, verdictChanges, "still wrong, but the rounds are spent")
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state after the second changes = %q, want active (forced proceed)", got)
	}
	if got := e.str("SELECT review_verdict FROM plans WHERE id = ?", res.PlanID); got != verdictChanges {
		t.Errorf("recorded verdict = %q, want the second changes kept", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 1 {
		t.Errorf("work jobs = %d, want the first wave's one", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evPlanReviewed); got != 2 {
		t.Errorf("plan_reviewed events = %d, want one per round", got)
	}
}

// TestPlanReviewCancelAnswerProceeds: an amendment that answers a changes
// verdict by cancelling a task, adding and respecifying none, asks for no
// new round; the plan proceeds through the same evaluation as approve.
func TestPlanReviewCancelAnswerProceeds(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}, {Name: "b", Spec: "do b"}})
	g := e.lease(e.runner2, nil, nil)
	e.reviewVerdict(g, verdictChanges, theFindings)

	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Cancel: []string{"b"}}); err != nil {
		t.Fatalf("amend the reviewing plan: %v", err)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state after a cancel-only answer = %q, want active", got)
	}
	if got := e.int("SELECT review_round FROM plans WHERE id = ?", res.PlanID); got != 1 {
		t.Errorf("review round = %d, want 1 (no new round)", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status = 'queued'"); got != 0 {
		t.Errorf("queued plan_review jobs = %d, want none", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 1 {
		t.Errorf("work jobs = %d, want task a's one", got)
	}
}

// TestPlanReviewAmendmentHoldsAddedTasks: an amendment that adds tasks to
// an active plan queues a plan_review round over them; the added tasks
// stay pending until the verdict, then start — or hold alone at the plan
// gate when a per-task rule trips — while the plan's earlier work went
// on. Cancel, re-point and retier never trigger a review.
func TestPlanReviewAmendmentHoldsAddedTasks(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	// A single review-class task skips the creation review (§6.2),
	// so the plan is active and running when the foreman adds to it.
	res := e.createPlan([]PlanTask{{Name: "solo", Spec: "look at the branch", Class: "review"}})
	if res.State != "active" {
		t.Fatalf("plan state = %q, want active", res.State)
	}
	am, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add: []PlanTask{
			{Name: "plain", Spec: "add the feature"},
			{Name: "gated", Spec: "touch infra/deploy.sh", Class: "deploy"},
		},
	})
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if am.State != planReviewing {
		t.Fatalf("amended plan state = %q, want %s", am.State, planReviewing)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Errorf("stored plan state = %q", got)
	}
	// The added tasks are held pending; the plan's own work job stays.
	if got := e.str("SELECT state FROM tasks WHERE name = 'plain' AND plan_id = ?", res.PlanID); got != taskPending {
		t.Errorf("added plain task = %s, want pending until the verdict", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE name = 'gated' AND plan_id = ?", res.PlanID); got != taskPending {
		t.Errorf("added gated task = %s, want pending until the verdict", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 1 {
		t.Errorf("work jobs = %d, want only the plan's own first one", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status = 'queued'"); got != 1 {
		t.Fatalf("plan_review jobs = %d, want the amendment's round", got)
	}
	// The round's input names the added tasks and their specs verbatim.
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'plan_review'")
	if !strings.Contains(prompt, "add the feature") || !strings.Contains(prompt, "touch infra/deploy.sh") {
		t.Errorf("amendment round input lacks an added spec:\n%s", prompt)
	}

	g := e.lease(e.runner2, nil, nil)
	e.reviewVerdict(g, verdictApprove, theFindings)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state after approve = %q, want active", got)
	}
	// The plain addition started; the deploy-class one holds alone.
	if got := e.str("SELECT state FROM tasks WHERE name = 'plain' AND plan_id = ?", res.PlanID); got != taskReady {
		t.Errorf("plain addition = %s, want ready and queued", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs j JOIN tasks t ON t.id = j.task_id WHERE t.name = 'plain'"); got != 1 {
		t.Errorf("plain addition queued %d work jobs, want one", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE name = 'gated' AND plan_id = ?", res.PlanID); got != taskNeedsForeman {
		t.Errorf("gated addition = %s, want needs_foreman at the plan gate", got)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND payload LIKE '%class deploy%'", evTaskPlanHeld); got != 1 {
		t.Errorf("task_plan_held events = %d, want the gated addition's", got)
	}

	// Cancel, re-point and retier never trigger a review.
	plain := am.Added[0].TaskID
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Cancel: []string{fmt.Sprintf("%d", plain)},
		Retier: []PlanRetier{{Task: "solo", Tier: "heavy"}},
	}); err != nil {
		t.Fatalf("cancel-and-retier amendment: %v", err)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review'"); got != 1 {
		t.Errorf("plan_review jobs after cancel-and-retier = %d, want the one above", got)
	}
}

// TestPlanReviewCancelCancelsTheJob: cancelling the plan cancels its
// pending plan_review job with it.
func TestPlanReviewCancelCancelsTheJob(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	job := e.int("SELECT id FROM jobs WHERE kind = 'plan_review'")
	if job == 0 {
		t.Fatal("no plan_review job to cancel")
	}
	if _, err := e.eng.ArchiveProject(e.ctx, envRepo, true); err != nil {
		t.Fatalf("archive with cancel: %v", err)
	}
	if got := e.str("SELECT status FROM jobs WHERE id = ?", job); got != statusCancelled {
		t.Errorf("plan_review job = %s, want cancelled with the plan", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planCancelled {
		t.Errorf("plan = %s, want cancelled", got)
	}
}

// TestPlanReviewUnrunnableProceeds: a project with no reviewer at all —
// no agent of another family offers the review class — never queues the
// review job; the plan proceeds through the same evaluation as approve,
// the reason recorded and shown.
func TestPlanReviewUnrunnableProceeds(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1) // the foreman's own claude agent only
	res := e.createPlan(nTasks(2))
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active (the review cannot run)", got)
	}
	if res.State != "active" || res.JobID == 0 || res.Tasks[0].JobID == 0 || res.Tasks[1].JobID == 0 {
		t.Errorf("result = state %q, job %d, task jobs %d/%d; want active with the first wave's jobs",
			res.State, res.JobID, res.Tasks[0].JobID, res.Tasks[1].JobID)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review'"); got != 0 {
		t.Errorf("plan_review jobs = %d, want none", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 2 {
		t.Errorf("work jobs = %d, want the first wave's two", got)
	}
	if got := e.str("SELECT review_verdict FROM plans WHERE id = ?", res.PlanID); got != planReviewSkipped {
		t.Errorf("recorded verdict = %q, want %s", got, planReviewSkipped)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	if !strings.Contains(prompt, "skipped") || !strings.Contains(prompt, "review class") {
		t.Errorf("the foreman's turn lacks the skip's reason:\n%s", prompt)
	}
}

// TestPlanReviewSettingsParse: the plan review's keys parse, validate and
// refuse like the other project settings.
func TestPlanReviewSettingsParse(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		want       string
	}{
		{SettingPlanReview, "true", "true"},
		{SettingPlanReview, "false", "false"},
		{SettingPlanReviewTier, "light", "light"},
		{SettingPlanReviewTier, "heavy", "heavy"},
		{SettingPlanReviewEffort, "max", "max"},
	} {
		raw, err := parseSettingValue(tc.key, tc.value)
		if err != nil || strings.Trim(string(raw), `"`) != tc.want {
			t.Errorf("parse %s=%s = %s, %v; want %q", tc.key, tc.value, raw, err, tc.want)
		}
	}
	for _, tc := range []struct{ key, value string }{
		{SettingPlanReview, "maybe"},
		{SettingPlanReviewTier, "huge"},
		{SettingPlanReviewEffort, "extreme"},
	} {
		if _, err := parseSettingValue(tc.key, tc.value); err == nil {
			t.Errorf("parse %s=%q accepted, want a refusal", tc.key, tc.value)
		}
	}
}

// TestPlanReviewStatusShowsRoundAndVerdict: the status carries the
// review's state — the round under way, then the verdict and findings —
// so `brigada plan show`/status and the foreman's status tool read them.
func TestPlanReviewStatusShowsRoundAndVerdict(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	var plan *StatusPlan
	for i := range st.Plans {
		if st.Plans[i].ID == res.PlanID {
			plan = &st.Plans[i]
		}
	}
	if plan == nil || plan.State != planReviewing || plan.ReviewRound != 1 {
		t.Fatalf("status plan = %+v, want reviewing at round 1", plan)
	}
	if plan.ReviewVerdict != "" || plan.ReviewFindings != "" {
		t.Errorf("review = %q %q before a verdict, want neither", plan.ReviewVerdict, plan.ReviewFindings)
	}
	g := e.lease(e.runner2, nil, nil)
	e.reviewVerdict(g, verdictChanges, theFindings)
	st, err = e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	for i := range st.Plans {
		if st.Plans[i].ID == res.PlanID {
			plan = &st.Plans[i]
		}
	}
	if plan.State != planNeedsForeman || plan.ReviewRound != 1 || plan.ReviewVerdict != verdictChanges || plan.ReviewFindings != theFindings {
		t.Fatalf("status plan after changes = %+v, want parked with the verdict and findings kept", plan)
	}
}

// TestComposePlanReviewInput: the input is a pure function of its views —
// every spec verbatim, the graph by name, the other active plans
// summarized with their branches, and the previous round's verdict and
// findings on a round past the first.
func TestComposePlanReviewInput(t *testing.T) {
	plan := planReviewPlanView{
		ID: 12, Title: "ship it",
		Tasks: []planReviewTaskView{
			{ID: 1, Name: "base", Title: "base", Spec: "do\nthe base\nwork", Class: "", Tier: "", Effort: "high", Check: "go test ./..."},
			{ID: 2, Name: "top", Title: "top", Spec: "do the top", Class: "deploy", Deps: []string{"base"}, Base: "base", Continue: "base"},
		},
	}
	others := []otherPlanView{{
		ID: 7, Title: "other plan",
		Tasks: []otherPlanTaskView{{ID: 3, Name: "running one", State: "running", Branch: "brigada/task-3"}},
	}}
	got := composePlanReviewInput(1, plan, others, "", "")
	for _, want := range []string{
		"round 1 of 2",
		`plan 12 "ship it", 2 tasks`,
		"spec, verbatim:\ndo\nthe base\nwork",
		"check: go test ./...",
		"task 2 top: class deploy, tier standard, effort default.",
		"it waits on base; its work starts from base; it continues base.",
		`plan 7 "other plan", 1 tasks`,
		"task 3 running one: running, branch brigada/task-3.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("input lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "previous round") {
		t.Errorf("round 1 names a previous round:\n%s", got)
	}
	again := composePlanReviewInput(2, plan, nil, verdictChanges, theFindings)
	if !strings.Contains(again, "round 2 of 2") || !strings.Contains(again, "The previous round's verdict was changes.") ||
		!strings.Contains(again, theFindings) || !strings.Contains(again, "no other active plans") {
		t.Errorf("round 2 input lacks the round, verdict, findings or empty-others note:\n%s", again)
	}
	if composePlanReviewInput(1, plan, others, "", "") != got {
		t.Error("the input is not a pure function of its views")
	}
}

// TestPlanReviewAmendedCreationRunsWholePlanRules: a creation review the
// foreman answers by adding a task reviews the whole plan again in round
// 2, and its approve runs the whole-plan rules (§6.3), so an added
// deploy task holds the plan for the user instead of only itself.
func TestPlanReviewAmendedCreationRunsWholePlanRules(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}, {Name: "b", Spec: "do b"}})
	e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictChanges, theFindings)

	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add: []PlanTask{{Name: "c", Spec: "deploy it", Class: "deploy"}},
	}); err != nil {
		t.Fatalf("amend the reviewing plan: %v", err)
	}
	if got := e.str("SELECT review_batch FROM plans WHERE id = ?", res.PlanID); got != "" {
		t.Errorf("round 2 batch = %q, want the whole plan", got)
	}
	e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictApprove, theFindings)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planAwaitingApproval {
		t.Fatalf("plan state after round 2 approve = %q, want %s", got, planAwaitingApproval)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("work jobs = %d, want none while the plan awaits the user", got)
	}
}

// TestPlanReviewAmendDuringLiveRoundRefused: tasks added or respecified
// while a review round runs would start at its verdict unreviewed, so
// the amendment is refused; a cancel goes through.
func TestPlanReviewAmendDuringLiveRoundRefused(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}, {Name: "b", Spec: "do b"}})
	_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{{Name: "c", Spec: "do c"}}})
	if !errors.Is(err, ErrPlanReviewRunning) {
		t.Fatalf("add during a live round: err = %v, want ErrPlanReviewRunning", err)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Cancel: []string{"b"}}); err != nil {
		t.Fatalf("cancel during a live round: %v", err)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Errorf("plan state = %q, want reviewing (the round still runs)", got)
	}
}

// TestPlanReviewNoReviewerAmendment: an active plan's amendment with no
// reviewer to be had judges the added tasks alone — a rule-held task
// holds by itself, the plan stays active — and the result says active
// with the started work.
func TestPlanReviewNoReviewerAmendment(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1) // no agent of another family
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}})
	am, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add: []PlanTask{{Name: "c", Spec: "deploy it", Class: "deploy"}, {Name: "d", Spec: "do d"}},
	})
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if am.State != "active" {
		t.Errorf("amend result state = %q, want active", am.State)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active (only the deploy task holds)", got)
	}
	if got := e.str("SELECT state FROM tasks WHERE plan_id = ? AND name = 'c'", res.PlanID); got != taskNeedsForeman {
		t.Errorf("task c = %s, want held at %s", got, taskNeedsForeman)
	}
	started := false
	for _, s := range am.Started {
		started = started || s.Name == "d"
	}
	if !started || am.Added[1].JobID == 0 {
		t.Errorf("result started = %+v, added = %+v; want task d's work job", am.Started, am.Added)
	}
}

// TestPlanReviewHardCapProceeds: a plan review past its hard cap gives no
// verdict, and the plan proceeds instead of waiting in reviewing.
func TestPlanReviewHardCapProceeds(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.PlanReview = nil
		c.HardCaps = map[Kind]time.Duration{KindPlanReview: time.Minute}
		c.LeaseTTL = 10 * time.Minute // the cap fires, not the expiry
	})
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	if g := e.lease(e.runner2, nil, nil); g == nil || g.Kind != KindPlanReview {
		t.Fatalf("lease = %+v, want the plan review", g)
	}
	e.clock.Advance(2 * time.Minute)
	if rep := e.mustSweep(); len(rep.HardCapped) != 1 {
		t.Fatalf("the sweep hard-capped nothing: %+v", rep)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active after the capped review", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 2 {
		t.Errorf("work jobs = %d, want the first wave's two", got)
	}
}

// TestPlanReviewLazyApproveIsNoVerdict: an approve without substantive
// findings is no verdict, as a task review's is (§7), and counts nothing
// on the reviewer's outcome record; a substantive approve counts.
func TestPlanReviewLazyApproveIsNoVerdict(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	lazy := e.createPlan(nTasks(2))
	e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictApprove, "LGTM")
	if got := e.str("SELECT review_verdict FROM plans WHERE id = ?", lazy.PlanID); got != planReviewNoRun {
		t.Errorf("lazy approve recorded %q, want %s", got, planReviewNoRun)
	}
	if got := e.int("SELECT COUNT(*) FROM outcomes"); got != 0 {
		t.Errorf("outcome rows = %d after a lazy approve, want none", got)
	}
	real := e.createPlan(nTasks(2))
	e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictApprove, theFindings)
	if got := e.str("SELECT review_verdict FROM plans WHERE id = ?", real.PlanID); got != verdictApprove {
		t.Errorf("substantive approve recorded %q, want approve", got)
	}
	if got := e.int("SELECT COALESCE(SUM(successes), 0) FROM outcomes WHERE class = 'review'"); got != 1 {
		t.Errorf("review successes = %d, want the substantive approve's one", got)
	}
}

// TestPlanReviewAmendmentRoundTwoKeepsTheBatch: round 2 of an active
// plan's amendment review covers the tasks round 1 held as well as the
// answer's, so each still gets its per-task rules at the verdict.
func TestPlanReviewAmendmentRoundTwoKeepsTheBatch(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}})
	e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictApprove, theFindings)

	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add: []PlanTask{{Name: "c", Spec: "deploy c", Class: "deploy"}, {Name: "d", Spec: "deploy d", Class: "deploy"}},
	}); err != nil {
		t.Fatalf("amend: %v", err)
	}
	e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictChanges, theFindings)
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Respec: []PlanRespec{{Task: "c", Spec: "deploy c, carefully"}},
	}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	e.reviewVerdict(e.lease(e.runner2, nil, nil), verdictApprove, theFindings)
	for _, name := range []string{"c", "d"} {
		if got := e.str("SELECT state FROM tasks WHERE plan_id = ? AND name = ?", res.PlanID, name); got != taskNeedsForeman {
			t.Errorf("task %s = %s, want held at %s by the deploy class", name, got, taskNeedsForeman)
		}
	}
}

// offerSecondReviewer seeds, on runner 2, a kimi-family agent offering
// the review class: another reviewer for a plan review's retry.
func (e *env) offerSecondReviewer() {
	e.t.Helper()
	e.exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
		VALUES (?, 'kimi', 'kimi', 'kimi-k2', 'kimi', 'kimi', '["review"]', ?, ?)`, e.runner2, seedTS, seedTS)
}

// TestPlanReviewFailedRetriesThenHolds: a plan reviewer that runs and
// gives no verdict is retried once on another reviewer, the first one
// excluded; a retry that gives none either keeps the plan reviewing and
// wakes the foreman with the reason, and the foreman's amendment that
// respecifies a task starts the next round.
func TestPlanReviewFailedRetriesThenHolds(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	e.offerSecondReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}, {Name: "b", Spec: "do b"}})

	g1 := e.lease(e.runner2, nil, nil)
	if g1 == nil || g1.Kind != KindPlanReview {
		t.Fatalf("lease = %+v, want the plan review", g1)
	}
	first := e.int("SELECT agent_id FROM jobs WHERE id = ?", g1.JobID)
	e.complete(g1, Completion{Result: "I looked at it and it seems fine."})
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Fatalf("plan state after a malformed review = %q, want %s (retried)", got, planReviewing)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status = 'queued'"); got != 1 {
		t.Fatalf("queued plan_review jobs = %d, want the retry", got)
	}
	if got := e.int("SELECT exclude_agent FROM jobs WHERE kind = 'plan_review' AND status = 'queued'"); got != first {
		t.Errorf("retry excludes agent %d, want the first reviewer %d", got, first)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND payload LIKE '%retried on another reviewer%'", evPlanReviewQueued); got != 1 {
		t.Errorf("retry events = %d, want one", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("work jobs = %d, want none while the retry runs", got)
	}

	g2 := e.lease(e.runner2, nil, nil)
	if g2 == nil || g2.Kind != KindPlanReview {
		t.Fatalf("lease = %+v, want the retry", g2)
	}
	if got := e.int("SELECT agent_id FROM jobs WHERE id = ?", g2.JobID); got == first {
		t.Fatalf("the retry ran on the first reviewer %d", got)
	}
	e.complete(g2, Completion{Error: "worker CLI failed", ErrorKind: "other"})
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Fatalf("plan state after the failed retry = %q, want %s (held for the foreman)", got, planReviewing)
	}
	if got := e.str("SELECT review_verdict FROM plans WHERE id = ?", res.PlanID); got != planReviewNoRun {
		t.Errorf("recorded verdict = %q, want %s", got, planReviewNoRun)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status IN ('queued', 'leased', 'running')"); got != 0 {
		t.Errorf("live plan_review jobs = %d, want none past the retry", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("work jobs = %d, want none: the plan did not proceed", got)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	for _, want := range []string{"worker CLI failed", "gave no verdict either", "stays reviewing"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the woken turn's prompt lacks %q:\n%s", want, prompt)
		}
	}

	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Respec: []PlanRespec{{Task: "a", Spec: "do a, smaller"}},
	}); err != nil {
		t.Fatalf("answer the held review: %v", err)
	}
	if got := e.int("SELECT review_round FROM plans WHERE id = ?", res.PlanID); got != 2 {
		t.Errorf("review round = %d, want 2", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status = 'queued' AND exclude_agent IS NULL"); got != 1 {
		t.Errorf("round 2 plan_review jobs = %d, want one, no agent excluded", got)
	}
}

// TestPlanReviewFailedNoOtherReviewerHolds: a reviewer that gives no
// verdict with no other agent to take the retry holds the plan for the
// foreman at once; a cancel-only answer lets it proceed.
func TestPlanReviewFailedNoOtherReviewerHolds(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}, {Name: "b", Spec: "do b"}})
	e.complete(e.lease(e.runner2, nil, nil), Completion{Error: "usage limit reached", ErrorKind: "other"})
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Fatalf("plan state = %q, want %s", got, planReviewing)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review'"); got != 1 {
		t.Errorf("plan_review jobs = %d, want no retry", got)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	if !strings.Contains(prompt, "no other agent can take the retry") {
		t.Errorf("the woken turn's prompt lacks the reason:\n%s", prompt)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Cancel: []string{"b"}}); err != nil {
		t.Fatalf("cancel-only answer: %v", err)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state after a cancel-only answer = %q, want active", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 1 {
		t.Errorf("work jobs = %d, want task a's one", got)
	}
}

// TestPlanReviewLostRunnerProceeds: a plan review that lost its runner
// MaxAttempts times fails at the lease, and the plan proceeds instead of
// waiting in reviewing with no live job.
func TestPlanReviewLostRunnerProceeds(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	e.exec("UPDATE jobs SET attempts = 3 WHERE kind = 'plan_review'")
	e.lease(e.runner2, nil, nil)
	if got := e.str("SELECT status FROM jobs WHERE kind = 'plan_review'"); got != statusFailed {
		t.Errorf("plan_review job = %s, want failed", got)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active after the lost review", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 2 {
		t.Errorf("work jobs = %d, want the first wave's two", got)
	}
}

// TestPlanReviewRecoveredRoundRequeued: a reviewing plan with no live
// review job — what a recovered store holds — gets its round queued
// again, and proceeds when no reviewer can run it.
func TestPlanReviewRecoveredRoundRequeued(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	e.exec("UPDATE jobs SET status = 'failed' WHERE kind = 'plan_review'")
	recovered := func(plan int64) {
		t.Helper()
		if err := e.eng.store.Store.Update(e.ctx, func(tx *sql.Tx) error {
			return e.eng.planReviewRecoveredTx(tx, e.project, plan, e.clock.Now())
		}); err != nil {
			t.Fatalf("recover the review: %v", err)
		}
	}
	recovered(res.PlanID)
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", planReviewKey(res.PlanID)); got != 1 {
		t.Fatalf("queued plan_review jobs = %d, want the round requeued", got)
	}
	recovered(res.PlanID) // a live round stays the plan's review
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status = 'queued'"); got != 1 {
		t.Errorf("queued plan_review jobs = %d, want the one round", got)
	}

	e.exec("UPDATE jobs SET status = 'failed' WHERE kind = 'plan_review'")
	e.exec("UPDATE agents SET family = 'claude' WHERE family = 'codex'")
	recovered(res.PlanID)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state = %q, want active with no reviewer to be had", got)
	}
}

// TestPlanReviewInfraFailureRequeuesOnTheBackoff pins the plan review's
// infra retry (§5.1): a round whose fetch or CLI connection failed is
// requeued on the plan's key under the infra backoff — the same
// question, the same exclusions, no retry round spent and no foreman
// wake — and the retried round ends as any round does.
func TestPlanReviewInfraFailureRequeuesOnTheBackoff(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	key := planReviewKey(res.PlanID)
	leaseRound := func() *LeaseGrant {
		t.Helper()
		g := e.lease(e.runner2, nil, nil)
		if g == nil || g.Kind != KindPlanReview {
			t.Fatalf("plan review lease = %+v", g)
		}
		return g
	}
	if ans := e.complete(leaseRound(), Completion{Error: "Cannot connect to API", ErrorKind: ErrorKindInfra}); ans != AnsOK {
		t.Fatalf("complete the infra failure: %s", ans)
	}
	retry := e.int("SELECT id FROM jobs WHERE key = ? AND status = 'queued'", key)
	if retry == 0 {
		t.Fatal("no infra retry of the plan review queued")
	}
	if nb := e.str("SELECT not_before FROM jobs WHERE id = ?", retry); nb != fmtTime(e.clock.Now().Add(infraDelayFor(1))) {
		t.Errorf("retry not_before = %s, want now + %s", nb, infraDelayFor(1))
	}
	if exclude := e.str("SELECT exclude_family FROM jobs WHERE id = ?", retry); exclude != "claude" {
		t.Errorf("retry exclude_family = %q, want the foreman's family kept", exclude)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Fatalf("plan state = %q, want reviewing through the retry", got)
	}
	if got := e.foremanJobs(); got != 0 {
		t.Errorf("foreman turns = %d, want none while the window lasts", got)
	}
	if got := e.countEvents(evPlanReviewed); got != 0 {
		t.Errorf("plan_reviewed events = %d, want none through the retry", got)
	}
	// The retry runs after the backoff and its verdict moves the plan.
	e.elapse(infraDelayFor(1))
	if ans := e.reviewVerdict(leaseRound(), verdictApprove, theFindings); ans != AnsOK {
		t.Fatalf("complete the retried round: %s", ans)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Errorf("plan state after the retried round = %q, want active", got)
	}
}

// TestPlanReviewInfraWindowSpentHoldsThePlan: an outage that outlasts
// the infra retry window holds the plan for the foreman with the outage
// named (§5.1) — the round never ran, so the plan stays reviewing and
// the foreman reads why in its wake.
func TestPlanReviewInfraWindowSpentHoldsThePlan(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(nTasks(2))
	key := planReviewKey(res.PlanID)
	start := e.clock.Now()
	for {
		g := e.lease(e.runner2, nil, nil)
		if g == nil || g.Kind != KindPlanReview {
			t.Fatal("no plan review round to lease")
		}
		if ans := e.complete(g, Completion{Error: "Cannot connect to API", ErrorKind: ErrorKindInfra}); ans != AnsOK {
			t.Fatalf("complete = %s", ans)
		}
		if e.countEvents(evPlanReviewed) > 0 {
			break
		}
		if e.clock.Now().Sub(start) > infraRetryWindow+30*time.Minute {
			t.Fatal("the outage outlasted the window but the plan was never held")
		}
		e.waitOutInfraRetry(key)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Fatalf("plan state = %q, want reviewing: held for the foreman", got)
	}
	if got := e.str("SELECT review_verdict FROM plans WHERE id = ?", res.PlanID); got != planReviewNoRun {
		t.Errorf("recorded verdict = %q, want %s", got, planReviewNoRun)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status IN ('queued', 'leased', 'running')", key); got != 0 {
		t.Errorf("live plan_review jobs = %d, want none past the window", got)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	if !strings.Contains(prompt, "the network outage outlasted the "+capText(infraRetryWindow)+" infra retry window") {
		t.Errorf("the woken turn's prompt lacks the outage:\n%s", prompt)
	}
}
