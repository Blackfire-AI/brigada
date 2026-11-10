package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The re-queued landing on a stale base (issue #205, related; the rule
// of issue #124): a land_ready hold's re-check and a not-ready retry are
// queued straight from the land job's completion, past the land rule's
// stale-base check, so a dependency rebuilt while they wait must still
// keep the dependent from landing on its old round.

// requeuedStackLand runs lib and mid — mid stacked on lib's round-0
// branch — to mid's first land job under gate_push ask, lib landed
// waiting beneath it, and completes mid's land job with gate. It returns
// the env, both tasks and the agents; mid's re-queued land job waits on
// its not-before.
func requeuedStackLand(t *testing.T, gate Completion) (e *env, lib, mid, author, reviewer int64) {
	t.Helper()
	e = newEnv(t)
	e.set(map[string]string{SettingGatePush: GateAsk})
	author = e.agented("author", "anthropic", "implement")
	reviewer = e.agented("reviewer", "google", "implement", "review")
	res, err := e.eng.CreatePlan(e.ctx, e.project, "re-queued stack", []PlanTask{
		{Name: "lib", Title: "the lib", Spec: "write the lib"},
		{Name: "mid", Title: "the mid", Spec: "use the lib", Deps: []string{"lib"}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lib, mid = res.Tasks[0].TaskID, res.Tasks[1].TaskID
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
	lg := e.leaseGit(author)
	if lg == nil || lg.TaskID != lib || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want lib's land job", lg)
	}
	if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"lib-1","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
		t.Fatalf("land lib waiting: %s", ans)
	}
	if ans := e.complete(g, doneOn(branchOf(mid), passCheck)); ans != AnsOK {
		t.Fatalf("complete mid: %s", ans)
	}
	e.approveReview(reviewer)
	lg = e.leaseGit(author)
	if lg == nil || lg.TaskID != mid || lg.Kind != KindLand {
		t.Fatalf("lease = %+v, want mid's land job", lg)
	}
	if ans := e.complete(lg, gate); ans != AnsOK {
		t.Fatalf("complete mid's land job with %s: %s", gate.Outcome, ans)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", mid); n != 1 {
		t.Fatalf("mid's re-queued land jobs = %d, want 1", n)
	}
	return e, lib, mid, author, reviewer
}

// requeueDelay is how long each gate answer's re-queue waits.
func requeueDelay(gate Completion) time.Duration {
	if gate.Outcome == OutcomeLandHeld {
		return landReadyRecheckDelay
	}
	return retryDelayFor(1)
}

// TestRequeuedLandOnStaleBaseIsRefused: mid's landing is re-queued by
// the land_ready gate — not ready, or held with the engine's re-check —
// then lib's push is rejected and lib is rebuilt from -r1. The re-queued
// job must not lease on the old round: it is cancelled at its lease,
// mid parks with task_base_stale naming both refs, and nothing lands.
func TestRequeuedLandOnStaleBaseIsRefused(t *testing.T) {
	for _, gate := range []Completion{landNotReady(2, "ci pending"), landHeld("ci failed")} {
		t.Run(gate.Outcome, func(t *testing.T) {
			e, lib, mid, author, reviewer := requeuedStackLand(t, gate)
			if _, err := e.eng.PushApprove(e.ctx, envRepo); err != nil {
				t.Fatalf("approve the push: %v", err)
			}
			pg := e.leaseGit(author)
			if pg == nil || pg.TaskID != lib || !pg.Push {
				t.Fatalf("lease = %+v, want lib's push job", pg)
			}
			if ans := e.complete(pg, Completion{Error: "push rejected: non-fast-forward"}); ans != AnsOK {
				t.Fatalf("fail lib's push: %s", ans)
			}
			if _, err := e.eng.RestartTask(e.ctx, e.project, lib, "rebuild on the target"); err != nil {
				t.Fatalf("restart lib: %v", err)
			}
			r1 := fmt.Sprintf("brigada/task-%d-r1", lib)
			g := e.leaseAny(author)
			if g == nil || g.TaskID != lib || g.Branch != r1 {
				t.Fatalf("lease = %+v, want lib's restarted work on %s", g, r1)
			}
			if ans := e.complete(g, doneOn(r1, passCheck)); ans != AnsOK {
				t.Fatalf("complete the restarted lib: %s", ans)
			}
			e.approveReview(reviewer)

			e.clock.Advance(requeueDelay(gate) + time.Second)
			for lg := e.leaseGit(author); lg != nil; lg = e.leaseGit(author) {
				if lg.TaskID == mid {
					t.Fatalf("mid's re-queued land job leased on the stale base: %+v", lg)
				}
				if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"lib-2","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
					t.Fatalf("land lib again: %s", ans)
				}
			}
			if state := e.str("SELECT state FROM tasks WHERE id = ?", mid); state != taskNeedsForeman {
				t.Fatalf("mid state = %q, want needs_foreman on its stale base", state)
			}
			p, ok := e.taskEvent(mid, evTaskBaseStale)
			if !ok || !strings.Contains(p.Reason, "the landing is refused") ||
				!strings.Contains(p.Reason, branchOf(lib)) || !strings.Contains(p.Reason, r1) {
				t.Errorf("mid's task_base_stale = %+v (%t), want the refusal naming both refs", p, ok)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land' AND status IN ('queued', 'leased', 'running')", mid); n != 0 {
				t.Errorf("mid's live land jobs = %d, want the re-queued one cancelled", n)
			}
			if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", mid); n != 0 {
				t.Errorf("mid landings = %d, want none on the old round", n)
			}
			if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", mid); n != 0 {
				t.Errorf("mid's held land holds = %d, want the refused landing's hold dropped", n)
			}
		})
	}
}

// TestRequeuedLandOnFreshBaseLands: with no rebuild beneath it, mid's
// re-queued landing keeps the gate's own bookkeeping — the not-ready
// backoff and land_retry_at, the hold and its re-check — leases after
// its not-before, and lands as any landing does.
func TestRequeuedLandOnFreshBaseLands(t *testing.T) {
	for _, gate := range []Completion{landNotReady(2, "ci pending"), landHeld("ci failed")} {
		t.Run(gate.Outcome, func(t *testing.T) {
			e, _, mid, author, _ := requeuedStackLand(t, gate)
			want := fmtTime(e.clock.Now().Add(requeueDelay(gate)))
			if got := e.str("SELECT not_before FROM jobs WHERE task_id = ? AND kind = 'land' AND status = 'queued'", mid); got != want {
				t.Errorf("the re-queue's not_before = %q, want %q", got, want)
			}
			if gate.Outcome == OutcomeLandNotReady {
				if got := e.str("SELECT land_retry_at FROM tasks WHERE id = ?", mid); got != want {
					t.Errorf("mid land_retry_at = %q, want %q", got, want)
				}
			} else if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'held'", mid); n != 1 {
				t.Errorf("mid's held land holds = %d, want the hold kept beside its re-check", n)
			}
			e.clock.Advance(requeueDelay(gate) + time.Second)
			lg := e.leaseGit(author)
			if lg == nil || lg.TaskID != mid || lg.Kind != KindLand || lg.Push {
				t.Fatalf("lease = %+v, want mid's re-queued squash", lg)
			}
			if ans := e.complete(lg, Completion{Result: `{"target_branch":"main","commit":"mid-1","check_passed":true,"pushed":"waiting"}`}); ans != AnsOK {
				t.Fatalf("land mid: %s", ans)
			}
			if pushed := e.str("SELECT pushed FROM landings WHERE task_id = ?", mid); pushed != "waiting" {
				t.Errorf("mid landing = %q, want it recorded waiting", pushed)
			}
			if _, ok := e.taskEvent(mid, evTaskBaseStale); ok {
				t.Error("a fresh base was refused as stale")
			}
		})
	}
}
