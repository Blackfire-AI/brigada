package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
)

// heartbeatEvery is how often the runner heartbeats a running job
// (§5.1, §5.6): comfortably inside the default 90 s lease, so one lost
// beat does not lapse it.
const heartbeatEvery = 20 * time.Second

// leaseWait is the long-poll wait of each lease call (§12.3): under the
// API's 30 s cap, so one poll cannot outlive it.
const leaseWait = 25 * time.Second

// Config is what a runner needs to run (§5.1). The zero value is not
// usable; Run fills the defaults for the tuning fields and refuses the
// rest.
type Config struct {
	// Socket is the engine API's Unix socket path (§12.1).
	Socket string
	// Token is the runner token's secret; its name is the runner's
	// identity (§12.3).
	Token string
	// StateDir holds the runner's workspace: the per-repo mirror cache
	// under mirrors/, the per-task worktrees under worktrees/ and the
	// run records (transcripts, check logs) under records/.
	StateDir string
	// Agents is the runner's advertisement (§5.1): one entry per
	// (adapter, harness, model) it offers, persisted by register.
	Agents []api.Agent
	// Adapters are the adapter implementations the runner can drive;
	// each agent's Adapter names one of them.
	Adapters []adapter.Adapter
	// Labels are offered on every lease poll (§8).
	Labels []string
	// HeartbeatEvery, LeaseWait and CheckTimeout override the defaults;
	// zero keeps them.
	HeartbeatEvery time.Duration
	LeaseWait      time.Duration
	CheckTimeout   time.Duration
	// StallAfter and ToolCap are the stall policy (§5.6): zero keeps
	// detection off — it arrives with BP8a-4b.
	StallAfter time.Duration
	ToolCap    time.Duration
	// GitEnv is extra environment for the runner's own git commands —
	// where its credentials ride when they come from config (§5.1).
	// They never reach the worker's argv or prompt.
	GitEnv []string
	// Log is the runner's logger; nil uses the default.
	Log *slog.Logger
}

func (cfg *Config) defaults() error {
	switch {
	case cfg.Socket == "":
		return fmt.Errorf("runner: engine socket is required")
	case cfg.Token == "":
		return fmt.Errorf("runner: token is required")
	case cfg.StateDir == "":
		return fmt.Errorf("runner: state directory is required")
	case len(cfg.Agents) == 0:
		return fmt.Errorf("runner: at least one advertised agent is required")
	case len(cfg.Adapters) == 0:
		return fmt.Errorf("runner: at least one adapter is required")
	}
	for _, a := range cfg.Agents {
		found := false
		for _, ad := range cfg.Adapters {
			if ad.Name() == a.Adapter {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("runner: agent %s/%s names adapter %q, which the runner does not carry", a.Adapter, a.Model, a.Adapter)
		}
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = heartbeatEvery
	}
	if cfg.LeaseWait <= 0 {
		cfg.LeaseWait = leaseWait
	}
	if cfg.CheckTimeout <= 0 {
		cfg.CheckTimeout = defaultCheckTimeout
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return nil
}

// harnessesOffered is the distinct set of harnesses the advertisement
// carries: what the runner offers on every lease poll.
func (cfg *Config) harnessesOffered() []string {
	var out []string
	for _, a := range cfg.Agents {
		if !slices.Contains(out, a.Harness) {
			out = append(out, a.Harness)
		}
	}
	return out
}

// adapterFor picks the adapter and model to run a job with: the agent
// advertised for the job's harness constraint, or the first agent's
// when the job leaves the harness open (§8). Choosing between several
// agents of one harness is the dispatcher's to do, a later phase.
func (cfg *Config) adapterFor(grant *api.JobGrant) (adapter.Adapter, string) {
	byName := make(map[string]adapter.Adapter, len(cfg.Adapters))
	for _, ad := range cfg.Adapters {
		byName[ad.Name()] = ad
	}
	choice := cfg.Agents[0]
	if grant.Harness != "" {
		for _, a := range cfg.Agents {
			if a.Harness == grant.Harness {
				choice = a
				break
			}
		}
	}
	return byName[choice.Adapter], choice.Model
}

// leaseBackoffMin and leaseBackoffMax bound the wait between lease
// attempts after a failure: long enough to let a restarting engine come
// back, short enough that a blink of the socket costs the runner a
// second, not a morning.
const (
	leaseBackoffMin = time.Second
	leaseBackoffMax = 30 * time.Second
)

// Run is the runner (§5.1): it registers the runner and its
// advertisement, then long-polls leases and runs one job at a time —
// workspace, worker, result contract, check, push — until ctx is done.
// It takes cfg by value and fills its defaults in its own copy, so the
// caller's Config is never mutated; the job runs hold the filled copy.
//
// The engine going away is not a reason to stop: registration and lease
// failures that are not the caller's own mistake — the socket blinking,
// the engine restarting — are retried with a doubling backoff, and the
// runner re-registers before it polls again, because a restarted engine
// may have a fresh store with no row for it. Run returns only on a
// configuration error (the caller's, or its own) or ctx cancellation; a
// single job's failure is carried to the engine, not into the loop.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.defaults(); err != nil {
		return err
	}
	client := api.NewClient(cfg.Socket, cfg.Token)
	registered := false
	backoff := leaseBackoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !registered {
			if err := client.Register(ctx, cfg.Agents); err != nil {
				if permanent := callerMistake(err); permanent != nil {
					return permanent
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// The engine is not answering: it may not be up yet (it
				// starts on demand, §13) or it is restarting. Wait and
				// try again rather than dying.
				cfg.Log.WarnContext(ctx, "register failed; retrying", "socket", cfg.Socket, "err", err)
				if !sleepBackoff(ctx, backoff) {
					return ctx.Err()
				}
				backoff = min(backoff*2, leaseBackoffMax)
				continue
			}
			registered = true
			backoff = leaseBackoffMin
			cfg.Log.InfoContext(ctx, "runner registered", "socket", cfg.Socket, "agents", len(cfg.Agents))
		}

		grant, err := client.Lease(ctx, cfg.LeaseWait, cfg.harnessesOffered(), cfg.Labels)
		if err != nil {
			if permanent := callerMistake(err); permanent != nil {
				return permanent
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A failed poll costs one backoff, and the next round
			// registers again: the engine may have restarted with a
			// fresh store, and register is an idempotent upsert.
			cfg.Log.WarnContext(ctx, "lease failed; retrying", "err", err)
			registered = false
			if !sleepBackoff(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, leaseBackoffMax)
			continue
		}
		backoff = leaseBackoffMin
		if grant == nil {
			continue // the wait ran out with nothing to do
		}
		r := &jobRun{cfg: &cfg, client: client, grant: grant}
		r.execute(ctx)
	}
}

// callerMistake filters an API error for the ones retrying cannot fix:
// a 4xx names a mistake in the call itself — a malformed advertisement,
// a token that is unknown or lacks the runner role — and is returned
// for the caller to correct. Everything else (transport, the engine's
// own 5xx) is nil: transient, worth another round. The one 4xx that is
// not a caller's mistake is a lease's 403 unknown_runner: it is what a
// restarted engine says to a runner whose row it no longer has, and
// the re-register cures it.
func callerMistake(err error) error {
	var se *api.StatusError
	if errors.As(err, &se) && se.IsClientError() &&
		!(se.Status == http.StatusForbidden && se.APICode == "unknown_runner") {
		return err
	}
	return nil
}

// sleepBackoff waits d and reports whether it ran out (true) or ctx
// ended it (false).
func sleepBackoff(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
