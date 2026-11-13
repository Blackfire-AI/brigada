package engine

import "testing"

// TestLeaseGrantCarriesTheAgentsPermission: a runner registers an agent with
// its resolved permission= value (§5.2); the grant of a job that agent takes
// carries it, and an agent that registered none leaves it empty.
func TestLeaseGrantCarriesTheAgentsPermission(t *testing.T) {
	e := newEnv(t)
	runner, err := e.eng.Register(e.ctx, "permitted", 0, []AgentAdvert{
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Family: "anthropic", Account: "work",
			Classes: []string{"implement", "review"}, Permission: "acceptEdits"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := e.str("SELECT permission FROM agents WHERE runner_id = ?", runner); got != "acceptEdits" {
		t.Fatalf("stored permission = %q, want acceptEdits", got)
	}
	e.submit(e.work())
	g := e.lease(runner, []string{"claude"}, nil)
	if g == nil {
		t.Fatal("the agented runner found nothing")
	}
	if g.Permission != "acceptEdits" {
		t.Errorf("grant Permission = %q, want acceptEdits", g.Permission)
	}

	// Registering again with a changed value updates the row in place.
	if _, err := e.eng.Register(e.ctx, "permitted", 0, []AgentAdvert{
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Family: "anthropic", Account: "work",
			Classes: []string{"implement", "review"}, Permission: "plan"},
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := e.str("SELECT permission FROM agents WHERE runner_id = ?", runner); got != "plan" {
		t.Errorf("re-registered permission = %q, want plan", got)
	}
}

// TestLeaseGrantWorkerToolsScopesClaudeCodeWorkers: the project's
// deprecated worker_tools (§5.2) stands in for a claude agent's permission
// on a work or fix job, as it replaced the worker's scope before
// permission= existed; a review, an agent of another harness and a
// project that sets none keep the agent's own value.
func TestLeaseGrantWorkerToolsScopesClaudeCodeWorkers(t *testing.T) {
	for _, tc := range []struct {
		name, harness, permission, workerTools string
		kind                                   Kind
		want                                   string
	}{
		{"claude work", "claude", "bypassPermissions", "Bash(go test:*)+Read", KindWork, "Bash(go test:*)+Read"},
		{"claude fix", "claude", "bypassPermissions", "none", KindFix, "none"},
		{"claude review", "claude", "bypassPermissions", "Bash(go test:*)+Read", KindReview, "bypassPermissions"},
		{"claude unset", "claude", "bypassPermissions", "", KindWork, "bypassPermissions"},
		{"codex work", "codex", "workspace-write", "none", KindWork, "workspace-write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			runner, err := e.eng.Register(e.ctx, "scoped", 0, []AgentAdvert{
				{Adapter: tc.harness, Harness: tc.harness, Model: "m", Family: "f", Account: "work",
					Classes: []string{"implement", "review"}, Permission: tc.permission},
			})
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if tc.workerTools != "" {
				e.set(map[string]string{SettingWorkerTools: tc.workerTools})
			}
			job := e.work()
			job.Kind = tc.kind
			if tc.kind == KindReview {
				job.TaskID = 0
				job.Key = "review:standalone"
				job.Branch = "feature"
			}
			e.submit(job)
			g := e.lease(runner, []string{tc.harness}, nil)
			if g == nil {
				t.Fatal("the agented runner found no job")
			}
			if g.Permission != tc.want {
				t.Errorf("grant Permission = %q, want %q", g.Permission, tc.want)
			}
		})
	}
}
