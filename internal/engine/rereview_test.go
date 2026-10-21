package engine

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// headResult is a done result block as the runner delivers it: the summary
// and the head it pushed.
func headResult(branch, summary, head string) Completion {
	return Completion{
		Result:      `{"status":"done","summary":"` + summary + `","branch":"` + branch + `","head":"` + head + `"}`,
		CheckResult: passCheck,
	}
}

// reReviewFixture is the task after one changes verdict and the fix round
// that answered it: the author and the reviewer on separate runners, the
// reviewer's session id on its review job, the fix round done at head h2.
type reReviewFixture struct {
	e                *env
	author, reviewer int64
	review           int64 // the changes review
}

const (
	authorSession   = "hs-author-1"
	reviewerSession = "hs-reviewer-1"
)

// changesRound drives the task to a changes verdict and stops with the fix
// round queued, not yet run. Pass adjust to change the reviewer's
// completion.
func changesRound(t *testing.T, review func(*Completion)) *reReviewFixture {
	t.Helper()
	e := newEnv(t)
	f := &reReviewFixture{e: e}
	f.author = e.agented("author", "anthropic", "implement")
	f.reviewer = e.agented("reviewer", "google", "review")
	e.submit(e.work())
	g := e.authorLease(f.author)
	first := headResult("task/1-1", "added the thing", "h1")
	first.SessionID = authorSession
	if ans := e.complete(g, first); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	rg := e.leaseAny(f.reviewer)
	if rg == nil || rg.Kind != KindReview || rg.Mode != ModeFresh {
		t.Fatalf("first review grant = %+v, want a fresh review", rg)
	}
	f.review = rg.JobID
	c := Completion{Result: `{"verdict":"changes","findings":"the loop leaks"}`, SessionID: reviewerSession}
	if review != nil {
		review(&c)
	}
	if ans := e.complete(rg, c); ans != AnsOK {
		t.Fatalf("complete the review: %s", ans)
	}
	return f
}

// reviewerLease is the reviewer's runner polling with its harness offered,
// as the pinned re-review needs.
func (f *reReviewFixture) reviewerLease() *LeaseGrant {
	f.e.t.Helper()
	return f.e.lease(f.reviewer, []string{"reviewer"}, nil)
}

// fixDone runs the queued fix round and completes it at head.
func (f *reReviewFixture) fixDone(head string) {
	f.e.t.Helper()
	g := f.e.authorLease(f.author)
	if g == nil || g.Kind != KindFix {
		f.e.t.Fatalf("author leased %+v, want the fix round", g)
	}
	c := headResult("task/1-1", "closed the leak", head)
	c.SessionID = authorSession
	if ans := f.e.complete(g, c); ans != AnsOK {
		f.e.t.Fatalf("complete the fix round: %s", ans)
	}
}

// secondReview is the review job queued after the fix round.
func (f *reReviewFixture) secondReview() *jobRow {
	f.e.t.Helper()
	var j *jobRow
	id := f.e.int("SELECT COALESCE(MAX(id), 0) FROM jobs WHERE task_id = ? AND kind = 'review'", f.e.task)
	err := f.e.st.View(f.e.ctx, func(tx *sql.Tx) error {
		var err error
		j, err = loadJobTx(tx, id)
		return err
	})
	if err != nil || j.ID == f.review {
		f.e.t.Fatalf("no second review job queued (%v)", err)
	}
	return j
}

func (f *reReviewFixture) wantFresh(why string) {
	f.e.t.Helper()
	j := f.secondReview()
	if j.Mode != ModeFresh || j.RequireAgent != 0 {
		f.e.t.Errorf("review mode/require = %s/%d, want fresh with no pin", j.Mode, j.RequireAgent)
	}
	if strings.Contains(j.Prompt, "re-review") || !strings.Contains(j.Prompt, "<task-spec>") {
		f.e.t.Errorf("fresh review prompt is not the full review template:\n%.200s", j.Prompt)
	}
	if p, ok := f.e.eventPayloadOf(evTaskReviewing); !ok || p.Pick != "" || (why != "" && !strings.Contains(p.Reason, why)) {
		f.e.t.Errorf("task_reviewing = %+v (%t), want no resumed pick and a reason with %q", p, ok, why)
	}
}

// TestReReviewResumesTheReviewersSession: a changes verdict, then a fix
// round, queues the next review in session mode on the agent that gave
// the verdict. It leases on the reviewer's own runner — not the author's,
// whose runner holds the key's session — resumes the reviewer's session
// and is shown the fix round alone, and the author's session stays the
// key's, so the next fix round still resumes it.
func TestReReviewResumesTheReviewersSession(t *testing.T) {
	f := changesRound(t, nil)
	e := f.e
	f.fixDone("h2")

	j := f.secondReview()
	if j.Mode != ModeSession || j.RequireAgent != e.agentOf(f.reviewer) || j.Harness != "reviewer" {
		t.Fatalf("re-review mode/require/harness = %s/%d/%q, want session pinned to the reviewer", j.Mode, j.RequireAgent, j.Harness)
	}
	if j.ExcludeFamily != "anthropic" {
		t.Errorf("re-review excludes family %q, want the author's", j.ExcludeFamily)
	}
	var spec, base string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) (err error) {
		spec, base, err = taskSpecAndBaseTx(tx, e.task)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := prompt.ComposeReReviewPrompt("closed the leak", passCheck, base, e.str("SELECT branch FROM tasks WHERE id = ?", e.task), "h1"); j.Prompt != want {
		t.Errorf("re-review prompt =\n%s\nwant\n%s", j.Prompt, want)
	}
	if strings.Contains(j.Prompt, spec) && spec != "" {
		t.Errorf("the re-review repeats the spec; the reviewer's session holds it")
	}
	if p, ok := e.eventPayloadOf(evTaskReviewing); !ok || p.Pick != pickResumed || p.Agent != e.agentOf(f.reviewer) ||
		!strings.HasPrefix(p.Reason, "resumed review:") {
		t.Errorf("task_reviewing = %+v (%t), want the resumed pick on the reviewer", p, ok)
	}

	// The author's runner never sees it, and the other family's does.
	if g := e.authorLease(f.author); g != nil {
		t.Fatalf("the author's runner leased %+v", g)
	}
	g := f.reviewerLease()
	if g == nil || g.JobID != j.ID {
		t.Fatalf("the reviewer's runner leased %+v, want the re-review %d", g, j.ID)
	}
	if g.Mode != ModeSession || g.ResumeSessionID != reviewerSession || g.ReviewSince != "h1" || g.ReviewBranch != e.str("SELECT branch FROM tasks WHERE id = ?", e.task) {
		t.Errorf("re-review grant mode/resume/since/branch = %s/%q/%q/%q", g.Mode, g.ResumeSessionID, g.ReviewSince, g.ReviewBranch)
	}
	if p, ok := e.eventPayloadOf(evJobAgentPicked); !ok || p.Pick != pickResumed || p.Agent != e.agentOf(f.reviewer) {
		t.Errorf("job_agent_picked = %+v (%t), want pick %q on the reviewer", p, ok, pickResumed)
	}
	// The reviewer's session is not the key's: the row still names the
	// author's session and runner.
	if sid := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", j.Key); sid != authorSession {
		t.Errorf("key session = %q, want the author's %q", sid, authorSession)
	}
	if runner := e.int("SELECT runner_id FROM sessions WHERE key = ?", j.Key); runner != f.author {
		t.Errorf("key session runner = %d, want the author's %d", runner, f.author)
	}

	// A second changes verdict from the resumed session: the fix round
	// resumes the author's session, and its re-review the reviewer's.
	rc := Completion{Result: `{"verdict":"changes","findings":"still leaks"}`, SessionID: "hs-reviewer-2"}
	if ans := e.complete(g, rc); ans != AnsOK {
		t.Fatalf("complete the re-review: %s", ans)
	}
	fg := e.authorLease(f.author)
	if fg == nil || fg.Kind != KindFix || fg.ResumeSessionID != authorSession {
		t.Fatalf("second fix grant = %+v, want the author's session %q", fg, authorSession)
	}
	if ans := e.complete(fg, headResult("task/1-1", "really closed it", "h3")); ans != AnsOK {
		t.Fatalf("complete the second fix round: %s", ans)
	}
	g3 := f.reviewerLease()
	if g3 == nil || g3.Kind != KindReview || g3.ResumeSessionID != "hs-reviewer-2" || g3.ReviewSince != "h2" {
		t.Errorf("second re-review grant = %+v, want session hs-reviewer-2 since h2", g3)
	}
}

// TestReReviewVerdictIsTheSameContract: an approve from the resumed
// session finishes the task like any other.
func TestReReviewVerdictIsTheSameContract(t *testing.T) {
	f := changesRound(t, nil)
	f.fixDone("h2")
	g := f.reviewerLease()
	if ans := f.e.complete(g, Completion{Result: `{"verdict":"approve","findings":"` + substantiveFindings + `"}`}); ans != AnsOK {
		t.Fatal(ans)
	}
	if state := f.e.str("SELECT state FROM tasks WHERE id = ?", f.e.task); state != taskDone {
		t.Errorf("task state = %s, want done", state)
	}
}

// TestFirstReviewStaysFresh: no verdict stands before a first
// review, so nothing resumes (and nothing is said about it).
func TestFirstReviewStaysFresh(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.agented("reviewer", "google", "review")
	review := e.runToReview(author)
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", review); mode != string(ModeFresh) {
		t.Errorf("first review mode = %s, want fresh", mode)
	}
	if p, _ := e.eventPayloadOf(evTaskReviewing); p.Pick != "" || p.Reason != "" {
		t.Errorf("task_reviewing = %+v, want no resume talk", p)
	}
}

// TestReReviewIsFreshAfterARestart: a restart starts a new attempt, whose
// first review is fresh on any qualifying reviewer.
func TestReReviewIsFreshAfterARestart(t *testing.T) {
	f := changesRound(t, nil)
	e := f.e
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := e.eng.queueTaskRestartTx(tx, e.project, e.task, "start over", 0, time.Time{}, e.clock.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	g := e.authorLease(f.author)
	if g == nil || g.Kind != KindWork {
		t.Fatalf("author leased %+v, want the restarted work", g)
	}
	if ans := e.complete(g, headResult("task/1-2", "redid it", "h9")); ans != AnsOK {
		t.Fatal(ans)
	}
	f.wantFresh("")
}

// TestReReviewIsFreshWhenTheReviewerCannotBeHad covers each way the
// previous reviewer is out: the review is fresh and the event says why.
func TestReReviewIsFreshWhenTheReviewerCannotBeHad(t *testing.T) {
	cases := []struct {
		name   string
		review func(*Completion)
		break_ func(f *reReviewFixture)
		why    string
	}{
		{name: "session unknown", review: func(c *Completion) { c.SessionID = "" }, why: "session id is unknown"},
		{name: "agent broken", break_: func(f *reReviewFixture) {
			f.e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", f.e.agentOf(f.reviewer))
		}, why: "is broken"},
		{name: "cooling beyond longCooling", break_: func(f *reReviewFixture) {
			f.e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(f.e.clock.Now().Add(longCooling+time.Minute)), f.e.agentOf(f.reviewer))
		}, why: "is cooling until"},
		{name: "agent gone", break_: func(f *reReviewFixture) {
			f.e.exec("DELETE FROM agents WHERE id = ?", f.e.agentOf(f.reviewer))
		}, why: "agent is gone"},
		{name: "head unknown", break_: func(f *reReviewFixture) {
			f.e.exec(`UPDATE jobs SET result = REPLACE(result, 'head', 'tip') WHERE task_id = ? AND kind = 'work'`, f.e.task)
		}, why: "head the previous review judged is unknown"},
		{name: "reviewer no longer offers review", break_: func(f *reReviewFixture) {
			f.e.exec(`UPDATE agents SET classes = '["implement"]' WHERE id = ?`, f.e.agentOf(f.reviewer))
		}, why: "no longer qualifies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := changesRound(t, tc.review)
			if tc.break_ != nil {
				tc.break_(f)
			}
			// A second reviewer of a third family can take the fresh one.
			f.e.agented("other", "moonshot", "review")
			f.fixDone("h2")
			f.wantFresh(tc.why)
		})
	}
}

// TestReReviewResumesThroughAShortCooling: a cooling within longCooling
// still counts as ready (§8), and the re-review waits it out.
func TestReReviewResumesThroughAShortCooling(t *testing.T) {
	f := changesRound(t, nil)
	f.e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(f.e.clock.Now().Add(30*time.Minute)), f.e.agentOf(f.reviewer))
	f.fixDone("h2")
	if j := f.secondReview(); j.Mode != ModeSession {
		t.Fatalf("review mode = %s, want session through a short cooling", j.Mode)
	}
	if g := f.reviewerLease(); g != nil {
		t.Fatalf("leased %+v while the reviewer cools", g)
	}
	f.e.clock.Advance(time.Hour)
	if g := f.reviewerLease(); g == nil || g.Mode != ModeSession {
		t.Errorf("grant after the cooling = %+v, want the re-review", g)
	}
}

// TestReReviewFamilyRuleStillApplies: a fix round whose author is now of
// the reviewer's family cannot be re-reviewed by it.
func TestReReviewFamilyRuleStillApplies(t *testing.T) {
	f := changesRound(t, nil)
	f.e.exec("UPDATE agents SET family = 'google' WHERE id = ?", f.e.agentOf(f.author))
	f.e.agented("other", "moonshot", "review")
	f.fixDone("h2")
	f.wantFresh("no longer qualifies")
}

// TestReReviewRetryStaysFresh: an unusable answer from the resumed
// reviewer is retried on another agent, in a fresh session and with the
// full review prompt, not the re-review's.
func TestReReviewRetryStaysFresh(t *testing.T) {
	f := changesRound(t, nil)
	e := f.e
	other := e.agented("other", "moonshot", "review")
	f.fixDone("h2")
	re := f.secondReview()
	g := f.reviewerLease()
	if g == nil || g.JobID != re.ID {
		t.Fatalf("leased %+v, want the re-review", g)
	}
	if ans := e.complete(g, Completion{Result: "{}"}); ans != AnsOK {
		t.Fatal(ans)
	}
	retry := f.secondReview()
	if retry.ID == re.ID {
		t.Fatal("no retry queued")
	}
	if retry.Mode != ModeFresh || retry.RequireAgent != 0 || retry.ExcludeAgent != e.agentOf(f.reviewer) {
		t.Errorf("retry mode/require/exclude = %s/%d/%d, want fresh, unpinned, off the first reviewer", retry.Mode, retry.RequireAgent, retry.ExcludeAgent)
	}
	if strings.Contains(retry.Prompt, "re-review") || !strings.Contains(retry.Prompt, "<task-spec>") || !strings.Contains(retry.Prompt, "git diff main...brigada/task-1") {
		t.Errorf("retry prompt is not the full review prompt:\n%s", retry.Prompt)
	}
	// The feedback the author was given for the round rides along, as for
	// any first review of a fix round.
	if !strings.Contains(retry.Prompt, "the loop leaks") {
		t.Errorf("retry prompt lacks the round's feedback:\n%s", retry.Prompt)
	}
	rg := e.leaseAny(other)
	if rg == nil || rg.JobID != retry.ID || rg.Mode != ModeFresh || rg.ResumeSessionID != "" || rg.ReviewSince != "" {
		t.Errorf("retry grant = %+v, want a fresh review with nothing to resume", rg)
	}
}

// TestReReviewThatCannotResumeGoesFreshWithoutSpendingTheRetry: a session
// the CLI no longer holds says nothing about the change. The review is
// requeued fresh, and the cycle's one retry for an unusable answer is
// still there for the fresh review.
func TestReReviewThatCannotResumeGoesFreshWithoutSpendingTheRetry(t *testing.T) {
	f := changesRound(t, nil)
	e := f.e
	other := e.agented("other", "moonshot", "review")
	f.fixDone("h2")
	g := f.reviewerLease()
	e.complete(g, Completion{Error: "reviewer CLI failed (resume_failed): no conversation found", ErrorKind: "resume_failed"})
	again := f.secondReview()
	if again.Mode != ModeFresh || again.RequireAgent != 0 || strings.Contains(again.Prompt, "re-review") {
		t.Fatalf("requeued review mode/require = %s/%d, prompt %.120s; want a fresh full review", again.Mode, again.RequireAgent, again.Prompt)
	}
	if p, _ := e.eventPayloadOf(evReviewRetry); !strings.Contains(p.Reason, "re-review could not resume") {
		t.Errorf("review_retry reason = %q", p.Reason)
	}
	if e.spentRetry(g.JobID) {
		t.Error("the failed resume spent the cycle's retry")
	}
	rg := e.leaseAny(other)
	if rg == nil || rg.JobID != again.ID {
		t.Fatalf("leased %+v, want the requeued fresh review", rg)
	}
	e.complete(rg, Completion{Result: "{}"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Errorf("task state = %s, want reviewing with the retry queued", state)
	}
}

// TestReReviewRequeueStaysFresh: a re-review whose reviewer hit its limit
// is requeued as a fresh review for whoever qualifies.
func TestReReviewRequeueStaysFresh(t *testing.T) {
	f := changesRound(t, nil)
	e := f.e
	other := e.agented("other", "moonshot", "review")
	f.fixDone("h2")
	g := f.reviewerLease()
	e.complete(g, Completion{Error: "usage limit", ErrorKind: "limit"})
	again := f.secondReview()
	if again.Mode != ModeFresh || again.RequireAgent != 0 || strings.Contains(again.Prompt, "re-review") {
		t.Errorf("requeued review mode/require = %s/%d, prompt %.120s; want fresh and the full review", again.Mode, again.RequireAgent, again.Prompt)
	}
	if rg := e.leaseAny(other); rg == nil || rg.JobID != again.ID {
		t.Errorf("leased %+v, want the requeued review", rg)
	}
}
