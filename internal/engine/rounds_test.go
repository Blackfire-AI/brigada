package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// doneOnSession is doneOn with the harness session the run ended on
// (§4.2): the id the next session-mode job on the key resumes.
func doneOnSession(branch, check, session string) Completion {
	c := doneOn(branch, check)
	c.SessionID = session
	return c
}

// fixJob reads the task's queued fix job, failing the test when the
// completion under test did not queue one.
func (e *env) fixJob() int64 {
	e.t.Helper()
	id := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task)
	if id == 0 {
		e.t.Fatalf("no fix job queued; have %v", e.eventKinds())
	}
	return id
}

// authorLease polls as the author's runner does with its harness
// offered: the fix round carries the author agent's harness as its
// placement constraint (§3), so a poll that offers nothing never sees it.
func (e *env) authorLease(author int64) *LeaseGrant {
	e.t.Helper()
	return e.lease(author, []string{"author"}, nil)
}

// runWork leases the task's next author job — the work job or a fix
// round — and completes it as named.
func (e *env) runWork(author int64, c Completion) {
	e.t.Helper()
	g := e.authorLease(author)
	if g == nil {
		e.t.Fatalf("no author job to lease; have %v", e.eventKinds())
	}
	if ans := e.complete(g, c); ans != AnsOK {
		e.t.Fatalf("complete the author job %d: %s", g.JobID, ans)
	}
}

// runReview leases the task's queued review job with runner and
// completes it with the verdict block verbatim.
func (e *env) runReview(runner int64, result string) {
	e.t.Helper()
	rg := e.leaseAny(runner)
	if rg == nil || rg.Kind != KindReview {
		e.t.Fatalf("the review job was not leased: %+v", rg)
	}
	if ans := e.complete(rg, Completion{Result: result}); ans != AnsOK {
		e.t.Fatalf("complete the review job %d: %s", rg.JobID, ans)
	}
}

// changesVerdict is a changes verdict with substantive findings: what
// every changes round below asks for.
var changesVerdict = `{"verdict":"changes","findings":"` + substantiveFindings + `"}`

// TestCheckFailFixRoundRunsAgainAndReviews: a done whose check failed
// queues a fix round on the author's session (§3) — the worker template
// with the spec and the check's own output as the feedback, session
// mode, the author's harness — and the fixed work goes to review like a
// first done would (§7).
func TestCheckFailFixRoundRunsAgainAndReviews(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")

	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, doneOnSession("task/1-1", failCheck, "hs-author-1")); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}
	// The task never left running, and the fix round keeps it there.
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Fatalf("task state behind the failed check = %q, want running", state)
	}
	fix := e.fixJob()
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", fix); mode != string(ModeSession) {
		t.Errorf("fix job mode = %q, want session: the round resumes the author's session", mode)
	}
	if key := e.str("SELECT key FROM jobs WHERE id = ?", fix); key != keyOf(e.task) {
		t.Errorf("fix job key = %q, want the task's own %q", key, keyOf(e.task))
	}
	if harness := e.str("SELECT harness FROM jobs WHERE id = ?", fix); harness != "author" {
		t.Errorf("fix job harness = %q, want the author agent's own %q", harness, "author")
	}
	// The engine composes the fix round's prompt itself (§1.4, §5.3):
	// the worker template, the spec, the check command, and the failed
	// check rendered as the feedback: command, exit code, output tail.
	wantFeedback := "The check \"go test ./...\" exited 3.\nFAIL TestBoom\nthe frobnicator was off"
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", fix),
		prompt.ComposeWorkerPrompt("the spec", "go test ./...", wantFeedback); got != want {
		t.Errorf("fix prompt = %q\nwant the composed\n%q", got, want)
	}
	if p, ok := e.eventPayloadOf(evTaskCheckFailed); !ok || p.Check != "go test ./..." || p.ExitCode != 3 || p.CheckOutput != "FAIL TestBoom\nthe frobnicator was off" {
		t.Errorf("check-failed event = %+v (%t), want the command, exit code and tail", p, ok)
	}
	if p, ok := e.eventPayloadOf(evTaskCheckFailed); !ok || p.Status != "" {
		t.Errorf("check-failed event = %+v (%t), want the tail without a parking status", p, ok)
	}
	if p, ok := e.eventPayloadOf(evTaskFixRound); !ok || p.Reason != roundFix {
		t.Errorf("fix-round event = %+v (%t), want the %s budget named", p, ok, roundFix)
	}

	// The round runs on the author's agent and resumes the author's
	// session: same runner by affinity, same harness, the session id the
	// failed run wrote back.
	fg := e.authorLease(author)
	if fg == nil || fg.JobID != fix {
		t.Fatalf("the author's runner leased %+v, want the fix job %d", fg, fix)
	}
	if fg.Mode != ModeSession || fg.ResumeSessionID != "hs-author-1" {
		t.Errorf("fix grant mode/resume = %s/%q, want session resuming hs-author-1", fg.Mode, fg.ResumeSessionID)
	}
	if agent := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", fix); agent != e.agentOf(author) {
		t.Errorf("fix job agent = %d, want the author's %d", agent, e.agentOf(author))
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state under the fix round = %q, want running", state)
	}

	// A done whose check passes leaves the loop: the review follows (§7).
	reviewer := e.agented("reviewer", "google", "implement", "review")
	if ans := e.complete(fg, doneOnSession("task/1-2", passCheck, "hs-author-2")); ans != AnsOK {
		t.Fatalf("complete the fixed work: %s", ans)
	}
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", e.task)
	if review == 0 {
		t.Fatal("no review job queued behind the fixed work")
	}
	if exclude := e.str("SELECT exclude_family FROM jobs WHERE id = ?", review); exclude != "anthropic" {
		t.Errorf("review job exclude_family = %q, want the author's family", exclude)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state after the approve = %q, want done", state)
	}
}

// TestFixRoundIsPinnedToTheAuthorsAgent: the fix round's placement names
// the author's agent, not only its harness (§3): the harness is not the
// agent — another runner may offer the same one, and the author's own
// runner may hold a second agent of it, family apart — so the round
// waits for the one agent that ran the work. The work here is fresh
// mode and ends without a session id, so nothing but the pin holds the
// round to its author: a runner offering the harness alone must come
// away empty.
func TestFixRoundIsPinnedToTheAuthorsAgent(t *testing.T) {
	e := newEnv(t)
	// One runner, two agents of the same harness and different families:
	// the author's and a rival's, so a pick that stops at the harness
	// has two to choose between.
	twin, err := e.eng.Register(e.ctx, "twin", 0, []AgentAdvert{
		{Adapter: "fake", Harness: "author", Model: "author-1", Family: "anthropic", Account: "anthropic", Classes: []string{"implement"}},
		{Adapter: "fake", Harness: "author", Model: "rival-1", Family: "google", Account: "google", Classes: []string{"implement"}},
	})
	if err != nil {
		t.Fatalf("register the twin-agent runner: %v", err)
	}
	author := e.int("SELECT id FROM agents WHERE runner_id = ? AND model = 'author-1'", twin)
	rival := e.int("SELECT id FROM agents WHERE runner_id = ? AND model = 'rival-1'", twin)
	if author == 0 || rival == 0 {
		t.Fatalf("the twin-agent runner holds agents %d and %d, want two", author, rival)
	}
	// A stranger of a third family, offering the same harness.
	stranger, err := e.eng.Register(e.ctx, "stranger", 0, []AgentAdvert{
		{Adapter: "fake", Harness: "author", Model: "stranger-1", Family: "meta", Account: "meta", Classes: []string{"implement"}},
	})
	if err != nil {
		t.Fatalf("register the stranger: %v", err)
	}

	e.submit(e.work())
	g := e.lease(twin, []string{"author"}, nil)
	if g == nil {
		t.Fatal("the work job was not leased by the author's runner")
	}
	if agent := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", g.JobID); agent != author {
		t.Fatalf("the work ran on agent %d, want the author's %d", agent, author)
	}
	if ans := e.complete(g, doneOn("task/1-1", failCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}

	fix := e.fixJob()
	if pin := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", fix); pin != author {
		t.Errorf("fix job require_agent = %d, want the author's agent %d", pin, author)
	}
	if got := e.lease(stranger, []string{"author"}, nil); got != nil {
		t.Fatalf("the stranger leased job %d: the author's harness is not the author's agent", got.JobID)
	}
	fg := e.lease(twin, []string{"author"}, nil)
	if fg == nil || fg.JobID != fix {
		t.Fatalf("the author's runner leased %+v, want the fix job %d", fg, fix)
	}
	if agent := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", fix); agent != author {
		t.Errorf("fix job agent = %d, want the author's %d, not the rival's %d", agent, author, rival)
	}
}

// TestChangesFixRoundSecondReviewApproves: a changes verdict queues a
// fix round with the findings as the feedback (§3, §7), on the author's
// session — which the review's own session must not have displaced —
// and the review of the fixed work excludes the author's family again.
func TestChangesFixRoundSecondReviewApproves(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, doneOnSession("task/1-1", passCheck, "hs-author-1")); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	findings := "the frobnicator is never initialized; the spec's second clause is unaddressed"
	rg := e.leaseAny(reviewer)
	if ans := e.complete(rg, Completion{
		Result:    `{"verdict":"changes","findings":"` + findings + `"}`,
		SessionID: "hs-reviewer-1",
	}); ans != AnsOK {
		t.Fatalf("complete the changes review: %s", ans)
	}
	// The task sits in reviewing until the fix round's lease takes it
	// back to running (§3, §6.1).
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state behind the changes verdict = %q, want reviewing", state)
	}
	fix := e.fixJob()
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", fix); mode != string(ModeSession) {
		t.Errorf("fix job mode = %q, want session", mode)
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", fix),
		prompt.ComposeWorkerPrompt("the spec", "go test ./...", findings); got != want {
		t.Errorf("fix prompt = %q\nwant the composed\n%q", got, want)
	}
	if p, ok := e.eventPayloadOf(evReviewChanges); !ok || p.Findings != findings {
		t.Errorf("review_changes payload = %+v (%t), want the findings verbatim", p, ok)
	}
	if p, ok := e.eventPayloadOf(evTaskFixRound); !ok || p.Reason != roundReview {
		t.Errorf("fix-round event = %+v (%t), want the %s budget named", p, ok, roundReview)
	}

	// The author — not the reviewer — resumes: the key held the
	// reviewer's session for the review's own run, and the fix round
	// comes back to the author's session and agent (§7, §8).
	fg := e.authorLease(author)
	if fg == nil || fg.JobID != fix {
		t.Fatalf("the author's runner leased %+v, want the fix job %d", fg, fix)
	}
	if fg.ResumeSessionID != "hs-author-1" {
		t.Errorf("fix grant resumes %q, want the author's hs-author-1", fg.ResumeSessionID)
	}
	if agent := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", fix); agent != e.agentOf(author) {
		t.Errorf("fix job agent = %d, want the author's %d", agent, e.agentOf(author))
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state under the fix round = %q, want back to running", state)
	}
	if got := e.leaseAny(reviewer); got != nil {
		t.Fatalf("the reviewer's runner leased job %d behind the author's session", got.JobID)
	}

	if ans := e.complete(fg, doneOnSession("task/1-2", passCheck, "hs-author-2")); ans != AnsOK {
		t.Fatalf("complete the fixed work: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != 2 {
		t.Fatalf("review jobs = %d, want a second one behind the fix round", n)
	}
	// The next review excludes the author's family again (§7): the fix
	// round ran on the author's agent, so the rule reads the same family.
	rg2 := e.leaseAny(reviewer)
	if rg2 == nil {
		t.Fatal("the second review job was not leased")
	}
	// The second review sees the findings that steered the fix round (§7).
	if p := e.str("SELECT prompt FROM jobs WHERE id = ?", rg2.JobID); !strings.Contains(p, "<feedback>") || !strings.Contains(p, findings) {
		t.Errorf("second review prompt lacks the fix round's feedback:\n%s", p)
	}
	if exclude := e.str("SELECT exclude_family FROM jobs WHERE id = ?", rg2.JobID); exclude != "anthropic" {
		t.Errorf("second review exclude_family = %q, want the author's family again", exclude)
	}
	if ans := e.complete(rg2, Completion{Result: `{"verdict":"approve","findings":"` + substantiveFindings + `"}`}); ans != AnsOK {
		t.Fatalf("complete the second review: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state after the second review = %q, want done", state)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != planDone {
		t.Errorf("plan state = %q, want done", state)
	}
}

// TestRoundsExhaustedParksTheTask: when the budget the next fix round
// would spend is used up — the task's caps JSON naming it, or the
// default of two (§7) — the task parks at needs_foreman with a
// task_out_of_rounds event naming which cap ran out (§3).
func TestRoundsExhaustedParksTheTask(t *testing.T) {
	// One feedback cycle: the author job that ends done, and — on the
	// changes path — the review of the passed work asking again. Each
	// cycle's completion queues the fix round the caps must allow.
	for _, tc := range []struct {
		path string // "check" or "changes": what asks for the round
	}{
		{"check"}, {"changes"},
	} {
		t.Run("the default two "+tc.path+" rounds", func(t *testing.T) {
			e := newEnv(t)
			e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", e.task)
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			check, verdict := failCheck, ""
			if tc.path == "changes" {
				check, verdict = passCheck, changesVerdict
			}
			e.submit(e.work())
			e.runWork(author, doneOn("task/1-1", check))
			if verdict != "" {
				e.runReview(reviewer, verdict)
			}
			for i := 0; i < 2; i++ {
				e.runWork(author, doneOn("task/1-1", check))
				if verdict != "" {
					e.runReview(reviewer, verdict)
				}
				if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state == taskNeedsForeman && i == 0 {
					t.Fatalf("the first round parked the task before the budget was spent")
				}
			}
			// The third ask finds the budget spent, whichever path asks:
			// the second round's completion already parked the task, and
			// nothing was queued behind it.
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Fatalf("task state past the rounds = %q, want needs_foreman", state)
			}
			p, ok := e.eventPayloadOf(evTaskOutOfRounds)
			if !ok {
				t.Fatalf("no task_out_of_rounds event; have %v", e.eventKinds())
			}
			want := roundFix
			if tc.path == "changes" {
				want = roundReview
			}
			if p.Status != taskNeedsForeman || !strings.Contains(p.Reason, want) {
				t.Errorf("out-of-rounds payload = %+v, want the %s cap named on the parking", p, want)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 2 {
				t.Errorf("fix jobs = %d, want the two the caps allowed", n)
			}
		})
	}

	// A cap of zero on either budget parks on the first ask.
	for _, tc := range []struct {
		caps  string
		check string
		want  string
	}{
		{`{"fix_rounds":0}`, failCheck, roundFix},
		{`{"review_rounds":0}`, passCheck, roundReview},
	} {
		t.Run("cap of zero: "+tc.want, func(t *testing.T) {
			e := newEnv(t)
			e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", e.task)
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			e.exec("UPDATE tasks SET caps = ? WHERE id = ?", tc.caps, e.task)
			e.submit(e.work())
			e.runWork(author, doneOn("task/1-1", tc.check))
			if tc.check == passCheck {
				e.runReview(reviewer, changesVerdict)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Fatalf("caps %s: task state = %q, want needs_foreman", tc.caps, state)
			}
			p, ok := e.eventPayloadOf(evTaskOutOfRounds)
			if !ok || !strings.Contains(p.Reason, tc.want) {
				t.Errorf("caps %s: out-of-rounds payload = %+v (%t), want the %s cap named", tc.caps, p, ok, tc.want)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 0 {
				t.Errorf("caps %s: fix jobs = %d, want none", tc.caps, n)
			}
		})
	}
}

// TestFixRoundsShareOneCount: the two fix paths draw on one count of
// the task's fix jobs but close only their own budget (§7): a spent
// fix_rounds parks the check path while review_rounds still allows its
// own rounds, and the other way around.
func TestFixRoundsShareOneCount(t *testing.T) {
	fresh := func(t *testing.T, caps string) (*env, int64, int64) {
		e := newEnv(t)
		e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", e.task)
		e.exec("UPDATE tasks SET caps = ? WHERE id = ?", caps, e.task)
		return e, e.agented("author", "anthropic", "implement"), e.agented("reviewer", "google", "implement", "review")
	}
	t.Run("a spent fix_rounds closes the check path alone", func(t *testing.T) {
		e, author, reviewer := fresh(t, `{"fix_rounds":1,"review_rounds":2}`)
		e.submit(e.work())
		// The check's one allowed round…
		e.runWork(author, doneOn("task/1-1", failCheck))
		e.runWork(author, doneOn("task/1-1", failCheck))
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
			t.Fatalf("task state past fix_rounds = %q, want needs_foreman", state)
		}
		if p, _ := e.eventPayloadOf(evTaskOutOfRounds); !strings.Contains(p.Reason, roundFix) {
			t.Errorf("out-of-rounds payload = %+v, want the %s cap named", p, roundFix)
		}
		// …while the same count leaves the changes path its own two: a
		// new task under the same caps runs both review rounds.
		other := e.addTask(`{"fix_rounds":1,"review_rounds":2}`)
		p := e.work()
		p.Key, p.TaskID = fmt.Sprintf("task:%d", other), other
		e.submit(p)
		e.runWork(author, doneOn("task/2-1", passCheck))
		e.runReview(reviewer, changesVerdict)
		e.runWork(author, doneOn("task/2-1", passCheck))
		e.runReview(reviewer, changesVerdict)
		if state := e.str("SELECT state FROM tasks WHERE id = ?", other); state != taskReviewing {
			t.Fatalf("second task state after two review rounds = %q, want reviewing behind a queued fix", state)
		}
		if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", other); n != 2 {
			t.Errorf("fix jobs of the second task = %d, want its two allowed rounds", n)
		}
	})
	t.Run("a spent review_rounds closes the changes path alone", func(t *testing.T) {
		e, author, reviewer := fresh(t, `{"fix_rounds":2,"review_rounds":1}`)
		e.submit(e.work())
		e.runWork(author, doneOn("task/1-1", passCheck))
		e.runReview(reviewer, changesVerdict)
		e.runWork(author, doneOn("task/1-1", passCheck))
		e.runReview(reviewer, changesVerdict)
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
			t.Fatalf("task state past review_rounds = %q, want needs_foreman", state)
		}
		if p, _ := e.eventPayloadOf(evTaskOutOfRounds); !strings.Contains(p.Reason, roundReview) {
			t.Errorf("out-of-rounds payload = %+v, want the %s cap named", p, roundReview)
		}
	})
}

// TestNoVerdictRetriesOnAnotherAgentAndApproves: an unusable verdict —
// none, or an approve without substantive findings — goes to another
// reviewer once (§7): a new review job excluding the author's family
// and the first reviewer's agent, so the retry lands on another agent
// of a non-author family, and its approve finishes the task.
func TestNoVerdictRetriesOnAnotherAgentAndApproves(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "implement", "review")
	second := e.agented("second", "meta", "implement", "review")

	e.submit(e.work())
	e.runWork(author, doneOn("task/1-1", passCheck))
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", e.task)
	rg := e.leaseAny(first)
	if rg == nil || rg.JobID != review {
		t.Fatalf("the first reviewer leased %+v, want the review job %d", rg, review)
	}
	if ans := e.complete(rg, Completion{Result: `{"verdict":"approve","findings":"` + shortFindings + `"}`}); ans != AnsOK {
		t.Fatalf("complete the lazy review: %s", ans)
	}

	retryID := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
	if retryID == review {
		t.Fatal("no retry review job was queued")
	}
	if exclude := e.str("SELECT exclude_family FROM jobs WHERE id = ?", retryID); exclude != "anthropic" {
		t.Errorf("retry exclude_family = %q, want the author's family", exclude)
	}
	if exclude := e.int("SELECT COALESCE(exclude_agent, 0) FROM jobs WHERE id = ?", retryID); exclude != e.agentOf(first) {
		t.Errorf("retry exclude_agent = %d, want the first reviewer's %d", exclude, e.agentOf(first))
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", retryID),
		e.str("SELECT prompt FROM jobs WHERE id = ?", review); got != want {
		t.Errorf("retry prompt differs from the review's own composition:\n%q\nwant\n%q", got, want)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Errorf("task state behind the retry = %q, want reviewing", state)
	}
	if p, ok := e.eventPayloadOf(evReviewRetry); !ok || !strings.Contains(p.Reason, "substantive") {
		t.Errorf("review_retry payload = %+v (%t), want why the first review was unusable", p, ok)
	}
	if _, ok := e.eventPayloadOf(evReviewNoVerdict); ok {
		t.Error("a review_no_verdict event parked a task the retry carries")
	}

	// The first reviewer cannot take its own retry: its one agent is
	// the excluded one, and the author's family stays closed.
	if got := e.leaseAny(first); got != nil {
		t.Fatalf("the first reviewer leased its own retry, job %d", got.JobID)
	}
	if got := e.leaseAny(author); got != nil {
		t.Fatalf("the author's runner leased the retry, job %d", got.JobID)
	}
	rg2 := e.leaseAny(second)
	if rg2 == nil || rg2.JobID != retryID {
		t.Fatalf("the second reviewer leased %+v, want the retry job %d", rg2, retryID)
	}
	if ans := e.complete(rg2, Completion{Result: `{"verdict":"approve","findings":"` + substantiveFindings + `"}`}); ans != AnsOK {
		t.Fatalf("complete the retry: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state after the retry's approve = %q, want done", state)
	}
	// Only the substantive approve counts on the outcome record (§7):
	// the lazy one before it scored nothing.
	attempts := e.int("SELECT attempts FROM outcomes WHERE agent_id = ? AND class = 'review'", e.agentOf(second))
	if attempts != 1 {
		t.Errorf("the retrying agent's outcome attempts = %d, want its one substantive approve", attempts)
	}
	if n := e.int("SELECT COUNT(*) FROM outcomes WHERE class = 'review'"); n != 1 {
		t.Errorf("review outcome rows = %d, want the second reviewer's alone", n)
	}
}

// TestRestartedReviewKeepsItsRetry: a restarted review is the same
// review asked again (§5.6), not the cycle's retry spent (§7): when the
// replacement ends with no verdict, the retry it never had is still
// queued, rather than the task parking as though a second reviewer had
// already spoken.
func TestRestartedReviewKeepsItsRetry(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "implement", "review")
	second := e.agented("second", "meta", "implement", "review")

	e.submit(e.work())
	e.runWork(author, doneOn("task/1-1", passCheck))
	// The review stalls and is restarted (§5.6): the live one is
	// cancelled, a replacement queued in its place.
	e.submit(SubmitParams{
		Key: keyOf(e.task), ProjectID: e.project, TaskID: e.task,
		Kind: KindReview, Mode: ModeFresh, Prompt: "the composed review, again",
		ExcludeFamily: "anthropic", Restart: true,
	})
	rg := e.leaseAny(first)
	if rg == nil || rg.Kind != KindReview || rg.Attempt != 1 {
		t.Fatalf("the restarted review was not leased: %+v", rg)
	}
	if ans := e.complete(rg, Completion{Result: `{}`}); ans != AnsOK {
		t.Fatalf("complete the restarted review: %s", ans)
	}

	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state behind the restarted review's no verdict = %q, want reviewing behind the retry", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != 3 {
		t.Fatalf("review jobs = %d, want the stalled one, its restart and the retry", n)
	}
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
	if exclude := e.int("SELECT COALESCE(exclude_agent, 0) FROM jobs WHERE id = ?", retry); exclude != e.agentOf(first) {
		t.Errorf("retry exclude_agent = %d, want the restarted review's agent %d", exclude, e.agentOf(first))
	}
	if _, ok := e.eventPayloadOf(evReviewNoVerdict); ok {
		t.Error("a review_no_verdict event parked a task the retry carries")
	}
	rg2 := e.leaseAny(second)
	if rg2 == nil || rg2.JobID != retry {
		t.Fatalf("the second reviewer leased %+v, want the retry job %d", rg2, retry)
	}
	if ans := e.complete(rg2, Completion{Result: `{"verdict":"approve","findings":"` + substantiveFindings + `"}`}); ans != AnsOK {
		t.Fatalf("complete the retry: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state after the retry's approve = %q, want done", state)
	}
}

// TestNoVerdictTwiceParksTheTask: the retry is one per review cycle
// (§7) — a second unusable verdict parks the task at needs_foreman with
// the no-verdict event, and nothing is queued behind it. A lone
// reviewer of another family, with no second agent to take the retry,
// parks the task on the first unusable verdict instead.
func TestNoVerdictTwiceParksTheTask(t *testing.T) {
	t.Run("the retry itself ends unusable", func(t *testing.T) {
		e := newEnv(t)
		author := e.agented("author", "anthropic", "implement")
		first := e.agented("first", "google", "implement", "review")
		second := e.agented("second", "meta", "implement", "review")
		e.submit(e.work())
		e.runWork(author, doneOn("task/1-1", passCheck))
		e.runReview(first, `{}`)
		e.runReview(second, `I looked at it, seems fine`)
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
			t.Fatalf("task state after two unusable reviews = %q, want needs_foreman", state)
		}
		p, ok := e.eventPayloadOf(evReviewNoVerdict)
		if !ok {
			t.Fatalf("no review_no_verdict event; have %v", e.eventKinds())
		}
		if p.Status != taskNeedsForeman {
			t.Errorf("no-verdict payload status = %q, want the task's state", p.Status)
		}
		if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != 2 {
			t.Errorf("review jobs = %d, want the first and its retry alone", n)
		}
	})

	t.Run("no agent can take the retry", func(t *testing.T) {
		e := newEnv(t)
		author := e.agented("author", "anthropic", "implement")
		lone := e.agented("lone", "google", "implement", "review")
		e.submit(e.work())
		e.runWork(author, doneOn("task/1-1", passCheck))
		e.runReview(lone, `{}`)
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
			t.Fatalf("task state = %q, want needs_foreman with nobody to take the retry", state)
		}
		p, ok := e.eventPayloadOf(evReviewNoVerdict)
		if !ok || !strings.Contains(p.Reason, "no other agent") {
			t.Errorf("no-verdict payload = %+v (%t), want the missing reviewer named", p, ok)
		}
		if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != 1 {
			t.Errorf("review jobs = %d, want the lone one alone", n)
		}
	})
}

// emptyTailCheck is a failed check that printed nothing: the case the
// bare JSON left nobody able to act on.
const emptyTailCheck = `{"exit_code":1,"timed_out":false,"output_tail":""}`

// TestCheckFailFeedbackNamesAnEmptyTail: a check that printed nothing and
// one the timeout cut short are worded for the worker, not left as JSON.
func TestCheckFailFeedbackNamesAnEmptyTail(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.exec(`UPDATE tasks SET check_cmd = 'test -f DOES_NOT_EXIST' WHERE id = ?`, e.task)

	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, doneOnSession("task/1-1", emptyTailCheck, "hs-author-1")); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}
	fix := e.fixJob()
	want := prompt.ComposeWorkerPrompt("the spec", "test -f DOES_NOT_EXIST",
		"The check \"test -f DOES_NOT_EXIST\" exited 1.\n(the check printed nothing)")
	if got := e.str("SELECT prompt FROM jobs WHERE id = ?", fix); got != want {
		t.Errorf("fix prompt = %q\nwant the composed\n%q", got, want)
	}
	if !strings.Contains(want, "<check-command>\ntest -f DOES_NOT_EXIST\n</check-command>") {
		t.Errorf("the prompt does not carry the check command in its delimiters:\n%s", want)
	}

	fg := e.authorLease(author)
	if fg == nil || fg.JobID != fix {
		t.Fatalf("the author's runner leased %+v, want the fix job %d", fg, fix)
	}
	timedOut := `{"exit_code":-1,"timed_out":true,"output_tail":""}`
	if ans := e.complete(fg, doneOnSession("task/1-1", timedOut, "hs-author-1")); ans != AnsOK {
		t.Fatalf("complete the timed-out fix round: %s", ans)
	}
	fix2 := e.int("SELECT MAX(id) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task)
	if fix2 == fix {
		t.Fatalf("no second fix round queued; have %v", e.eventKinds())
	}
	if got := e.str("SELECT prompt FROM jobs WHERE id = ?", fix2); !strings.Contains(got, "<feedback>\nThe check \"test -f DOES_NOT_EXIST\" exited -1 after the timeout.\n") {
		t.Errorf("second fix prompt = %q, want the timeout worded", got)
	}
}

// TestOutOfRoundsNeedsYouNamesTheCheck: when the fix rounds are spent the
// needs-you reason and the foreman's wake carry the check command and its
// exit code first, then the worker's summary.
func TestOutOfRoundsNeedsYouNamesTheCheck(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE tasks SET effective_tier = 'heavy' WHERE id = ?", e.task)
	author := e.agented("author", "anthropic", "implement")
	e.exec(`UPDATE tasks SET check_cmd = 'test -f DOES_NOT_EXIST', caps = ? WHERE id = ?`, `{"fix_rounds":0}`, e.task)

	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, doneOn("task/1-1", emptyTailCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}
	p, ok := e.eventPayloadOf(evTaskOutOfRounds)
	if !ok || p.Check != "test -f DOES_NOT_EXIST" || p.ExitCode != 1 {
		t.Fatalf("out-of-rounds payload = %+v (%t), want the check command and exit code", p, ok)
	}
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	want := `check "test -f DOES_NOT_EXIST" exited 1 (no output); fix_rounds spent: added the thing`
	var got string
	for _, it := range st.NeedsYou {
		if it.Kind == "task" && it.ID == e.task {
			got = it.Reason
		}
	}
	if got != want {
		t.Errorf("needs-you reason = %q, want %q", got, want)
	}
	raw, _ := json.Marshal(p)
	if text := wakeEventText(evTaskOutOfRounds, string(raw)); text != want {
		t.Errorf("wake text = %q, want %q", text, want)
	}
}

func TestOutOfRoundsBumpsTheTier(t *testing.T) {
	for _, path := range []string{roundFix, roundReview} {
		t.Run(path, func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "review")
			e.exec(`UPDATE tasks SET caps = '{"fix_rounds":0,"review_rounds":0}' WHERE id = ?`, e.task)
			e.submit(e.work())
			if path == roundFix {
				e.runWork(author, doneOn("", failCheck))
			} else {
				e.runWork(author, doneOn("", passCheck))
				e.runReview(reviewer, changesVerdict)
			}
			if got := e.str("SELECT effective_tier FROM tasks WHERE id = ?", e.task); got != "heavy" {
				t.Fatalf("effective tier = %q, want heavy", got)
			}
			if got := e.str("SELECT tier FROM tasks WHERE id = ?", e.task); got != "standard" {
				t.Errorf("foreman's tier changed to %q", got)
			}
			p, ok := e.eventPayloadOf(evTaskTierBumped)
			if !ok || p.TierAsked != "standard" || p.TierServed != "heavy" || !strings.Contains(p.Reason, path) {
				t.Fatalf("bump = %+v (%t)", p, ok)
			}
			if got, _ := checkEventText(evTaskTierBumped, p); got != "task tier standard -> heavy: "+p.Reason {
				t.Errorf("bump text = %q", got)
			}
			if e.taskState(e.task) != taskReady || e.countEvents(evTaskOutOfRounds) != 0 || e.countEvents(evTaskFailed) != 0 {
				t.Fatalf("bumped task parked: state %s, events %v", e.taskState(e.task), e.eventKinds())
			}
			if n := e.int("SELECT COUNT(*) FROM foreman_inbox"); n != 0 {
				t.Errorf("bump created %d inbox rows", n)
			}
			if a, s := e.outcomeOf(e.agentOf(author), "implement"); a != 1 || s != 0 {
				t.Errorf("outcome = %d/%d, want 1/0", a, s)
			}
			job := e.int("SELECT id FROM jobs WHERE task_id = ? AND restart = 1", e.task)
			feedback := e.str("SELECT prompt FROM jobs WHERE id = ?", job)
			if !strings.Contains(feedback, "previous attempt (tier standard) "+path+" spent:") {
				t.Errorf("restart feedback = %q", feedback)
			}
			if path == roundFix && !strings.Contains(feedback, "FAIL TestBoom") {
				t.Errorf("restart lost check tail: %q", feedback)
			}
			if path == roundReview {
				var v verdictBlock
				_ = json.Unmarshal([]byte(changesVerdict), &v)
				if !strings.Contains(feedback, v.Findings) {
					t.Errorf("restart lost findings: %q", feedback)
				}
			}
			g := e.authorLease(author)
			if g == nil || g.JobID != job || g.Branch != fmt.Sprintf("brigada/task-%d-r1", e.task) {
				t.Fatalf("fresh attempt = %+v", g)
			}
		})
	}
}

func TestOutOfRoundsAtHeavyParks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.exec(`UPDATE tasks SET effective_tier = 'heavy', caps = '{"fix_rounds":0}' WHERE id = ?`, e.task)
	e.submit(e.work())
	e.runWork(author, doneOn("", failCheck))
	if e.taskState(e.task) != taskNeedsForeman || e.countEvents(evTaskTierBumped) != 0 || e.countEvents(evTaskOutOfRounds) != 1 {
		t.Fatalf("heavy task = %s, events %v", e.taskState(e.task), e.eventKinds())
	}
	if a, s := e.outcomeOf(e.agentOf(author), "implement"); a != 1 || s != 0 {
		t.Errorf("outcome = %d/%d", a, s)
	}
}

func TestFailedResultBumpsTheTier(t *testing.T) {
	for _, result := range []string{`{"status":"failed","summary":"could not fix the parser"}`, "no JSON block", "{}"} {
		t.Run(result, func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", "anthropic", "implement")
			e.exec("UPDATE tasks SET tier = 'light', effective_tier = 'light' WHERE id = ?", e.task)
			e.submit(e.work())
			e.runWork(author, Completion{Result: result})
			if e.taskState(e.task) != taskReady || e.str("SELECT effective_tier FROM tasks WHERE id = ?", e.task) != "standard" || e.queuedWork(e.task) != 1 {
				t.Fatalf("failure did not bump: state %s, events %v", e.taskState(e.task), e.eventKinds())
			}
			p, ok := e.eventPayloadOf(evTaskTierBumped)
			if !ok || p.TierAsked != "light" || p.TierServed != "standard" {
				t.Fatalf("bump = %+v", p)
			}
			if strings.Contains(result, "parser") && !strings.Contains(e.str("SELECT prompt FROM jobs WHERE task_id = ? AND restart = 1", e.task), "could not fix the parser") {
				t.Error("restart lost worker summary")
			}
			if a, s := e.outcomeOf(e.agentOf(author), "implement"); a != 1 || s != 0 {
				t.Errorf("outcome = %d/%d", a, s)
			}
		})
	}
}

func TestStalledNeverBumps(t *testing.T) {
	for _, c := range []Completion{
		{Result: `{"status":"failed","summary":"stalled"}`, Outcome: OutcomeStalled},
		{Result: `{"status":"failed","summary":"time cap"}`, Outcome: OutcomeTimeCap},
		{Result: `{"status":"stopped","summary":"stopped"}`},
		{Result: `{"status":"needs_help","question":"help"}`},
		{Error: "CLI failed", ErrorKind: "other"},
	} {
		t.Run(c.Result+c.Error, func(t *testing.T) {
			e := newEnv(t)
			e.exec("UPDATE tasks SET effective_tier = 'light' WHERE id = ?", e.task)
			e.submit(e.work())
			e.runWork(e.agented("author", "anthropic", "implement"), c)
			if e.taskState(e.task) != taskNeedsForeman || e.countEvents(evTaskTierBumped) != 0 {
				t.Fatalf("stopped/help/error task = %s, events %v", e.taskState(e.task), e.eventKinds())
			}
		})
	}
}

func TestBumpRespectsTheRestartCap(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			e := newEnv(t)
			e.exec(`UPDATE tasks SET effective_tier = 'light', caps = '{"fix_rounds":0,"restart":0}' WHERE id = ?`, e.task)
			e.submit(e.work())
			c := doneOn("", failCheck)
			if failed {
				c = Completion{Result: `{"status":"failed","summary":"cannot fix"}`}
			}
			e.runWork(e.agented("author", "anthropic", "implement"), c)
			if e.taskState(e.task) != taskNeedsForeman || e.countEvents(evTaskTierBumped) != 0 || e.countEvents(evTaskRestarted) != 0 {
				t.Fatalf("spent restart budget = %s, events %v", e.taskState(e.task), e.eventKinds())
			}
		})
	}
}

func TestSecondBumpReachesHeavyThenParks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.exec(`UPDATE tasks SET tier = 'light', effective_tier = 'light', caps = '{"fix_rounds":1,"restart":3}' WHERE id = ?`, e.task)
	e.submit(e.work())
	for i, tier := range []string{"standard", "heavy", "heavy"} {
		// Each fresh attempt gets its own fix round before exhausting it.
		e.runWork(author, doneOn("", failCheck))
		e.runWork(author, doneOn("", failCheck))
		if got := e.str("SELECT effective_tier FROM tasks WHERE id = ?", e.task); got != tier {
			t.Fatalf("attempt %d tier = %s, want %s", i, got, tier)
		}
		want := taskReady
		if i == 2 {
			want = taskNeedsForeman
		}
		if e.taskState(e.task) != want {
			t.Fatalf("attempt %d state = %s, want %s", i, e.taskState(e.task), want)
		}
	}
	if e.countEvents(evTaskTierBumped) != 2 || e.countEvents(evTaskOutOfRounds) != 1 {
		t.Fatalf("events = %v", e.eventKinds())
	}
	if a, s := e.outcomeOf(e.agentOf(author), "implement"); a != 3 || s != 0 {
		t.Errorf("outcome = %d/%d, want 3/0", a, s)
	}
}
