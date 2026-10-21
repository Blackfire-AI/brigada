package runner

import (
	"context"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// probeEvery is how often the runner asks the engine for due
// broken-agent probes (§8 "Health"): the probe's own delay is the
// engine's (a few minutes and growing), this is only how promptly the
// runner notices one is due.
const probeEvery = time.Minute

// probeTimeout bounds one probe command (§8 "Health"): a probe is a
// cheap version or status check, so three seconds is plenty for a CLI
// that answers; a hung one is a failing probe, not a stuck runner.
const probeTimeout = 3 * time.Second

// probeWaitDelay bounds the wait for a timed-out probe's pipes to close,
// as the check's does (§5.5).
const probeWaitDelay = 500 * time.Millisecond

// probeLoop drives the broken-agent recovery (§8 "Health"): every
// ProbeEvery it asks the engine for the runner's due probes, runs each
// one's cheap CLI probe and reports the result. It returns when ctx
// ends.
func probeLoop(ctx context.Context, cfg *Config, client *api.Client) {
	ticker := cfg.Clock.NewTicker(cfg.ProbeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
			runDueProbes(ctx, cfg, client)
		}
	}
}

// runDueProbes probes every agent the engine names and reports each
// result back. A probe that cannot run counts as failing — the engine
// backs the next probe off and tries again, rather than silently
// clearing an agent the runner could not check.
func runDueProbes(ctx context.Context, cfg *Config, client *api.Client) {
	probes, err := client.DueProbes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			cfg.Log.WarnContext(ctx, "list broken-agent probes failed", "err", err)
		}
		return
	}
	for _, p := range probes {
		if ctx.Err() != nil {
			return
		}
		ok := probeAgent(ctx, cfg, p)
		if err := client.ReportProbe(ctx, p.AgentID, ok); err != nil && ctx.Err() == nil {
			cfg.Log.WarnContext(ctx, "report broken-agent probe failed", "agent_id", p.AgentID, "err", err)
		}
	}
}

// probeAgent runs one agent's cheap CLI probe and reports whether it
// answered (exited 0). An agent whose adapter the runner does not carry,
// or whose adapter builds no probe, counts as failing. The deadline
// binds the command: when it fires, CommandContext kills the process, so
// a wedged CLI — the very state being probed — reads as a failing probe
// instead of a stuck runner.
func probeAgent(ctx context.Context, cfg *Config, p engine.Probe) bool {
	ad := cfg.probeAdapter(p.Adapter)
	if ad == nil {
		cfg.Log.WarnContext(ctx, "no adapter to probe the agent", "agent_id", p.AgentID, "adapter", p.Adapter)
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := ad.ProbeCommand(ctx)
	if cmd == nil {
		cfg.Log.WarnContext(ctx, "the adapter built no probe command", "agent_id", p.AgentID, "adapter", p.Adapter)
		return false
	}
	cmd.WaitDelay = probeWaitDelay
	_, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return false
	}
	return err == nil
}

// probeAdapter finds the adapter that drives the named harness, for the
// probe's command. The probe is harness-level — the CLI's binary is one
// per harness — so the first adapter of the name answers it.
func (cfg *Config) probeAdapter(name string) adapter.Adapter {
	for _, ad := range cfg.Adapters {
		if ad.Name() == name {
			return ad
		}
	}
	return nil
}
