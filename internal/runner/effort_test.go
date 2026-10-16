package runner

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// A worker or reviewer runs at its agent's effort; a foreman turn runs at
// the project's foreman effort when one is chosen, else at the agent's.
func TestEffortFor(t *testing.T) {
	cfg := &Config{Agents: []api.Agent{
		{Adapter: "claude", Harness: "claude", Model: "opus", Effort: "low"},
		{Adapter: "codex", Harness: "codex", Model: "default"},
	}}
	cases := []struct {
		name  string
		grant api.JobGrant
		want  string
	}{
		{"worker keeps its agent's effort", api.JobGrant{Kind: "work", Agent: "claude"}, "low"},
		{"worker ignores the foreman effort", api.JobGrant{Kind: "work", Agent: "claude", ForemanEffort: "max"}, "low"},
		{"reviewer keeps its agent's effort", api.JobGrant{Kind: "review", Agent: "claude", ForemanEffort: "max"}, "low"},
		{"foreman turn takes the project's effort", api.JobGrant{Kind: "foreman", Agent: "claude", ForemanEffort: "max"}, "max"},
		{"foreman turn without one keeps the agent's", api.JobGrant{Kind: "foreman", Agent: "claude"}, "low"},
		{"agent without effort is unset", api.JobGrant{Kind: "work", Agent: "codex"}, ""},
		{"foreman on an agent without effort takes the project's", api.JobGrant{Kind: "foreman", Agent: "codex", ForemanEffort: "high"}, "high"},
		{"unknown agent is unset", api.JobGrant{Kind: "work", Agent: "nope"}, ""},
	}
	for _, tc := range cases {
		if got := cfg.effortFor(&tc.grant); got != tc.want {
			t.Errorf("%s: effortFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}
