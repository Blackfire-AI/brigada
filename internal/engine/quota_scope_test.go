package engine

import (
	"testing"
	"time"
)

// TestRegisterKeepsStoredQuotaScope covers the old-runner rule (§8): an
// advert with no quota_scope re-registers the agent and must not blank
// the backfilled scope — only an advert carrying a scope sets it.
func TestRegisterKeepsStoredQuotaScope(t *testing.T) {
	e := newEnv(t)
	first := []AgentAdvert{{
		Adapter: "opencode", Harness: "opencode", Model: "zai-coding-plan/glm-5.3",
		Family: "zhipu", Account: "opencode", QuotaScope: "opencode/zai-coding-plan",
		Classes: []string{"implement"},
	}}
	id, err := e.eng.Register(e.ctx, "opencode-runner", 0, first)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	agent := e.agentOf(id)
	if got := e.str("SELECT quota_scope FROM agents WHERE id = ?", agent); got != "opencode/zai-coding-plan" {
		t.Fatalf("stored scope = %q, want opencode/zai-coding-plan", got)
	}

	// An old runner re-registers with no scope; the stored scope stays.
	old := []AgentAdvert{{
		Adapter: "opencode", Harness: "opencode", Model: "zai-coding-plan/glm-5.3",
		Family: "zhipu", Account: "opencode", Classes: []string{"implement"},
	}}
	if _, err := e.eng.Register(e.ctx, "opencode-runner", 0, old); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := e.str("SELECT quota_scope FROM agents WHERE id = ?", agent); got != "opencode/zai-coding-plan" {
		t.Errorf("stored scope after a scope-less re-register = %q, want kept opencode/zai-coding-plan", got)
	}
}

// TestCoolingFallsBackToAccountWhenScopeEmpty covers §8 "Quota": an
// agent whose quota_scope is empty — an old runner's brand-new agent —
// cools with its account, not its scope, so two scope-less agents on one
// account cool together and a scoped agent on the same account does not.
func TestCoolingFallsBackToAccountWhenScopeEmpty(t *testing.T) {
	e := newEnv(t)
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "claude", "opus", "work")
	scoped := e.addAgent(e.runner2, "opencode", "zai-coding-plan/glm-5.3", "work")
	e.exec("UPDATE agents SET quota_scope = 'opencode/zai-coding-plan' WHERE id = ?", scoped)

	until := e.clock.Now().Add(3 * time.Hour)
	e.runPinned(e.runner1, "claude", Completion{
		Error: "worker CLI failed (limit): quota", ErrorKind: "limit", LimitUntil: until.Format(time.RFC3339)})

	for _, id := range []int64{a1, a2} {
		if _, _, cool := e.agentState(id); cool != fmtTime(until) {
			t.Errorf("account agent %d cooling = %q, want %s", id, cool, fmtTime(until))
		}
	}
	if _, _, cool := e.agentState(scoped); cool != "" {
		t.Errorf("scoped agent %d on the account cooled too: %q", scoped, cool)
	}
}

// TestCoolingGroupsByScope covers §8 "Quota": agents sharing a quota
// scope cool together across accounts, and an agent on another scope —
// even the same account — is left alone.
func TestCoolingGroupsByScope(t *testing.T) {
	e := newEnv(t)
	a1 := e.addAgent(e.runner1, "opencode", "zai-coding-plan/glm-5.3", "work")
	e.exec("UPDATE agents SET quota_scope = 'opencode/zai-coding-plan' WHERE id = ?", a1)
	a2 := e.addAgent(e.runner2, "opencode", "deepseek/chat", "home")
	e.exec("UPDATE agents SET quota_scope = 'opencode/zai-coding-plan' WHERE id = ?", a2)
	a3 := e.addAgent(e.runner1, "opencode", "opencode/mimo", "work")
	e.exec("UPDATE agents SET quota_scope = 'opencode/opencode' WHERE id = ?", a3)

	until := e.clock.Now().Add(3 * time.Hour)
	e.runPinned(e.runner1, "opencode", Completion{
		Error: "worker CLI failed (limit): quota", ErrorKind: "limit", LimitUntil: until.Format(time.RFC3339)})

	for _, id := range []int64{a1, a2} {
		if _, _, cool := e.agentState(id); cool != fmtTime(until) {
			t.Errorf("scope agent %d cooling = %q, want %s", id, cool, fmtTime(until))
		}
	}
	if _, _, cool := e.agentState(a3); cool != "" {
		t.Errorf("agent %d on another scope cooled too: %q", a3, cool)
	}
}
