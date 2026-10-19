package caps

import (
	"github.com/Blackfire-AI/brigada/internal/adapter"
	"testing"
)

func limit(n int) *int { return &n }

// oneJob is one live job counted at every layer: runner 1, the work
// account, the sonnet model, plan 2, and the global count of 1.
func oneJob() Usage {
	var u Usage
	u.Add(1, "work", "sonnet", 2)
	return u
}

// TestRoom is the §8.1 rule as a table: each layer full on its own, a
// layer with room, an unset layer, the foreman exemptions, a zero cap,
// and the runner fold of the advertised and configured caps.
func TestRoom(t *testing.T) {
	work := Job{Runner: 1, Account: "work", Model: "sonnet", Plan: 2}
	foreman := Job{Runner: 1, Account: "work", Model: "sonnet", Foreman: true}
	personal := Job{Runner: 2, Account: "personal", Model: "k2", Plan: 3}
	tests := []struct {
		name   string
		limits Limits
		usage  Usage
		job    Job
		room   bool
		full   string
	}{
		{"no caps at all", Limits{}, oneJob(), work, true, ""},
		{"a layer with room", Limits{Runner: limit(2)}, oneJob(), work, true, ""},
		{"another account's cap does not charge this job", Limits{Account: map[string]int{"work": 1}}, oneJob(), personal, true, ""},
		{"an uncapped model is never full", Limits{Model: map[string]int{"heavy": 1}}, oneJob(), work, true, ""},

		{"runner full", Limits{Runner: limit(1)}, oneJob(), work, false, LayerRunner},
		{"account full", Limits{Account: map[string]int{"work": 1}}, oneJob(), work, false, LayerAccount},
		{"model full", Limits{Model: map[string]int{"sonnet": 1}}, oneJob(), work, false, LayerModel},
		{"plan full", Limits{Plan: limit(1)}, oneJob(), work, false, LayerPlan},
		{"global full", Limits{Global: limit(1)}, oneJob(), work, false, LayerGlobal},
		{"the first full layer is named", Limits{Runner: limit(1), Account: map[string]int{"work": 1}}, oneJob(), work, false, LayerRunner},

		{"a foreman turn ignores the plan cap", Limits{Plan: limit(1)}, oneJob(), foreman, true, ""},
		{"a foreman turn ignores the model cap", Limits{Model: map[string]int{"sonnet": 1}}, oneJob(), foreman, true, ""},
		{"a foreman turn counts at the runner", Limits{Runner: limit(1)}, oneJob(), foreman, false, LayerRunner},
		{"a foreman turn counts at the account", Limits{Account: map[string]int{"work": 1}}, oneJob(), foreman, false, LayerAccount},
		{"a foreman turn counts globally", Limits{Global: limit(1)}, oneJob(), foreman, false, LayerGlobal},

		{"a zero cap holds an empty engine", Limits{Global: limit(0)}, Usage{}, work, false, LayerGlobal},
		{"a zero model cap holds everything", Limits{Model: map[string]int{"sonnet": 0}}, Usage{}, work, false, LayerModel},

		{"the advertised runner cap alone", Limits{}, oneJob(), Job{Runner: 1, RunnerCap: 1}, false, LayerRunner},
		{"the advertised cap loses to the smaller config one", Limits{Runner: limit(1)}, oneJob(), Job{Runner: 1, RunnerCap: 5}, false, LayerRunner},
		{"the advertised cap wins when smaller", Limits{Runner: limit(5)}, oneJob(), Job{Runner: 1, RunnerCap: 1}, false, LayerRunner},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			room, full := Room(tt.limits, tt.usage, tt.job)
			if room != tt.room || full != tt.full {
				t.Errorf("Room = (%v, %q); want (%v, %q)", room, full, tt.room, tt.full)
			}
		})
	}
}

// TestRunnerCap covers the fold of §8.1: the effective runner cap is
// the smaller of the advertised value and the config's, ignoring
// whichever is unset.
func TestRunnerCap(t *testing.T) {
	tests := []struct {
		config, advertised, want int // want -1 for unset
	}{
		{0, 0, -1},
		{4, 0, 4},
		{0, 3, 3},
		{2, 5, 2},
		{5, 2, 2},
		{4, 4, 4},
	}
	for _, tt := range tests {
		var limits Limits
		if tt.config > 0 {
			limits.Runner = limit(tt.config)
		}
		got := limits.RunnerCap(tt.advertised)
		switch {
		case tt.want < 0 && got != nil:
			t.Errorf("RunnerCap(config %d, advertised %d) = %d; want unset", tt.config, tt.advertised, *got)
		case tt.want >= 0 && (got == nil || *got != tt.want):
			s := -1
			if got != nil {
				s = *got
			}
			t.Errorf("RunnerCap(config %d, advertised %d) = %d; want %d", tt.config, tt.advertised, s, tt.want)
		}
	}
}

// TestUsageAdd covers what one live job counts: every layer it touches,
// and none of the layers it cannot name — no account or model without
// an agent, no plan without a task.
func TestUsageAdd(t *testing.T) {
	var u Usage
	u.Add(1, "work", "sonnet", 2)
	u.Add(1, "work", "sonnet", 3)
	u.Add(2, "personal", "k2", 2)
	u.Add(3, "", "", 0) // a job with no agent recorded, a foreman turn
	if got := u.Runner[1]; got != 2 {
		t.Errorf("runner 1 = %d, want 2", got)
	}
	if got := u.Runner[3]; got != 1 {
		t.Errorf("runner 3 = %d, want 1: no agent does not mean no runner", got)
	}
	for account, want := range map[string]int{"work": 2, "personal": 1} {
		if got := u.Account[account]; got != want {
			t.Errorf("account %s = %d, want %d", account, got, want)
		}
	}
	if _, named := u.Account[""]; named {
		t.Error("an empty account was counted")
	}
	for model, want := range map[string]int{"sonnet": 2, "k2": 1} {
		if got := u.Model[model]; got != want {
			t.Errorf("model %s = %d, want %d", model, got, want)
		}
	}
	for plan, want := range map[int64]int{2: 2, 3: 1} {
		if got := u.Plan[plan]; got != want {
			t.Errorf("plan %d = %d, want %d", plan, got, want)
		}
	}
	if _, named := u.Plan[0]; named {
		t.Error("a plan of 0 was counted")
	}
	if u.Global != 4 {
		t.Errorf("global = %d, want 4", u.Global)
	}
}

func TestCostOf(t *testing.T) {
	limits := Limits{Cost: map[string]int{"claude/heavy": 2}}
	if got := limits.CostOf("claude", "heavy"); got != 2 {
		t.Errorf("named cost = %d, want 2", got)
	}
	if got := limits.CostOf("claude", "sonnet"); got != 1 {
		t.Errorf("unnamed cost = %d, want the default 1", got)
	}
	if got := (Limits{}).CostOf("claude", "heavy"); got != 1 {
		t.Errorf("cost with no config = %d, want the default 1", got)
	}
}

func TestTierOf(t *testing.T) {
	limits := Limits{Tier: map[string]string{
		"claude/sonnet":          adapter.TierHeavy,
		"opencode/provider/opus": adapter.TierLight,
	}}
	for _, tc := range []struct{ harness, model, want string }{
		{"claude", "sonnet", adapter.TierHeavy},
		{"opencode", "provider/opus", adapter.TierLight},
		{"claude", "opus", adapter.TierHeavy},
		{"claude", "haiku", adapter.TierLight},
		{"opencode", "sonnet", adapter.TierStandard},
		{"codex", "default", adapter.TierStandard},
		{"kimi", "default", adapter.TierStandard},
		{"opencode", "default", adapter.TierStandard},
	} {
		if got := limits.TierOf(tc.harness, tc.model); got != tc.want {
			t.Errorf("TierOf(%q, %q) = %q, want %q", tc.harness, tc.model, got, tc.want)
		}
	}
	if got := (Limits{}).TierOf("claude", "sonnet"); got != adapter.TierStandard {
		t.Errorf("zero Limits tier = %q, want standard", got)
	}
}
