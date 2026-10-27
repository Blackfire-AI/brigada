package engine

import (
	"context"
	"errors"
	"testing"
)

// A poll whose caller is gone takes no grant: the job stays queued for the
// runners still polling instead of sitting leased to one that cannot hear.
func TestLeaseOfGoneCallerKeepsJobQueued(t *testing.T) {
	e := newEnv(t)
	job := e.submit(e.work()).JobID

	gone, cancel := context.WithCancel(e.ctx)
	cancel()
	g, err := e.eng.Lease(gone, e.runner1, nil, nil)
	if g != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Lease for a gone caller = %+v, %v; want no grant and context.Canceled", g, err)
	}
	if st := e.str("SELECT status FROM jobs WHERE id = ?", job); st != "queued" {
		t.Fatalf("job status = %q, want queued", st)
	}
	if g := e.leaseAny(e.runner2); g == nil || g.JobID != job {
		t.Fatalf("another runner leased %+v, want job %d", g, job)
	}
}
