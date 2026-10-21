package engine

import (
	"strconv"
	"strings"
	"testing"
)

// The loop tests (§5.6): the two repetition signals, and their route to
// the foreman — the inbox and the wake a stall parks through, the loop
// as the reason.

// failCheckWith is a completion whose check failed with the given
// output tail (§5.5): what a check-loop round answers with.
func failCheckWith(output string) Completion {
	c := done()
	c.CheckResult = `{"exit_code":1,"timed_out":false,"output_tail":` + strconv.Quote(output) + `}`
	return c
}

// TestCheckLoopParksTheTask: the same check failure — the same failing
// test names — across consecutive fix rounds parks the task at
// needs_foreman as a loop (§5.6) instead of queueing another round, even
// with rounds left, and the attempt counts as a spent round's. The
// noise between rounds (timings, paths) does not break the chain.
func TestCheckLoopParksTheTask(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.ExactRoundCaps = &RoundCaps{Fix: 5, Review: 2, Restart: 3} // rounds left when the loop trips
	})
	author := e.agented("author", "anthropic", "implement")

	e.submit(e.work())
	e.runWork(author, failCheckWith("--- FAIL: TestBoom\nthe frobnicator was off"))
	e.runWork(author, failCheckWith("--- FAIL: TestBoom (0.31s)\n/tmp/br-41/src/pkg/a_test.go:12: boom"))
	e.runWork(author, failCheckWith("--- FAIL: TestBoom (0.87s)\n/tmp/br-87/src/pkg/a_test.go:12: boom"))

	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want %q; have %v", state, taskNeedsForeman, e.eventKinds())
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 2 {
		t.Errorf("fix jobs = %d, want 2: the loop parks before a third round", n)
	}
	p, ok := e.eventPayloadOf(evTaskLoop)
	if !ok {
		t.Fatalf("no %s event; have %v", evTaskLoop, e.eventKinds())
	}
	if !strings.Contains(p.Reason, "the same check failure") || !strings.Contains(p.Reason, "TestBoom") {
		t.Errorf("loop reason = %q, want the repetition and the failing test named", p.Reason)
	}
	if got := e.eventKinds(); containsKind(got, evTaskRestarted) || containsKind(got, evTaskEffortBumped) {
		t.Errorf("the loop park escalated anyway: %v", got)
	}
	// The attempt counts, as a spent round's does.
	if n := e.int("SELECT COALESCE(SUM(attempts), 0) FROM outcomes"); n != 1 {
		t.Errorf("outcome attempts = %d, want the loop's attempt counted", n)
	}
}

// TestCheckLoopNeedsTheSameFailure: a failure that changes between
// rounds is work, not a loop (§5.6): the chain breaks at the first
// different fingerprint and the fix rounds go on.
func TestCheckLoopNeedsTheSameFailure(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.ExactRoundCaps = &RoundCaps{Fix: 5, Review: 2, Restart: 3}
	})
	author := e.agented("author", "anthropic", "implement")

	e.submit(e.work())
	e.runWork(author, failCheckWith("--- FAIL: TestBoom"))
	e.runWork(author, failCheckWith("--- FAIL: TestOther")) // a different failure: no chain
	e.runWork(author, failCheckWith("--- FAIL: TestOther")) // its first repeat only

	if containsKind(e.eventKinds(), evTaskLoop) {
		t.Fatalf("a changed failure looped: %v", e.eventKinds())
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 3 {
		t.Errorf("fix jobs = %d, want 3: a changed failure earns its rounds", n)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state = %q, want running behind the next round", state)
	}
}

// TestCheckLoopFingerprintNormalisesTheTail: with no failing test names
// to read, the fingerprint falls to the hash of the normalised output
// tail (§5.6): timings, paths, durations and addresses stripped, so the
// same failure in different runs and worktrees reads the same.
func TestCheckLoopFingerprintNormalisesTheTail(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.ExactRoundCaps = &RoundCaps{Fix: 5, Review: 2, Restart: 3}
	})
	author := e.agented("author", "anthropic", "implement")

	e.submit(e.work())
	e.runWork(author, failCheckWith("error: /tmp/br-1/src/a.go:31: widget jammed (0.12s) at 0x7f00ffa1"))
	e.runWork(author, failCheckWith("error: /tmp/br-2/src/a.go:31: widget jammed (1.30s) at 0x7f00ffb2"))
	e.runWork(author, failCheckWith("error: /tmp/br-3/src/a.go:31: widget jammed (0.44s) at 0x7f00ffc3"))

	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want %q; have %v", state, taskNeedsForeman, e.eventKinds())
	}
	if !containsKind(e.eventKinds(), evTaskLoop) {
		t.Fatalf("the normalised tail did not loop: %v", e.eventKinds())
	}
	p, _ := e.eventPayloadOf(evTaskLoop)
	if !strings.Contains(p.Reason, "the same normalised check output") {
		t.Errorf("loop reason = %q, want the fingerprint named", p.Reason)
	}
}

// TestHeartbeatLoopReportRoutesToForeman: a runner's loop report — the
// same tool call, repeated — lands as one task_loop event whatever the
// beats repeat, wakes a foreman turn, and stops nothing: the job keeps
// its lease and the task keeps running (§5.6). A foreman turn reports
// nothing: it never routes to itself, as for a stall.
func TestHeartbeatLoopReportRoutesToForeman(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.offerForeman(e.runner1)

	e.submit(e.work())
	g := e.leaseAny(author)
	if _, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID,
		Progress{Loop: true, LoopCall: `Read {"file_path":"a.go"}`}); err != nil {
		t.Fatalf("heartbeat the loop report: %v", err)
	}

	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND job_id = ?", evTaskLoop, g.JobID); n != 1 {
		t.Fatalf("task_loop events = %d, want one", n)
	}
	p, _ := e.eventPayloadOf(evTaskLoop)
	if !strings.Contains(p.Reason, "the same tool call") || !strings.Contains(p.Reason, `Read {"file_path":"a.go"}`) {
		t.Errorf("loop reason = %q, want the repetition and the call named", p.Reason)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = ?", KindForeman); n != 1 {
		t.Errorf("foreman turns = %d, want the loop's wake", n)
	}
	if state := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); state != statusLeased {
		t.Errorf("job status = %q, want %q: a loop kills nothing", state, statusLeased)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state = %q, want running: the report stops nothing", state)
	}

	// The report rides every beat until the run ends; one event is the
	// whole signal.
	if _, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID,
		Progress{Loop: true, LoopCall: `Read {"file_path":"a.go"}`}); err != nil {
		t.Fatalf("heartbeat the loop report again: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND job_id = ?", evTaskLoop, g.JobID); n != 1 {
		t.Errorf("task_loop events after the second beat = %d, want still one", n)
	}

	// A beat without the signal reports nothing.
	if _, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evTaskLoop); n != 1 {
		t.Errorf("task_loop events = %d, want one still", n)
	}

	// A foreman turn never routes to itself (§5.6), a stall's rule.
	e.complete(g, doneOn("task/1-1", passCheck))
	fg := e.submit(e.foremanTurn())
	foreman := e.leaseClaude(e.runner1)
	if foreman == nil || foreman.JobID != fg.JobID {
		t.Fatalf("the foreman turn was not leased: %+v", foreman)
	}
	if _, _, err := e.eng.Heartbeat(e.ctx, foreman.LeaseOwner, foreman.JobID, foreman.LeaseID,
		Progress{Loop: true, LoopCall: "status"}); err != nil {
		t.Fatalf("heartbeat the foreman turn: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ? AND job_id = ?", evTaskLoop, fg.JobID); n != 0 {
		t.Errorf("a foreman turn's loop reported = %d, want none: it never routes to itself", n)
	}
}

// TestCheckFingerprint covers the fingerprint itself (§5.6): the
// failing test names when the output carries them, stable under the
// noise between runs; else the hash of the normalised tail; "" when the
// check said nothing.
func TestCheckFingerprint(t *testing.T) {
	names := checkFingerprint("--- FAIL: TestOne (0.12s)\n--- FAIL: TestTwo (9.8s)")
	if names != "tests:TestOne TestTwo" {
		t.Errorf("names fingerprint = %q", names)
	}
	if noisy := checkFingerprint("--- FAIL: TestOne (0.99s)\n--- FAIL: TestTwo (1.00s)\n/tmp/x/src/a_test.go:3: nope"); noisy != names {
		t.Errorf("noise changed the names fingerprint: %q vs %q", noisy, names)
	}
	if other := checkFingerprint("--- FAIL: TestThree (0.12s)"); other == names {
		t.Error("different failing tests share a fingerprint")
	}
	quiet1 := checkFingerprint("boom at /tmp/br-1/x.go:7 (0.1s) ptr 0x1a")
	quiet2 := checkFingerprint("boom at /tmp/br-2/x.go:7 (1.9s) ptr 0x2b")
	if quiet1 == "" || quiet1 != quiet2 {
		t.Errorf("hash fingerprint = %q and %q, want equal and non-empty", quiet1, quiet2)
	}
	if changed := checkFingerprint("a different boom at /tmp/br-1/x.go:7 (0.1s)"); changed == quiet1 {
		t.Error("a different tail shares the hash fingerprint")
	}
	if empty := checkFingerprint("  \n "); empty != "" {
		t.Errorf("an empty output fingerprinted as %q", empty)
	}
}

// containsKind reports whether kinds holds kind.
func containsKind(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
