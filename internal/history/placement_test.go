package history

import "testing"

// TestRenderReviewPlacement: a review's job_agent_picked line names the
// placement kind the lease recorded (issue #126): all four kinds, the
// non-cross ones plainly. The pick word follows every placement.
func TestRenderReviewPlacement(t *testing.T) {
	pick := func(payload string) Item {
		return Item{Type: TypeEvent, Kind: "job_agent_picked", JobID: 9, Payload: []byte(payload)}
	}
	for _, tc := range []struct{ name, payload, want string }{
		{"cross_family", `{"agent":3,"pick":"sampled","placement":"cross_family"}`,
			"reviewer agent 3 (cross_family: another family than the author) pick=sampled"},
		{"same_family", `{"agent":3,"pick":"sampled","placement":"same_family"}`,
			"reviewer agent 3 (same_family: same family as the author, review.family=prefer) pick=sampled"},
		{"same_agent", `{"agent":3,"pick":"sampled","placement":"same_agent"}`,
			"reviewer agent 3 (same_agent: the author's own agent, review.family=prefer) pick=sampled"},
		{"unknown", `{"agent":3,"pick":"sampled","placement":"unknown"}`,
			"reviewer agent 3 (unknown: the author's family unknown) pick=sampled"},
		{"a work job's pick", `{"agent":3,"pick":"sampled"}`, "agent 3 pick=sampled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summary(pick(tc.payload)); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderPickedDecision: a job_agent_picked line carries the whole
// placement decision (§8) — score, cost, draw, the tier asked and
// served, the family policy and fallback, a pin or session, the retry
// exclusions in force, and every dropped candidate with the reason it
// was dropped — so an operator can read why an agent won.
func TestRenderPickedDecision(t *testing.T) {
	pick := func(payload string) Item {
		return Item{Type: TypeEvent, Kind: "job_agent_picked", JobID: 9, Payload: []byte(payload)}
	}
	tests := []struct{ name, payload, want string }{
		{
			"sampled work pick with removals",
			`{"agent":4,"pick":"sampled","score":0.75,"weight":2,"draw":0.6,"tier_asked":"standard","tier_served":"standard","exclude_agents":[1,2],"removed":[{"agent":1,"reason":"class implement not offered"},{"agent":2,"reason":"broken"}]}`,
			"agent 4 pick=sampled score=0.75 weight=2 draw=0.6 tier=standard excluded=1,2 removed=1: class implement not offered, 2: broken",
		},
		{
			"prefer review falls to same family",
			`{"agent":3,"pick":"sampled","score":0.5,"weight":2,"tier_asked":"standard","tier_served":"standard","placement":"same_family","family_policy":"prefer","fallback":"same_family"}`,
			"reviewer agent 3 (same_family: same family as the author, review.family=prefer) pick=sampled score=0.5 weight=2 tier=standard family=prefer fallback=same_family",
		},
		{
			"a pinned session fix",
			`{"agent":7,"pick":"pinned","score":0.5,"weight":3,"tier_asked":"heavy","tier_served":"heavy","pin":7,"session":true}`,
			"agent 7 pick=pinned score=0.5 weight=3 tier=heavy pin=7 session",
		},
		{
			"a tier climb",
			`{"agent":9,"pick":"sampled","score":0.5,"weight":3,"tier_asked":"light","tier_served":"standard"}`,
			"agent 9 pick=sampled score=0.5 weight=3 tier=light->standard",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := summary(pick(tc.payload)); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderCapHeld: a job_cap_held line names the layer that had no
// room and, when it names an instance, that instance.
func TestRenderCapHeld(t *testing.T) {
	pick := func(payload string) Item {
		return Item{Type: TypeEvent, Kind: "job_cap_held", JobID: 9, Payload: []byte(payload)}
	}
	tests := []struct{ name, payload, want string }{
		{"a runner slot", `{"layer":"runner"}`, "held: runner"},
		{"an account", `{"layer":"account","key":"opencode"}`, "held: account on opencode"},
		{"a plan", `{"layer":"plan","key":"205"}`, "held: plan on 205"},
		{"a global hold", `{"layer":"global"}`, "held: global"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := summary(pick(tc.payload)); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}
