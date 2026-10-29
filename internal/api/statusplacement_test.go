package api

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestStatusSectionsShowReviewPlacement: a reviewing task and a plan
// review placed off cross_family say so; cross_family stays unsaid
// because the engine leaves it empty (issue #126).
func TestStatusSectionsShowReviewPlacement(t *testing.T) {
	out := StatusSections(Status{Plans: []engine.StatusPlan{{
		ID: 1, Title: "p", State: "reviewing", ReviewRound: 1, ReviewPlacement: "same_agent",
		Tasks: []engine.StatusTask{
			{ID: 2, State: "reviewing", Title: "a", ReviewPlacement: "same_family"},
			{ID: 3, State: "reviewing", Title: "b"},
		},
	}}})
	for _, want := range []string{"  review: same_agent\n", "review: same_family"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "review:") != 2 {
		t.Errorf("status names a placement for the unplaced task:\n%s", out)
	}
}
