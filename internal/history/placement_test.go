package history

import (
	"strings"
	"testing"
)

// TestLinePlacement: a review's job_agent_picked line names the placement
// kind; all four read plainly, and unknown is never shown as cross_family
// (issue #126). A pick without a placement renders as before.
func TestLinePlacement(t *testing.T) {
	cases := []struct {
		placement string
		want      []string
	}{
		{"cross_family", []string{"reviewer agent 4", "another family", "placement=cross_family"}},
		{"same_family", []string{"reviewer agent 4", "same family as the author", "placement=same_family"}},
		{"same_agent", []string{"reviewer agent 4", "author's own agent", "placement=same_agent"}},
		{"unknown", []string{"reviewer agent 4", "author's family unknown", "placement=unknown"}},
	}
	for _, tc := range cases {
		it := Item{Type: TypeEvent, Kind: "job_agent_picked", TaskID: 3, JobID: 9,
			Payload: []byte(`{"agent":4,"pick":"sampled","placement":"` + tc.placement + `"}`)}
		got := Line(it)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: line %q lacks %q", tc.placement, got, w)
			}
		}
		if tc.placement == "unknown" && strings.Contains(got, "cross_family") {
			t.Errorf("unknown shown as cross_family: %q", got)
		}
	}
	plain := Item{Type: TypeEvent, Kind: "job_agent_picked", TaskID: 3, JobID: 9, Payload: []byte(`{"agent":4,"pick":"sampled"}`)}
	if got := Line(plain); strings.Contains(got, "placement") {
		t.Errorf("a pick with no placement rendered %q", got)
	}
}
