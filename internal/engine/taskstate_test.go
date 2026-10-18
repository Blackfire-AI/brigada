package engine

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// agented registers a runner advertising exactly one agent — family and
// classes as named — and returns the runner's id. The engine's tests
// have no adapters to run; what matters is the advertisement the
// dispatcher reads (§5.1).
func (e *env) agented(name, family string, classes ...string) int64 {
	e.t.Helper()
	id, err := e.eng.Register(e.ctx, name, 0, []AgentAdvert{{
		Adapter: "fake", Harness: name, Model: family + "-1",
		Family: family, Account: family, Classes: classes,
	}})
	if err != nil {
		e.t.Fatalf("register %s: %v", name, err)
	}
	return id
}

// agentOf reads the runner's one agent id.
func (e *env) agentOf(runner int64) int64 {
	e.t.Helper()
	id := e.int("SELECT id FROM agents WHERE runner_id = ?", runner)
	if id == 0 {
		e.t.Fatalf("runner %d has no agent", runner)
	}
	return id
}

// eventPayloadOf returns the newest event of kind, when there is one.
func (e *env) eventPayloadOf(kind string) (eventPayload, bool) {
	e.t.Helper()
	rows := e.eventRows()
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].kind == kind {
			return rows[i].payload, true
		}
	}
	return eventPayload{}, false
}

// passCheck and failCheck are the runner's check results (§5.5).
const (
	passCheck = `{"exit_code":0,"timed_out":false,"output_tail":"ok\nall tests passed"}`
	failCheck = `{"exit_code":3,"timed_out":false,"output_tail":"FAIL TestBoom\nthe frobnicator was off"}`
)

// substantiveFindings clears an approve's minimum (§7); shortFindings
// stays under it.
var (
	substantiveFindings = strings.Repeat("a finding with its reasons; ", 4) // 116 bytes
	shortFindings       = "looks fine"
)

// doneOn is the standard implement completion: done, a branch, a check.
func doneOn(branch, check string) Completion {
	return Completion{
		Result:      `{"status":"done","summary":"added the thing","branch":"` + branch + `"}`,
		CheckResult: check,
	}
}

// runToReview drives the fixture's task to reviewing the short way —
// work leased by runner, done under a passing check — and returns the
// id of the review job its completion queued (§7).
func (e *env) runToReview(runner int64) int64 {
	e.t.Helper()
	e.submit(e.work())
	g := e.leaseAny(runner)
	if g == nil {
		e.t.Fatal("the work job was not leased")
	}
	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		e.t.Fatalf("complete the work: %s", ans)
	}
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", e.task)
	if review == 0 {
		e.t.Fatal("no review job was queued")
	}
	return review
}

// TestTaskRunsReviewsAndFinishes is the whole happy path: a lease takes
// the task from ready to running; a done with its check passed moves it
// to reviewing and queues the review job — placed on another family
// than the author's (§7); a substantive approve finishes the task, its
// plan with it (§6.2), and counts on the outcome record (§11).
func TestTaskRunsReviewsAndFinishes(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("the author's runner found nothing to lease")
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Fatalf("task state after the lease = %q, want running", state)
	}
	if agent := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", g.JobID); agent != e.agentOf(author) {
		t.Errorf("work job agent = %d, want the author's %d", agent, e.agentOf(author))
	}
	if _, ok := e.eventPayloadOf(evTaskRunning); !ok {
		t.Error("no task_running event")
	}

	if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Fatalf("task state after the done = %q, want reviewing", state)
	}
	if branch := e.str("SELECT branch FROM tasks WHERE id = ?", e.task); branch != branchOf(e.task) {
		t.Errorf("task branch = %q, want the engine's %q, not the result block's name",
			branch, branchOf(e.task))
	}
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", e.task)
	if review == 0 {
		t.Fatal("no review job was queued")
	}
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", review); mode != string(ModeFresh) {
		t.Errorf("review job mode = %q, want fresh", mode)
	}
	if key := e.str("SELECT key FROM jobs WHERE id = ?", review); key != keyOf(e.task) {
		t.Errorf("review job key = %q, want the task's own %q", key, keyOf(e.task))
	}
	if exclude := e.str("SELECT exclude_family FROM jobs WHERE id = ?", review); exclude != "anthropic" {
		t.Errorf("review job exclude_family = %q, want the author's family", exclude)
	}
	// The engine composes the reviewer's prompt itself (§1.4, §7) and
	// stores it on the job: the review template with the spec and the
	// check result verbatim, and the diff command naming the task's base
	// and the result branch — the engine's own name for it (§5.1), never
	// the worker's "task/1-1" the block above carried. The implementer's
	// summary — "added the thing", in the completion above — is none of
	// its inputs either (§7's anti-steering rule).
	promptOf := e.str("SELECT prompt FROM jobs WHERE id = ?", review)
	if want := prompt.ComposeReviewPrompt("the spec", "", passCheck, "main", branchOf(e.task)); promptOf != want {
		t.Errorf("review prompt = %q\nwant the composed\n%q", promptOf, want)
	}
	if strings.Contains(promptOf, "added the thing") {
		t.Errorf("the review prompt carries the implementer's summary (§7):\n%s", promptOf)
	}
	if strings.Contains(promptOf, "task/1-1") {
		t.Errorf("the review prompt carries the worker's branch name (§5.1):\n%s", promptOf)
	}
	// The branch and base stay on the task's own row (§6.1), where the
	// review's grant reads them at lease time.
	if branch := e.str("SELECT branch FROM tasks WHERE id = ?", e.task); branch != branchOf(e.task) {
		t.Errorf("task branch = %q, want the engine's %q", branch, branchOf(e.task))
	}

	// The family rule (§7, §8): the author's runner — its only agent is
	// the author's family — does not get the review.
	if got := e.leaseAny(author); got != nil {
		t.Fatalf("the author's runner leased the review job %d", got.JobID)
	}
	rg := e.leaseAny(reviewer)
	if rg == nil || rg.JobID != review {
		t.Fatalf("the other-family runner leased %+v, want the review job %d", rg, review)
	}
	// The review's grant carries the branch to review and the base ref
	// of the diff (§7) — the engine's branch, not the block's — and no
	// check to run: the task's check already spoke, at the work round
	// this review judges.
	if rg.ReviewBranch != branchOf(e.task) || rg.BaseRef != "main" {
		t.Errorf("review grant branch/base = %q/%q, want %s on main",
			rg.ReviewBranch, rg.BaseRef, branchOf(e.task))
	}
	if rg.CheckCmd != "" {
		t.Errorf("review grant check = %q, want none", rg.CheckCmd)
	}
	if agent := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", review); agent != e.agentOf(reviewer) {
		t.Errorf("review job agent = %d, want the reviewer's %d", agent, e.agentOf(reviewer))
	}
	// The review running does not disturb the task: it stays reviewing
	// (§6.1), not back to running.
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReviewing {
		t.Errorf("task state under the review = %q, want reviewing", state)
	}

	if ans := e.complete(rg, Completion{Result: `{"verdict":"approve","findings":"` + substantiveFindings + `"}`}); ans != AnsOK {
		t.Fatalf("complete the review: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state after the approve = %q, want done", state)
	}
	if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != planDone {
		t.Errorf("plan state after its only task finished = %q, want done", state)
	}
	if p, ok := e.eventPayloadOf(evTaskDone); !ok || p.Status != taskDone {
		t.Errorf("task_done payload = %+v (%t)", p, ok)
	}
	if _, ok := e.eventPayloadOf(evPlanDone); !ok {
		t.Error("no plan_done event")
	}
	// The outcome record counts the reviewer's substantive approve (§7,
	// §11): one attempt, one success, on the review class.
	attempts := e.int("SELECT attempts FROM outcomes WHERE agent_id = ? AND class = 'review'", e.agentOf(reviewer))
	successes := e.int("SELECT successes FROM outcomes WHERE agent_id = ? AND class = 'review'", e.agentOf(reviewer))
	if attempts != 1 || successes != 1 {
		t.Errorf("outcome of the review = %d/%d, want 1/1", attempts, successes)
	}
	if n := e.int("SELECT COUNT(*) FROM outcomes WHERE class = 'review'"); n != 1 {
		t.Errorf("review outcome rows = %d, want the reviewer's alone", n)
	}
}

// outcomeOf reads an agent's attempts and successes on a class; a
// missing row reads 0/0.
func (e *env) outcomeOf(agent int64, class string) (attempts, successes int64) {
	e.t.Helper()
	return e.int("SELECT COALESCE(SUM(attempts), 0) FROM outcomes WHERE agent_id = ? AND class = ?", agent, class),
		e.int("SELECT COALESCE(SUM(successes), 0) FROM outcomes WHERE agent_id = ? AND class = ?", agent, class)
}

// TestApprovedTaskScoresTheFinalAuthor: an approved task counts an
// attempt and a success on the implement class for the agent of the
// last work or fix job — not an earlier job's agent — and the reviewer's
// own row lands beside it without collision (§8, §11).
func TestApprovedTaskScoresTheFinalAuthor(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	earlier := e.agented("earlier", "openai", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")

	e.submit(e.work())
	first := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'work'", e.task)
	e.runWork(author, doneOn("task/1-1", failCheck))
	e.runWork(author, doneOn("task/1-1", passCheck))
	// Fix rounds pin to the author's agent, so the earlier job's agent is
	// set by hand: the final result's author is still the one scored.
	e.exec("UPDATE jobs SET agent_id = ? WHERE id = ?", e.agentOf(earlier), first)
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)

	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Fatalf("task state = %q, want done", state)
	}
	if a, s := e.outcomeOf(e.agentOf(author), "implement"); a != 1 || s != 1 {
		t.Errorf("final author's implement outcome = %d/%d, want 1/1", a, s)
	}
	if a, s := e.outcomeOf(e.agentOf(earlier), "implement"); a != 0 || s != 0 {
		t.Errorf("earlier agent's implement outcome = %d/%d, want none", a, s)
	}
	if a, s := e.outcomeOf(e.agentOf(reviewer), "review"); a != 1 || s != 1 {
		t.Errorf("reviewer's review outcome = %d/%d, want 1/1", a, s)
	}
	if n := e.int("SELECT COUNT(*) FROM outcomes"); n != 2 {
		t.Errorf("outcome rows = %d, want the author's and the reviewer's", n)
	}
}

// TestOutOfRoundsScoresTheAttemptOnly: a task that runs out of rounds
// counts the attempt and no success.
func TestOutOfRoundsScoresTheAttemptOnly(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.exec("UPDATE tasks SET caps = ? WHERE id = ?", `{"fix_rounds":0}`, e.task)
	e.submit(e.work())
	e.runWork(author, doneOn("task/1-1", failCheck))

	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want needs_foreman", state)
	}
	if a, s := e.outcomeOf(e.agentOf(author), "implement"); a != 1 || s != 0 {
		t.Errorf("implement outcome = %d/%d, want 1/0", a, s)
	}
}

// TestTaskWithoutAnAuthorAgentScoresNothing: a work job that ran on an
// agent-less runner has nobody to score.
func TestTaskWithoutAnAuthorAgentScoresNothing(t *testing.T) {
	e := newEnv(t)
	reviewer := e.agented("reviewer", "google", "review")
	e.runToReview(e.runner1)
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)

	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Fatalf("task state = %q, want done", state)
	}
	if n := e.int("SELECT COUNT(*) FROM outcomes WHERE class = 'implement'"); n != 0 {
		t.Errorf("implement outcome rows = %d, want none", n)
	}
}

// TestTerminalTransitionCountsOnce: processing the same end twice, the
// way a later re-read would, adds nothing the second time.
func TestTerminalTransitionCountsOnce(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.submit(e.work())
	e.runWork(author, doneOn("task/1-1", passCheck))
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)

	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", e.task)
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		job, err := loadJobTx(tx, review)
		if err != nil {
			return err
		}
		return e.eng.taskDoneTx(tx, job, e.eng.cfg.Now())
	}); err != nil {
		t.Fatalf("process the terminal transition again: %v", err)
	}
	if a, s := e.outcomeOf(e.agentOf(author), "implement"); a != 1 || s != 1 {
		t.Errorf("implement outcome after a second processing = %d/%d, want 1/1", a, s)
	}
}

// TestReviewJobFamilyRule walks §7's placement: a review job that
// excludes the author's family is invisible to a runner with no agent
// of another family that offers review, and the first qualifying agent
// by id is the one a lease records.
func TestReviewJobFamilyRule(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	review := e.runToReview(author)

	sameFamily := e.agented("same", "anthropic", "implement", "review") // the family, even review-capable
	noReviewClass := e.agented("no-review", "google", "implement")      // another family, wrong classes
	unknownFamily := e.agented("unknown", "", "review")
	if got := e.leaseAny(unknownFamily); got != nil {
		t.Errorf("a runner of unknown family leased job %d", got.JobID)
	}
	if got := e.leaseAny(sameFamily); got != nil {
		t.Errorf("a runner of the author's family leased job %d", got.JobID)
	}
	if got := e.leaseAny(noReviewClass); got != nil {
		t.Errorf("a runner without the review class leased job %d", got.JobID)
	}
	if got := e.leaseAny(e.runner1); got != nil { // the fixture's runner has no agents at all
		t.Errorf("an agent-less runner leased job %d", got.JobID)
	}

	// The mixed runner skips the unknown and author's families and
	// picks its known other-family reviewer.
	mixed, err := e.eng.Register(e.ctx, "mixed", 0, []AgentAdvert{
		{Adapter: "fake", Harness: "mixed-unknown", Model: "default", Family: "", Account: "a", Classes: []string{"review"}},
		{Adapter: "fake", Harness: "mixed-anthropic", Model: "m1", Family: "anthropic", Account: "a", Classes: []string{"review"}},
		{Adapter: "fake", Harness: "mixed-google", Model: "m2", Family: "google", Account: "a", Classes: []string{"implement", "review"}},
	})
	if err != nil {
		t.Fatalf("register the mixed runner: %v", err)
	}
	g := e.leaseAny(mixed)
	if g == nil || g.JobID != review {
		t.Fatalf("the mixed runner leased %+v, want the review job %d", g, review)
	}
	want := e.int("SELECT id FROM agents WHERE runner_id = ? AND harness = 'mixed-google'", mixed)
	if agent := e.int("SELECT COALESCE(agent_id, 0) FROM jobs WHERE id = ?", review); agent != want {
		t.Errorf("review job agent = %d, want the other family's %d", agent, want)
	}
}

func TestUnknownAuthorFamilyReview(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "", "implement")
	review := e.runToReview(author)
	if got := e.str("SELECT exclude_family FROM jobs WHERE id = ?", review); got != "" {
		t.Fatalf("exclude_family = %q, want unknown", got)
	}
	reviewer := e.agented("reviewer", "", "review")
	if got := e.leaseAny(reviewer); got == nil || got.JobID != review {
		t.Errorf("unknown-family reviewer leased %+v, want job %d", got, review)
	}
}

// TestReviewJobWithoutARunnerIsNotAHeldJob: a review job no runner can
// take waits like any queued job (§8) — the family rule is an unmet
// placement, not a cap hold, so nothing is stamped on the job and the
// queue TTL, not a slot, is what ends its wait.
func TestReviewJobWithoutARunnerIsNotAHeldJob(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.QueueTTL = time.Minute })
	author := e.agented("author", "anthropic", "implement", "review")
	review := e.runToReview(author)

	if got := e.leaseAny(author); got != nil {
		t.Fatalf("the only runner leased the review job %d behind its own family", got.JobID)
	}
	if held := e.str("SELECT COALESCE(cap_held_at, '') FROM jobs WHERE id = ?", review); held != "" {
		t.Errorf("the family-blocked review was held at a cap: %q", held)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evJobCapHeld); n != 0 {
		t.Errorf("job_cap_held events = %d, want none for an unmet placement", n)
	}

	e.clock.Advance(2 * time.Minute)
	rep := e.mustSweep()
	if len(rep.QueueTimedOut) != 1 || rep.QueueTimedOut[0] != review {
		t.Fatalf("the sweep's queue timeouts = %v, want the review job %d", rep.QueueTimedOut, review)
	}
}

// TestWorkCompletionParksTheTaskAtNeedsForeman: a worker's question, a
// failed run or a result that never parsed leaves the rules nothing to
// carry forward, so the task parks at needs_foreman with the event
// naming what waited (§3) — and nothing queued behind it. A failed check
// of a class without fix rounds (§7: no review, no rounds) parks the
// same way.
func TestWorkCompletionParksTheTaskAtNeedsForeman(t *testing.T) {
	tests := []struct {
		name    string
		class   string // '' = the fixture's implement task
		version func() Completion
		event   string
		check   func(*testing.T, eventPayload)
	}{
		{
			name:  "a failed check of a class without fix rounds",
			class: "answer",
			event: evTaskCheckFailed,
			version: func() Completion {
				return doneOn("task/1-1", failCheck)
			},
			check: func(t *testing.T, p eventPayload) {
				if !strings.Contains(p.Reason, "FAIL TestBoom") {
					t.Errorf("check-failed payload = %+v, want the check's tail", p)
				}
			},
		},
		{
			name:  "needs_help",
			event: evTaskNeedsHelp,
			version: func() Completion {
				return Completion{Result: `{"status":"needs_help","question":"should I split the parser out?"}`}
			},
			check: func(t *testing.T, p eventPayload) {
				if p.Question != "should I split the parser out?" {
					t.Errorf("needs_help payload = %+v, want the question verbatim", p)
				}
			},
		},
		{
			name:  "failed",
			event: evTaskFailed,
			version: func() Completion {
				return Completion{Result: `{"status":"failed","summary":"the repo would not clone"}`}
			},
			check: func(t *testing.T, p eventPayload) {
				if !strings.Contains(p.Reason, "would not clone") {
					t.Errorf("failed payload = %+v, want the summary", p)
				}
			},
		},
		{
			name:  "an error in place of a result",
			event: evTaskFailed,
			version: func() Completion {
				return Completion{Error: "the CLI crashed"}
			},
			check: func(t *testing.T, p eventPayload) {
				if !strings.Contains(p.Reason, "CLI crashed") {
					t.Errorf("failed payload = %+v, want the error", p)
				}
			},
		},
		{
			name:  "no result block",
			event: evTaskFailed,
			version: func() Completion {
				return Completion{Result: "I am sure I finished, trust me"}
			},
			check: func(t *testing.T, p eventPayload) {
				if p.Reason == "" {
					t.Errorf("failed payload = %+v, want something naming the failure", p)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.class != "" {
				e.exec("UPDATE tasks SET class = ? WHERE id = ?", tc.class, e.task)
			}
			e.submit(e.work())
			g := e.leaseAny(e.runner1)
			if ans := e.complete(g, tc.version()); ans != AnsOK {
				t.Fatalf("complete = %s", ans)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Fatalf("task state = %q, want needs_foreman", state)
			}
			p, ok := e.eventPayloadOf(tc.event)
			if !ok {
				t.Fatalf("no %s event; have %v", tc.event, e.eventKinds())
			}
			if p.Status != taskNeedsForeman {
				t.Errorf("the event's status = %q, want the task's state", p.Status)
			}
			tc.check(t, p)
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 0 {
				t.Errorf("fix jobs = %d, want none behind a parked task", n)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'review'", e.task); n != 0 {
				t.Errorf("review jobs = %d, want none behind a parked task", n)
			}
		})
	}
}

// TestReviewVerdicts walks §7's outcomes: a substantive approve alone
// finishes the task; changes queues a fix round on the author's session
// (§3, below); and every unusable verdict here parks — the reviewer is
// the only agent of another family, so the retry has nobody to go to.
func TestReviewVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		result   string
		state    string
		event    string
		outcomes bool
	}{
		{
			name:     "approve with substantive findings",
			result:   `{"verdict":"approve","findings":"` + substantiveFindings + `"}`,
			state:    taskDone,
			event:    evTaskDone,
			outcomes: true,
		},
		{
			name:   "approve without substantive findings",
			result: `{"verdict":"approve","findings":"` + shortFindings + `"}`,
			state:  taskNeedsForeman,
			event:  evReviewNoVerdict,
		},
		{
			name:   "changes queues a fix round",
			result: `{"verdict":"changes","findings":"` + substantiveFindings + `"}`,
			state:  taskReviewing,
			event:  evReviewChanges,
		},
		{
			name:   "no verdict block",
			result: `{"status":"done","summary":"I looked at it"}`,
			state:  taskNeedsForeman,
			event:  evReviewNoVerdict,
		},
		{
			name:   "an unknown verdict",
			result: `{"verdict":"abstain","findings":"` + substantiveFindings + `"}`,
			state:  taskNeedsForeman,
			event:  evReviewNoVerdict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			reviewer := e.agented("reviewer", "google", "review")
			review := e.runToReview(e.runner1)
			g := e.leaseAny(reviewer)
			if g == nil || g.JobID != review {
				t.Fatalf("the reviewer leased %+v, want the review job %d", g, review)
			}
			if ans := e.complete(g, Completion{Result: tc.result}); ans != AnsOK {
				t.Fatalf("complete the review: %s", ans)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != tc.state {
				t.Fatalf("task state = %q, want %q", state, tc.state)
			}
			if _, ok := e.eventPayloadOf(tc.event); !ok {
				t.Fatalf("no %s event; have %v", tc.event, e.eventKinds())
			}
			// Only the substantive approve counts on the outcome record
			// (§7): everything else is worth nothing yet.
			n := e.int("SELECT COALESCE(SUM(attempts + successes), 0) FROM outcomes")
			if want := boolInt(tc.outcomes) * 2; n != int64(want) {
				t.Errorf("outcome rows carry %d, want %d", n, want)
			}
			if tc.state != taskDone {
				if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != "active" {
					t.Errorf("plan state = %q, want still active with its task parked", state)
				}
			}
		})
	}
	// The changes verdict carries its findings (§7): the fix round's
	// feedback is built on them.
	t.Run("changes carries the findings", func(t *testing.T) {
		e := newEnv(t)
		reviewer := e.agented("reviewer", "google", "review")
		review := e.runToReview(e.runner1)
		g := e.leaseAny(reviewer)
		if g == nil || g.JobID != review {
			t.Fatal("the review job was not leased")
		}
		e.complete(g, Completion{Result: `{"verdict":"changes","findings":"` + substantiveFindings + `"}`})
		if p, ok := e.eventPayloadOf(evReviewChanges); !ok || p.Findings != substantiveFindings {
			t.Errorf("review_changes payload = %+v (%t), want the findings verbatim", p, ok)
		}
	})
}

// TestNonImplementTaskCompletesDone: only implement tasks go through
// review (§7); a work job of any other class that ends done finishes
// its task — and with it, its plan (§6.2). A worker's question still
// parks it (§3): judgment is class-less.
func TestNonImplementTaskCompletesDone(t *testing.T) {
	for _, class := range []string{"review", "answer"} {
		t.Run(class, func(t *testing.T) {
			e := newEnv(t)
			e.exec("UPDATE tasks SET class = ? WHERE id = ?", class, e.task)
			e.submit(e.work())
			g := e.leaseAny(e.runner1)
			if ans := e.complete(g, doneOn("task/1-1", passCheck)); ans != AnsOK {
				t.Fatalf("complete = %s", ans)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
				t.Fatalf("task state = %q, want done without a review round", state)
			}
			if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != planDone {
				t.Errorf("plan state = %q, want done", state)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'review'"); n != 0 {
				t.Errorf("review jobs = %d, want none for a %s task", n, class)
			}
			if _, ok := e.eventPayloadOf(evPlanDone); !ok {
				t.Error("no plan_done event")
			}
		})
	}
	t.Run("needs_help still parks", func(t *testing.T) {
		e := newEnv(t)
		e.exec("UPDATE tasks SET class = 'answer' WHERE id = ?", e.task)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		e.complete(g, Completion{Result: `{"status":"needs_help","question":"sources?"}`})
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
			t.Fatalf("task state = %q, want needs_foreman", state)
		}
	})
}

// TestPlanDoneNeedsEveryTaskDone: a plan closes only when its last open
// task finishes (§6.2) — one task done leaves it active, and the second
// done closes it with exactly one plan_done event.
func TestPlanDoneNeedsEveryTaskDone(t *testing.T) {
	e := newEnv(t)
	second := e.addTask("{}")
	e.exec("UPDATE tasks SET class = 'answer' WHERE id IN (?, ?)", e.task, second)

	for i, task := range []int64{e.task, second} {
		p := e.work()
		p.Key, p.TaskID = keyOf(task), task
		e.submit(p)
		g := e.leaseAny(e.runner1)
		if ans := e.complete(g, done()); ans != AnsOK {
			t.Fatalf("complete task %d: %s", task, ans)
		}
		want := "active"
		if i == 1 {
			want = planDone
		}
		if state := e.str("SELECT state FROM plans WHERE id = ?", e.plan); state != want {
			t.Fatalf("plan state after %d of 2 tasks = %q, want %q", i+1, state, want)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evPlanDone); n != 1 {
		t.Errorf("plan_done events = %d, want exactly one", n)
	}
}
