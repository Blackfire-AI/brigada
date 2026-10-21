package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

func TestTransientWorkFailureRestartsOnAnotherAgent(t *testing.T) {
	for _, kind := range []string{"limit", "auth", "other"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t) // omitted config means automatic retry is on
			author := e.agented("author", "same-family", "implement")
			other := e.agented("other", "same-family", "implement")
			otherAgent := e.agentOf(other)
			// The family may match, but a limit must not cool both accounts.
			e.exec("UPDATE agents SET account = 'other-account' WHERE id = ?", otherAgent)
			e.submit(e.work())
			g := e.leaseAny(author)
			c := Completion{Error: "worker CLI failed: temporarily unavailable", ErrorKind: kind, SessionID: "old-session"}
			if ans := e.complete(g, c); ans != AnsOK {
				t.Fatalf("complete = %s, want ok", ans)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
				t.Fatalf("task state = %s, want ready", state)
			}
			retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND restart = 1", e.task)
			if agent := e.int("SELECT require_agent FROM jobs WHERE id = ?", retry); agent != otherAgent {
				t.Fatalf("retry pin = %d, want other agent %d", agent, otherAgent)
			}
			reason := kind + ": " + c.Error
			if p, ok := e.eventPayloadOf(evTaskRestarted); !ok || p.Reason != reason || p.Agent != otherAgent || p.Run != 2 {
				t.Errorf("restart event = %+v (%t), want reason, other agent and run 2", p, ok)
			}
			if got := e.str("SELECT prompt FROM jobs WHERE id = ?", retry); got != prompt.ComposeWorkerPrompt("the spec", "go test ./...", reason) {
				t.Errorf("restart prompt = %q, want failure context in worker feedback", got)
			}
			if n := e.countEvents(evTaskFailed); n != 0 {
				t.Errorf("task_failed events = %d, want coalesced into the restart", n)
			}
			if n := e.int("SELECT COUNT(*) FROM foreman_inbox WHERE project_id = ?", e.project); n != 1 {
				t.Errorf("inbox rows = %d, want the single restart judgment", n)
			}
			if got := e.str("SELECT ev.kind FROM foreman_inbox fi JOIN events ev ON ev.id = fi.event_id WHERE fi.project_id = ?", e.project); got != evTaskRestarted {
				t.Errorf("inbox event = %s, want task_restarted", got)
			}
			if n := e.countEvents(evForemanWoken); n != 1 {
				t.Errorf("foreman wakes = %d, want one", n)
			}
			if ans := e.complete(g, c); ans != AnsAlready {
				t.Errorf("repeat completion = %s, want already", ans)
			}
			if n := e.countEvents(evTaskRestarted); n != 1 {
				t.Errorf("restart events = %d, want one after duplicate completion", n)
			}
			// Even a healthy first agent cannot take the restart.
			if got := e.leaseAny(author); got != nil {
				t.Fatalf("failed agent leased %+v, want nothing", got)
			}
			fresh := e.leaseAny(other)
			if fresh == nil || fresh.JobID != retry {
				t.Fatalf("other agent leased %+v, want retry %d", fresh, retry)
			}
			if fresh.Mode != ModeFresh || fresh.ResumeSessionID != "" || fresh.Branch != fmt.Sprintf("brigada/task-%d-r1", e.task) {
				t.Errorf("retry grant = %+v, want fresh session and -r1 branch", fresh)
			}
			if picks := e.pickEvents(retry); len(picks) != 1 || picks[0].Agent != otherAgent {
				t.Errorf("dispatcher picks = %+v, want the other agent's pick", picks)
			}
			if kind == "auth" {
				if health, why, _ := e.agentState(e.agentOf(author)); health != quotaBroken || why != brokenAuth {
					t.Errorf("failed agent health = %s (%s), want broken auth", health, why)
				}
			}
			if kind == "limit" {
				if _, _, cooling := e.agentState(e.agentOf(author)); cooling == "" {
					t.Error("limit did not cool the failed agent")
				}
			}
		})
	}
}

func TestTransientWorkFailureParksWithoutEligibleOtherAgent(t *testing.T) {
	for _, scenario := range []string{"alone", "broken", "cooling", "wrong class", "shared cooling account", "config off", "restart cap zero", "restart already spent"} {
		t.Run(scenario, func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) {
				if scenario == "config off" {
					off := false
					c.TaskAutoRetry = &off
				}
			})
			author := e.agented("author", "family", "implement")
			if scenario != "alone" {
				classes := []string{"implement"}
				if scenario == "wrong class" {
					classes = []string{"review"}
				}
				other := e.agented("other", "family", classes...)
				id := e.agentOf(other)
				switch scenario {
				case "broken":
					e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", id)
				case "cooling":
					e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(e.clock.Now().Add(time.Hour)), id)
				}
			}
			if scenario == "restart cap zero" {
				e.exec(`UPDATE tasks SET caps = '{"restart":0}' WHERE id = ?`, e.task)
			}
			p := e.work()
			p.Restart = scenario == "restart already spent"
			e.submit(p)
			g := e.leaseAny(author)
			kind := "other"
			if scenario == "shared cooling account" {
				kind = "limit"
			}
			e.complete(g, Completion{Error: "worker CLI failed", ErrorKind: kind})
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Errorf("task state = %s, want needs_foreman", state)
			}
			if n := e.countEvents(evTaskRestarted); n != 0 {
				t.Errorf("restart events = %d, want none", n)
			}
			if n := e.countEvents(evTaskFailed); n != 1 {
				t.Errorf("failure events = %d, want one", n)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", e.task); n != 1 {
				t.Errorf("task jobs = %d, want only the failed work", n)
			}
		})
	}
}

func TestSecondTransientWorkFailureParks(t *testing.T) {
	e := newEnv(t)
	first := e.agented("first", "a", "implement")
	second := e.agented("second", "b", "implement")
	e.agented("third", "c", "implement") // another healthy agent cannot buy a second retry
	e.submit(e.work())
	e.complete(e.leaseAny(first), Completion{Error: "CLI crashed", ErrorKind: "other"})
	retry := e.leaseAny(second)
	if retry == nil || !strings.HasSuffix(retry.Branch, "-r1") {
		t.Fatalf("retry = %+v, want the first restart", retry)
	}
	e.complete(retry, Completion{Error: "CLI crashed again", ErrorKind: "other"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %s, want needs_foreman", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 1 {
		t.Errorf("restart count = %d, want one", n)
	}
	if p, ok := e.eventPayloadOf(evTaskFailed); !ok || p.Reason != "CLI crashed again" {
		t.Errorf("park event = %+v (%t), want second failure", p, ok)
	}
}

func TestNonTransientWorkFailureDoesNotRestart(t *testing.T) {
	for name, completion := range map[string]Completion{
		"check failed":       doneOnSession("", failCheck, "author-session"),
		"bad result":         {Result: "not JSON"},
		"worker failed":      {Result: `{"status":"failed","summary":"cannot do the task"}`},
		"needs help":         {Result: `{"status":"needs_help","question":"which approach?"}`},
		"unclassified error": {Error: "push failed"},
		"stopped":            {Result: `{"status":"stalled","reason":"no progress"}`, Outcome: OutcomeStalled},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			// Heavy tasks retain the park path; tier escalation has its own coverage.
			e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE id = ?", e.task)
			author := e.agented("author", "a", "implement")
			e.agented("other", "b", "implement")
			e.submit(e.work())
			e.complete(e.leaseAny(author), completion)
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 0 {
				t.Errorf("restarts = %d, want none", n)
			}
			if name == "check failed" {
				fix := e.authorLease(author)
				if fix == nil || fix.Kind != KindFix || fix.ResumeSessionID != "author-session" {
					t.Fatalf("fix grant = %+v, want fix round on author's session", fix)
				}
				// A CLI failure in a fix round is a fix retry (§3), not a
				// work restart: the task stays running behind the retry.
				e.complete(fix, Completion{Error: "CLI crashed", ErrorKind: "other"})
				if n := e.countEvents(evTaskRestarted); n != 0 {
					t.Errorf("fix failure restarted the task: %d events", n)
				}
				if n := e.countEvents(evTaskFixRetry); n != 1 {
					t.Errorf("fix retries = %d, want one", n)
				}
				if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
					t.Errorf("task state = %s, want running behind the fix retry", state)
				}
				return
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Errorf("task state = %s, want needs_foreman", state)
			}
		})
	}
}

func TestRunnerLossRetriesWorkOnce(t *testing.T) {
	for _, otherAgent := range []bool{false, true} {
		t.Run(fmt.Sprintf("other-agent-%t", otherAgent), func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) { c.MaxAttempts = 1 })
			author := e.agented("author", "a", "implement")
			other := e.runner2
			if otherAgent {
				other = e.agented("other", "a", "implement")
			}
			e.submit(e.work())
			e.leaseAny(author)
			e.expireLease()
			e.leaseAny(other) // terminal runner loss is recognized at lease time
			wantState := taskNeedsForeman
			if otherAgent {
				wantState = taskReady
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != wantState {
				t.Fatalf("task state = %s, want %s", state, wantState)
			}
			if otherAgent {
				fresh := e.leaseAny(other)
				if fresh == nil || !strings.HasSuffix(fresh.Branch, "-r1") || fresh.Mode != ModeFresh {
					t.Fatalf("runner-loss retry = %+v, want fresh restart", fresh)
				}
				if p, ok := e.eventPayloadOf(evTaskRestarted); !ok || p.Reason != "runner_lost: runner lost 1 times" || p.Agent != e.agentOf(other) {
					t.Errorf("restart event = %+v (%t), want runner-loss reason and other agent", p, ok)
				}
				if n := e.countEvents(evTaskFailed); n != 0 {
					t.Errorf("task_failed events = %d, want coalesced restart", n)
				}
				e.expireLease()
				e.leaseAny(author)
				if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
					t.Errorf("second runner loss left state %s, want needs_foreman", state)
				}
			}
		})
	}
}

func TestAutomaticRestartReasonIsBounded(t *testing.T) {
	e := newEnv(t)
	first := e.agented("first", "a", "implement")
	e.agented("second", "b", "implement")
	e.submit(e.work())
	e.complete(e.leaseAny(first), Completion{Error: strings.Repeat("故障", 200), ErrorKind: "other"})
	p, ok := e.eventPayloadOf(evTaskRestarted)
	if !ok || len(p.Reason) > 500 || !utf8.ValidString(p.Reason) || !strings.HasPrefix(p.Reason, "other: ") {
		t.Errorf("reason = %q (%t), want bounded UTF-8 with failure kind", p.Reason, ok)
	}
}

func TestAutomaticRestartUsesTheTaskClass(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE tasks SET class = 'research' WHERE id = ?", e.task)
	author := e.agented("author", "same-family", "implement")
	implementer := e.agented("implementer", "same-family", "implement")
	other := e.agented("researcher", "same-family", "research")
	e.submit(e.work())
	e.complete(e.leaseAny(author), Completion{Error: "CLI crashed", ErrorKind: "other"})
	if got := e.leaseAny(implementer); got != nil {
		t.Fatalf("implement-only agent leased %+v, want the research retry pinned elsewhere", got)
	}
	fresh := e.leaseAny(other)
	if fresh == nil || fresh.Kind != KindWork || !strings.HasSuffix(fresh.Branch, "-r1") {
		t.Fatalf("research retry = %+v, want fresh work on the other research agent", fresh)
	}
	if id := e.jobAgent(fresh.JobID); id != e.agentOf(other) {
		t.Errorf("retry agent = %d, want the research-capable agent", id)
	}
}

func TestAutomaticRestartCanUseAnotherAgentOnTheSameRunner(t *testing.T) {
	e := newEnv(t)
	first := e.addAgent(e.runner1, "first", "m1", "a")
	second := e.addAgent(e.runner1, "second", "m2", "b")
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if e.jobAgent(g.JobID) != first {
		t.Fatal("initial job did not use the first agent")
	}
	e.complete(g, Completion{Error: "CLI crashed", ErrorKind: "other"})
	fresh := e.lease(e.runner1, []string{"first", "second"}, nil)
	if fresh == nil || fresh.Kind != KindWork || e.jobAgent(fresh.JobID) != second {
		t.Errorf("retry = %+v, want the second agent on the same runner", fresh)
	}
}

func TestAutomaticRestartRequiresTaskClassCandidate(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE tasks SET class = 'research' WHERE id = ?", e.task)
	author := e.agented("author", "a", "implement")
	e.agented("other", "b", "implement")
	e.submit(e.work())
	g := e.leaseAny(author)
	if g == nil {
		t.Fatal("research task did not lease normally on an implement agent")
	}
	e.complete(g, Completion{Error: "CLI crashed", ErrorKind: "other"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %s, want needs_foreman without a research retry candidate", state)
	}
	if n := e.countEvents(evTaskRestarted); n != 0 {
		t.Errorf("restarts = %d, want no retry on an implement-only agent", n)
	}
}

func TestAutomaticRestartCarriesLastProgress(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "a", "implement")
	other := e.agented("other", "b", "implement")
	e.submit(e.work())
	g := e.leaseAny(author)
	ans, _, err := e.eng.Heartbeat(e.ctx, author, g.JobID, g.LeaseID,
		Progress{Events: []string{"tool_start: inspect the parser", "tool_end: call-1"}})
	if err != nil || ans != AnsOK {
		t.Fatalf("heartbeat = %s, %v", ans, err)
	}
	e.complete(g, Completion{Error: "CLI crashed", ErrorKind: "other"})
	fresh := e.leaseAny(other)
	if fresh == nil || !strings.Contains(fresh.Prompt, "other: CLI crashed") ||
		!strings.Contains(fresh.Prompt, "Last progress: tool_start: inspect the parser") {
		t.Errorf("fresh grant = %+v, want failure and last progress in context", fresh)
	}
}

func TestTransientReviewFailureUsesReviewRetry(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "a", "implement")
	first := e.agented("first", "b", "review")
	second := e.agented("second", "c", "review")
	e.runToReview(author)
	e.complete(e.leaseAny(first), Completion{Error: "review CLI auth failed", ErrorKind: "auth"})
	if n := e.countEvents(evTaskRestarted); n != 0 {
		t.Errorf("work restarts after review failure = %d, want none", n)
	}
	if n := e.countEvents(evReviewRetry); n != 1 {
		t.Errorf("review retries = %d, want one", n)
	}
	if retry := e.leaseAny(second); retry == nil || retry.Kind != KindReview {
		t.Errorf("retry grant = %+v, want another review", retry)
	}
}

// driveFixRound drives the fixture's task to a leased fix round of the
// named budget — roundFix behind a failed check, roundReview behind a
// changes verdict — and returns the author's fix grant.
func driveFixRound(e *env, author int64, round string) *LeaseGrant {
	e.t.Helper()
	e.submit(e.work())
	g := e.leaseAny(author)
	if round == roundFix {
		if ans := e.complete(g, doneOnSession("task/1-1", failCheck, "hs-author-1")); ans != AnsOK {
			e.t.Fatalf("complete the failed-check work: %s", ans)
		}
	} else {
		if ans := e.complete(g, doneOnSession("task/1-1", passCheck, "hs-author-1")); ans != AnsOK {
			e.t.Fatalf("complete the work: %s", ans)
		}
		reviewer := e.agented("reviewer", "google", "review")
		rg := e.leaseAny(reviewer)
		if ans := e.complete(rg, Completion{Result: changesVerdict}); ans != AnsOK {
			e.t.Fatalf("complete the changes review: %s", ans)
		}
	}
	fix := e.fixJob()
	fg := e.authorLease(author)
	if fg == nil || fg.JobID != fix {
		e.t.Fatalf("author leased %+v, want fix job %d", fg, fix)
	}
	return fg
}

// TestTransientFixFailureRetriesOnTheSameSession: a fix round whose CLI
// fails transiently (kind other, a server error) is retried once on the
// author's session and agent, spending no round, keeping the round's
// feedback — for a check-fix round and a review-fix round alike.
func TestTransientFixFailureRetriesOnTheSameSession(t *testing.T) {
	for _, round := range []string{roundFix, roundReview} {
		t.Run(round, func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", "anthropic", "implement")
			fix := driveFixRound(e, author, round)
			if ans := e.complete(fix, Completion{Error: "Unexpected server error", ErrorKind: "other"}); ans != AnsOK {
				t.Fatalf("complete the fix round: %s", ans)
			}
			// The same-session resume needs the author's agent still ready:
			// one transient failure is not a streak.
			if h, why, _ := e.agentState(e.agentOf(author)); h != "ready" || why != "" {
				t.Fatalf("author agent health = %s (%s), want ready for the same-session retry", h, why)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
				t.Fatalf("task state = %s, want running behind the retry", state)
			}
			retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'fix' AND retry = 1", e.task)
			if retry == 0 {
				t.Fatal("no fix retry queued")
			}
			if mode := e.str("SELECT mode FROM jobs WHERE id = ?", retry); mode != string(ModeSession) {
				t.Errorf("retry mode = %s, want session", mode)
			}
			if round := e.str("SELECT round FROM jobs WHERE id = ?", retry); round != "" {
				t.Errorf("retry round = %q, want none: the round was never attempted", round)
			}
			if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", retry); agent != e.agentOf(author) {
				t.Errorf("retry pin = %d, want the author's %d", agent, e.agentOf(author))
			}
			// The retry asks the same round: the failed round's prompt, feedback intact.
			if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", retry),
				e.str("SELECT prompt FROM jobs WHERE id = ?", fix.JobID); got != want {
				t.Errorf("retry prompt = %q, want the failed round's %q", got, want)
			}
			if p, ok := e.eventPayloadOf(evTaskFixRetry); !ok || !strings.Contains(p.Reason, "other: Unexpected server error") {
				t.Errorf("retry event = %+v (%t), want the transient reason", p, ok)
			}
			if n := e.countEvents(evTaskFailed); n != 0 {
				t.Errorf("task_failed events = %d, want coalesced into the retry", n)
			}
			// The retry resumes the author's session after the backoff.
			e.clock.Advance(retryDelayFor(1) + time.Second)
			fresh := e.authorLease(author)
			if fresh == nil || fresh.JobID != retry || fresh.Mode != ModeSession || fresh.ResumeSessionID != "hs-author-1" {
				t.Fatalf("retry grant = %+v, want %d resuming hs-author-1", fresh, retry)
			}
		})
	}
}

// TestTransientFixFailureGoesFreshOnAnotherAgent: when the author's
// agent cannot take the retry, the fix round goes to another eligible
// agent of the tier on a fresh session, with the round's feedback — the
// review's findings — in its prompt and the task's branch under it.
func TestTransientFixFailureGoesFreshOnAnotherAgent(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	other := e.agented("other", "google", "implement")
	findings := "the frobnicator is never initialized; the spec's second clause is unaddressed"
	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, doneOnSession("task/1-1", passCheck, "hs-author-1")); ans != AnsOK {
		t.Fatalf("complete the work: %s", ans)
	}
	reviewer := e.agented("reviewer", "google", "review")
	rg := e.leaseAny(reviewer)
	if ans := e.complete(rg, Completion{
		Result: `{"verdict":"changes","findings":"` + findings + `"}`,
	}); ans != AnsOK {
		t.Fatalf("complete the changes review: %s", ans)
	}
	fix := e.fixJob()
	fg := e.authorLease(author)
	if fg == nil || fg.JobID != fix {
		t.Fatalf("author leased %+v, want fix %d", fg, fix)
	}
	// The author's agent is gone: the retry must go elsewhere.
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", e.agentOf(author))
	if ans := e.complete(fg, Completion{Error: "CLI crashed", ErrorKind: "other"}); ans != AnsOK {
		t.Fatalf("complete the fix round: %s", ans)
	}
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'fix' AND retry = 1", e.task)
	if retry == 0 {
		t.Fatal("no fix retry queued")
	}
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", retry); mode != string(ModeFresh) {
		t.Errorf("retry mode = %s, want fresh", mode)
	}
	if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", retry); agent != e.agentOf(other) {
		t.Errorf("retry pin = %d, want the other agent %d", agent, e.agentOf(other))
	}
	if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", retry); !strings.Contains(prompt, findings) {
		t.Errorf("retry prompt lacks the review findings:\n%s", prompt)
	}
	e.clock.Advance(retryDelayFor(1) + time.Second)
	fresh := e.leaseAny(other)
	if fresh == nil || fresh.JobID != retry || fresh.Mode != ModeFresh {
		t.Fatalf("retry grant = %+v, want the fresh fix retry %d on the other agent", fresh, retry)
	}
	// The branch is the task's own, not a restart's -r1.
	if fresh.Branch != fmt.Sprintf("brigada/task-%d", e.task) {
		t.Errorf("retry branch = %q, want the task's own %q", fresh.Branch, fmt.Sprintf("brigada/task-%d", e.task))
	}
}

// TestSecondTransientFixFailureParks: the fix retry is once; its own
// transient failure parks the task instead of retrying again.
func TestSecondTransientFixFailureParks(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.agented("other", "google", "implement") // another healthy agent cannot buy a second retry
	fix := driveFixRound(e, author, roundFix)
	if ans := e.complete(fix, Completion{Error: "CLI crashed", ErrorKind: "other"}); ans != AnsOK {
		t.Fatalf("complete the fix round: %s", ans)
	}
	e.clock.Advance(retryDelayFor(1) + time.Second)
	retry := e.authorLease(author)
	if retry == nil || retry.JobID != e.int("SELECT id FROM jobs WHERE task_id = ? AND retry = 1", e.task) {
		t.Fatalf("retry = %+v, want the fix retry", retry)
	}
	if ans := e.complete(retry, Completion{Error: "CLI crashed again", ErrorKind: "other"}); ans != AnsOK {
		t.Fatalf("complete the retry: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %s, want needs_foreman", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND retry = 1", e.task); n != 1 {
		t.Errorf("retry count = %d, want one", n)
	}
	if p, ok := e.eventPayloadOf(evTaskFailed); !ok || p.Reason != "CLI crashed again" {
		t.Errorf("park event = %+v (%t), want the second failure", p, ok)
	}
}

// TestNonTransientFixFailureParksAtOnce: a fix round that ends for any
// other reason — a result-block failure, a question, an error without a
// transient kind — parks at once, without a retry.
func TestNonTransientFixFailureParksAtOnce(t *testing.T) {
	for name, completion := range map[string]Completion{
		"bad result":         {Result: "not JSON"},
		"worker failed":      {Result: `{"status":"failed","summary":"cannot do the task"}`},
		"needs help":         {Result: `{"status":"needs_help","question":"which approach?"}`},
		"unclassified error": {Error: "push failed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", "anthropic", "implement")
			e.agented("other", "google", "implement")
			// A result-block failure or an unparsable block climbs the
			// escalation ladder before parking; pin the task at the top so
			// the test sees the park, not a tier bump.
			e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE id = ?", e.task)
			fix := driveFixRound(e, author, roundFix)
			if ans := e.complete(fix, completion); ans != AnsOK {
				t.Fatalf("complete the fix round: %s", ans)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND retry = 1", e.task); n != 0 {
				t.Errorf("retries = %d, want none", n)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Errorf("task state = %s, want needs_foreman", state)
			}
		})
	}
}

// TestTransientFixFailureParksWithoutBudgetOrConfig: the fix retry is
// counted like the first-job retry — the restart budget gates it and the
// task_auto_retry setting turns it off.
func TestTransientFixFailureParksWithoutBudgetOrConfig(t *testing.T) {
	for _, scenario := range []string{"config off", "restart cap zero"} {
		t.Run(scenario, func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) {
				if scenario == "config off" {
					off := false
					c.TaskAutoRetry = &off
				}
			})
			author := e.agented("author", "anthropic", "implement")
			e.agented("other", "google", "implement")
			if scenario == "restart cap zero" {
				e.exec(`UPDATE tasks SET caps = '{"restart":0}' WHERE id = ?`, e.task)
			}
			fix := driveFixRound(e, author, roundFix)
			if ans := e.complete(fix, Completion{Error: "CLI crashed", ErrorKind: "other"}); ans != AnsOK {
				t.Fatalf("complete the fix round: %s", ans)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				t.Errorf("task state = %s, want needs_foreman", state)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND retry = 1", e.task); n != 0 {
				t.Errorf("retries = %d, want none", n)
			}
		})
	}
}

// TestFixRetryWaitsOutItsBackoff: the fix retry uses the not-before
// backoff the engine already holds infra and setup retries back with
// (§5.7), so it waits its short pause before any runner leases it.
func TestFixRetryWaitsOutItsBackoff(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	fix := driveFixRound(e, author, roundFix)
	if ans := e.complete(fix, Completion{Error: "CLI crashed", ErrorKind: "other"}); ans != AnsOK {
		t.Fatalf("complete the fix round: %s", ans)
	}
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND retry = 1", e.task)
	if retry == 0 {
		t.Fatal("no fix retry queued")
	}
	if until := e.str("SELECT COALESCE(not_before, '') FROM jobs WHERE id = ?", retry); until == "" {
		t.Error("the retry carries no not-before lease time")
	}
	if got := e.authorLease(author); got != nil {
		t.Fatalf("a runner leased %+v inside the backoff window", got)
	}
	e.clock.Advance(retryDelayFor(1) + time.Second)
	if fresh := e.authorLease(author); fresh == nil || fresh.JobID != retry {
		t.Fatalf("retry after the backoff = %+v, want job %d", fresh, retry)
	}
}
