package history

import "testing"

// TestRenderReviewPlacement: a review's job_agent_picked line names the
// placement kind the lease recorded (issue #126): all four kinds, the
// non-cross ones plainly, and a pick with no placement as before.
func TestRenderReviewPlacement(t *testing.T) {
	pick := func(payload string) Item {
		return Item{Type: TypeEvent, Kind: "job_agent_picked", JobID: 9, Payload: []byte(payload)}
	}
	for _, tc := range []struct{ name, payload, want string }{
		{"cross_family", `{"agent":3,"pick":"sampled","placement":"cross_family"}`,
			"reviewer agent 3 (cross_family: another family than the author)"},
		{"same_family", `{"agent":3,"pick":"sampled","placement":"same_family"}`,
			"reviewer agent 3 (same_family: same family as the author, review.family=prefer)"},
		{"same_agent", `{"agent":3,"pick":"sampled","placement":"same_agent"}`,
			"reviewer agent 3 (same_agent: the author's own agent, review.family=prefer)"},
		{"unknown", `{"agent":3,"pick":"sampled","placement":"unknown"}`,
			"reviewer agent 3 (unknown: the author's family unknown)"},
		{"a work job's pick", `{"agent":3,"pick":"sampled"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summary(pick(tc.payload)); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}
