package runner

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
)

// TestAdapterForSelectsClaude covers the wiring (§5.2, §5.1): a runner
// that carries the claude adapter and advertises an agent under harness
// `claude` hands that adapter — with the agent's model — to a job that
// asks for the harness, and the adapter it carries is the claude one
// this repository ships.
func TestAdapterForSelectsClaude(t *testing.T) {
	cfg := Config{
		Agents: []api.Agent{
			{Adapter: "fake", Harness: "fake", Model: "fake-1"},
			{Adapter: "claude", Harness: "claude", Model: "sonnet"},
		},
		Adapters: []adapter.Adapter{&fake.Adapter{}, &claude.Adapter{}},
	}
	ad, model := cfg.adapterFor(&api.JobGrant{Harness: "claude"})
	if ad == nil {
		t.Fatal("no adapter selected for harness claude")
	}
	if ad.Name() != "claude" || model != "sonnet" {
		t.Errorf("selected %s/%s, want claude/sonnet", ad.Name(), model)
	}
	// A job that leaves the harness open takes the first agent's pair,
	// not the claude one.
	ad, model = cfg.adapterFor(&api.JobGrant{})
	if ad == nil || ad.Name() != "fake" || model != "fake-1" {
		t.Errorf("open harness selected %v/%s, want fake/fake-1", ad, model)
	}
}
