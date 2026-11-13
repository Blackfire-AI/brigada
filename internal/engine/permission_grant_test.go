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
