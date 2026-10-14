package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
)

// TestAgentForSelectsTheNamedAgent covers the wiring (§5.1, §8): the
// grant names the agent the lease picked, and the runner hands that
// agent's adapter and model to the job — never the job's own harness
// constraint, and never a fallback to the first agent. Two agents may
// share an adapter name (two scripted fakes); they pair with that
// name's adapters in advertisement order.
func TestAgentForSelectsTheNamedAgent(t *testing.T) {
	cfg := Config{
		Agents: []api.Agent{
			{Adapter: "fake", Harness: "fake", Model: "fake-1"},
			{Adapter: "claude", Harness: "claude", Model: "sonnet"},
		},
		Adapters: []adapter.Adapter{&fake.Adapter{}, &claude.Adapter{}},
	}
	ad, model, err := cfg.agentFor(&api.JobGrant{Agent: "claude"})
	if err != nil {
		t.Fatalf("agentFor(claude): %v", err)
	}
	if ad == nil || ad.Name() != "claude" || model != "sonnet" {
		t.Errorf("agentFor(claude) = %v/%s, want claude/sonnet", ad, model)
	}
	// The named agent decides even when the job's own harness
	// constraint says another: the constraint placed the job, the
	// agent names the worker.
	ad, model, err = cfg.agentFor(&api.JobGrant{Harness: "claude", Agent: "fake"})
	if err != nil {
		t.Fatalf("agentFor(fake): %v", err)
	}
	if ad == nil || ad.Name() != "fake" || model != "fake-1" {
		t.Errorf("agentFor(fake) = %v/%s, want fake/fake-1", ad, model)
	}
	// Two agents of one adapter name pair with that name's adapters in
	// order, so each scripted fake drives its own script.
	two := Config{
		Agents: []api.Agent{
			{Adapter: "fake", Harness: "fakea", Model: "fake-1", Family: "fama"},
			{Adapter: "fake", Harness: "fakeb", Model: "fake-2", Family: "famb"},
		},
		Adapters: []adapter.Adapter{&fake.Adapter{Script: "one"}, &fake.Adapter{Script: "two"}},
	}
	for _, h := range []string{"fakea", "fakeb"} {
		if _, _, err := two.agentFor(&api.JobGrant{Agent: h}); err != nil {
			t.Errorf("agentFor(%s): %v", h, err)
		}
	}
}

// TestAgentForRefusesTheUnnamed covers the failure (§5.1, §8): a grant
// naming an agent the runner does not offer — or naming none — is an
// error naming what the runner does offer, the message a job the
// runner cannot run rightly fails with.
func TestAgentForRefusesTheUnnamed(t *testing.T) {
	cfg := Config{
		Agents: []api.Agent{
			{Adapter: "fake", Harness: "fake", Model: "fake-1"},
		},
		Adapters: []adapter.Adapter{&fake.Adapter{}},
	}
	_, _, err := cfg.agentFor(&api.JobGrant{Agent: "claude"})
	if err == nil || !strings.Contains(err.Error(), `agent "claude", which this runner does not offer (offered: fake)`) {
		t.Errorf("agentFor(claude) error = %v, want it to name the miss and the offer", err)
	}
	_, _, err = cfg.agentFor(&api.JobGrant{})
	if err == nil || !strings.Contains(err.Error(), "the lease named no agent") {
		t.Errorf("agentFor(none) error = %v, want it to name the empty pick", err)
	}
}

// TestRunnerStateDirLock covers the one-process rule (§5.1): a second
// Run on the same state directory fails fast with the holder named,
// and the lock dies with the process, so a stopped runner's state
// directory takes a new one.
func TestRunnerStateDirLock(t *testing.T) {
	dir := t.TempDir()
	first := Config{
		Socket: "nonexistent.sock", Token: "t", StateDir: dir,
		Agents:    []api.Agent{{Adapter: "fake", Harness: "fake", Model: "fake-1"}},
		Adapters:  []adapter.Adapter{&fake.Adapter{}},
		LeaseWait: 50 * time.Millisecond,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, first) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "runner.lock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first runner never took its lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := Run(context.Background(), Config{
		Socket: "nonexistent.sock", Token: "t", StateDir: dir,
		Agents:   []api.Agent{{Adapter: "fake", Harness: "fake", Model: "fake-1"}},
		Adapters: []adapter.Adapter{&fake.Adapter{}},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err == nil || !strings.Contains(err.Error(), "another runner") || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("the second Run on one state dir = %v, want it to refuse naming another runner", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the first Run stopped with %v, want the cancellation", err)
	}
	// The lock went with it: the state directory takes a new runner.
	unlock, err := lockStateDir(dir)
	if err != nil {
		t.Fatalf("lock the freed state dir: %v", err)
	}
	unlock()
}
