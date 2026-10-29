package engine

import "testing"

// reviewPlacementOf is the placement the status shows for the env's task.
func (e *env) reviewPlacementOf() string {
	e.t.Helper()
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range st.Plans {
		for _, task := range p.Tasks {
			if task.ID == e.task {
				return task.ReviewPlacement
			}
		}
	}
	e.t.Fatalf("task %d is not in the status", e.task)
	return ""
}

// TestStatusShowsTheLeasedReviewPlacement: a queued review names no
// placement; once leased, a same_agent review shows it and a
// cross_family one stays unsaid (issue #126).
func TestStatusShowsTheLeasedReviewPlacement(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement", "review")
	review := e.runToReview(author)
	if got := e.reviewPlacementOf(); got != "" {
		t.Errorf("queued review placement = %q, want none", got)
	}
	if g := e.leaseAny(author); g == nil || g.JobID != review {
		t.Fatalf("the author's agent leased %+v, want review %d", g, review)
	}
	if got := e.reviewPlacementOf(); got != placeSameAgent {
		t.Errorf("same-agent review placement = %q, want %q", got, placeSameAgent)
	}
}

func TestStatusLeavesCrossFamilyUnsaid(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "review")
	review := e.runToReview(author)
	if g := e.leaseAny(reviewer); g == nil || g.JobID != review {
		t.Fatalf("the reviewer leased %+v, want review %d", g, review)
	}
	if got := e.reviewPlacementOf(); got != "" {
		t.Errorf("cross_family review placement = %q, want it unsaid", got)
	}
}

func TestPlacementNote(t *testing.T) {
	for kind, want := range map[string]string{
		"":               "",
		placeCrossFamily: "",
		placeSameFamily:  " (review placement: same_family)",
		placeSameAgent:   " (review placement: same_agent)",
		placeUnknown:     " (review placement: unknown)",
	} {
		if got := placementNote(kind); got != want {
			t.Errorf("placementNote(%q) = %q, want %q", kind, got, want)
		}
	}
}
