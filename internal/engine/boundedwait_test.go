package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// The bounded-waiting and terminal-placement acceptance (issue #263,
// criteria 7-8, re-asserting criterion 2): a full cap queues without
// parking; a cooling wait is bounded and visible; an unservable
// placement names its failed constraints; and an exhausted review retry
// parks with the completed head intact and offers only non-implementation
// ways forward. The foreman exact-pair and the land/sweep invariants hold
// through the suite's existing placement, cap and sweep tests.

// TestCoolingWaitIsBoundedAndVisible: a job whose only agent cools waits
// visibly — the status names the agent and its reset — and past the
// cooling bound the sweep parks it with a reason naming the agent, the
// reset and the bound, instead of holding silently for the reset.
func TestCoolingWaitIsBoundedAndVisible(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.CoolingWait = 30 * time.Minute
		c.RunnerStale = time.Hour // the runner stays seen: the cooling alone is at stake
	})
	codex := e.addAgent(e.runner1, "codex", "gpt-5", "work")
	until := e.coolUntil(codex, 2*time.Hour)
	fix := e.pinnedFix(codex)

	// The wait is visible before the bound: the cooling hold names the
	// agent and its reset.
	e.elapse(10 * time.Minute)
	if got := e.heldLayer(fix.JobID); got != layerCooling {
		t.Fatalf("hold layer = %q, want %q", got, layerCooling)
	}
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(info.Held) != 1 || info.Held[0].Agent != "codex/gpt-5" || info.Held[0].Until != fmtTime(until) {
		t.Fatalf("held = %+v, want codex/gpt-5 cooling until %s", info.Held, fmtTime(until))
	}

	// Past the bound, the sweep parks the job — never a silent hold.
	e.elapse(21 * time.Minute)
	if status := e.str("SELECT status FROM jobs WHERE id = ?", fix.JobID); status != statusFailed {
		t.Fatalf("status past the bound = %q, want failed", status)
	}
	result := e.str("SELECT result FROM jobs WHERE id = ?", fix.JobID)
	for _, want := range []string{"cooling wait exceeded", "codex/gpt-5", "cooling until", capText(30 * time.Minute)} {
		if !strings.Contains(result, want) {
			t.Errorf("result = %q, want it to contain %q", result, want)
		}
	}
	// The fix job's task parked at needs_foreman with the reason, not
	// left queued.
	task := e.int("SELECT task_id FROM jobs WHERE id = ?", fix.JobID)
	if state := e.str("SELECT state FROM tasks WHERE id = ?", task); state != taskNeedsForeman {
		t.Errorf("task state = %q, want needs_foreman past the bound", state)
	}
}

// TestCapFullQueuesWithoutParking re-asserts criterion 2: a job whose
// cap is merely full holds — the status names the layer — and never
// parks however long the hold, until a slot frees.
func TestCapFullQueuesWithoutParking(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Runner = limitOf(1)
		c.LeaseTTL = time.Hour // the filling lease outlives the wait
	})
	e.submit(e.work())
	if g := e.leaseAny(e.runner1); g == nil {
		t.Fatal("nothing to lease")
	}
	held := e.submit(e.workOn(e.addTask("{}")))
	if got := e.leaseAny(e.runner1); got != nil {
		t.Fatalf("the cap-full job leaked out: %+v", got)
	}
	if got := e.heldLayer(held.JobID); got != caps.LayerRunner {
		t.Fatalf("hold layer = %q, want %q", got, caps.LayerRunner)
	}
	e.elapse(e.eng.cfg.QueueTTL / 2)
	if got := e.leaseAny(e.runner1); got != nil {
		t.Fatalf("the cap-full job leaked out mid-window: %+v", got)
	}
	e.elapse(e.eng.cfg.QueueTTL/2 + 2*time.Minute)
	e.mustSweep()
	if st := e.str("SELECT status FROM jobs WHERE id = ?", held.JobID); st != statusQueued {
		t.Fatalf("cap-full job status = %q, want queued: a full cap waits, never parks", st)
	}
}

// TestUnservablePlacementNamesItsConstraints re-asserts criterion 2: a
// review no agent could ever take names the failed constraint — the
// family rule with its setting — in its needs-you line, never a bare
// queue stall.
func TestUnservablePlacementNamesItsConstraints(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Review.Family = ReviewFamilyRequire })
	author := e.agented("author", "anthropic", "implement", "review")
	e.agented("same", "anthropic", "review") // only the author's family: never eligible under require
	e.runToReview(author)
	item, ok := e.needsYouTask(e.task)
	if !ok {
		t.Fatal("no needs-you entry for the unservable review")
	}
	if !strings.Contains(item.Reason, "review.family=require") {
		t.Errorf("needs-you reason = %q, want it to name review.family=require", item.Reason)
	}
}

// TestExhaustedReviewRetriesOfferReviewNotImplementation re-asserts the
// terminal review placement: when the retry bound is spent the task
// parks with the completed head intact — no new implementation attempt —
// and the offers are review-only: retry the review alone, raise the
// review tier, or change the policy. task_restart is never offered.
func TestExhaustedReviewRetriesOfferReviewNotImplementation(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	first := e.agented("first", "google", "review")
	second := e.agented("second", "meta", "review")
	third := e.agented("third", "openai", "review")
	e.runToReview(author)
	for _, runner := range []int64{first, second, third} {
		e.runReview(runner, `{}`)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want needs_foreman", state)
	}
	// The head is intact: one work job, no fix round, no restart.
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind IN ('work', 'fix')", e.task); n != 1 {
		t.Errorf("author jobs = %d, want the original work alone", n)
	}
	p, ok := e.eventPayloadOf(evReviewNoVerdict)
	if !ok {
		t.Fatalf("no review_no_verdict event; have %v", e.eventKinds())
	}
	for _, want := range []string{
		"retry the review alone",
		"raise the review tier",
		"change the review policy",
	} {
		if !strings.Contains(p.Reason, want) {
			t.Errorf("event text = %q, want the offer %q in it", p.Reason, want)
		}
	}
	if strings.Contains(p.Reason, "task_restart") {
		t.Errorf("event text = %q, must never offer task_restart (new implementation work)", p.Reason)
	}
}
