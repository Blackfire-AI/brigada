package engine

import (
	"strings"
	"testing"
)

func TestReviewTierFor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       ReviewConfig
		effective string
		own       string
		want      string
	}{
		{"default heavy", ReviewConfig{}, "heavy", "", "standard"},
		{"default standard", ReviewConfig{}, "standard", "", "light"},
		{"default light stays light", ReviewConfig{}, "light", "", "light"},
		{"below named", ReviewConfig{Tier: ReviewTierBelow}, "heavy", "", "standard"},
		{"same heavy", ReviewConfig{Tier: ReviewTierSame}, "heavy", "", "heavy"},
		{"same light", ReviewConfig{Tier: ReviewTierSame}, "light", "", "light"},
		{"task wins over below", ReviewConfig{}, "light", "heavy", "heavy"},
		{"task wins over same", ReviewConfig{Tier: ReviewTierSame}, "heavy", "light", "light"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.reviewTierFor(tc.effective, tc.own); got != tc.want {
				t.Errorf("reviewTierFor(%q, %q) = %q, want %q", tc.effective, tc.own, got, tc.want)
			}
		})
	}
}

func TestReviewEffortFor(t *testing.T) {
	for _, tc := range []struct {
		cfg  ReviewConfig
		own  string
		want string
	}{
		{ReviewConfig{}, "", "medium"},
		{ReviewConfig{Effort: "low"}, "", "low"},
		{ReviewConfig{Effort: "low"}, "max", "max"},
		{ReviewConfig{}, "high", "high"},
	} {
		if got := tc.cfg.reviewEffortFor(tc.own); got != tc.want {
			t.Errorf("reviewEffortFor(%+v, %q) = %q, want %q", tc.cfg, tc.own, got, tc.want)
		}
	}
}

// reviewTierEnv has one other-family reviewer per tier, so the tier a
// review is served shows which agent it got. The models carry their tier
// by config, whatever their names say.
func reviewTierEnv(t *testing.T, adjust func(*Config)) (e *env, light, standard, heavy int64) {
	t.Helper()
	e = newEnvCfg(t, func(c *Config) {
		c.Caps.Tier = map[string]string{"codex/r-light": "light", "kimi/r-standard": "standard", "opencode/r-heavy": "heavy"}
		if adjust != nil {
			adjust(c)
		}
	})
	light = e.addAgent(e.runner1, "codex", "r-light", "a-light")
	standard = e.addAgent(e.runner1, "kimi", "r-standard", "a-standard")
	heavy = e.addAgent(e.runner1, "opencode", "r-heavy", "a-heavy")
	return e, light, standard, heavy
}

// reviewOfTask submits a review of a claude-written task of the given
// effective tier, with the task's own review settings left as set by set.
func (e *env) reviewOfTask(tier, set string, args ...any) SubmitResult {
	e.t.Helper()
	task := e.addTask("{}")
	e.exec("UPDATE tasks SET tier = ?, effective_tier = ? WHERE id = ?", tier, tier, task)
	if set != "" {
		e.exec("UPDATE tasks SET "+set+" WHERE id = ?", append(args, task)...)
	}
	p := e.workOn(task)
	p.Kind = KindReview
	p.ExcludeFamily = "claude"
	return e.submit(p)
}

// A review defaults to one tier below its task's effective tier, light
// staying light (decided 2026-10-20).
func TestReviewDefaultsOneTierBelow(t *testing.T) {
	for _, tc := range []struct {
		task string
		want func(light, standard, heavy int64) int64
		tier string
	}{
		{"heavy", func(_, s, _ int64) int64 { return s }, "standard"},
		{"standard", func(l, _, _ int64) int64 { return l }, "light"},
		{"light", func(l, _, _ int64) int64 { return l }, "light"},
	} {
		t.Run(tc.task, func(t *testing.T) {
			e, light, standard, heavy := reviewTierEnv(t, nil)
			job := e.reviewOfTask(tc.task, "")
			if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
				t.Fatalf("lease = %+v, want review %d", g, job.JobID)
			}
			e.assertTierPick(job.JobID, tc.want(light, standard, heavy), tc.tier, tc.tier)
		})
	}
}

// A review follows the task's effective tier after an escalation bump: a
// task the ladder raised from light to heavy is reviewed one tier below
// heavy (standard), not below its original light.
func TestReviewTierFollowsEscalation(t *testing.T) {
	e, _, standard, _ := reviewTierEnv(t, nil)
	job := e.reviewOfTask("light", "effective_tier = ?", "heavy")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want review %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, standard, "standard", "standard")
}

// review.tier = same asks for the task's own effective tier.
func TestReviewTierSame(t *testing.T) {
	e, _, _, heavy := reviewTierEnv(t, func(c *Config) { c.Review.Tier = ReviewTierSame })
	job := e.reviewOfTask("heavy", "")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want review %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, heavy, "heavy", "heavy")
}

// A task's own review_tier wins over the default, whichever way it points.
func TestReviewTierTaskOverride(t *testing.T) {
	e, light, _, heavy := reviewTierEnv(t, nil)
	up := e.reviewOfTask("light", "review_tier = ?", "heavy")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != up.JobID {
		t.Fatalf("lease = %+v, want review %d", g, up.JobID)
	}
	e.assertTierPick(up.JobID, heavy, "heavy", "heavy")

	down := e.reviewOfTask("heavy", "review_tier = ?", "light")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != down.JobID {
		t.Fatalf("lease = %+v, want review %d", g, down.JobID)
	}
	e.assertTierPick(down.JobID, light, "light", "light")
}

// A work job still asks for the task's effective tier, whatever the review
// default says.
func TestWorkTierIgnoresReviewTier(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.Review.Tier = ReviewTierBelow })
	e.addAgent(e.runner1, "claude", "haiku", "light")
	opus := e.addAgent(e.runner1, "claude", "opus", "heavy")
	e.exec("UPDATE tasks SET review_tier = 'light'")
	job := e.tierWork("heavy")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, opus, "heavy", "heavy")
}

// The review tier climbs like a work tier when it has no available
// other-family reviewer: the lower tier is empty here.
func TestReviewTierFallbackClimbs(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Tier = map[string]string{"kimi/r-standard": "standard", "opencode/r-heavy": "heavy"}
	})
	standard := e.addAgent(e.runner1, "kimi", "r-standard", "a-standard")
	heavy := e.addAgent(e.runner1, "opencode", "r-heavy", "a-heavy")

	// Standard task: asks light, nothing there, climbs to standard.
	job := e.reviewOfTask("standard", "")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want review %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, standard, "light", "standard")

	// A broken standard reviewer leaves only the heavy one: a heavy task
	// asks standard and climbs to heavy.
	e.exec("UPDATE agents SET health = 'broken' WHERE id = ?", standard)
	job = e.reviewOfTask("heavy", "")
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want review %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, heavy, "standard", "heavy")
}

// The retry exclusion still applies on top of the review tier.
func TestReviewTierKeepsRetryExclusion(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Tier = map[string]string{"codex/r-a": "light", "kimi/r-b": "light"}
	})
	first := e.addAgent(e.runner1, "codex", "r-a", "a")
	second := e.addAgent(e.runner1, "kimi", "r-b", "b")
	task := e.addTask("{}")
	e.exec("UPDATE tasks SET tier = 'standard', effective_tier = 'standard' WHERE id = ?", task)
	p := e.workOn(task)
	p.Kind = KindReview
	p.ExcludeFamily = "claude"
	p.ExcludeAgent = first
	job := e.submit(p)
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want review %d", g, job.JobID)
	}
	e.assertTierPick(job.JobID, second, "light", "light")
}

// The review effort is the default (medium) or the task's own review effort,
// whatever the task's work effort has been escalated to.
func TestReviewEffortIgnoresTaskEffort(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ReviewConfig
		set  string
		want string
	}{
		{"default medium", ReviewConfig{}, "", "medium"},
		{"configured", ReviewConfig{Effort: "low"}, "", "low"},
		{"task wins", ReviewConfig{Effort: "low"}, "review_effort = 'max'", "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) { c.Review = tc.cfg })
			e.addAgent(e.runner1, "codex", "gpt-5", "a")
			task := e.addTask("{}")
			// A task the ladder pushed to high effort.
			e.exec("UPDATE tasks SET effort = 'low', effective_effort = 'high' WHERE id = ?", task)
			if tc.set != "" {
				e.exec("UPDATE tasks SET "+tc.set+" WHERE id = ?", task)
			}
			p := e.workOn(task)
			p.Kind = KindReview
			p.ExcludeFamily = "claude"
			job := e.submit(p)
			g := e.leaseAny(e.runner1)
			if g == nil || g.JobID != job.JobID {
				t.Fatalf("lease = %+v, want review %d", g, job.JobID)
			}
			if g.Effort != tc.want {
				t.Errorf("review effort = %q, want %q", g.Effort, tc.want)
			}
			if got := e.str("SELECT effort FROM jobs WHERE id = ?", g.JobID); got != tc.want {
				t.Errorf("job effort = %q, want %q", got, tc.want)
			}
		})
	}
}

// A work job keeps the task's effective effort.
func TestWorkEffortIgnoresReviewEffort(t *testing.T) {
	e := newEnv(t)
	e.addAgent(e.runner1, "codex", "gpt-5", "a")
	task := e.addTask("{}")
	e.exec("UPDATE tasks SET effort = 'low', effective_effort = 'high', review_effort = 'max' WHERE id = ?", task)
	job := e.submit(e.workOn(task))
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != job.JobID || g.Effort != "high" {
		t.Fatalf("grant = %+v, want job %d at high", g, job.JobID)
	}
}

// plan_create and plan_amend store a task's review tier and effort, and
// refuse a word that is not a level.
func TestPlanReviewTierEffort(t *testing.T) {
	e := newEnv(t)
	plan, err := e.eng.CreatePlan(e.ctx, e.project, "review plan", []PlanTask{
		{Name: "a", Spec: "do a", ReviewTier: "heavy", ReviewEffort: "high"},
		{Name: "b", Spec: "do b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := e.int("SELECT id FROM tasks WHERE plan_id = ? AND name = 'a'", plan.PlanID)
	if got := e.str("SELECT review_tier || '/' || review_effort FROM tasks WHERE id = ?", a); got != "heavy/high" {
		t.Errorf("task a review settings = %q, want heavy/high", got)
	}
	if got := e.str("SELECT review_tier || '/' || review_effort FROM tasks WHERE plan_id = ? AND name = 'b'", plan.PlanID); got != "/" {
		t.Errorf("task b review settings = %q, want none", got)
	}
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "bad", []PlanTask{{Name: "x", Spec: "x", ReviewTier: "huge"}}); err == nil || !strings.Contains(err.Error(), "review") {
		t.Errorf("create with a bad review tier = %v, want a refusal naming review", err)
	}
	if _, err := e.eng.CreatePlan(e.ctx, e.project, "bad", []PlanTask{{Name: "x", Spec: "x", ReviewEffort: "extreme"}}); err == nil || !strings.Contains(err.Error(), "review") {
		t.Errorf("create with a bad review effort = %v, want a refusal naming review", err)
	}

	// retier changes one and keeps the other.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, plan.PlanID, PlanAmendment{
		Retier: []PlanRetier{{Task: "a", ReviewTier: "light"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.str("SELECT review_tier || '/' || review_effort FROM tasks WHERE id = ?", a); got != "light/high" {
		t.Errorf("task a after retier = %q, want light/high", got)
	}
	// add carries them too.
	if _, err := e.eng.AmendPlan(e.ctx, e.project, plan.PlanID, PlanAmendment{
		Add: []PlanTask{{Name: "c", Spec: "do c", ReviewTier: "standard", ReviewEffort: "low"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.str("SELECT review_tier || '/' || review_effort FROM tasks WHERE plan_id = ? AND name = 'c'", plan.PlanID); got != "standard/low" {
		t.Errorf("added task c review settings = %q, want standard/low", got)
	}
	if _, err := e.eng.AmendPlan(e.ctx, e.project, plan.PlanID, PlanAmendment{
		Retier: []PlanRetier{{Task: "a", ReviewEffort: "extreme"}},
	}); err == nil {
		t.Error("retier with a bad review effort was accepted")
	}
}
