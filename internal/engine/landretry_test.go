package engine

import (
	"fmt"
	"strings"
	"testing"
)

// The land-retry tests (#133): RetryLanding queues again the landing of
// a task a rejected push or an environment failure parked, through the
// land rule and every gate, and refuses everything else with the reason
// named and nothing changed.

// landRejectedErr is a land job's error as a push the target moved under
// words it: not the environment, not the task's content.
const landRejectedErr = "the push was rejected again after one retry (the target moved): non-fast-forward"

// parkLanding drives the fixture's task to done, leases its land job and
// completes it with c, the failure that parks it.
func (e *env) parkLanding(c Completion) (author int64) {
	e.t.Helper()
	author = e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		e.t.Fatalf("the land job was not leased: %+v", g)
	}
	if ans := e.complete(g, c); ans != AnsOK {
		e.t.Fatalf("complete the land job: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		e.t.Fatalf("task state after the failed landing = %q, want needs_foreman", state)
	}
	return author
}

// retryLanding calls RetryLanding for the fixture's task.
func (e *env) retryLanding(project, task int64) LandRetryResult {
	e.t.Helper()
	res, err := e.eng.RetryLanding(e.ctx, project, task)
	if err != nil {
		e.t.Fatalf("RetryLanding: %v", err)
	}
	return res
}

// queuedLandJobs counts the task's land jobs still waiting to be leased.
func (e *env) queuedLandJobs(task int64) int64 {
	e.t.Helper()
	return e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", task)
}

// TestRetryLandingAfterRejectedPush: a retry of a task a rejected push
// parked queues one land job — the outcome names it, the land_retried
// event names it — whose grant runs land_ready rather than skipping it;
// the task is done again, off the needs-you queue, with no restart spent.
func TestRetryLandingAfterRejectedPush(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingLandReady: ".brigada/land-ready.sh"})
	author := e.parkLanding(Completion{Error: landRejectedErr})
	if _, ok := e.needsYouTask(e.task); !ok {
		t.Fatal("the parked task is not in the needs-you queue")
	}

	res := e.retryLanding(e.project, e.task)
	if res.Outcome != LandRetryQueued || res.JobID == 0 || res.TaskID != e.task {
		t.Fatalf("RetryLanding = %+v, want queued with its land job", res)
	}
	if n := e.queuedLandJobs(e.task); n != 1 {
		t.Errorf("queued land jobs = %d, want 1", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done", state)
	}
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("needs-you entry = %+v, want none after the retry", it)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != 0 {
		t.Errorf("restart jobs = %d, want none spent", n)
	}
	if n := e.countEvents(evLandRetried); n != 1 {
		t.Errorf("land_retried events = %d, want 1", n)
	}
	if job := e.int("SELECT job_id FROM events WHERE kind = ?", evLandRetried); job != res.JobID {
		t.Errorf("land_retried job = %d, want the queued land job %d", job, res.JobID)
	}
	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand || g.JobID != res.JobID {
		t.Fatalf("the retried land job was not leased: %+v", g)
	}
	if g.LandReady != ".brigada/land-ready.sh" || g.SkipLandReady {
		t.Errorf("retried grant land_ready/skip = %q/%v, want the command run, not skipped", g.LandReady, g.SkipLandReady)
	}
}

// TestRetryLandingLandReadyHolds: a retry while land_ready holds the
// landing ends in the gate's hold, not a landing.
func TestRetryLandingLandReadyHolds(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingLandReady: ".brigada/land-ready.sh"})
	author := e.parkLanding(Completion{Error: landRejectedErr})

	if res := e.retryLanding(e.project, e.task); res.Outcome != LandRetryQueued {
		t.Fatalf("RetryLanding = %+v, want queued", res)
	}
	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand || g.SkipLandReady {
		t.Fatalf("the retried land job = %+v, want one that runs land_ready", g)
	}
	if ans := e.complete(g, landHeld("the release window is closed")); ans != AnsOK {
		t.Fatalf("complete the held land job: %s", ans)
	}
	if reason := e.str("SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); !strings.Contains(reason, "land_ready") {
		t.Errorf("land hold reason = %q, want the land_ready hold", reason)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Errorf("landing records = %d, want none: the gate held the retry", n)
	}
}

// TestRetryLandingAfterEnvFailure: a retry of a task an environment
// failure parked queues its landing, takes it off the cause's line, and
// settles the cause as a restart would: the same cause failing again
// opens a new episode and wakes anew.
func TestRetryLandingAfterEnvFailure(t *testing.T) {
	e := newEnv(t)
	author := e.parkLanding(Completion{Error: landEnvFetchErr})
	if got := e.countEvents(evLandEnvFailed); got != 1 {
		t.Fatalf("land_env_failed events = %d, want 1", got)
	}

	res := e.retryLanding(e.project, e.task)
	if res.Outcome != LandRetryQueued || res.JobID == 0 {
		t.Fatalf("RetryLanding = %+v, want queued", res)
	}
	if cause := e.str("SELECT land_env_cause FROM tasks WHERE id = ?", e.task); cause != "" {
		t.Errorf("land cause after the retry = %q, want none", cause)
	}
	if items := e.needsYouTaskItems(); len(items) != 0 {
		t.Errorf("needs-you task entries = %v, want none", items)
	}
	if ans := e.complete(e.leaseForeman(), Completion{Result: "judged"}); ans != AnsOK {
		t.Fatalf("complete the wake turn: %s", ans)
	}
	g := e.leaseGit(author)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("the retried land job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Error: landEnvFetchErr}); ans != AnsOK {
		t.Fatalf("complete the second failure: %s", ans)
	}
	if got := e.countEvents(evLandEnvFailed); got != 2 {
		t.Errorf("land_env_failed events = %d, want 2: the retry settled the first episode", got)
	}
}

// TestRetryLandingHeldAtTheGate: a land-gate rule that trips on the
// retry holds the task — a success, no job queued — with the rule named
// in the result and the land_retried event, beside task_land_held.
func TestRetryLandingHeldAtTheGate(t *testing.T) {
	e := newEnv(t)
	e.parkLanding(Completion{Error: landRejectedErr})
	e.set(map[string]string{SettingGateLand: GateAsk})

	res := e.retryLanding(e.project, e.task)
	if res.Outcome != LandRetryHeld || res.JobID != 0 || !strings.Contains(res.Reason, "gate_land") {
		t.Fatalf("RetryLanding = %+v, want held by gate_land with no job", res)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", e.task); n != 1 {
		t.Errorf("held land_holds rows = %d, want 1", n)
	}
	if n := e.queuedLandJobs(e.task); n != 0 {
		t.Errorf("queued land jobs = %d, want none while held", n)
	}
	if n := e.countEvents(evTaskLandHeld); n != 1 {
		t.Errorf("task_land_held events = %d, want 1", n)
	}
	p, ok := e.taskEvent(e.task, evLandRetried)
	if !ok || !strings.Contains(p.Reason, "held") || !strings.Contains(p.Reason, "gate_land") {
		t.Errorf("land_retried = %+v (%t), want the hold's rule", p, ok)
	}
	if job := e.int("SELECT COALESCE(job_id, 0) FROM events WHERE kind = ?", evLandRetried); job != 0 {
		t.Errorf("land_retried job = %d, want none", job)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskDone {
		t.Errorf("task state = %q, want done", state)
	}
}

// TestRetryLandingStaleBase: a task whose base became a superseded round
// of its dependency after the rejected push is refused by the land rule
// on the retry — parked again with task_base_stale, no job — a recorded
// outcome naming task_restart; a second retry is then refused outright.
func TestRetryLandingStaleBase(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	plan, err := e.eng.CreatePlan(e.ctx, e.project, "the stack", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "mid", Title: "the mid", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, mid := plan.Tasks[0].TaskID, plan.Tasks[1].TaskID
	g := e.leaseAny(author)
	if g == nil || g.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(lib), passCheck)); ans != AnsOK {
		t.Fatalf("complete lib: %s", ans)
	}
	e.approveReview(reviewer)
	g = e.leaseAny(author)
	if g == nil || g.TaskID != mid {
		t.Fatalf("lease = %+v, want mid's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(mid), passCheck)); ans != AnsOK {
		t.Fatalf("complete mid: %s", ans)
	}
	e.approveReview(reviewer)
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"lib-1","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("land lib: %s", ans)
	}
	lg = e.leaseGit(author)
	if lg == nil || lg.TaskID != mid {
		t.Fatalf("lease = %+v, want mid's land job", lg)
	}
	if ans := e.complete(lg, Completion{Error: landRejectedErr}); ans != AnsOK {
		t.Fatalf("fail mid's push: %s", ans)
	}
	// The dependency's result moved to a new round while mid waited.
	e.exec("UPDATE tasks SET branch = ? WHERE id = ?", branchOf(lib)+"-r1", lib)

	res := e.retryLanding(e.project, mid)
	if res.Outcome != LandRetryStaleBase || res.JobID != 0 ||
		!strings.Contains(res.Reason, branchOf(lib)+"-r1") || !strings.Contains(res.Reason, "task_restart") {
		t.Fatalf("RetryLanding = %+v, want stale_base naming the new base and task_restart", res)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", mid); state != taskNeedsForeman {
		t.Errorf("mid state = %q, want needs_foreman again", state)
	}
	if n := e.queuedLandJobs(mid); n != 0 {
		t.Errorf("queued land jobs = %d, want none on a stale base", n)
	}
	if _, ok := e.taskEvent(mid, evTaskBaseStale); !ok {
		t.Error("no task_base_stale event for the refused landing")
	}
	if p, ok := e.taskEvent(mid, evLandRetried); !ok || !strings.Contains(p.Reason, LandRetryStaleBase) || p.Status != taskNeedsForeman {
		t.Errorf("land_retried = %+v (%t), want the stale-base outcome", p, ok)
	}
	again := e.retryLanding(e.project, mid)
	if again.Outcome != LandRetryRefused || !strings.Contains(again.Reason, "task_restart") {
		t.Errorf("second RetryLanding = %+v, want refused naming task_restart", again)
	}
}

// TestRetryLandingRefusals: every task the retry does not take is
// refused with the reason naming what it found, no row changed and no
// land_retried event recorded.
func TestRetryLandingRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env) (project int64)
		want  string
	}{
		{"pushed landing record", func(e *env) int64 {
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			e.runToDone(author, reviewer)
			e.landPushed(author)
			return e.project
		}, "landing record"},
		{"waiting landing record", func(e *env) int64 {
			e.set(map[string]string{SettingGatePush: GateAsk})
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			e.runToDone(author, reviewer)
			g := e.leaseGit(author)
			if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
				e.t.Fatalf("land waiting: %s", ans)
			}
			if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
				e.t.Fatalf("PushApprove: %v", err)
			}
			pg := e.leaseGit(author)
			if pg == nil || !pg.Push {
				e.t.Fatalf("push grant = %+v", pg)
			}
			if ans := e.complete(pg, Completion{Error: landRejectedErr}); ans != AnsOK {
				e.t.Fatalf("reject the push: %s", ans)
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
				e.t.Fatalf("task state after the rejected push = %q", state)
			}
			return e.project
		}, "task_restart"},
		{"not parked", func(e *env) int64 { return e.project }, "not parked"},
		{"parked by a question", func(e *env) int64 {
			author := e.agented("author", "anthropic", "implement")
			e.submit(e.workFor(e.task))
			g := e.leaseAny(author)
			if ans := e.complete(g, Completion{Result: `{"status":"needs_help","question":"which way?"}`}); ans != AnsOK {
				e.t.Fatalf("park on the question: %s", ans)
			}
			return e.project
		}, "not by a failed landing"},
		{"land conflict", func(e *env) int64 {
			e.parkLanding(Completion{Result: `{"target_branch":"main","commit":"","check_passed":false,"conflict_files":["rejected.go"]}`})
			return e.project
		}, "task_restart"},
		{"failed land check", func(e *env) int64 {
			e.parkLanding(Completion{Result: `{"target_branch":"main","commit":"x","check_passed":false,"check_log":"TestPush: rejected"}`})
			return e.project
		}, "task_restart"},
		{"moved approved head", func(e *env) int64 {
			e.parkLanding(Completion{Error: "brigada/task-1 is at abc, not the approved head def: the branch moved after its review; nothing landed, push rejected"})
			return e.project
		}, "task_restart"},
		{"held at the land gate", func(e *env) int64 {
			e.parkLanding(Completion{Error: landRejectedErr})
			e.exec("INSERT INTO land_holds (task_id, reason, created_at) VALUES (?, 'held by rule gate_land: ask', ?)",
				e.task, fmtTime(e.clock.Now()))
			return e.project
		}, "land gate"},
		{"live land job", func(e *env) int64 {
			e.parkLanding(Completion{Error: landRejectedErr})
			e.submit(SubmitParams{Key: fmt.Sprintf("task:%d", e.task), ProjectID: e.project, TaskID: e.task,
				Kind: KindLand, Mode: ModeFresh, Labels: []string{"git"}})
			return e.project
		}, "live"},
		{"finished plan", func(e *env) int64 {
			e.parkLanding(Completion{Error: landRejectedErr})
			e.exec("UPDATE plans SET state = ? WHERE id = (SELECT plan_id FROM tasks WHERE id = ?)", planCancelled, e.task)
			return e.project
		}, "cancelled"},
		{"another project", func(e *env) int64 {
			e.parkLanding(Completion{Error: landRejectedErr})
			return e.project + 1
		}, "belongs to project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			project := tc.setup(e)
			state := e.str("SELECT state FROM tasks WHERE id = ?", e.task)
			landings := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task)
			jobs := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", e.task)
			holds := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task)

			res := e.retryLanding(project, e.task)
			if res.Outcome != LandRetryRefused || res.JobID != 0 || !strings.Contains(res.Reason, tc.want) {
				t.Errorf("RetryLanding = %+v, want refused naming %q", res, tc.want)
			}
			if got := e.str("SELECT state FROM tasks WHERE id = ?", e.task); got != state {
				t.Errorf("task state = %q, want %q unchanged", got, state)
			}
			if got := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); got != landings {
				t.Errorf("landing records = %d, want %d unchanged", got, landings)
			}
			if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", e.task); got != jobs {
				t.Errorf("jobs = %d, want %d unchanged", got, jobs)
			}
			if got := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", e.task); got != holds {
				t.Errorf("land holds = %d, want %d unchanged", got, holds)
			}
			if n := e.countEvents(evLandRetried); n != 0 {
				t.Errorf("land_retried events = %d, want none for a refusal", n)
			}
		})
	}
}
