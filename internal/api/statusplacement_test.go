package api

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestStatusSectionsShowReviewPlacement: a reviewing task and a reviewing
// plan name a leased review's placement when it is not cross_family, and
// say nothing for cross_family or a review not yet leased (issue #126).
func TestStatusSectionsShowReviewPlacement(t *testing.T) {
	st := Status{Plans: []engine.StatusPlan{
		{ID: 1, Title: "tasks", State: "active", Tasks: []engine.StatusTask{
			{ID: 1, Title: "a", State: "reviewing", ReviewPlacement: "same_family"},
			{ID: 2, Title: "b", State: "reviewing", ReviewPlacement: "same_agent"},
			{ID: 3, Title: "c", State: "reviewing", ReviewPlacement: "unknown"},
			{ID: 4, Title: "d", State: "reviewing"},
		}},
		{ID: 2, Title: "plan", State: "reviewing", ReviewRound: 1, ReviewPlacement: "same_family"},
		{ID: 3, Title: "quiet", State: "reviewing", ReviewRound: 1},
	}}
	got := StatusSections(st)
	for _, want := range []string{
		"task #1 reviewing a  review: same_family",
		"task #2 reviewing b  review: same_agent",
		"task #3 reviewing c  review: unknown",
		"#2 reviewing (round 1) plan\n  review: same_family\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status sections lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "task #4 reviewing d  review") || strings.Contains(got, "#3 reviewing (round 1) quiet\n  review") {
		t.Errorf("a review with no placement to name grew a line:\n%s", got)
	}
}
