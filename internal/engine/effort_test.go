package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const effortRepo = "https://example.invalid/brigada.git" // the fixture's project

// An effort change on the same harness keeps the foreman's session; a
// harness change still drops it. The effort reads back, and unset reads
// as empty.
func TestForemanEffortKeepsTheSession(t *testing.T) {
	e := newEnv(t)
	key := fmt.Sprintf("foreman:%d", e.project)
	e.exec(`INSERT INTO sessions (key, runner_id, harness_session_id, state, last_used_at, created_at)
		VALUES (?, ?, 'fs-1', 'active', ?, ?)`, key, e.runner1, seedTS, seedTS)

	got, err := e.eng.ProjectForeman(e.ctx, effortRepo)
	if err != nil || got == nil || got.Effort != "" {
		t.Fatalf("effort before any choice = %+v, %v; want unset", got, err)
	}

	set, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, "claude", "sonnet", "high")
	if err != nil {
		t.Fatalf("set effort: %v", err)
	}
	if set.Effort != "high" {
		t.Errorf("the setting's effort = %q, want high", set.Effort)
	}
	if got := e.str("SELECT foreman_effort FROM projects WHERE id = ?", e.project); got != "high" {
		t.Errorf("foreman_effort = %q, want high", got)
	}
	if got := e.str("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", key); got != "fs-1" {
		t.Errorf("session after an effort change = %q, want fs-1 kept", got)
	}
	read, err := e.eng.ProjectForeman(e.ctx, effortRepo)
	if err != nil || read == nil || read.Effort != "high" {
		t.Fatalf("effort read back = %+v, %v; want high", read, err)
	}

	// Choosing again without an effort clears it, session kept.
	if _, err := e.eng.SetProjectForeman(e.ctx, effortRepo, "claude", "sonnet"); err != nil {
		t.Fatalf("set without effort: %v", err)
	}
	if got := e.str("SELECT foreman_effort FROM projects WHERE id = ?", e.project); got != "" {
		t.Errorf("foreman_effort after a choice without one = %q, want unset", got)
	}
	if got := e.str("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", key); got != "fs-1" {
		t.Errorf("session after clearing effort = %q, want fs-1 kept", got)
	}

	// A harness change drops the session, as before.
	if _, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, "codex", "gpt-5", "xhigh"); err != nil {
		t.Fatalf("switch harness: %v", err)
	}
	if got := e.str("SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", key); got != "" {
		t.Errorf("session after a harness change = %q, want dropped", got)
	}
}

func TestForemanEffortIsValidatedPerHarness(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ harness, effort string }{
		{"claude", "turbo"},
		{"claude", "ultra"},
		{"codex", "turbo"},
	} {
		_, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, tc.harness, "m", tc.effort)
		if !errors.Is(err, ErrInvalidForeman) || !strings.Contains(err.Error(), tc.effort) {
			t.Errorf("%s/%s: error = %v, want ErrInvalidForeman naming the level", tc.harness, tc.effort, err)
		}
	}
	if got := e.str("SELECT foreman_effort FROM projects WHERE id = ?", e.project); got != "" {
		t.Errorf("foreman_effort after refusals = %q, want unset", got)
	}
}

// The project's foreman effort rides a foreman turn's grant; with none
// chosen the grant carries none, so the runner agent's effort stands.
func TestForemanTurnGrantCarriesTheProjectEffort(t *testing.T) {
	for _, tc := range []struct{ effort, agentEffort, want string }{
		{"", "", ""}, {"max", "", "max"}, {"", "high", "high"}, {"max", "high", "max"},
	} {
		t.Run("effort="+tc.effort+"/agent="+tc.agentEffort, func(t *testing.T) {
			effort := tc.effort
			e := newEnv(t)
			e.offerForeman(e.runner1)
			e.exec("UPDATE agents SET effort = ? WHERE runner_id = ?", tc.agentEffort, e.runner1)
			if _, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, "claude", "sonnet", effort); err != nil {
				t.Fatalf("set the foreman: %v", err)
			}
			res, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
			if err != nil {
				t.Fatalf("submit the turn: %v", err)
			}
			g := e.lease(e.runner1, []string{"claude"}, nil)
			if g == nil || g.JobID != res.JobID {
				t.Fatalf("the lease did not take the turn: %+v", g)
			}
			if got := e.str("SELECT effort FROM jobs WHERE id = ?", g.JobID); got != tc.want {
				t.Errorf("job effort = %q, want %q", got, tc.want)
			}
			if g.Effort != "" {
				t.Errorf("foreman grant effort = %q, want empty", g.Effort)
			}
			if g.ForemanEffort != effort {
				t.Errorf("grant ForemanEffort = %q, want %q", g.ForemanEffort, effort)
			}
		})
	}
}

// A turn queued on one harness and leased after the project switched to
// another must not take the new harness's effort: the grant keeps the
// effort of the harness that runs the turn (here none, so the agent's own).
func TestQueuedTurnIgnoresEffortOfALaterHarness(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1) // the queued turn's own pair, claude/sonnet
	if _, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, "claude", "sonnet", "high"); err != nil {
		t.Fatalf("set the foreman: %v", err)
	}
	res, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
	if err != nil {
		t.Fatalf("submit the turn: %v", err)
	}
	// The switch cancels nothing: the queued turn stays on its claude constraint.
	if _, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, "codex", "gpt-5", "ultra"); err != nil {
		t.Fatalf("switch the foreman: %v", err)
	}
	g := e.lease(e.runner1, []string{"claude"}, nil)
	if g == nil || g.JobID != res.JobID {
		t.Fatalf("the lease did not take the queued turn: %+v", g)
	}
	if g.ForemanEffort != "" {
		t.Errorf("grant ForemanEffort = %q on a claude turn after a switch to codex, want none", g.ForemanEffort)
	}
}

// A task job's grant never carries a foreman effort, whatever the
// project chose: workers and reviewers keep their agent's own.
func TestTaskGrantCarriesNoForemanEffort(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.SetProjectForemanEffort(e.ctx, effortRepo, "claude", "sonnet", "max"); err != nil {
		t.Fatalf("set the foreman: %v", err)
	}
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("no task job leased")
	}
	if g.Kind == KindForeman || g.ForemanEffort != "" {
		t.Errorf("task grant = kind %s, ForemanEffort %q; want none", g.Kind, g.ForemanEffort)
	}
}

// An agent registers with its effort, validated per harness; effort is
// not part of the agent's key, so a re-register at another effort updates
// the one row and its outcomes survive.
func TestAgentEffortRegisters(t *testing.T) {
	e := newEnv(t)
	adv := func(effort string) []AgentAdvert {
		return []AgentAdvert{{Adapter: "claude", Harness: "claude", Model: "opus", Family: "claude",
			Account: "claude", Classes: []string{"implement"}, Effort: effort}}
	}
	if _, err := e.eng.Register(e.ctx, "runner-1", 0, adv("high")); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := e.str("SELECT effort FROM agents WHERE model = 'opus'"); got != "high" {
		t.Errorf("agent effort = %q, want high", got)
	}
	id := e.int("SELECT id FROM agents WHERE model = 'opus'")
	if _, err := e.eng.Register(e.ctx, "runner-1", 0, adv("max")); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := e.int("SELECT id FROM agents WHERE model = 'opus'"); got != id {
		t.Errorf("agent id after an effort change = %d, want the same %d", got, id)
	}
	if got := e.str("SELECT effort FROM agents WHERE model = 'opus'"); got != "max" {
		t.Errorf("agent effort after re-register = %q, want max", got)
	}
	if _, err := e.eng.Register(e.ctx, "runner-1", 0, adv("turbo")); !errors.Is(err, ErrInvalidAgent) {
		t.Errorf("register with a bad effort = %v, want ErrInvalidAgent", err)
	}
	bad := []AgentAdvert{{Adapter: "kimi", Harness: "kimi", Model: "default", Family: "kimi",
		Account: "kimi", Classes: []string{"implement"}, Effort: "high"}}
	if _, err := e.eng.Register(e.ctx, "runner-1", 0, bad); !errors.Is(err, ErrInvalidAgent) {
		t.Errorf("register kimi with an effort = %v, want ErrInvalidAgent", err)
	}
}

func TestTaskEffortRidesTheGrant(t *testing.T) {
	for _, tc := range []struct{ harness, taskEffort, agentEffort, want string }{
		{"claude", "high", "", "high"}, {"claude", "high", "max", "high"},
		{"opencode", "low", "", "minimal"}, {"opencode", "medium", "", ""},
		{"opencode", "medium", "high", "high"}, {"kimi", "high", "", ""},
		{"claude", "", "max", "max"},
	} {
		t.Run(tc.harness+"/"+tc.taskEffort+"/"+tc.agentEffort, func(t *testing.T) {
			e := newEnv(t)
			runner, err := e.eng.Register(e.ctx, "effort-runner", 0, []AgentAdvert{{Adapter: tc.harness, Harness: tc.harness, Model: "model", Classes: []string{"implement", "review"}, Effort: tc.agentEffort}})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := e.eng.CreatePlan(e.ctx, e.project, "effort plan", []PlanTask{{Name: "work", Spec: "do work", Effort: tc.taskEffort}})
			if err != nil {
				t.Fatal(err)
			}
			g := e.lease(runner, []string{tc.harness}, nil)
			if g == nil || g.JobID != plan.JobID {
				t.Fatalf("grant = %+v, want job %d", g, plan.JobID)
			}
			if g.Effort != tc.want {
				t.Errorf("grant effort = %q, want %q", g.Effort, tc.want)
			}
			if got := e.str("SELECT effort FROM jobs WHERE id = ?", g.JobID); got != tc.want {
				t.Errorf("job effort = %q, want %q", got, tc.want)
			}
		})
	}
}
