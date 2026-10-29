package notify

import (
	"strings"
	"testing"
)

// TestMessageNamesTheReviewPlacement: a parked review's text names a
// non-cross_family placement once; cross_family stays unsaid, and an
// unservable review — never leased — names its reason, never a placement
// (issue #126).
func TestMessageNamesTheReviewPlacement(t *testing.T) {
	for _, tc := range []struct {
		placement, want string
	}{
		{"same_family", "(review placement: same_family)"},
		{"same_agent", "(review placement: same_agent)"},
	} {
		_, body, ok := Message(Event{Kind: KindReviewNoVerdict, TaskID: 5, Parked: true,
			Reason: "no verdict in the review result", Placement: tc.placement})
		if !ok || strings.Count(body, "review placement") != 1 || !strings.Contains(body, tc.want) {
			t.Errorf("%s: body %q (ok %v) lacks %q once", tc.placement, body, ok, tc.want)
		}
	}
	_, body, _ := Message(Event{Kind: KindReviewNoVerdict, TaskID: 5, Parked: true, Reason: "x", Placement: "cross_family"})
	if strings.Contains(body, "placement") {
		t.Errorf("cross_family named: %q", body)
	}
	reason := "no reviewer: no agent of a family other than anthropic offers review, review.family=require"
	_, body, _ = Message(Event{Kind: KindReviewUnservable, TaskID: 5, Reason: reason, Placement: "same_family"})
	if !strings.Contains(body, reason) || strings.Contains(body, "placement") {
		t.Errorf("unservable body %q must name the reason and no placement", body)
	}
}
