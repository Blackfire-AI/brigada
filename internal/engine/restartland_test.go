package engine

import (
	"testing"
)

// completeLandOf leases the queued land job of task and completes it
// with the landing record rec verbatim, the runner's report of a
// landing (§7.1).
func (e *env) completeLandOf(task, owner int64, rec string) {
	e.t.Helper()
	g := e.leaseGit(owner)
	if g == nil || g.Kind != KindLand || g.TaskID != task {
		e.t.Fatalf("task %d: the land job was not leased: %+v", task, g)
	}
	if ans := e.complete(g, Completion{Result: rec}); ans != AnsOK {
		e.t.Fatalf("task %d: complete the land job: %s", task, ans)
	}
}

// TestRestartDropsWaitingLandingForTheNewHead (issue #205): a landing
// recorded waiting outlived the failed push phase that parked the task,
// so the restart's fresh head could not land — the land rule read the
// stale record as the task's one landing — while push approve still
// released the pre-restart commit. The restart forgets the waiting
// landing, marks the target's waiting ref stale for the next land job of
// that target, and the reworked head lands once approved; nothing of
// the old commit is released again.
func TestRestartDropsWaitingLandingForTheNewHead(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)
	e.completeLandOf(e.task, author, `{"target_branch":"main","commit":"old123","check_passed":true,"pushed":"waiting"}`)

	// The user approves the push; the push phase is rejected and parks
	// the task, the landing still recorded waiting — issue #205's start.
	if res, err := e.eng.PushApprove(e.ctx, envRepo); err != nil || res.Released != 1 {
		t.Fatalf("PushApprove before the failure = (%d, %v), want 1 released", res.Released, err)
	}
	g := e.leaseGit(author)
	if g == nil || g.Kind != KindLand {
		t.Fatalf("the push job was not leased: %+v", g)
	}
	if ans := e.complete(g, Completion{Error: "! [rejected] main -> main (non-fast-forward)"}); ans != AnsOK {
		t.Fatalf("complete the rejected push: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state after the rejected push = %q, want needs_foreman", state)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ? AND pushed = 'waiting'", e.task); n != 1 {
		t.Fatalf("waiting landing records after the rejected push = %d, want 1", n)
	}

	// The foreman restarts the task: the waiting landing is forgotten
	// and the target's waiting ref is marked stale, so nothing releases
	// the old commit again.
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "rework the head"); err != nil {
		t.Fatalf("RestartTask: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != 0 {
		t.Fatalf("landing records after the restart = %d, want the waiting one forgotten", n)
	}
	if n := e.int("SELECT COUNT(*) FROM stale_land_refs WHERE project_id = ? AND target_branch = 'main'", e.project); n != 1 {
		t.Fatalf("stale waiting-ref marks after the restart = %d, want 1", n)
	}
	if res, err := e.eng.PushApprove(e.ctx, envRepo); err != nil || res.Released != 0 {
		t.Fatalf("PushApprove after the restart = (%d, %v), want nothing old released", res.Released, err)
	}

	// The reworked head runs to done and approved: the land rule queues
	// its landing — the stale record no longer refuses it.
	g = e.leaseAny(author)
	if g == nil || g.Kind != KindWork {
		t.Fatalf("the restart's work job was not leased: %+v", g)
	}
	if ans := e.complete(g, doneOn("task/1-2", passCheck)); ans != AnsOK {
		t.Fatalf("complete the reworked work: %s", ans)
	}
	e.runReview(reviewer, `{"verdict":"approve","findings":"`+substantiveFindings+`"}`)
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", e.task); n != 1 {
		t.Fatalf("queued land jobs for the reworked head = %d, want 1", n)
	}

	// The re-made landing's grant tells the runner the waiting ref is
	// stale: delete it before stacking. The new head's commit is the one
	// a later approval pushes.
	lg := e.leaseGit(author)
	if lg == nil || lg.Push || !lg.DropWaitingRef {
		t.Fatalf("the re-made land grant = %+v, want the squash phase with the stale-ref mark", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"new456","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("complete the re-made landing: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ? AND commit_sha = 'new456' AND pushed = 'waiting'", e.task); n != 1 {
		t.Errorf("the re-made landing record = %d rows, want the new head's commit waiting", n)
	}
	if n := e.int("SELECT COUNT(*) FROM stale_land_refs WHERE project_id = ? AND target_branch = 'main'", e.project); n != 0 {
		t.Errorf("stale waiting-ref marks after the re-made landing = %d, want the mark spent", n)
	}
	if res, err := e.eng.PushApprove(e.ctx, envRepo); err != nil || res.Released != 1 {
		t.Fatalf("PushApprove after the re-made landing = (%d, %v), want 1 released", res.Released, err)
	}
	pg := e.leaseGit(author)
	if pg == nil || !pg.Push || pg.LandCommit != "new456" || pg.DropWaitingRef {
		t.Fatalf("the push grant after the restart = %+v, want the new head's commit alone", pg)
	}
}

// TestRestartForgetsTheWaitingStack (issue #205): dropping the
// restarted task's landing is not enough, for every landing stacked
// above it on the waiting ref carries the old commit beneath its own,
// and the one beneath it sits under the same stale history. The restart
// forgets the target's whole waiting stack — exactly as a runner's
// stale-ref drop does — re-queues the bystander's landing, marks the
// ref stale for the next land job of that target, and push approve
// releases nothing of the old stack.
func TestRestartForgetsTheWaitingStack(t *testing.T) {
	for _, c := range []struct {
		name      string
		restart   func(a, b int64) int64
		bystander func(a, b int64) int64
	}{
		{"a landing stacked above the restarted one", func(a, b int64) int64 { return a }, func(a, b int64) int64 { return b }},
		{"a landing beneath the restarted one", func(a, b int64) int64 { return b }, func(a, b int64) int64 { return a }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.set(map[string]string{SettingGatePush: GateAsk})
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			a := e.task
			b := e.addTask("{}")
			e.runTaskToDone(a, author, reviewer)
			e.completeLandOf(a, author, `{"target_branch":"main","commit":"aaa111","check_passed":true,"pushed":"waiting"}`)
			e.runTaskToDone(b, author, reviewer)
			e.completeLandOf(b, author, `{"target_branch":"main","commit":"bbb222","check_passed":true,"pushed":"waiting"}`)

			// The restarted task parks — its push was rejected — and
			// restarts; the other landing of the target is a bystander.
			r, o := c.restart(a, b), c.bystander(a, b)
			e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskNeedsForeman, r)
			if _, err := e.eng.RestartTask(e.ctx, e.project, r, "rework the head"); err != nil {
				t.Fatalf("RestartTask: %v", err)
			}
			for _, task := range []int64{a, b} {
				if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", task); n != 0 {
					t.Errorf("task %d: landing records after the restart = %d, want the stack forgotten", task, n)
				}
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", o); n != 1 {
				t.Errorf("task %d: queued land jobs after the restart = %d, want its landing re-queued", o, n)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", r); n != 0 {
				t.Errorf("task %d: queued land jobs after the restart = %d, want none before the new head is approved", r, n)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND restart = 1", r); n != 1 {
				t.Errorf("task %d: restart work jobs = %d, want the restart to go on", r, n)
			}
			if n := e.int("SELECT COUNT(*) FROM stale_land_refs WHERE project_id = ? AND target_branch = 'main'", e.project); n != 1 {
				t.Fatalf("stale waiting-ref marks after the restart = %d, want 1", n)
			}
			if res, err := e.eng.PushApprove(e.ctx, envRepo); err != nil || res.Released != 0 {
				t.Errorf("PushApprove after the restart = (%d, %v), want nothing of the old stack released", res.Released, err)
			}

			// The bystander's re-made landing is told the ref is stale,
			// and — its last landing waited — it keeps waiting for the
			// user's yes even under auto (heldPushTx).
			e.set(map[string]string{SettingGatePush: GateAuto})
			g := e.leaseGit(author)
			if g == nil || g.TaskID != o || g.Push || !g.DropWaitingRef {
				t.Fatalf("the bystander's re-made land grant = %+v, want the squash phase with the stale-ref mark", g)
			}
			if g.GatePush != GateAsk {
				t.Errorf("the re-made landing's push gate = %q, want ask held for the user's yes", g.GatePush)
			}
			if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"ccc333","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
				t.Fatalf("complete the bystander's re-made landing: %s", ans)
			}
			if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ? AND commit_sha = 'ccc333'", o); n != 1 {
				t.Errorf("task %d: re-made landing records = %d, want its new commit recorded", o, n)
			}
			if n := e.int("SELECT COUNT(*) FROM stale_land_refs WHERE project_id = ? AND target_branch = 'main'", e.project); n != 0 {
				t.Errorf("stale waiting-ref marks after the re-made landing = %d, want the mark spent", n)
			}
		})
	}
}

// TestRestartKeepsSettledLandings (issue #205): only a landing that
// still waits on the target's waiting ref is forgotten — pushed is
// history, and a local landing under a gate no longer never keeps its
// exact hand push command. A waiting landing, and a local one under
// never, are dropped with the ref marked stale.
func TestRestartKeepsSettledLandings(t *testing.T) {
	local := `{"target_branch":"main","commit":"lll111","check_passed":true,"pushed":"local","push_cmd":"git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main"}`
	for _, c := range []struct {
		name     string
		gate     string
		rec      string
		wantKept bool
	}{
		{"pushed is history", GateAsk, `{"target_branch":"main","commit":"ppp111","check_passed":true,"pushed":"pushed"}`, true},
		{"local under ask keeps the hand push", GateAsk, local, true},
		{"local under auto keeps the hand push", GateAuto, local, true},
		{"waiting is dropped", GateAsk, `{"target_branch":"main","commit":"www111","check_passed":true,"pushed":"waiting"}`, false},
		{"local under never is dropped", GateNever, local, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.set(map[string]string{SettingGatePush: c.gate})
			author := e.agented("author", "anthropic", "implement")
			reviewer := e.agented("reviewer", "google", "implement", "review")
			e.runToDone(author, reviewer)
			e.completeLandOf(e.task, author, c.rec)
			e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskNeedsForeman, e.task)
			if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "again"); err != nil {
				t.Fatalf("RestartTask: %v", err)
			}
			if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", e.task); n != map[bool]int64{true: 1, false: 0}[c.wantKept] {
				t.Errorf("landing records after the restart = %d, want %d", n, map[bool]int64{true: 1, false: 0}[c.wantKept])
			}
			if n := e.int("SELECT COUNT(*) FROM stale_land_refs WHERE project_id = ? AND target_branch = 'main'", e.project); n != map[bool]int64{true: 0, false: 1}[c.wantKept] {
				t.Errorf("stale waiting-ref marks after the restart = %d, want %d", n, map[bool]int64{true: 0, false: 1}[c.wantKept])
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'work' AND restart = 1", e.task); n != 1 {
				t.Errorf("restart work jobs = %d, want the restart to go on", n)
			}
		})
	}
}
