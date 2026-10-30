package engine

import (
	"testing"
	"time"
)

// TestLeaseWalkStampsItsOwnTime covers a job submitted between a lease
// poll's arrival transaction and its queue walk: the submit commits
// with a later time than the arrival read, so the walk must stamp the
// lease with its own time, or history (ordered by created_at, id) would
// show the job leased before it was created.
func TestLeaseWalkStampsItsOwnTime(t *testing.T) {
	e := newEnv(t)
	var job int64
	e.eng.betweenLeaseTx = func() {
		e.eng.betweenLeaseTx = nil
		e.clock.Advance(time.Minute)
		job = e.submit(e.work()).JobID
		e.clock.Advance(time.Minute)
	}
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != job {
		t.Fatalf("lease = %+v, want the job submitted mid-poll (%d)", g, job)
	}
	created := e.str("SELECT created_at FROM events WHERE kind = ? AND job_id = ?", evJobCreated, job)
	if created == "" {
		t.Fatalf("no job_created event for job %d", job)
	}
	for _, typ := range []string{evJobLeased, evJobAgentPicked, evTaskRunning} {
		at := e.str("SELECT COALESCE(MAX(created_at), '') FROM events WHERE kind = ? AND job_id = ?", typ, job)
		if at == "" {
			continue
		}
		if at < created {
			t.Errorf("%s at %s precedes job_created at %s", typ, at, created)
		}
	}
	if leased := e.str("SELECT created_at FROM events WHERE kind = ? AND job_id = ?", evJobLeased, job); leased == "" {
		t.Fatalf("no job_leased event for job %d", job)
	}
}
