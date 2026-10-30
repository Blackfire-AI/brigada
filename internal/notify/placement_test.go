package notify

import (
	"strings"
	"testing"
)

// TestReasonNamesANonCrossFamilyPlacementOnce: a review verdict from a
// same_family or same_agent placement says so once; cross_family stays
// unsaid (issue #126).
func TestReasonNamesANonCrossFamilyPlacementOnce(t *testing.T) {
	for _, tc := range []struct{ kind, payload, want string }{
		{KindReviewChanges, `{"findings":"off by one","placement":"same_family"}`,
			"review placement same_family: off by one"},
		{KindReviewNoVerdict, `{"reason":"no verdict in the review result","placement":"same_agent"}`,
			"review placement same_agent: no verdict in the review result"},
		{KindReviewChanges, `{"findings":"off by one","placement":"cross_family"}`, "off by one"},
		{KindReviewChanges, `{"findings":"off by one"}`, "off by one"},
	} {
		got := reasonOf(tc.kind, tc.payload)
		if got != tc.want {
			t.Errorf("reasonOf(%s, %s) = %q, want %q", tc.kind, tc.payload, got, tc.want)
		}
	}
	_, body, ok := Message(Event{Kind: KindReviewChanges, Parked: true, TaskID: 6,
		Reason: reasonOf(KindReviewChanges, `{"findings":"nit","placement":"same_agent"}`)})
	if !ok || strings.Count(body, "same_agent") != 1 {
		t.Errorf("banner = %q (%t), want the placement named once", body, ok)
	}
}

// TestUnservableReviewNamesNoPlacement: a review nobody was picked for
// carries its unservable reason, never a placement label.
func TestUnservableReviewNamesNoPlacement(t *testing.T) {
	reason := "no reviewer: needs a reviewer of another family than the author (claude), review.family=require"
	_, body, ok := Message(Event{Kind: KindReviewUnservable, TaskID: 4, Reason: reason})
	if !ok || !strings.Contains(body, reason) {
		t.Fatalf("banner = %q (%t), want the unservable reason", body, ok)
	}
	for _, kind := range []string{"cross_family", "same_family", "same_agent", "unknown"} {
		if strings.Contains(body, kind) {
			t.Errorf("banner %q names the placement %q of a review nobody took", body, kind)
		}
	}
}
