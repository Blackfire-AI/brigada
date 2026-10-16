package engine

import (
	"strings"
	"testing"
	"time"
)

// budgetEntry reads the needs-you budget_spent entry of the fixture's
// project (§4.3, §10), nil when the queue holds none.
func (e *env) budgetEntry() *NeedsYouItem {
	e.t.Helper()
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	for i := range info.NeedsYou {
		if info.NeedsYou[i].Kind == "foreman" && info.NeedsYou[i].State == "budget_spent" {
			return &info.NeedsYou[i]
		}
	}
	return nil
}

// spendTheBudget drives the fixture to a spent wake budget: the first
// park wakes the day's one turn, its failure returns the event pending,
// and the re-evaluation has no budget for a second turn.
func spendTheBudget(e *env) {
	e.t.Helper()
	e.park("which file?")
	e.complete(e.leaseForeman(), Completion{Error: "boom"})
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 1 {
		e.t.Fatalf("foreman_wake_budget_spent events = %d, want 1", got)
	}
}

// TestWakeBudgetSpentIsVisibleOncePerWindow is §4.3's visibility: a
// skipped wake records one event per project per budget window — the
// budget in Count, the window's end in Until — and the needs-you queue
// shows the entry with the use, the reset time and the events waiting.
func TestWakeBudgetSpentIsVisibleOncePerWindow(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 1 })
	spendTheBudget(e)
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns with the budget spent = %d, want 1", got)
	}
	p, ok := e.eventPayloadOf(evForemanWakeBudgetSpent)
	if !ok || p.Count != 1 || p.Until != "2026-10-13T00:00:00.000000000Z" {
		t.Errorf("foreman_wake_budget_spent payload = %+v, want count 1 until the window's end", p)
	}
	// A second park the same window: no second event, still no turn,
	// and the entry counts both waiting events.
	e.park("again?")
	if got := e.countEvents(evForemanWakeBudgetSpent); got != 1 {
		t.Errorf("foreman_wake_budget_spent events after a second park = %d, want still 1", got)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after the second park = %d, want still 1", got)
	}
	it := e.budgetEntry()
	if it == nil {
		t.Fatal("the needs-you queue holds no budget_spent entry while events wait")
	}
	for _, want := range []string{
		"wake budget spent (1/1 today, resets 2026-10-13T00:00:00.000000000Z)",
		"2 events waiting",
		"`brigada budget --wakes N`",
	} {
		if !strings.Contains(it.Reason, want) {
			t.Errorf("entry reason = %q, want it to carry %q", it.Reason, want)
		}
	}
	if it.Title != envRepo {
		t.Errorf("entry title = %q, want the repo %q", it.Title, envRepo)
	}
}

// TestRaisingTheWakeBudgetWakesAtOnce is the raise rule (§4.3): setting
// a bigger budget re-evaluates the waiting wake at once, and the
// needs-you entry goes with the wake.
func TestRaisingTheWakeBudgetWakesAtOnce(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 1 })
	spendTheBudget(e)
	if e.budgetEntry() == nil {
		t.Fatal("the needs-you queue holds no budget_spent entry before the raise")
	}
	info, err := e.eng.SetWakeBudget(e.ctx, envRepo, 3)
	if err != nil {
		t.Fatalf("raise the wake budget: %v", err)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the raise = %d, want 2: the waiting events woke at once", got)
	}
	if it := e.budgetEntry(); it != nil {
		t.Errorf("the budget_spent entry = %+v, want it gone with the wake", it)
	}
	if info.Budget != 3 || info.Default {
		t.Errorf("budget after the raise = %+v, want 3 as a set value", info)
	}
	if info.Waiting != 0 {
		t.Errorf("waiting after the raise = %d, want 0: the wake took the events", info.Waiting)
	}
	if info.Wakes != 2 {
		t.Errorf("wakes after the raise = %d, want 2", info.Wakes)
	}
}

// TestWakeBudgetResetsAtTheWindowBoundary is the window's end (§4.3):
// past UTC midnight the count starts at zero — the recovery wake goes
// through and the needs-you entry clears.
func TestWakeBudgetResetsAtTheWindowBoundary(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 1 })
	spendTheBudget(e)
	if e.budgetEntry() == nil {
		t.Fatal("the needs-you queue holds no budget_spent entry before the reset")
	}
	// The fixture's clock sits at 12:00 UTC; thirteen hours on lands
	// past the window's end at midnight.
	e.clock.Advance(13 * time.Hour)
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover foreman wakes at the window's end: %v", err)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns after the reset = %d, want 2: the budget reset with the window", got)
	}
	if it := e.budgetEntry(); it != nil {
		t.Errorf("the budget_spent entry = %+v, want it gone with the reset", it)
	}
	info, err := e.eng.WakeBudget(e.ctx, envRepo)
	if err != nil {
		t.Fatalf("read the wake budget: %v", err)
	}
	if info.Wakes != 1 {
		t.Errorf("wakes after the reset = %d, want 1: only the post-midnight turn counts", info.Wakes)
	}
	if info.ResetsAt != "2026-10-14T00:00:00.000000000Z" {
		t.Errorf("reset time after the window = %q, want the next UTC midnight", info.ResetsAt)
	}
}

// TestWakeBudgetDefaultAndSettingPrecedence is where the budget comes
// from (§4.3): the config default for projects without the setting, the
// project's foreman_wakes_per_day over it — and the wake obeys the
// effective one.
func TestWakeBudgetDefaultAndSettingPrecedence(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 2 })
	info, err := e.eng.WakeBudget(e.ctx, envRepo)
	if err != nil {
		t.Fatalf("read the wake budget: %v", err)
	}
	if info.Budget != 2 || !info.Default {
		t.Fatalf("budget without a setting = %+v, want 2 as the config default", info)
	}
	e.set(map[string]string{SettingForemanWakesPerDay: "3"})
	if info, err = e.eng.WakeBudget(e.ctx, envRepo); err != nil || info.Budget != 3 || info.Default {
		t.Fatalf("budget with a setting = %+v, %v; want 3 as a set value", info, err)
	}

	// The wake obeys the setting over the config default: a config
	// budget of 1 with a setting of 2 takes two turns in one day.
	e2 := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 1 })
	e2.set(map[string]string{SettingForemanWakesPerDay: "2"})
	e2.park("which file?")
	e2.complete(e2.leaseForeman(), Completion{Error: "boom"})
	if got := e2.foremanJobs(); got != 2 {
		t.Fatalf("foreman turns under the raised setting = %d, want 2", got)
	}
	if got := e2.countEvents(evForemanWakeBudgetSpent); got != 0 {
		t.Errorf("foreman_wake_budget_spent events = %d, want 0: the setting had room", got)
	}
}

// TestAUserMessageStillBypassesTheSpentBudget is §4.3 beside §4.2 item
// 3: a wake that carries a user message is not charged, so the ask goes
// through a spent budget and takes the waiting events with it — the
// needs-you entry goes too.
func TestAUserMessageStillBypassesTheSpentBudget(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 1 })
	spendTheBudget(e)
	res, err := e.eng.AskForeman(e.ctx, envRepo, "what is happening?", "test")
	if err != nil {
		t.Fatalf("ask past the spent budget: %v", err)
	}
	if res.Outcome != WakeWoken {
		t.Fatalf("ask outcome = %s, want woken: a user message is not charged", res.Outcome)
	}
	if it := e.budgetEntry(); it != nil {
		t.Errorf("the budget_spent entry = %+v, want it gone: the wake took the events", it)
	}
}

// TestSetWakeBudgetRefusals bounds the set (§4.3): zero, a negative and
// an over-the-ceiling budget are refused with the bound named, and
// nothing is stored.
func TestSetWakeBudgetRefusals(t *testing.T) {
	e := newEnv(t)
	for _, n := range []int{0, -3, MaxForemanWakesPerDay + 1} {
		_, err := e.eng.SetWakeBudget(e.ctx, envRepo, n)
		if err == nil || !strings.Contains(err.Error(), "at most") {
			t.Errorf("set wakes %d: err = %v, want the bound named", n, err)
		}
	}
	if raw := e.str("SELECT settings FROM projects WHERE id = ?", e.project); raw != "{}" {
		t.Errorf("settings = %s after the refusals, want {}", raw)
	}
}
