package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The continue restart tests (§5.6): task_restart with the continue
// option on a task a time-capped or stalled run parked.

// cappedOn is the completion the runner sends for a run past its time
// cap: the time_cap outcome, the branch it pushed and that branch's size
// against the base.
func cappedOn(branch string) Completion {
	return Completion{
		Result:  `{"status":"time_cap","reason":"past the work time cap","branch":"` + branch + `","diffstat":{"files":3,"added":120,"removed":8}}`,
		Outcome: OutcomeTimeCap,
	}
}

// guardRefusedStop is the completion of a time-capped run the branch
// guard refused: the guard's words, the stop's, and — when salvage is
// non-empty — the salvage branch the work went to, with its size.
func guardRefusedStop(salvage string) Completion {
	msg := "branch guard: the worktree is on branch elsewhere, not brigada/task-x; " + StoppedRunText + "past the work time cap"
	if salvage != "" {
		msg += "; " + SalvagedText(salvage) + " (2 files changed, +5/-1 against main)"
	}
	return Completion{Error: msg, ErrorKind: ErrorKindBranchGuard}
}

func (e *env) continueRestart(task int64, note string) (RestartResult, error) {
	return e.eng.RestartTaskWith(e.ctx, e.project, task, note, RestartOptions{Continue: true})
}

// TestContinueRestartSeedsFromTheCappedBranch: a time-capped run parks
// the task with task_failed naming its pushed branch and size; the
// continue restart queues a fresh attempt seeded from that branch, on
// the task's own base, and the review behind it reads the attempt
// against that base.
func TestContinueRestartSeedsFromTheCappedBranch(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "openai", "review")
	g := e.runningWork(author)
	if ans := e.complete(g, cappedOn(g.Branch)); ans != AnsOK {
		t.Fatalf("complete the capped run: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state after the cap = %q, want needs_foreman", state)
	}
	p, ok := e.eventPayloadOf(evTaskFailed)
	if !ok || p.Branch != g.Branch || p.FilesChanged != 3 || p.LinesAdded != 120 || p.LinesRemoved != 8 {
		t.Fatalf("task_failed payload = %+v (%t), want branch %s, 3 files, +120/-8", p, ok, g.Branch)
	}
	if text := wakeEventText(evTaskFailed, `{"reason":"past the work time cap","branch":"`+g.Branch+`","files_changed":3,"lines_added":120,"lines_removed":8}`); !strings.Contains(text, g.Branch) ||
		!strings.Contains(text, "3 files changed, +120/-8") {
		t.Errorf("the foreman's task_failed line = %q, want the branch and its size", text)
	}

	res, err := e.continueRestart(e.task, "finish items 4 and 5")
	if err != nil {
		t.Fatalf("continue restart: %v", err)
	}
	if res.Answer != AnsCreated {
		t.Fatalf("continue restart = %+v, want created", res)
	}
	if seed := e.str("SELECT COALESCE(seed_branch, '') FROM jobs WHERE id = ?", res.JobID); seed != g.Branch {
		t.Errorf("fresh attempt seed = %q, want %q", seed, g.Branch)
	}
	if rp, ok := e.eventPayloadOf(evTaskRestarted); !ok || rp.Branch != g.Branch {
		t.Errorf("task_restarted payload = %+v (%t), want the seed %s", rp, ok, g.Branch)
	}
	if base := e.str("SELECT base_ref FROM tasks WHERE id = ?", e.task); base != "main" {
		t.Errorf("task base_ref = %q, want main", base)
	}
	ng := e.leaseAny(author)
	if ng == nil || ng.JobID != res.JobID {
		t.Fatalf("lease = %+v, want the fresh attempt %d", ng, res.JobID)
	}
	if ng.SeedBranch != g.Branch || ng.BaseRef != "main" || ng.Branch == g.Branch {
		t.Errorf("fresh grant seed %q base %q branch %q, want seed %s on base main and a new branch", ng.SeedBranch, ng.BaseRef, ng.Branch, g.Branch)
	}
	if ans := e.complete(ng, doneOn(ng.Branch, passCheck)); ans != AnsOK {
		t.Fatalf("complete the continued attempt: %s", ans)
	}
	rg := e.leaseAny(reviewer)
	if rg == nil || rg.Kind != KindReview {
		t.Fatalf("lease = %+v, want the review", rg)
	}
	if rg.BaseRef != "main" || rg.ReviewBranch != ng.Branch {
		t.Errorf("review grant reads %s...%s, want main...%s", rg.BaseRef, rg.ReviewBranch, ng.Branch)
	}
}

// TestContinueRestartOfAStackedTaskKeepsItsBase: a stacked task's base
// is its dependency's branch; the continued attempt is seeded from the
// capped branch and its diff still reads against that branch.
func TestContinueRestartOfAStackedTaskKeepsItsBase(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "openai", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "the chain", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "app", Title: "the app", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, app := res.Tasks[0].TaskID, res.Tasks[1].TaskID
	e.driveSync(author)
	g := e.leaseAny(author)
	if g == nil || g.TaskID != lib {
		t.Fatalf("lease = %+v, want lib's work", g)
	}
	if ans := e.complete(g, doneOn(branchOf(lib), passCheck)); ans != AnsOK {
		t.Fatalf("complete lib: %s", ans)
	}
	e.approveReview(reviewer)
	g = e.leaseAny(author)
	if g == nil || g.TaskID != app || g.BaseRef != branchOf(lib) {
		t.Fatalf("lease = %+v, want app's work on %s", g, branchOf(lib))
	}
	if ans := e.complete(g, cappedOn(g.Branch)); ans != AnsOK {
		t.Fatalf("complete the capped run: %s", ans)
	}
	r, err := e.continueRestart(app, "")
	if err != nil {
		t.Fatalf("continue restart of the stacked task: %v", err)
	}
	if got := e.str("SELECT base_ref FROM tasks WHERE id = ?", app); got != branchOf(lib) {
		t.Errorf("app base_ref = %q, want %q", got, branchOf(lib))
	}
	ng := e.leaseAny(author)
	if ng == nil || ng.JobID != r.JobID {
		t.Fatalf("lease = %+v, want the fresh attempt %d", ng, r.JobID)
	}
	if ng.SeedBranch != g.Branch || ng.BaseRef != branchOf(lib) {
		t.Errorf("fresh grant seed %q base %q, want seed %s on base %s", ng.SeedBranch, ng.BaseRef, g.Branch, branchOf(lib))
	}
	if ans := e.complete(ng, doneOn(ng.Branch, passCheck)); ans != AnsOK {
		t.Fatalf("complete the continued attempt: %s", ans)
	}
	rg := e.leaseAny(reviewer)
	if rg == nil || rg.Kind != KindReview || rg.BaseRef != branchOf(lib) || rg.ReviewBranch != ng.Branch {
		t.Errorf("review grant = %+v, want %s...%s", rg, branchOf(lib), ng.Branch)
	}
}

// TestContinueRestartRefusals: the option is refused for a task a check
// failure parked and for a stopped run that pushed nothing; the task
// stays parked, and the plain restart still works.
func TestContinueRestartRefusals(t *testing.T) {
	t.Run("check failure", func(t *testing.T) {
		e := newEnv(t)
		e.exec("UPDATE tasks SET class = 'answer' WHERE id = ?", e.task)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		if ans := e.complete(g, doneOn(g.Branch, failCheck)); ans != AnsOK {
			t.Fatalf("complete = %s", ans)
		}
		if _, err := e.continueRestart(e.task, ""); err == nil || !strings.Contains(err.Error(), "not by a time-capped or stalled run") {
			t.Errorf("continue restart of a check-failure park = %v, want the not-stopped refusal", err)
		}
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
			t.Errorf("task state after the refusal = %q, want needs_foreman", state)
		}
		if n := e.restartJobs(e.task); n != 0 {
			t.Errorf("restart jobs after the refusal = %d, want none", n)
		}
	})
	t.Run("stopped run with no branch", func(t *testing.T) {
		e := newEnv(t)
		author := e.agented("author", "anthropic", "implement")
		g := e.runningWork(author)
		if ans := e.complete(g, stoppedOn("")); ans != AnsOK {
			t.Fatalf("complete the stalled run: %s", ans)
		}
		if p, ok := e.eventPayloadOf(evTaskFailed); !ok || p.Branch != "" {
			t.Errorf("task_failed payload = %+v (%t), want no branch", p, ok)
		}
		if _, err := e.continueRestart(e.task, ""); err == nil || !strings.Contains(err.Error(), "pushed no work") {
			t.Errorf("continue restart of a run with no branch = %v, want the no-work refusal", err)
		}
		if r, err := e.eng.RestartTask(e.ctx, e.project, e.task, ""); err != nil || r.Answer != AnsCreated {
			t.Fatalf("plain restart after the refusal = %+v, %v", r, err)
		}
		if seed := e.str("SELECT COALESCE(seed_branch, '') FROM jobs WHERE task_id = ? AND restart = 1", e.task); seed != "" {
			t.Errorf("plain restart seed = %q, want none", seed)
		}
	})
}

// TestContinueRestartFromASalvageBranch: a time-capped run the branch
// guard refused pushed nothing to the task branch; its work is on the
// salvage branch the error names. task_failed names that branch, and the
// continue restart confirms it on the remote and seeds from it. A
// refusal that salvaged nothing, or a salvage branch the remote does not
// hold, is a run with no pushed work.
func TestContinueRestartFromASalvageBranch(t *testing.T) {
	type asked struct{ repo, branch string }
	run := func(t *testing.T, salvaged, onRemote bool) (*env, *LeaseGrant, string, []asked, error) {
		t.Helper()
		var calls []asked
		e := newEnvCfg(t, func(c *Config) {
			c.RemoteBranchExists = func(_ context.Context, repo, branch string) (bool, error) {
				calls = append(calls, asked{repo, branch})
				return onRemote, nil
			}
		})
		author := e.agented("author", "anthropic", "implement")
		g := e.runningWork(author)
		salvage := SalvageBranch(g.Branch, g.JobID)
		named := ""
		if salvaged {
			named = salvage
		}
		if ans := e.complete(g, guardRefusedStop(named)); ans != AnsOK {
			t.Fatalf("complete the refused run: %s", ans)
		}
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
			t.Fatalf("task state = %q, want needs_foreman", state)
		}
		p, _ := e.eventPayloadOf(evTaskFailed)
		if want := named; p.Branch != want {
			t.Errorf("task_failed branch = %q, want %q", p.Branch, want)
		}
		if salvaged && !strings.Contains(p.Reason, "2 files changed") {
			t.Errorf("task_failed reason = %q, want the salvage branch's size", p.Reason)
		}
		_, err := e.continueRestart(e.task, "carry on")
		return e, g, salvage, calls, err
	}

	t.Run("salvaged", func(t *testing.T) {
		e, g, salvage, calls, err := run(t, true, true)
		if err != nil {
			t.Fatalf("continue restart from the salvage branch: %v", err)
		}
		if len(calls) != 1 || calls[0].branch != salvage {
			t.Errorf("remote asked %+v, want once for %s", calls, salvage)
		}
		ng := e.leaseAny(g.LeaseOwner)
		if ng == nil {
			ng = e.leaseAny(e.runner1)
		}
		if ng == nil || ng.SeedBranch != salvage || ng.BaseRef != "main" {
			t.Fatalf("fresh grant = %+v, want seeded from %s on main", ng, salvage)
		}
	})
	t.Run("nothing salvaged", func(t *testing.T) {
		_, _, _, calls, err := run(t, false, true)
		if err == nil || !strings.Contains(err.Error(), "pushed no work") {
			t.Errorf("continue restart of a refused run that salvaged nothing = %v, want the no-work refusal", err)
		}
		if len(calls) != 0 {
			t.Errorf("remote asked %+v, want never", calls)
		}
	})
	t.Run("salvage branch absent", func(t *testing.T) {
		e, _, _, _, err := run(t, true, false)
		if err == nil || !strings.Contains(err.Error(), "not on the remote") {
			t.Errorf("continue restart with the salvage branch gone = %v, want the no-work refusal", err)
		}
		if n := e.restartJobs(e.task); n != 0 {
			t.Errorf("restart jobs after the refusal = %d, want none", n)
		}
	})
}

// TestContinueRestartRemoteUnreachable: a remote that cannot be asked
// refuses the continue restart rather than seed from an unseen branch.
func TestContinueRestartRemoteUnreachable(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.RemoteBranchExists = func(context.Context, string, string) (bool, error) {
			return false, errors.New("network down")
		}
	})
	author := e.agented("author", "anthropic", "implement")
	g := e.runningWork(author)
	if ans := e.complete(g, guardRefusedStop(SalvageBranch(g.Branch, g.JobID))); ans != AnsOK {
		t.Fatalf("complete the refused run: %s", ans)
	}
	if _, err := e.continueRestart(e.task, ""); err == nil || !strings.Contains(err.Error(), "network down") {
		t.Errorf("continue restart with the remote down = %v, want the remote's error", err)
	}
}

// TestNamesSalvage: the salvage branch counts as named only as a whole
// branch name.
func TestNamesSalvage(t *testing.T) {
	for _, tc := range []struct {
		text, branch string
		want         bool
	}{
		{"x; edits saved on branch b-salvage-1", "b-salvage-1", true},
		{"x; edits saved on branch b-salvage-1 (1 files changed, +1/-0 against main)", "b-salvage-1", true},
		{"x; edits saved on branch b-salvage-12", "b-salvage-1", false},
		{"x; edits could not be saved on branch b-salvage-1: boom", "b-salvage-1", false},
	} {
		if got := namesSalvage(tc.text, tc.branch); got != tc.want {
			t.Errorf("namesSalvage(%q, %q) = %t, want %t", tc.text, tc.branch, got, tc.want)
		}
	}
}
