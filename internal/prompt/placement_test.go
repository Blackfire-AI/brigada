package prompt

import (
	"strings"
	"testing"
)

// TestWithPlacement: the review, plan review and re-review prompts state
// the placement the lease made (issue #126). Cross-family keeps the
// composed text; same-family, same-agent and unknown each say their own,
// never that the reviewer is of another family.
func TestWithPlacement(t *testing.T) {
	review := ComposeReviewPrompt("spec", "", "", "main", "br")
	plan := ComposePlanReviewPrompt("plan")
	reReview := ComposeReReviewPrompt("fixed", "", "main", "br", "abc")
	standalone := ComposeStandaloneReviewPrompt("main", "br")
	for name, p := range map[string]string{"review": review, "plan review": plan, "re-review": reReview, "standalone": standalone} {
		for _, kind := range []string{"", PlacementCrossFamily} {
			if got := WithPlacement(p, kind); got != p {
				t.Errorf("%s placed %q changed the composed prompt", name, kind)
			}
		}
	}
	cases := []struct {
		name, prompt, kind string
		want               []string
	}{
		{"review same_family", review, PlacementSameFamily, []string{"You were placed same-family", "a different\nagent of the author's own model family", "as independently as a reviewer"}},
		{"review same_agent", review, PlacementSameAgent, []string{"You run on the author's own agent and model", "fresh session of your own", "no access to the author's session", "as independently as a reviewer"}},
		{"review unknown", review, PlacementUnknown, []string{"The author's model family is not known", "as independently as a reviewer"}},
		{"standalone unknown", standalone, PlacementUnknown, []string{"The author's model family is not known"}},
		{"plan same_family", plan, PlacementSameFamily, []string{"You were placed same-family", "the foreman, which wrote the plan", "as independently as a reviewer"}},
		{"plan same_agent", plan, PlacementSameAgent, []string{"You run on the foreman's own agent and model", "no access to the foreman's session", "as independently as a reviewer"}},
		{"plan unknown", plan, PlacementUnknown, []string{"The model family of the foreman", "is not known"}},
		{"re-review same_family", reReview, PlacementSameFamily, []string{"You were placed same-family", "as independently as a reviewer"}},
		{"re-review same_agent", reReview, PlacementSameAgent, []string{"You run on the author's own agent and model", "your own review\nsession", "no access to the author's session"}},
		{"re-review unknown", reReview, PlacementUnknown, []string{"The author's model family is not known"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WithPlacement(tc.prompt, tc.kind)
			if !strings.HasPrefix(got, ReviewerOpener) {
				t.Errorf("placed prompt does not open with %q", ReviewerOpener)
			}
			if strings.Contains(got, "cross-family") {
				t.Errorf("placed prompt still says cross-family:\n%s", got)
			}
			// The sentences wrap where they wrap: match across lines.
			flat := strings.ReplaceAll(got, "\n", " ")
			for _, w := range tc.want {
				if !strings.Contains(flat, strings.ReplaceAll(w, "\n", " ")) {
					t.Errorf("placed prompt lacks %q:\n%s", w, got)
				}
			}
			// The rest of the prompt is untouched: the verdict contract
			// and the data still follow.
			if !strings.Contains(got, "End your final message with exactly one JSON verdict block") {
				t.Errorf("placed prompt lost its verdict contract")
			}
			if len(got) <= len(tc.prompt)-len(reviewPlacedCross) {
				t.Errorf("placed prompt lost text: %d bytes from %d", len(got), len(tc.prompt))
			}
		})
	}
	// The re-review's placement paragraph follows its first one.
	got := WithPlacement(reReview, PlacementSameAgent)
	first, _, _ := strings.Cut(reReview, "\n\n")
	if !strings.HasPrefix(got, first+"\n\n"+reReviewPlaced[PlacementSameAgent]+"\n\n") {
		t.Errorf("re-review placement is not the second paragraph:\n%s", got)
	}
	// A worker's prompt is no reviewer's: nothing to place.
	if w := ComposeWorkerPrompt("spec", "", ""); WithPlacement(w, PlacementSameAgent) != w {
		t.Error("a worker prompt was placed")
	}
}
