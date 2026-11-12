package engine

import "testing"

// The lease copies the picked agent's advertised permission into the
// grant, whatever the job kind; an agent with none leaves it empty.
func TestGrantCarriesTheAgentPermission(t *testing.T) {
	for _, perm := range []string{"", "bypassPermissions", "Bash(npm test:*)"} {
		t.Run("permission="+perm, func(t *testing.T) {
			e := newEnv(t)
			e.offerForeman(e.runner1)
			e.exec("UPDATE agents SET permission = ? WHERE runner_id = ?", perm, e.runner1)
			res, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
			if err != nil {
				t.Fatalf("submit the turn: %v", err)
			}
			g := e.lease(e.runner1, []string{"claude"}, nil)
			if g == nil || g.JobID != res.JobID {
				t.Fatalf("the lease did not take the turn: %+v", g)
			}
			if g.Permission != perm {
				t.Errorf("grant permission = %q, want %q", g.Permission, perm)
			}
		})
	}
}

// An advertisement stores the permission as the runner resolved it, a
// deprecated tools= alias with spaces included.
func TestAdvertStoresThePermission(t *testing.T) {
	adv := AgentAdvert{Adapter: "claude", Harness: "claude", Model: "sonnet", Permission: "Bash(go test:*)+Edit"}
	if err := adv.valid(); err != nil {
		t.Fatalf("an alias permission is refused: %v", err)
	}
}
