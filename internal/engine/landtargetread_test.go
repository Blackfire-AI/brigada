package engine

import "testing"

// TestForemanGrantReadsTheLandTarget: a foreman turn's grant names the
// land target as its base ref and the default branch beside it (issue
// #122); with no land target set both are the default branch.
func TestForemanGrantReadsTheLandTarget(t *testing.T) {
	for _, tc := range []struct{ name, target, want string }{
		{"unset", "", "main"},
		{"set", "brigada/landed", "brigada/landed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.target != "" {
				e.set(map[string]string{SettingLandTarget: tc.target})
			}
			e.offerForeman(e.runner1)
			e.submit(e.foremanTurn())
			g := e.leaseClaude(e.runner1)
			if g == nil || g.Kind != KindForeman {
				t.Fatalf("lease = %+v, want the foreman turn", g)
			}
			if g.BaseRef != tc.want || g.DefaultBranch != "main" {
				t.Errorf("foreman grant base/default = %q/%q, want %q/main", g.BaseRef, g.DefaultBranch, tc.want)
			}
		})
	}
}

// TestPlanReviewGrantReadsTheLandTarget: the plan reviewer's grant
// reads the land target the same way.
func TestPlanReviewGrantReadsTheLandTarget(t *testing.T) {
	for _, tc := range []struct{ name, target, want string }{
		{"unset", "", "main"},
		{"set", "brigada/landed", "brigada/landed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlanReviewEnv(t)
			if tc.target != "" {
				e.set(map[string]string{SettingLandTarget: tc.target})
			}
			e.offerForeman(e.runner1)
			e.offerReviewer()
			e.createPlan([]PlanTask{{Spec: "first thing"}, {Spec: "second thing"}})
			g := e.lease(e.runner2, nil, nil)
			if g == nil || g.Kind != KindPlanReview {
				t.Fatalf("lease = %+v, want the plan_review job", g)
			}
			if g.BaseRef != tc.want || g.DefaultBranch != "main" {
				t.Errorf("plan review grant base/default = %q/%q, want %q/main", g.BaseRef, g.DefaultBranch, tc.want)
			}
		})
	}
}

// TestStandaloneReviewKeepsTheDefaultBranch: a task-less branch review
// diffs against the default branch even when a land target is set; only
// the foreman and the plan reviewer read the target.
func TestStandaloneReviewKeepsTheDefaultBranch(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingLandTarget: "brigada/landed"})
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "openai", "review")
	branch := e.authored(author)
	res, err := e.reviewBranch(branch)
	if err != nil {
		t.Fatalf("review the branch: %v", err)
	}
	g := e.leaseAny(reviewer)
	if g == nil || g.JobID != res.JobID || g.Kind != KindReview {
		t.Fatalf("lease = %+v, want the standalone review", g)
	}
	if g.BaseRef != "main" || g.DefaultBranch != "" {
		t.Errorf("standalone review base/default = %q/%q, want main/empty", g.BaseRef, g.DefaultBranch)
	}
}

// TestAttachCarriesTheLandTarget: the attach result names the effective
// land target beside the default branch.
func TestAttachCarriesTheLandTarget(t *testing.T) {
	e := newEnv(t)
	if res := e.attach(false); res.LandTarget != "main" || res.DefaultBranch != "main" {
		t.Errorf("unset: attach land target/default = %q/%q, want main/main", res.LandTarget, res.DefaultBranch)
	}
	e2 := newEnv(t)
	e2.set(map[string]string{SettingLandTarget: "brigada/landed"})
	if res := e2.attach(false); res.LandTarget != "brigada/landed" || res.DefaultBranch != "main" {
		t.Errorf("set: attach land target/default = %q/%q, want brigada/landed/main", res.LandTarget, res.DefaultBranch)
	}
}
