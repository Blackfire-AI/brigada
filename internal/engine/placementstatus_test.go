package engine

import (
	"strings"
	"testing"
)

// reviewingPlacement is the review placement status shows for the
// plan's reviewing task, "" when it shows none.
func (e *env) reviewingPlacement() string {
	e.t.Helper()
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	for _, p := range st.Plans {
		for _, tk := range p.Tasks {
			if tk.State == taskReviewing {
				return tk.ReviewPlacement
			}
		}
	}
	e.t.Fatal("status lists no reviewing task")
	return ""
}

// TestStatusNamesASameAgentReviewPlacement: a leased review the author's
// own agent took shows its placement in status, and the changes verdict
// that follows carries it once into the needs-you and wake words
// (issue #126).
func TestStatusNamesASameAgentReviewPlacement(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement", "review")
	review := e.runToReview(author)

	// Queued, unleased: no reviewer picked, so no placement.
	if got := e.reviewingPlacement(); got != "" {
		t.Fatalf("a waiting review shows placement %q, want none", got)
	}
	g := e.leaseAny(author)
	if g == nil || g.JobID != review {
		t.Fatalf("the author did not lease its review: %+v", g)
	}
	if got := e.reviewingPlacement(); got != placeSameAgent {
		t.Errorf("status placement = %q, want %q", got, placeSameAgent)
	}
	if ans := e.complete(g, Completion{Result: `{"verdict":"changes","findings":"fix this"}`}); ans != AnsOK {
		t.Fatalf("complete the review: %s", ans)
	}
	p, ok := e.eventPayloadOf(evReviewChanges)
	if !ok || p.Placement != placeSameAgent {
		t.Fatalf("review_changes = %+v (%t), want placement %q", p, ok, placeSameAgent)
	}
	raw := `{"findings":"fix this","placement":"same_agent"}`
	if got := wakeEventText(evReviewChanges, raw); got != "fix this (review placement: same_agent)" {
		t.Errorf("wake text = %q", got)
	}
	if got := needsYouReason(evReviewChanges, raw); got != "fix this (review placement: same_agent)" {
		t.Errorf("needs-you reason = %q", got)
	}
	// Cross_family is the default and stays unsaid.
	cross := `{"findings":"fix this","placement":"cross_family"}`
	if got := wakeEventText(evReviewChanges, cross); got != "fix this" {
		t.Errorf("cross_family wake text = %q, want the findings alone", got)
	}
}

// TestStatusSaysNothingForCrossFamilyOrUnservable: a cross_family review
// shows no placement, and a review no reviewer can take (review.family=
// require, one family) shows none either — its needs-you entry names the
// unservable reason (issue #126).
func TestStatusSaysNothingForCrossFamilyOrUnservable(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "review")
	e.runToReview(author)
	if g := e.leaseAny(reviewer); g == nil || g.Kind != KindReview {
		t.Fatalf("the other family did not lease the review: %+v", g)
	}
	if got := e.reviewingPlacement(); got != "" {
		t.Errorf("cross_family placement shown as %q, want none", got)
	}

	r := newRequireEnv(t)
	solo := r.agented("solo", "anthropic", "implement", "review")
	r.runToReview(solo)
	if g := r.leaseAny(solo); g != nil {
		t.Fatalf("the author's agent leased its review under require: %+v", g)
	}
	if got := r.reviewingPlacement(); got != "" {
		t.Errorf("an unleased review shows placement %q, want none", got)
	}
	if !strings.Contains(noReviewerEntry("anthropic", ReviewFamilyRequire), "review.family=require") {
		t.Errorf("the unservable reason lacks review.family=require")
	}
}
