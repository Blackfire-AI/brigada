package engine

import "testing"

func TestBumpFenceEpoch(t *testing.T) {
	e := newEnv(t)
	if n := e.int("SELECT fence_epoch FROM meta WHERE id = 1"); n != 1 {
		t.Fatalf("fence_epoch starts at %d, want 1", n)
	}
	for _, want := range []int{2, 3} {
		got, err := e.eng.BumpFenceEpoch(e.ctx)
		if err != nil {
			t.Fatalf("bump: %v", err)
		}
		if got != want {
			t.Errorf("bump returned %d, want %d", got, want)
		}
	}

	// New leases carry the current epoch and work normally.
	r := e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	if epoch := e.int("SELECT lease_epoch FROM jobs WHERE id = ?", r.JobID); epoch != 3 {
		t.Errorf("lease_epoch = %d, want 3", epoch)
	}
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Errorf("heartbeat on a current-epoch lease = %s; want ok", ans)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Errorf("complete on a current-epoch lease = %s; want ok", ans)
	}
}

func TestStaleEpochRejected(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	if _, err := e.eng.BumpFenceEpoch(e.ctx); err != nil {
		t.Fatalf("bump: %v", err)
	}

	// The pre-recovery lease, heartbeat, complete and release are all
	// refused with the same distinct answer (§6.4).
	if ans := e.heartbeat(g); ans != AnsStaleEpoch {
		t.Errorf("heartbeat = %s; want stale_epoch", ans)
	}
	if ans := e.complete(g, done()); ans != AnsStaleEpoch {
		t.Errorf("complete = %s; want stale_epoch", ans)
	}
	if ans := e.release(e.runner1, g, ""); ans != AnsStaleEpoch {
		t.Errorf("release = %s; want stale_epoch", ans)
	}
	// None of that touched the job: it still holds the fenced lease.
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != statusLeased {
		t.Errorf("status = %q, want leased untouched", status)
	}
}

func TestCancelThroughFenceEpoch(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	if _, err := e.eng.BumpFenceEpoch(e.ctx); err != nil {
		t.Fatalf("bump: %v", err)
	}

	// Cancel carries no lease, so it still works after a recover, and
	// the fenced-out runner learns of it: cancelled outranks the epoch
	// on the runner's next call.
	if ans := e.cancel(g.JobID); ans != AnsOK {
		t.Fatalf("cancel = %s; want ok through the fence", ans)
	}
	if ans := e.heartbeat(g); ans != AnsCancelled {
		t.Errorf("heartbeat = %s; want cancelled", ans)
	}
}
