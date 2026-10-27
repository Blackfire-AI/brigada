package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// continuePlan creates build -> ship where ship continues build: both of
// the named class so a done finishes each outright, no review needed.
func (e *env) continuePlan(class string) CreatePlanResult {
	e.t.Helper()
	res, err := e.eng.CreatePlan(e.ctx, e.project, "continued", []PlanTask{
		{Name: "build", Spec: "build it", Class: class},
		{Name: "ship", Spec: "ship it", Class: class, Deps: []string{"build"}, Continue: "build"},
	})
	if err != nil {
		e.t.Fatalf("create plan: %v", err)
	}
	return res
}

// runBuild leases build's work job as author does and completes it done,
// ending on the given harness session ("" for none).
func (e *env) runBuild(author int64, session string) {
	e.t.Helper()
	g := e.lease(author, []string{"author"}, nil)
	if g == nil {
		e.t.Fatal("build's work job was not leased")
	}
	c := doneOn(branchOf(g.TaskID), "")
	c.SessionID = session
	if ans := e.complete(g, c); ans != AnsOK {
		e.t.Fatalf("complete build: %s", ans)
	}
}

// shipJob reads ship's queued work job, failing the test without one.
func (e *env) shipJob(res CreatePlanResult) int64 {
	e.t.Helper()
	ship := res.Tasks[1].TaskID
	id := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'work' AND status = 'queued'", ship)
	if id == 0 {
		e.t.Fatalf("no queued work job for ship; have %v", e.eventKinds())
	}
	return id
}

// TestContinueResumesDepAuthorSession: when the continued dep is done,
// the dependent's first work job runs in session mode on the agent that
// authored the dep's result, pinned to it, resuming the session the dep
// ended on — and the prompt says a new task follows in the same session.
func TestContinueResumesDepAuthorSession(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "claude", "implement")
	res := e.continuePlan("answer")
	ship := res.Tasks[1].TaskID

	e.runBuild(author, "hs-build-1")
	job := e.shipJob(res)
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", job); mode != string(ModeSession) {
		t.Errorf("ship's work job mode = %q, want session", mode)
	}
	if harness := e.str("SELECT harness FROM jobs WHERE id = ?", job); harness != "author" {
		t.Errorf("ship's work job harness = %q, want the dep author's own %q", harness, "author")
	}
	if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", job); agent != e.agentOf(author) {
		t.Errorf("ship's work job required agent = %d, want the dep author's %d", agent, e.agentOf(author))
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", job),
		prompt.ComposeContinuePrompt("ship it", ""); got != want {
		t.Errorf("ship's prompt = %q\nwant the composed\n%q", got, want)
	} else if !strings.Contains(got, "A new task follows in this session") {
		t.Errorf("ship's prompt does not say a new task follows in the session:\n%s", got)
	}
	// The key's session row is seeded with the dep author's session and
	// runner (§4.2, §8): the lease reads the resume id from it, and the
	// affinity keeps other runners off the job.
	if sid := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", keyOf(ship)); sid != "hs-build-1" {
		t.Errorf("ship's key session = %q, want hs-build-1", sid)
	}
	if g := e.lease(e.runner2, []string{"author"}, nil); g != nil {
		t.Errorf("runner2 leased ship's job (%d), want the session's affinity to keep it", g.JobID)
	}
	g := e.lease(author, []string{"author"}, nil)
	if g == nil || g.JobID != job {
		t.Fatalf("the dep author leased %+v, want ship's job %d", g, job)
	}
	if g.Mode != ModeSession || g.ResumeSessionID != "hs-build-1" {
		t.Errorf("ship grant mode/resume = %s/%q, want session resuming hs-build-1", g.Mode, g.ResumeSessionID)
	}
	// Status names the continued dep (§6.1).
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range st.Plans {
		if p.ID != res.PlanID {
			continue
		}
		for _, tk := range p.Tasks {
			if tk.ID == ship && tk.Continues != "build" {
				t.Errorf("status continues = %q, want build", tk.Continues)
			}
		}
	}
}

// TestContinueFallsBackFresh covers every way the continuation cannot be
// had: the dep ended with no session id, its author ran with no agent,
// the agent has since been dropped, is broken, is capped at exactly 0,
// is cooling — any cooling, however short — or
// is outside the task's tier. The state is in place before the dep
// completes, because the decision falls when the dependent starts, in
// the completion's own transaction. Each leaves the dependent's first
// work job the fresh job of today: fresh mode, no agent pin.
func TestContinueFallsBackFresh(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mutate runs while the dep's work job is leased, before its
		// completion readies the dependent.
		mutate    func(t *testing.T, e *env)
		session   string // "" leaves the dep's run without a session id
		agentLess bool   // lease the dep as an agent-less runner
	}{
		{name: "no session id", session: ""},
		{name: "agent-less author", session: "hs-build-1", agentLess: true},
		{name: "agent dropped", session: "hs-build-1", mutate: func(t *testing.T, e *env) {
			e.exec("DELETE FROM agents")
		}},
		{name: "agent broken", session: "hs-build-1", mutate: func(t *testing.T, e *env) {
			e.exec("UPDATE agents SET health = 'broken', health_reason = 'auth'")
		}},
		{name: "cooling", session: "hs-build-1", mutate: func(t *testing.T, e *env) {
			e.exec("UPDATE agents SET cooling_until = ?", fmtTime(e.clock.Now().Add(2*time.Hour)))
		}},
		{name: "cooling for minutes", session: "hs-build-1", mutate: func(t *testing.T, e *env) {
			e.exec("UPDATE agents SET cooling_until = ?", fmtTime(e.clock.Now().Add(5*time.Minute)))
		}},
		{name: "capped at 0", session: "hs-build-1", mutate: func(t *testing.T, e *env) {
			e.eng.cfg.Caps.Account = map[string]int{"claude": 0}
		}},
		{name: "agent outside the task's tier", session: "hs-build-1", mutate: func(t *testing.T, e *env) {
			e.eng.cfg.Caps.Tier = map[string]string{"author/claude-1": "heavy"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			author := e.agented("author", "claude", "implement")
			res := e.continuePlan("answer")
			var g *LeaseGrant
			if tc.agentLess {
				g = e.leaseAny(e.runner1)
			} else {
				g = e.lease(author, []string{"author"}, nil)
			}
			if g == nil {
				t.Fatal("build's work job was not leased")
			}
			if tc.mutate != nil {
				tc.mutate(t, e)
			}
			c := doneOn(branchOf(g.TaskID), "")
			c.SessionID = tc.session
			if ans := e.complete(g, c); ans != AnsOK {
				t.Fatalf("complete build: %s", ans)
			}
			job := e.shipJob(res)
			if got := e.str("SELECT mode FROM jobs WHERE id = ?", job); got != string(ModeFresh) {
				t.Errorf("mode = %q, want fresh", got)
			}
			if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", job); agent != 0 {
				t.Errorf("required agent = %d, want none", agent)
			}
			if got := e.str("SELECT prompt FROM jobs WHERE id = ?", job); strings.Contains(got, "A new task follows in this session") {
				t.Errorf("prompt says a new task follows, want the fresh work prompt")
			}
		})
	}
}

// TestContinueRestartStartsFresh: a restart of a continuing task is a
// fresh attempt (§5.6), never a resume of the dep author's session.
func TestContinueRestartStartsFresh(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "claude", "implement")
	res := e.continuePlan("answer")
	ship := res.Tasks[1].TaskID

	e.runBuild(author, "hs-build-1")
	job := e.shipJob(res)
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", job); mode != string(ModeSession) {
		t.Fatalf("ship's first work job mode = %q, want session before the restart", mode)
	}
	// Fail ship's run past every escalation step so it parks.
	e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE id = ?", ship)
	g := e.lease(author, []string{"author"}, nil)
	if ans := e.complete(g, Completion{Result: `{"status":"failed","summary":"cannot"}`}); ans != AnsOK {
		t.Fatalf("complete ship's first run: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", ship); state != taskNeedsForeman {
		t.Fatalf("ship state = %q, want needs_foreman", state)
	}
	r, err := e.eng.RestartTask(e.ctx, e.project, ship, "start over")
	if err != nil {
		t.Fatalf("restart ship: %v", err)
	}
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", r.JobID); mode != string(ModeFresh) {
		t.Errorf("restart job mode = %q, want fresh", mode)
	}
	if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", r.JobID); agent != 0 {
		t.Errorf("restart job required agent = %d, want none", agent)
	}
}

// TestContinueValidationRefused: a continued dep that is not one of the
// task's deps refuses plan_create whole, an amendment's added task and a
// retarget the same way; a retarget replaces the continued dep with the
// dependencies it names.
func TestContinueValidationRefused(t *testing.T) {
	e := newEnv(t)
	for name, tasks := range map[string][]PlanTask{
		"continue not a dep": {
			{Name: "a", Spec: "a"},
			{Name: "b", Spec: "b"},
			{Name: "c", Spec: "c", Deps: []string{"a"}, Continue: "b"},
		},
		"continue names no task": {
			{Name: "a", Spec: "a"},
			{Name: "c", Spec: "c", Deps: []string{"a"}, Continue: "zzz"},
		},
		"continue without deps": {
			{Name: "a", Spec: "a", Continue: "a"},
		},
	} {
		before := e.int("SELECT COUNT(*) FROM plans")
		if _, err := e.eng.CreatePlan(e.ctx, e.project, name, tasks); err == nil || !strings.Contains(err.Error(), "not one of its deps") {
			t.Errorf("%s: err = %v, want the continued dep refused with its reason", name, err)
		}
		if after := e.int("SELECT COUNT(*) FROM plans"); after != before {
			t.Errorf("%s: a refused plan left %d plans behind", name, after-before)
		}
	}

	res, err := e.eng.CreatePlan(e.ctx, e.project, "amend continues", []PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer"},
		{Name: "gate", Spec: "gate", Class: "answer", Deps: []string{"a", "b"}, Continue: "a"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	gate := res.Tasks[2].TaskID
	if got := e.int("SELECT depends_on_task_id FROM task_deps WHERE task_id = ? AND is_continue = 1", gate); got != res.Tasks[0].TaskID {
		t.Fatalf("gate's continued dep = task %d, want a (%d)", got, res.Tasks[0].TaskID)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add: []PlanTask{{Name: "bad", Spec: "x", Deps: []string{"a"}, Continue: "b"}},
	}); err == nil || !strings.Contains(err.Error(), "not one of its deps") {
		t.Errorf("added task with a continued dep outside its deps: err = %v, want refusal", err)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "gate", Deps: []string{"a"}, Continue: "b"}},
	}); err == nil || !strings.Contains(err.Error(), "not one of its deps") {
		t.Errorf("retarget with a continued dep outside its deps: err = %v, want refusal", err)
	}
	e.amend(res.PlanID, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "gate", Deps: []string{"a", "b"}, Continue: "b"}},
	})
	if got := e.int("SELECT depends_on_task_id FROM task_deps WHERE task_id = ? AND is_continue = 1", gate); got != res.Tasks[1].TaskID {
		t.Errorf("gate's continued dep after the retarget = task %d, want b (%d)", got, res.Tasks[1].TaskID)
	}
	// A retarget that names no continue drops the earlier one with the
	// earlier dependencies.
	e.amend(res.PlanID, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "gate", Deps: []string{"a", "b"}}},
	})
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ? AND is_continue = 1", gate); got != 0 {
		t.Errorf("gate's continued deps after a retarget without one = %d, want 0", got)
	}
}

// TestAmendAddedTaskContinues: a task added by plan_amend continues a
// done task's session the same way a created one does.
func TestAmendAddedTaskContinues(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "claude", "implement")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "amend continue", []PlanTask{
		{Name: "build", Spec: "build it", Class: "answer"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	am := e.amend(res.PlanID, PlanAmendment{
		Add: []PlanTask{{Name: "ship", Spec: "ship it", Class: "answer", Deps: []string{"build"}, Continue: "build"}},
	})
	ship := am.Added[0].TaskID

	e.runBuild(author, "hs-build-1")
	job := e.shipJob(CreatePlanResult{Tasks: []CreatedTask{{TaskID: res.Tasks[0].TaskID}, {TaskID: ship}}})
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", job); mode != string(ModeSession) {
		t.Errorf("the added task's work job mode = %q, want session", mode)
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", job),
		prompt.ComposeContinuePrompt("ship it", ""); got != want {
		t.Errorf("prompt = %q\nwant the composed\n%q", got, want)
	}
}

// TestContinueMultiDepsBaseIsTheContinuedDep: with several deps, continue
// set and no base named, the continued dep is the base (§6.1): its edge
// alone carries is_base, and the dependent's work starts from its branch,
// not the default branch or the other dep's.
func TestContinueMultiDepsBaseIsTheContinuedDep(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "claude", "implement")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "multi", []PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer"},
		{Name: "c", Spec: "c", Class: "answer", Deps: []string{"a", "b"}, Continue: "a"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	a, b, c := res.Tasks[0].TaskID, res.Tasks[1].TaskID, res.Tasks[2].TaskID
	if got := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ? AND is_base = 1 AND is_continue = 1", c); got != 1 {
		t.Fatalf("c's base+continue edges = %d, want the one continued dep", got)
	}
	if got := e.int("SELECT depends_on_task_id FROM task_deps WHERE task_id = ? AND is_base = 1", c); got != a {
		t.Errorf("c's base dep = task %d, want a (%d)", got, a)
	}

	// Both deps done, each ending on its own session: c starts from a's
	// branch and resumes a's author session.
	sessions := map[int64]string{a: "hs-a-1", b: "hs-b-1"}
	for range []int{0, 1} {
		g := e.lease(author, []string{"author"}, nil)
		if g == nil {
			t.Fatal("a dep's work job was not leased")
		}
		comp := doneOn(branchOf(g.TaskID), "")
		comp.SessionID = sessions[g.TaskID]
		if ans := e.complete(g, comp); ans != AnsOK {
			t.Fatalf("complete task %d: %s", g.TaskID, ans)
		}
	}
	if base := e.str("SELECT base_ref FROM tasks WHERE id = ?", c); base != branchOf(a) {
		t.Errorf("c's base_ref = %q, want a's branch %q", base, branchOf(a))
	}
	if n := e.int("SELECT COUNT(*) FROM task_deps WHERE task_id = ? AND depends_on_task_id = ? AND is_base = 1", c, b); n != 0 {
		t.Errorf("b's edge is the base too; want a's alone")
	}
	job := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'work' AND status = 'queued'", c)
	if job == 0 {
		t.Fatalf("no queued work job for c; have %v", e.eventKinds())
	}
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", job); mode != string(ModeSession) {
		t.Errorf("c's work job mode = %q, want session", mode)
	}
	if sid := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", keyOf(c)); sid != "hs-a-1" {
		t.Errorf("c's key session = %q, want hs-a-1", sid)
	}
}

// TestContinueBaseConflictRefused: a base naming another dep than the
// continued one refuses plan_create whole, an amendment's added task and
// a retarget the same way.
func TestContinueBaseConflictRefused(t *testing.T) {
	e := newEnv(t)
	before := e.int("SELECT COUNT(*) FROM plans")
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "conflict", []PlanTask{
		{Name: "a", Spec: "a"},
		{Name: "b", Spec: "b"},
		{Name: "c", Spec: "c", Deps: []string{"a", "b"}, Continue: "a", Base: "b"},
	}); err == nil || !strings.Contains(err.Error(), `has base "b" but continues "a"`) {
		t.Errorf("err = %v, want the base conflict refused with its reason", err)
	}
	if after := e.int("SELECT COUNT(*) FROM plans"); after != before {
		t.Errorf("a refused plan left %d plans behind", after-before)
	}

	res, err := e.eng.CreatePlan(e.ctx, e.project, "amend conflict", []PlanTask{
		{Name: "a", Spec: "a", Class: "answer"},
		{Name: "b", Spec: "b", Class: "answer"},
		{Name: "gate", Spec: "gate", Class: "answer", Deps: []string{"a", "b"}, Continue: "a"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Add: []PlanTask{{Name: "bad", Spec: "x", Deps: []string{"a", "b"}, Continue: "a", Base: "b"}},
	}); err == nil || !strings.Contains(err.Error(), `has base "b" but continues "a"`) {
		t.Errorf("added task with a conflicting base: err = %v, want refusal", err)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "gate", Deps: []string{"a", "b"}, Continue: "a", Base: "b"}},
	}); err == nil || !strings.Contains(err.Error(), `has base "b" but continues "a"`) {
		t.Errorf("retarget with a conflicting base: err = %v, want refusal", err)
	}
	// The matching base is kept, not merely allowed.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{
		Retarget: []PlanRetarget{{Task: "gate", Deps: []string{"a", "b"}, Continue: "a", Base: "a"}},
	}); err != nil {
		t.Errorf("retarget with base = continue: err = %v, want taken", err)
	}
}

// TestContinueAfterCheckReviewApproval: the usual path — an implement dep
// goes through its check, a cross-family review and an approve — and the
// dependent resumes the author's session id, never the reviewer's.
func TestContinueAfterCheckReviewApproval(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "reviewed continue", []PlanTask{
		{Name: "build", Spec: "build it"},
		{Name: "ship", Spec: "ship it", Deps: []string{"build"}, Continue: "build"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	build, ship := res.Tasks[0].TaskID, res.Tasks[1].TaskID

	g := e.lease(author, []string{"author"}, nil)
	if g == nil {
		t.Fatal("build's work job was not leased")
	}
	comp := doneOn(branchOf(build), passCheck)
	comp.SessionID = "hs-author-7"
	if ans := e.complete(g, comp); ans != AnsOK {
		t.Fatalf("complete build: %s", ans)
	}
	review := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review'", build)
	if review == 0 {
		t.Fatal("no review job was queued")
	}
	if family := e.str("SELECT exclude_family FROM jobs WHERE id = ?", review); family != "anthropic" {
		t.Errorf("review exclude_family = %q, want the author's anthropic", family)
	}
	if got := e.leaseAny(author); got != nil {
		t.Fatalf("the author's runner leased the review job %d", got.JobID)
	}
	rg := e.lease(reviewer, []string{"reviewer"}, nil)
	if rg == nil || rg.JobID != review {
		t.Fatalf("the reviewer leased %+v, want the review job %d", rg, review)
	}
	rev := Completion{Result: `{"verdict":"approve","findings":"` + substantiveFindings + `"}`}
	rev.SessionID = "hs-rev-9"
	if ans := e.complete(rg, rev); ans != AnsOK {
		t.Fatalf("complete the review: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", build); state != taskDone {
		t.Fatalf("build state after the approve = %q, want done", state)
	}

	job := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'work' AND status = 'queued'", ship)
	if job == 0 {
		t.Fatalf("no queued work job for ship; have %v", e.eventKinds())
	}
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", job); mode != string(ModeSession) {
		t.Errorf("ship's work job mode = %q, want session", mode)
	}
	if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", job); agent != e.agentOf(author) {
		t.Errorf("ship's work job required agent = %d, want the author's %d", agent, e.agentOf(author))
	}
	// The key's session is the author's, not the reviewer's (§7): the
	// reviewer's job ran on the same key.
	if sid := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", keyOf(ship)); sid != "hs-author-7" {
		t.Errorf("ship's key session = %q, want the author's hs-author-7", sid)
	}
	if got := e.lease(reviewer, []string{"reviewer"}, nil); got != nil {
		t.Errorf("the reviewer leased ship's job (%d), want the author's pin to keep it", got.JobID)
	}
	ag := e.lease(author, []string{"author"}, nil)
	if ag == nil || ag.JobID != job {
		t.Fatalf("the author leased %+v, want ship's job %d", ag, job)
	}
	if ag.Mode != ModeSession || ag.ResumeSessionID != "hs-author-7" {
		t.Errorf("ship grant mode/resume = %s/%q, want session resuming hs-author-7", ag.Mode, ag.ResumeSessionID)
	}
}

// TestContinuePlacementReadsTheDepAuthor: the session resumed is the dep
// author's latest work job's — an attempt that moved to another agent
// continues from that agent, not the first.
func TestContinuePlacementReadsTheDepAuthor(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "claude", "implement")
	other := e.agented("other", "codex", "implement")
	res := e.continuePlan("answer")
	build, ship := res.Tasks[0].TaskID, res.Tasks[1].TaskID

	// The first attempt fails on the author's agent; the task parks past
	// every escalation step.
	e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE id = ?", build)
	g := e.lease(author, []string{"author"}, nil)
	if g == nil {
		t.Fatal("build's work job was not leased")
	}
	if ans := e.complete(g, Completion{Result: `{"status":"failed","summary":"cannot"}`}); ans != AnsOK {
		t.Fatalf("complete the first attempt: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", build); state != taskNeedsForeman {
		t.Fatalf("build state = %q, want needs_foreman", state)
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, build, "again"); err != nil {
		t.Fatalf("restart build: %v", err)
	}
	// The restart is unpinned: the other agent takes it and authors the
	// approved result, ending on its own session.
	g = e.lease(other, []string{"other"}, nil)
	if g == nil {
		t.Fatal("the restart was not leased")
	}
	c := doneOn(branchOf(build), "")
	c.SessionID = "hs-other-9"
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete the restart: %s", ans)
	}
	job := e.shipJob(res)
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", job); mode != string(ModeSession) {
		t.Fatalf("ship's work job mode = %q, want session", mode)
	}
	if agent := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", job); agent != e.agentOf(other) {
		t.Errorf("ship's work job required agent = %d, want the latest author's %d", agent, e.agentOf(other))
	}
	if sid := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", keyOf(ship)); sid != "hs-other-9" {
		t.Errorf("ship's key session = %q, want hs-other-9", sid)
	}
}

// TestContinueTierReadsTheTask: the tier the continuation checks is the
// task's own, so a heavy dep author still continues a heavy dependent.
func TestContinueTierReadsTheTask(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Tier = map[string]string{"author/claude-1": "heavy"}
	})
	author := e.agented("author", "claude", "implement")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "heavy continued", []PlanTask{
		{Name: "build", Spec: "build it", Class: "answer", Tier: "heavy"},
		{Name: "ship", Spec: "ship it", Class: "answer", Deps: []string{"build"}, Continue: "build", Tier: "heavy"},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	e.runBuild(author, "hs-build-1")
	job := e.shipJob(res)
	if mode := e.str("SELECT mode FROM jobs WHERE id = ?", job); mode != string(ModeSession) {
		t.Errorf("mode = %q, want session: the heavy author is in the heavy task's tier", mode)
	}
}
