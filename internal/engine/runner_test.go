package engine

import (
	"testing"
)

func TestRegisterUpsertsByName(t *testing.T) {
	e := newEnv(t)

	first, err := e.eng.Register(e.ctx, "runner-a")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if first == 0 {
		t.Fatal("register returned id 0")
	}
	if got := e.int("SELECT COUNT(*) FROM runners WHERE name = 'runner-a'"); got != 1 {
		t.Fatalf("rows for runner-a = %d, want 1", got)
	}
	again, err := e.eng.Register(e.ctx, "runner-a")
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if again != first {
		t.Errorf("re-register id = %d, want %d: one row per name", again, first)
	}
	other, err := e.eng.Register(e.ctx, "runner-b")
	if err != nil {
		t.Fatalf("register second name: %v", err)
	}
	if other == first {
		t.Errorf("second name id = %d, want a fresh row", other)
	}

	if got, err := e.eng.RunnerID(e.ctx, "runner-a"); err != nil || got != first {
		t.Errorf("RunnerID(runner-a) = %d, %v; want %d", got, err, first)
	}
	if got, err := e.eng.RunnerID(e.ctx, "nobody"); err != nil || got != 0 {
		t.Errorf("RunnerID(nobody) = %d, %v; want 0", got, err)
	}
}

func TestHeartbeatAndCompleteRequireTheLeaseHolder(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	// The lease id alone is not enough: a runner the job was never
	// leased to is lost, same as a wrong lease id (§12.3).
	if ans, _, err := e.eng.Heartbeat(e.ctx, e.runner2, g.JobID, g.LeaseID); err != nil || ans != AnsLost {
		t.Errorf("heartbeat by a non-holder = %s, %v; want lost", ans, err)
	}
	if ans, err := e.eng.Complete(e.ctx, e.runner2, g.JobID, g.LeaseID, done()); err != nil || ans != AnsLost {
		t.Errorf("complete by a non-holder = %s, %v; want lost", ans, err)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != "leased" {
		t.Errorf("status after the non-holder calls = %q, want leased untouched", status)
	}

	// The holder still can.
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Errorf("holder heartbeat = %s, want ok", ans)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Errorf("holder complete = %s, want ok", ans)
	}
}

func TestJobReadsTheCurrentWord(t *testing.T) {
	e := newEnv(t)

	if info, err := e.eng.Job(e.ctx, 99999); err != nil || info != nil {
		t.Fatalf("Job(99999) = %v, %v; want nil, nil", info, err)
	}

	res := e.submit(e.work())
	info, err := e.eng.Job(e.ctx, res.JobID)
	if err != nil {
		t.Fatalf("read the submitted job: %v", err)
	}
	if info.Status != "queued" || info.Key != e.work().Key || info.Kind != KindWork {
		t.Errorf("queued job read back as %+v", info)
	}
	if info.CreatedAt == "" || info.UpdatedAt == "" {
		t.Errorf("timestamps missing from the read: %+v", info)
	}
	if len(info.Labels) != 0 {
		t.Errorf("labels = %v, want the empty list", info.Labels)
	}

	// An expired lease shows as queued, not leased: Job sweeps first.
	g := e.leaseAny(e.runner1)
	e.expireLease()
	info, err = e.eng.Job(e.ctx, g.JobID)
	if err != nil {
		t.Fatalf("read the expired job: %v", err)
	}
	if info.Status != "queued" {
		t.Errorf("status after the lease expired = %q, want queued", info.Status)
	}

	e.complete(g, done())
	info, err = e.eng.Job(e.ctx, g.JobID)
	if err != nil {
		t.Fatalf("read the finished job: %v", err)
	}
	if info.Status != "done" || string(info.Result) == "" {
		t.Errorf("finished job read back as %+v", info)
	}
	if info.LeaseExpiresAt != "" {
		t.Errorf("lease_expires_at = %q, want empty once the lease ended", info.LeaseExpiresAt)
	}
}
