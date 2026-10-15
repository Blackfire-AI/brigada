package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/notify"
)

// agentState reads an agent's health, the reason and its cooling
// deadline ("" when none).
func (e *env) agentState(id int64) (health, reason, cooling string) {
	e.t.Helper()
	return e.str("SELECT health FROM agents WHERE id = ?", id),
		e.str("SELECT health_reason FROM agents WHERE id = ?", id),
		e.str("SELECT COALESCE(cooling_until, '') FROM agents WHERE id = ?", id)
}

// runPinned submits a work job on a new task pinned to harness, leases
// it to the runner and completes it with c. The pin keeps the foreman
// turn an earlier failure woke out of the way.
func (e *env) runPinned(runner int64, harness string, c Completion) {
	e.t.Helper()
	e.submit(e.workHarnessed(harness))
	g := e.lease(runner, []string{harness}, nil)
	if g == nil {
		e.t.Fatalf("no %s job was leased to runner %d", harness, runner)
	}
	if ans := e.complete(g, c); ans != AnsOK {
		e.t.Fatalf("complete: %s", ans)
	}
}

// TestLimitCoolsTheWholeAccountAndNoOthers is §8 "Quota": a task job's
// limit cools every agent sharing the failed agent's account — across
// runners — until the completion's reset time, and nobody else.
func TestLimitCoolsTheWholeAccountAndNoOthers(t *testing.T) {
	e := newEnv(t)
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "claude", "opus", "work")
	a3 := e.addAgent(e.runner1, "kimi", "k2", "home")
	a4 := e.addAgent(e.runner2, "claude", "sonnet", "work")
	until := e.clock.Now().Add(3 * time.Hour)

	e.runPinned(e.runner1, "claude", Completion{
		Error: "worker CLI failed (limit): quota", ErrorKind: "limit", LimitUntil: until.Format(time.RFC3339)})

	for _, id := range []int64{a1, a2, a4} {
		if h, _, cool := e.agentState(id); h != "ready" || cool != fmtTime(until) {
			t.Errorf("account agent %d: health %q cooling %q, want ready, cooling until %s", id, h, cool, fmtTime(until))
		}
	}
	if h, _, cool := e.agentState(a3); h != "ready" || cool != "" {
		t.Errorf("other account's agent: health %q cooling %q, want ready and not cooling", h, cool)
	}
	if got := e.countEvents(evAgentCooling); got != 1 {
		t.Errorf("agent_cooling events = %d, want 1", got)
	}
	var found bool
	for _, r := range e.eventRows() {
		if r.kind == evAgentCooling && r.payload.Account == "work" && r.payload.Until == fmtTime(until) {
			found = true
		}
	}
	if !found {
		t.Errorf("no agent_cooling event naming account work until %s", fmtTime(until))
	}

	// Cooling is not waiting for a person: status lists it, not the queue.
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(info.Cooling) != 3 {
		t.Errorf("status cooling = %v, want the 3 account agents", info.Cooling)
	}
	for _, n := range info.NeedsYou {
		if n.Kind == "agent" {
			t.Errorf("needs-you lists cooling agent %+v", n)
		}
	}
}

// TestLimitUnknownUntilUsesTheDefault: no usable reset time cools for
// the configured default; a passed one counts as unknown.
func TestLimitUnknownUntilUsesTheDefault(t *testing.T) {
	for name, until := range map[string]string{
		"empty":  "",
		"passed": time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC).Format(time.RFC3339),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			a := e.addAgent(e.runner1, "claude", "sonnet", "work")
			e.runPinned(e.runner1, "claude", Completion{Error: "limit", ErrorKind: "limit", LimitUntil: until})
			want := fmtTime(e.clock.Now().Add(30 * time.Minute))
			if _, _, cool := e.agentState(a); cool != want {
				t.Errorf("cooling = %q, want now+30m = %q", cool, want)
			}
		})
	}
	t.Run("configured", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) { c.LimitCooling = 5 * time.Minute })
		a := e.addAgent(e.runner1, "claude", "sonnet", "work")
		e.runPinned(e.runner1, "claude", Completion{Error: "limit", ErrorKind: "limit"})
		want := fmtTime(e.clock.Now().Add(5 * time.Minute))
		if _, _, cool := e.agentState(a); cool != want {
			t.Errorf("cooling = %q, want %q", cool, want)
		}
	})
}

// TestForemanLimitCoolsNoAccount: the foreman key's limit is §4.4's;
// the dispatcher does not cool the account for it.
func TestForemanLimitCoolsNoAccount(t *testing.T) {
	e := newEnv(t)
	a := e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.limitForeman(e.clock.Now().Add(2 * time.Hour).Format(time.RFC3339))
	if h, _, cool := e.agentState(a); h != "ready" || cool != "" {
		t.Errorf("agent after the foreman's limit: health %q cooling %q, want untouched", h, cool)
	}
	if got := e.countEvents(evAgentCooling); got != 0 {
		t.Errorf("agent_cooling events = %d, want 0", got)
	}
}

// TestAuthErrorMarksTheAgentBroken is §8 "Health": an auth completion
// breaks that agent alone, and the event reaches the user's banner.
func TestAuthErrorMarksTheAgentBroken(t *testing.T) {
	e := newEnv(t)
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner1, "kimi", "k2", "work")
	e.runPinned(e.runner1, "claude", Completion{Error: "not logged in", ErrorKind: "auth"})

	if h, why, _ := e.agentState(a1); h != "broken" || why != "auth" {
		t.Errorf("agent: health %q reason %q, want broken(auth)", h, why)
	}
	if h, _, _ := e.agentState(a2); h != "ready" {
		t.Errorf("sibling agent health %q, want ready", h)
	}
	if got := e.countEvents(evAgentBroken); got != 1 {
		t.Errorf("agent_broken events = %d, want 1", got)
	}

	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var item *NeedsYouItem
	for i := range info.NeedsYou {
		if info.NeedsYou[i].Kind == "agent" {
			item = &info.NeedsYou[i]
		}
	}
	if item == nil || item.ID != a1 || item.State != "broken" || item.Reason != "auth" || item.Title != "claude/sonnet" {
		t.Errorf("needs-you = %+v, want the broken claude/sonnet agent with reason auth", info.NeedsYou)
	}

	fake := &notify.Fake{}
	if err := notify.NewLoop(e.st, fake, nil).Flush(e.ctx); err != nil {
		t.Fatalf("flush notifications: %v", err)
	}
	var banner bool
	for _, n := range fake.Notes() {
		if strings.Contains(n.Body, "agent broken") && strings.Contains(n.Body, "claude/sonnet: auth") {
			banner = true
		}
	}
	if !banner {
		t.Errorf("notes %v, want a banner for the broken agent", fake.Notes())
	}
}

// TestConsecutiveFailuresMarkTheAgentBroken: the first failure does not;
// the N-th in a row does; a success in between resets the count.
func TestConsecutiveFailuresMarkTheAgentBroken(t *testing.T) {
	e := newEnv(t)
	a := e.addAgent(e.runner1, "claude", "sonnet", "work")
	fail := Completion{Error: "worker CLI failed"}

	e.runPinned(e.runner1, "claude", fail)
	if h, _, _ := e.agentState(a); h != "ready" {
		t.Fatalf("health after one failure = %q, want ready", h)
	}
	e.runPinned(e.runner1, "claude", Completion{Result: `{"status":"done"}`})
	e.clock.Advance(time.Second)
	e.runPinned(e.runner1, "claude", fail)
	if h, _, _ := e.agentState(a); h != "ready" {
		t.Fatalf("health after fail, done, fail = %q, want ready", h)
	}
	e.clock.Advance(time.Second)
	e.runPinned(e.runner1, "claude", fail)
	if h, why, _ := e.agentState(a); h != "broken" || why != "repeated failures" {
		t.Errorf("health after two failures in a row = %q (%q), want broken(repeated failures)", h, why)
	}
	if got := e.countEvents(evAgentBroken); got != 1 {
		t.Errorf("agent_broken events = %d, want 1", got)
	}
}

// TestFailureStreakFollowsTheConfig: N is the config's.
func TestFailureStreakFollowsTheConfig(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.FailureStreak = 3 })
	a := e.addAgent(e.runner1, "claude", "sonnet", "work")
	for i := 0; i < 2; i++ {
		e.runPinned(e.runner1, "claude", Completion{Error: "boom"})
		e.clock.Advance(time.Second)
	}
	if h, _, _ := e.agentState(a); h != "ready" {
		t.Fatalf("health after two failures with N=3 = %q, want ready", h)
	}
	e.runPinned(e.runner1, "claude", Completion{Error: "boom"})
	if h, _, _ := e.agentState(a); h != "broken" {
		t.Errorf("health after three failures = %q, want broken", h)
	}
}

// TestStaleRunnerBreaksItsAgentsAndRegisterClearsOnlyThat is §8
// "Health": the sweep marks a long-unseen runner's agents broken
// (runner stale); registering again clears exactly that kind, leaves
// auth broken and cooling alone, and spares another runner's agents.
func TestStaleRunnerBreaksItsAgentsAndRegisterClearsOnlyThat(t *testing.T) {
	e := newEnv(t)
	stale := e.addAgent(e.runner1, "claude", "sonnet", "work")
	authBroken := e.addAgent(e.runner1, "kimi", "k2", "home")
	cooling := e.addAgent(e.runner1, "gemini", "flash", "free")
	other := e.addAgent(e.runner2, "claude", "opus", "work")
	e.exec("UPDATE agents SET health = 'broken', health_reason = 'auth' WHERE id = ?", authBroken)
	coolUntil := fmtTime(e.clock.Now().Add(6 * time.Hour))
	e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", coolUntil, cooling)

	// runner2 keeps polling; runner1 goes quiet past the window.
	e.clock.Advance(10 * time.Minute)
	e.leaseAny(e.runner2)
	e.mustSweep()
	if h, _, _ := e.agentState(stale); h != "ready" {
		t.Fatalf("agent health inside the window = %q, want ready", h)
	}
	e.clock.Advance(10 * time.Minute)
	e.leaseAny(e.runner2)
	e.mustSweep()

	if h, why, _ := e.agentState(stale); h != "broken" || why != "runner stale" {
		t.Errorf("stale runner's agent: %q (%q), want broken(runner stale)", h, why)
	}
	if h, why, _ := e.agentState(authBroken); h != "broken" || why != "auth" {
		t.Errorf("auth-broken agent: %q (%q), want its reason kept", h, why)
	}
	if h, _, _ := e.agentState(other); h != "ready" {
		t.Errorf("polling runner's agent = %q, want ready", h)
	}
	var brokenEvents int
	for _, k := range e.eventKinds() {
		if k == evAgentBroken {
			brokenEvents++
		}
	}
	if brokenEvents != 2 {
		t.Errorf("agent_broken events = %d, want 2 (runner1's stale pair)", brokenEvents)
	}

	// runner1 registers again, advertising the same agents.
	adverts := []AgentAdvert{
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Family: "claude", Account: "work"},
		{Adapter: "kimi", Harness: "kimi", Model: "k2", Family: "kimi", Account: "home"},
		{Adapter: "gemini", Harness: "gemini", Model: "flash", Family: "gemini", Account: "free"},
	}
	if _, err := e.eng.Register(e.ctx, "runner-1", 0, adverts); err != nil {
		t.Fatalf("register: %v", err)
	}
	if h, why, _ := e.agentState(stale); h != "ready" || why != "" {
		t.Errorf("stale agent after register: %q (%q), want ready", h, why)
	}
	if h, why, _ := e.agentState(authBroken); h != "broken" || why != "auth" {
		t.Errorf("auth-broken agent after register: %q (%q), want still broken(auth)", h, why)
	}
	if _, _, cool := e.agentState(cooling); cool != coolUntil {
		t.Errorf("cooling after register = %q, want %q untouched", cool, coolUntil)
	}
}

// TestPollingRunnerClearsItsStaleAgents: a runner that never
// re-registers but is seen again (a lease poll) is back too.
func TestPollingRunnerClearsItsStaleAgents(t *testing.T) {
	e := newEnv(t)
	a := e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.exec("UPDATE agents SET health = 'broken', health_reason = 'runner stale' WHERE id = ?", a)
	g := e.leaseAny(e.runner1) // nothing queued; the poll alone clears the stale mark
	if g != nil {
		t.Fatalf("unexpected lease %+v", g)
	}
	if h, _, _ := e.agentState(a); h != "ready" {
		t.Errorf("a polling runner's stale agent = %q, want ready", h)
	}
}

// TestResetClearsBrokenAndCooling is §8: the user's reset clears both on
// every agent of the harness and model, lands one event, and refuses an
// unknown pair.
func TestResetClearsBrokenAndCooling(t *testing.T) {
	e := newEnv(t)
	a1 := e.addAgent(e.runner1, "claude", "sonnet", "work")
	a2 := e.addAgent(e.runner2, "claude", "sonnet", "work")
	other := e.addAgent(e.runner1, "kimi", "k2", "home")
	until := fmtTime(e.clock.Now().Add(time.Hour))
	e.exec("UPDATE agents SET health = 'broken', health_reason = 'auth' WHERE id = ?", a1)
	e.exec("UPDATE agents SET cooling_until = ? WHERE id IN (?, ?)", until, a2, other)

	n, err := e.eng.ResetAgents(e.ctx, "claude", "sonnet")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if n != 2 {
		t.Errorf("matched = %d, want 2", n)
	}
	for _, id := range []int64{a1, a2} {
		if h, why, cool := e.agentState(id); h != "ready" || why != "" || cool != "" {
			t.Errorf("agent %d after reset: %q %q %q, want clean", id, h, why, cool)
		}
	}
	if _, _, cool := e.agentState(other); cool != until {
		t.Errorf("other agent cooling = %q, want %q untouched", cool, until)
	}
	var resets int
	for _, r := range e.eventRows() {
		if r.kind == evAgentReset && r.payload.Harness == "claude" && r.payload.Model == "sonnet" && r.payload.Count == 2 {
			resets++
		}
	}
	if resets != 1 {
		t.Errorf("agent_reset events = %d, want 1", resets)
	}
	if _, err := e.eng.ResetAgents(e.ctx, "claude", "nope"); !errors.Is(err, ErrNoAgent) {
		t.Errorf("reset of an unknown agent: err = %v, want ErrNoAgent", err)
	}
}
