package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/planfile"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// parkForChanges drives a plan to the park a changes verdict with a
// round left leaves (§6.2): creation, review round 1, a changes verdict.
// It returns the plan and the review job whose verdict parked it.
func parkForChanges(t *testing.T, e *env, tasks []PlanTask) (CreatePlanResult, int64) {
	t.Helper()
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan(tasks)
	g := e.lease(e.runner2, nil, nil)
	if g == nil || g.Kind != KindPlanReview {
		t.Fatalf("lease = %+v, want the plan review", g)
	}
	e.reviewVerdict(g, verdictChanges, theFindings)
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planNeedsForeman {
		t.Fatalf("plan state after changes = %q, want %s", got, planNeedsForeman)
	}
	return res, g.JobID
}

// TestPlanReviewChangesParksThePlan: a changes verdict with a round left
// parks the plan at needs_foreman — no work queued, no review job live,
// the findings whole on the plan and in the woken turn — and status
// lists the plan under needs you with the findings as its reason.
func TestPlanReviewChangesParksThePlan(t *testing.T) {
	e := newPlanReviewEnv(t)
	res, reviewJob := parkForChanges(t, e, []PlanTask{{Name: "a", Spec: "do a"}, {Name: "b", Spec: "do b"}})

	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("work jobs = %d, want none while the plan is parked", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review' AND status IN ('queued', 'leased', 'running')"); got != 0 {
		t.Errorf("live plan_review jobs = %d, want none: nothing auto-retries a parked plan", got)
	}
	if got := e.str("SELECT review_findings FROM plans WHERE id = ?", res.PlanID); got != theFindings {
		t.Errorf("recorded findings = %q, want them verbatim", got)
	}
	if got := e.int("SELECT COALESCE(job_id, 0) FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1", evPlanReviewed); got != reviewJob {
		t.Errorf("the parked plan_reviewed event names job %d, want the review job %d", got, reviewJob)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	for _, want := range []string{theFindings, "changes", planNeedsForeman, "plan_accept"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the woken turn's prompt lacks %q:\n%s", want, prompt)
		}
	}

	// Status: the parked plan sits in the needs-you queue with the
	// findings as its reason, and the plan line carries the review.
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	var entry *NeedsYouItem
	for i := range st.NeedsYou {
		if st.NeedsYou[i].Kind == "plan" && st.NeedsYou[i].ID == res.PlanID {
			entry = &st.NeedsYou[i]
		}
	}
	if entry == nil || entry.State != planNeedsForeman {
		t.Fatalf("needs-you entry = %+v, want the parked plan", entry)
	}
	if !strings.HasPrefix(entry.Reason, "The second task") {
		t.Errorf("needs-you reason = %q, want the findings' first words", entry.Reason)
	}
	var plan *StatusPlan
	for i := range st.Plans {
		if st.Plans[i].ID == res.PlanID {
			plan = &st.Plans[i]
		}
	}
	if plan == nil || plan.State != planNeedsForeman || plan.ReviewRound != 1 || plan.ReviewJob != reviewJob {
		t.Fatalf("status plan = %+v, want parked at round 1 naming its review job", plan)
	}
	if plan.ReviewVerdict != verdictChanges || plan.ReviewFindings != theFindings {
		t.Errorf("status review = %q %q, want the verdict and findings", plan.ReviewVerdict, plan.ReviewFindings)
	}
}

// TestPlanReviewParkSurvivesRestart: the park outlives the woken turn —
// a turn that never carried the event to the foreman returns it to the
// inbox, the start-of-engine wake re-wakes with the findings, and no
// sweep or wake requeues the review or any work.
func TestPlanReviewParkSurvivesRestart(t *testing.T) {
	e := newPlanReviewEnv(t)
	res, _ := parkForChanges(t, e, nTasks(2))

	// The woken turn dies unheard: its events return to the inbox, as a
	// cancelled turn's do (§4.2).
	turn := e.int("SELECT id FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	if _, err := e.eng.Cancel(e.ctx, turn); err != nil {
		t.Fatalf("cancel the woken turn: %v", err)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planNeedsForeman {
		t.Fatalf("plan state after the cancelled turn = %q, want still parked", got)
	}
	if rep := e.mustSweep(); len(rep.Expired) != 0 || len(rep.HardCapped) != 0 || len(rep.QueueTimedOut) != 0 {
		t.Errorf("the sweep acted (%+v), want nothing: a parked plan never auto-retries", rep)
	}
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover the wakes: %v", err)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	if !strings.Contains(prompt, theFindings) {
		t.Errorf("the recovered wake lacks the findings verbatim:\n%s", prompt)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planNeedsForeman {
		t.Errorf("plan state after recovery = %q, want still parked", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'plan_review'"); got != 1 {
		t.Errorf("plan_review jobs = %d, want the one completed round alone", got)
	}
}

// TestPlanReviewParkShowsInTheDigest: the fresh session's digest does
// not hide a parked plan (§4.2): it lists the plan with its state, so a
// rotated foreman still sees what waits for it.
func TestPlanReviewParkShowsInTheDigest(t *testing.T) {
	e := newPlanReviewEnv(t)
	res, _ := parkForChanges(t, e, nTasks(1))
	var digest string
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		digest, err = foremanDigestTx(tx, e.eng.cfg.Review.Family, e.project, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("- plan %d ", res.PlanID)
	at := strings.Index(digest, want)
	if at < 0 || !strings.Contains(digest[at:at+len(want)+digestLineLimit], planNeedsForeman) {
		t.Errorf("the digest hides the parked plan %d:\n%s", res.PlanID, digest)
	}
}

// TestPlanAcceptFromParked: the foreman accepts a parked plan as
// reviewed, with the reason recorded, and the plan proceeds through the
// same evaluation a spent round's verdict runs — active with its first
// wave queued, or awaiting the user's yes when a rule holds it, the
// foreman told which. Without a reason, or on a plan not parked, the
// call is refused whole.
func TestPlanAcceptFromParked(t *testing.T) {
	e := newPlanReviewEnv(t)
	res, _ := parkForChanges(t, e, []PlanTask{{Name: "a", Spec: "do a"}, {Name: "b", Spec: "do b", Deps: []string{"a"}}})

	if _, err := e.eng.AcceptPlan(e.ctx, e.project, res.PlanID, "  "); err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Fatalf("accept without a reason: err = %v, want the refusal", err)
	}
	dec, err := e.eng.AcceptPlan(e.ctx, e.project, res.PlanID, "the findings name follow-up work, not blockers")
	if err != nil {
		t.Fatalf("accept the parked plan: %v", err)
	}
	if dec.State != "active" || len(dec.Tasks) != 1 || dec.Tasks[0].Name != "a" || dec.Tasks[0].JobID == 0 {
		t.Fatalf("decision = %+v, want active with task a's work job", dec)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != "active" {
		t.Fatalf("plan state after accept = %q, want active", got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 1 {
		t.Errorf("work jobs = %d, want the first wave's one (b waits on a)", got)
	}
	if got := e.str("SELECT review_batch FROM plans WHERE id = ?", res.PlanID); got != "" {
		t.Errorf("review batch after accept = %q, want it cleared", got)
	}
	if got := e.str("SELECT json_extract(payload, '$.status') FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1", evPlanReviewed); got != planReviewAccepted {
		t.Errorf("the latest plan_reviewed status = %q, want %s", got, planReviewAccepted)
	}
	if got := e.str("SELECT json_extract(payload, '$.reason') FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1", evPlanReviewed); !strings.Contains(got, "the findings name follow-up work") {
		t.Errorf("the acceptance's reason = %q, want the foreman's words", got)
	}

	// A plan not parked is refused whole.
	if _, err := e.eng.AcceptPlan(e.ctx, e.project, res.PlanID, "again"); !errors.Is(err, ErrPlanNotParked) {
		t.Fatalf("accept an active plan: err = %v, want ErrPlanNotParked", err)
	}

	// A rule the plan trips holds it for the user's yes after the
	// acceptance, and the foreman hears which.
	e2 := newPlanReviewEnv(t)
	held, _ := parkForChanges(t, e2, []PlanTask{{Spec: "deploy it", Class: "deploy"}})
	dec2, err := e2.eng.AcceptPlan(e2.ctx, e2.project, held.PlanID, "the deploy class is expected here")
	if err != nil {
		t.Fatalf("accept the tripped plan: %v", err)
	}
	if dec2.State != planAwaitingApproval || e2.str("SELECT state FROM plans WHERE id = ?", held.PlanID) != planAwaitingApproval {
		t.Fatalf("tripped plan after accept = %q, want %s", e2.str("SELECT state FROM plans WHERE id = ?", held.PlanID), planAwaitingApproval)
	}
	if got := e2.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("tripped plan queued %d work jobs, want none", got)
	}
	if got := e2.int("SELECT COUNT(*) FROM foreman_inbox i JOIN events ev ON ev.id = i.event_id WHERE i.state = ? AND ev.kind = ? AND json_extract(ev.payload, '$.status') = ?", inboxPending, evPlanReviewed, planReviewAccepted); got != 1 {
		t.Errorf("pending acceptance events = %d, want the foreman's one", got)
	}
}

// TestPlanReviewFindingsReachTheForemanWhole: a multi-KB verdict reaches
// the foreman wake and the round-2 prompt intact under the findings'
// own bound (issue #70), while status cuts its views at render time and
// ends with the pointer to the history.
func TestPlanReviewFindingsReachTheForemanWhole(t *testing.T) {
	e := newPlanReviewEnv(t)
	e.offerForeman(e.runner1)
	e.offerReviewer()
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "do a"}})
	g := e.lease(e.runner2, nil, nil)
	variable := strings.Repeat("The reviewer's findings, at length: this verdict runs to several kilobytes and must survive whole. ", 120)
	if len(variable) <= maxPlanReviewFindings/2 {
		t.Fatalf("the findings are %d bytes, want a multi-KB verdict", len(variable))
	}
	e.reviewVerdict(g, verdictChanges, variable)

	if got := e.str("SELECT review_findings FROM plans WHERE id = ?", res.PlanID); got != variable {
		t.Errorf("stored findings = %d bytes, want the whole %d", len(got), len(variable))
	}
	wake := e.str("SELECT prompt FROM jobs WHERE kind = 'foreman' ORDER BY id DESC")
	if !strings.Contains(wake, variable) {
		t.Errorf("the woken turn's prompt lacks the multi-KB findings verbatim (%d bytes)", len(wake))
	}

	// Status keeps the findings whole in the data and cuts only its
	// rendered line, ending with the pointer to the review job.
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	var entry *NeedsYouItem
	for i := range st.NeedsYou {
		if st.NeedsYou[i].Kind == "plan" && st.NeedsYou[i].ID == res.PlanID {
			entry = &st.NeedsYou[i]
		}
	}
	if entry == nil {
		t.Fatal("the parked plan is not in the needs-you queue")
	}
	if len(entry.Reason) >= len(variable) || !strings.Contains(entry.Reason, fmt.Sprintf("(full: brigada history --job %d)", g.JobID)) {
		t.Errorf("needs-you reason = %q…, want the cut and the pointer to job %d", entry.Reason[:80], g.JobID)
	}
	var plan *StatusPlan
	for i := range st.Plans {
		if st.Plans[i].ID == res.PlanID {
			plan = &st.Plans[i]
		}
	}
	if plan == nil || plan.ReviewFindings != variable || plan.ReviewJob != g.JobID {
		t.Fatalf("status plan = %+v, want the whole findings and the review job named", plan)
	}

	// The foreman answers with an amendment: round 2's input carries the
	// previous findings whole.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Respec: []PlanRespec{{Task: "a", Spec: "do a, and only a"}},
	}); err != nil {
		t.Fatalf("amend the parked plan: %v", err)
	}
	if got := e.str("SELECT state FROM plans WHERE id = ?", res.PlanID); got != planReviewing {
		t.Fatalf("plan state after the answer = %q, want reviewing for round 2", got)
	}
	round2 := e.str("SELECT prompt FROM jobs WHERE kind = 'plan_review' ORDER BY id DESC")
	if !strings.Contains(round2, "round 2 of 2") || !strings.Contains(round2, variable) {
		t.Errorf("round 2 input lacks the round or the whole findings (%d bytes):\n%s", len(round2), round2[:200])
	}

	// A verdict past the bound is cut to it, still far above a note's
	// 500 bytes, and the store keeps what fit.
	e2 := newPlanReviewEnv(t)
	e2.offerForeman(e2.runner1)
	e2.offerReviewer()
	e2.createPlan(nTasks(2))
	huge := strings.Repeat("x", maxPlanReviewFindings+4096)
	e2.reviewVerdict(e2.lease(e2.runner2, nil, nil), verdictChanges, huge)
	parked := e2.int("SELECT id FROM plans WHERE state = ?", planNeedsForeman)
	if got := e2.str("SELECT review_findings FROM plans WHERE id = ?", parked); got != strings.Repeat("x", maxPlanReviewFindings) {
		t.Errorf("stored oversized findings = %d bytes, want the %d-byte bound", len(got), maxPlanReviewFindings)
	}
}

// TestRecoverKeepsAParkedPlanParked: a plan file that shows a plan
// parked at needs_foreman is applied as such (§6.4, §6.2): the plan
// keeps waiting for the foreman's answer — no review round is requeued,
// no work starts.
func TestRecoverKeepsAParkedPlanParked(t *testing.T) {
	r := &recoverEnv{env: newEnvCfg(t, func(c *Config) { c.PlanReview = nil }), remote: testsupport.NewGitRemote(t)}
	r.exec("UPDATE projects SET repo_url = ? WHERE id = ?", r.remote, r.project)
	r.offerForeman(r.runner1)
	r.offerReviewer()
	stored := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	r.pushFile(planfile.Plan{
		ID: 60, Title: "the parked plan", State: planNeedsForeman,
		Created: stored.Add(time.Hour), Updated: stored.Add(2 * time.Hour),
		Tasks: []planfile.Task{
			{ID: 601, Name: "a", Title: "build a", Class: "implement", State: "pending"},
			{ID: 602, Name: "b", Title: "build b", Class: "implement", State: "pending"},
		},
	})
	res := r.recoverPlans()
	if len(res) != 1 || res[0].Action != PlanRebuilt {
		t.Fatalf("results = %+v, want plan 60 rebuilt", res)
	}
	if got := r.str("SELECT state FROM plans WHERE id = 60"); got != planNeedsForeman {
		t.Fatalf("plan state after recovery = %q, want %s", got, planNeedsForeman)
	}
	if got := r.int("SELECT COUNT(*) FROM jobs WHERE key = ?", planReviewKey(60)); got != 0 {
		t.Errorf("plan_review jobs = %d, want none: the review is done, the foreman answers", got)
	}
	if got := r.int("SELECT COUNT(*) FROM jobs WHERE kind = 'work'"); got != 0 {
		t.Errorf("work jobs = %d, want none while the plan is parked", got)
	}
}
