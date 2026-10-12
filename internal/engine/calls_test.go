package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHeartbeatAnswers(t *testing.T) {
	t.Run("no such job", func(t *testing.T) {
		e := newEnv(t)
		if ans := e.heartbeat(&LeaseGrant{JobID: 999, LeaseID: "x"}); ans != AnsGone {
			t.Errorf("heartbeat on a missing job = %s; want gone", ans)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		if ans := e.cancel(g.JobID); ans != AnsOK {
			t.Fatalf("cancel = %s; want ok", ans)
		}
		if ans := e.heartbeat(g); ans != AnsCancelled {
			t.Errorf("heartbeat on a cancelled job = %s; want cancelled", ans)
		}
	})

	t.Run("gone after its own complete", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		if ans := e.complete(g, done()); ans != AnsOK {
			t.Fatalf("complete = %s; want ok", ans)
		}
		if ans := e.heartbeat(g); ans != AnsGone {
			t.Errorf("heartbeat after completing = %s; want gone", ans)
		}
	})

	t.Run("lost for a foreign lease id", func(t *testing.T) {
		e := newEnv(t)
		r := e.submit(e.work())
		g := e.leaseAny(e.runner1)
		if g == nil {
			t.Fatal("nothing to lease")
		}
		if ans := e.heartbeat(&LeaseGrant{JobID: r.JobID, LeaseID: "1-not-this-one"}); ans != AnsLost {
			t.Errorf("heartbeat with a wrong lease id = %s; want lost", ans)
		}
	})

	t.Run("ok extends a healthy lease", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		e.clock.Advance(30 * time.Second)
		ans, expires, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{})
		if err != nil || ans != AnsOK {
			t.Fatalf("heartbeat = %s, %v; want ok", ans, err)
		}
		if want := e.clock.Now().Add(DefaultConfig().LeaseTTL); !expires.Equal(want) {
			t.Errorf("new expiry = %v, want %v", expires, want)
		}
		if got := e.str("SELECT lease_expires_at FROM jobs WHERE id = ?", g.JobID); got != fmtTime(expires) {
			t.Errorf("stored expiry = %q, want %q", got, fmtTime(expires))
		}
		// last_leased holds when the lease started, not the heartbeat.
		if last := e.str("SELECT last_leased FROM jobs WHERE id = ?", g.JobID); last != fmtTime(g.LeaseExpiresAt.Add(-DefaultConfig().LeaseTTL)) {
			t.Errorf("last_leased = %q, want the lease start", last)
		}
	})

	t.Run("revived after expiry", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		g := e.leaseAny(e.runner1)
		e.expireLease()
		if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusQueued {
			t.Fatalf("status after expiry = %q, want queued", status)
		}
		if ans := e.heartbeat(g); ans != AnsOK {
			t.Fatalf("heartbeat on the lapsed lease = %s; want ok (revived)", ans)
		}
		if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased {
			t.Errorf("status after revive = %q, want leased", status)
		}
		if last := e.str("SELECT last_leased FROM jobs WHERE id = ?", g.JobID); last != fmtTime(e.clock.Now()) {
			t.Errorf("last_leased = %q, want it refreshed at the revive", last)
		}
	})

	t.Run("lost once another runner re-leased", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		first := e.leaseAny(e.runner1)
		e.expireLease()
		if g := e.leaseAny(e.runner2); g == nil {
			t.Fatal("the other runner could not re-lease")
		}
		if ans := e.heartbeat(first); ans != AnsLost {
			t.Errorf("heartbeat from the first runner = %s; want lost", ans)
		}
	})
}

func TestCompleteValidatesItsArguments(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	if _, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Completion{Result: "r", Error: "e"}); err == nil {
		t.Error("complete with both result and error succeeded; want refusal")
	}
	if _, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Completion{}); err == nil {
		t.Error("complete with neither result nor error succeeded; want refusal")
	}
	if _, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Completion{Result: "r", Outcome: "finished"}); err == nil {
		t.Error("complete with an outcome that is not a stop outcome succeeded; want refusal")
	}
	if _, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Completion{Error: "e", Outcome: OutcomeStalled}); err == nil {
		t.Error("complete with an outcome beside an error succeeded; want refusal: an outcome rides with a result")
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased {
		t.Errorf("status after the refusals = %q, want leased untouched", status)
	}
}

// TestCompleteStopOutcomeIsStalled covers the §5.6 completion: a run the
// runner stopped itself — stalled, or past its class time cap — ends
// the job as stalled (both outcomes; the result block tells them
// apart), terminal like any other end, with the result it carried kept.
func TestCompleteStopOutcomeIsStalled(t *testing.T) {
	for _, outcome := range []string{OutcomeStalled, OutcomeTimeCap} {
		t.Run(outcome, func(t *testing.T) {
			e := newEnv(t)
			e.submit(e.work())
			g := e.leaseAny(e.runner1)

			c := Completion{
				Result:  fmt.Sprintf(`{"status":%q,"reason":"no activity for over 10m"}`, outcome),
				Outcome: outcome,
			}
			if ans := e.complete(g, c); ans != AnsOK {
				t.Fatalf("complete = %s, want ok", ans)
			}
			if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusStalled {
				t.Errorf("status = %q, want stalled", status)
			}
			if result := e.str("SELECT result FROM jobs WHERE id = ?", g.JobID); !strings.Contains(result, outcome) {
				t.Errorf("result = %q, want the outcome block kept", result)
			}
			// Stalled is terminal: the key is free for a restart (§5.6,
			// §11), and a late completion on the same lease is already.
			if ans := e.complete(g, c); ans != AnsAlready {
				t.Errorf("repeat complete = %s, want already", ans)
			}
		})
	}
}

func TestCompleteAfterExpiryAcceptedUntilReLeased(t *testing.T) {
	t.Run("accepted while nobody re-leased", func(t *testing.T) {
		e := newEnv(t)
		r := e.submit(e.work())
		g := e.leaseAny(e.runner1)
		e.expireLease()
		if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
			t.Fatalf("status after expiry = %q, want queued", status)
		}
		if ans := e.complete(g, done()); ans != AnsOK {
			t.Fatalf("complete after expiry = %s; want ok: the answer beats a re-run", ans)
		}
		if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusDone {
			t.Errorf("status = %q, want done", status)
		}
	})

	t.Run("lost after a re-lease", func(t *testing.T) {
		e := newEnv(t)
		e.submit(e.work())
		first := e.leaseAny(e.runner1)
		e.expireLease()
		second := e.leaseAny(e.runner2)
		if second == nil {
			t.Fatal("the other runner could not re-lease")
		}
		if ans := e.complete(first, done()); ans != AnsLost {
			t.Fatalf("complete with the first lease = %s; want lost", ans)
		}
		if status := e.str("SELECT status FROM jobs WHERE id = ?", second.JobID); status != statusLeased {
			t.Errorf("status = %q, want leased by the second runner", status)
		}
		if ans := e.complete(second, done()); ans != AnsOK {
			t.Fatalf("complete with the live lease = %s; want ok", ans)
		}
	})
}

func TestCompleteIdempotentPerLease(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	c := Completion{
		Result:    `{"status":"done","summary":"finished"}`,
		SessionID: "hs-9",
		Artifacts: []Artifact{{Kind: "transcript", Path: "artifacts/1.jsonl", Size: 42, SHA256: "abc"}},
	}
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete = %s; want ok", ans)
	}
	if ans := e.complete(g, c); ans != AnsAlready {
		t.Errorf("second complete = %s; want already", ans)
	}
	if got := e.int("SELECT COUNT(*) FROM artifacts WHERE job_id = ?", g.JobID); got != 1 {
		t.Errorf("artifacts = %d, want 1: the repeat stored nothing", got)
	}
	// Someone else's lease id gets gone, not already.
	if ans, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, "9-deadbeef", c); err != nil || ans != AnsGone {
		t.Errorf("complete with a foreign lease = %s, %v; want gone", ans, err)
	}
}

func TestCompleteWritesFailureAndSession(t *testing.T) {
	e := newEnv(t)
	p := e.work()
	p.Mode = ModeSession
	e.submit(p)
	g := e.leaseAny(e.runner1)
	if ans := e.complete(g, Completion{Error: "the CLI crashed", SessionID: "hs-2"}); ans != AnsOK {
		t.Fatalf("complete = %s; want ok", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if result := e.str("SELECT result FROM jobs WHERE id = ?", g.JobID); !strings.Contains(result, "the CLI crashed") {
		t.Errorf("result = %q, want the error kept", result)
	}
	if sid := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", p.Key); sid != "hs-2" {
		t.Errorf("harness session id = %q, want hs-2 written back", sid)
	}
}

func TestReleaseIdempotentAndLateCallsLost(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if ans := e.release(e.runner1, g, "re-planned"); ans != AnsOK {
		t.Fatalf("release = %s; want ok", ans)
	}
	if ans := e.release(e.runner1, g, "re-planned"); ans != AnsAlready {
		t.Errorf("second release = %s; want already", ans)
	}
	if leaseID := e.str("SELECT lease_id FROM jobs WHERE id = ?", r.JobID); leaseID != "" {
		t.Errorf("lease_id after release = %q, want it cleared", leaseID)
	}
	// The releaser's late heartbeat and complete are lost: the lease is
	// gone even though the id still matches nothing.
	if ans := e.heartbeat(g); ans != AnsLost {
		t.Errorf("late heartbeat = %s; want lost", ans)
	}
	if ans := e.complete(g, done()); ans != AnsLost {
		t.Errorf("late complete = %s; want lost", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
		t.Errorf("status = %q, want still queued", status)
	}
}

// A release under a foreign name is lost even with the right lease id:
// released_by and its exclusion window belong to the lease's owner
// alone, so no other runner can pin them — or dodge them — on the job.
func TestReleaseByForeignOwnerLost(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}

	if ans := e.release(e.runner2, g, "found a job"); ans != AnsLost {
		t.Fatalf("release by another runner = %s; want lost", ans)
	}
	// Nothing was written: the lease stands and no exclusion was pinned.
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusLeased {
		t.Errorf("status = %q, want leased untouched", status)
	}
	if by := e.int("SELECT COALESCE(released_by, 0) FROM jobs WHERE id = ?", r.JobID); by != 0 {
		t.Errorf("released_by = %d, want none", by)
	}
	if until := e.str("SELECT released_until FROM jobs WHERE id = ?", r.JobID); until != "" {
		t.Errorf("released_until = %q, want none set", until)
	}

	// The owner's own release still works and pins the exclusion on it.
	if ans := e.release(e.runner1, g, "quota cooling"); ans != AnsOK {
		t.Fatalf("release by the owner = %s; want ok", ans)
	}
	if by := e.int("SELECT released_by FROM jobs WHERE id = ?", r.JobID); by != e.runner1 {
		t.Errorf("released_by = %d, want %d", by, e.runner1)
	}
}

func TestReleaseNoteIsBounded(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())
	g := e.leaseAny(e.runner1)

	long := strings.Repeat("x", 600)
	if ans := e.release(e.runner1, g, long); ans != AnsOK {
		t.Fatalf("release = %s; want ok", ans)
	}
	if got := e.str("SELECT release_note FROM jobs WHERE id = ?", r.JobID); len(got) != 500 {
		t.Errorf("release_note length = %d, want 500", len(got))
	}

	// A multi-byte reason is not split at the cut.
	e2 := newEnv(t)
	r2 := e2.submit(e2.work())
	g2 := e2.leaseAny(e2.runner1)
	if ans := e2.release(e2.runner1, g2, "x"+strings.Repeat("é", 300)); ans != AnsOK {
		t.Fatalf("release = %s; want ok", ans)
	}
	note := e2.str("SELECT release_note FROM jobs WHERE id = ?", r2.JobID)
	if len(note) != 499 || !utf8.ValidString(note) {
		t.Errorf("release_note length = %d, valid = %t; want 499 (the cut backs off the split rune) and valid UTF-8", len(note), utf8.ValidString(note))
	}
}

func TestCancelLiveJob(t *testing.T) {
	e := newEnv(t)
	r := e.submit(e.work())
	// A coalesced submit flagged a follow-up; the cancel clears it.
	if second := e.submit(e.work()); second.Answer != AnsPending {
		t.Fatalf("second submit = %s; want pending", second.Answer)
	}
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}

	if ans := e.cancel(r.JobID); ans != AnsOK {
		t.Fatalf("cancel = %s; want ok", ans)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusCancelled {
		t.Errorf("status = %q, want cancelled", status)
	}
	if n := e.int("SELECT followup FROM jobs WHERE id = ?", r.JobID); n != 0 {
		t.Errorf("followup = %d, want 0: the cancel cleared the flag", n)
	}
	if ans := e.cancel(r.JobID); ans != AnsAlready {
		t.Errorf("second cancel = %s; want already", ans)
	}
	// The runner learns on both of its remaining calls.
	if ans := e.heartbeat(g); ans != AnsCancelled {
		t.Errorf("heartbeat after cancel = %s; want cancelled", ans)
	}
	if ans := e.complete(g, done()); ans != AnsCancelled {
		t.Errorf("complete after cancel = %s; want cancelled", ans)
	}
	// A done job cannot be cancelled either.
	done2 := e.submit(e.work())
	g2 := e.leaseAny(e.runner1)
	if e.complete(g2, done()) != AnsOK {
		t.Fatal("second job did not complete")
	}
	if ans := e.cancel(done2.JobID); ans != AnsGone {
		t.Errorf("cancel of a done job = %s; want gone", ans)
	}
}
