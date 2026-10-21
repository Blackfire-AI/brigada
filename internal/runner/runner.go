package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
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
	// AgentSpecs records the local launch configuration for init comparisons,
	// including agent-file, which is not part of the engine advertisement.
	AgentSpecs []string
	// Adapters are the adapter implementations the runner can drive;
	// each agent's Adapter names one of them.
	Adapters []adapter.Adapter
	// Labels are offered on every lease poll (§8). The git label is
	// added by defaults when absent (§7.1), so a land job — which needs
	// no agent, only the git label — can always be leased.
	Labels []string
	// MaxParallel is the runner's own cap on parallel jobs (§8.1),
	// advertised at register; 0 means unset.
	MaxParallel int
	// HeartbeatEvery, LeaseWait and CheckTimeout override the defaults;
	// zero keeps them.
	HeartbeatEvery time.Duration
	LeaseWait      time.Duration
	CheckTimeout   time.Duration
	// SetupTimeout, when set, replaces the time bound a task's recorded
	// setup carries (§5.7); zero keeps the recorded one.
	SetupTimeout time.Duration
	// StallAfter is how long without any worker activity a run counts
	// as stalled (§5.6); zero keeps the 10-minute default, negative
	// turns the silence watch off.
	StallAfter time.Duration
	// ClassCaps overrides the per-class caps (§5.6), keyed by job kind
	// — work, fix, review, foreman. A zero entry keeps that class's
	// default, a set field overrides it, a negative one switches that
	// bound off for the class.
	ClassCaps map[string]ClassCaps
	// KillGrace is how long a stopped worker's process tree has between
	// the runner's SIGTERM and its SIGKILL (§5.6); zero keeps 10 s.
	KillGrace time.Duration
	// ProbeEvery is how often the runner asks the engine for due
	// broken-agent probes (§8 "Health"); zero keeps 1 minute.
	ProbeEvery time.Duration
	// Clock is the runner's time (§5.6): the wall the activity clock
	// reads and the tickers the heartbeats beat on. Nil is real time;
	// tests inject a manual one so no test sleeps minutes.
	Clock Clock
	// GitEnv is extra environment for the runner's own git commands —
	// where its credentials ride when they come from config (§5.1).
	// They never reach the worker's argv or prompt.
	GitEnv []string
	// Ready, when set, runs once the runner holds its state directory's
	// lock and its configuration has passed — the moment it is up. The
	// caller's startup line belongs here: printed before Run, a second
	// process on the same state directory would announce itself up and
	// then be refused at the lock.
	Ready func()
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
	// Every runner offers git (§7.1): a land job needs no agent, only
	// the git label, and the runner's own git always drives it. The
	// label is added rather than required, so an explicit --labels list
	// is widened, never replaced.
	cfg.Labels = appendLabel(cfg.Labels, "git")
	if cfg.StallAfter == 0 {
		cfg.StallAfter = defaultStallAfter
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = killGrace
	}
	if cfg.ProbeEvery <= 0 {
		cfg.ProbeEvery = probeEvery
	}
	cfg.Clock.fill()
	cfg.ClassCaps = mergeClassCaps(cfg.ClassCaps)
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return nil
}

// The stall policy's defaults (§5.6, §14): a worker silent for 10
// minutes is stalled, one tool call may stay open for 30, and a killed
// tree has 10 seconds between the term and the kill.
const (
	defaultStallAfter = 10 * time.Minute
	defaultToolCap    = 30 * time.Minute
	killGrace         = 10 * time.Second
)

// ClassCaps are one job class's own bounds (§5.6): how long a single
// tool call may stay open — the bound on the suspension it buys the
// stall clock — and how long the whole run may wall. The shipped
// defaults sit just under the engine's hard caps (§14: 45 m work and
// fix, 25 m review, 30 m foreman), so a runner that reaches its cap
// completes the job with a useful result instead of racing the engine's
// cap to a lost lease. A negative ToolCap leaves an in-flight call
// suspending the stall clock unbounded — silence inside a tool call is
// then never a stall — so switch it off only for a class whose long
// calls are trusted.
type ClassCaps struct {
	ToolCap time.Duration
	TimeCap time.Duration
}

// defaultClassCaps is what a class gets when nothing overrides it.
func defaultClassCaps() map[string]ClassCaps {
	return map[string]ClassCaps{
		"work":    {ToolCap: defaultToolCap, TimeCap: 40 * time.Minute},
		"fix":     {ToolCap: defaultToolCap, TimeCap: 40 * time.Minute},
		"review":  {ToolCap: defaultToolCap, TimeCap: 20 * time.Minute},
		"foreman": {ToolCap: defaultToolCap, TimeCap: 25 * time.Minute},
	}
}

// mergeClassCaps folds the configured per-class caps over the defaults:
// a set field overrides its default, a zero field keeps it, a negative
// one switches that bound off for the class. A class the defaults do
// not know starts from nothing — its tool calls get the 30-minute cap,
// its runs no wall cap — so an unknown kind is watched for silence but
// never cut short by a cap it never agreed to.
func mergeClassCaps(configured map[string]ClassCaps) map[string]ClassCaps {
	out := defaultClassCaps()
	for kind, caps := range configured {
		merged := out[kind]
		if merged.ToolCap == 0 {
			merged.ToolCap = defaultToolCap
		}
		if caps.ToolCap != 0 {
			merged.ToolCap = caps.ToolCap
		}
		if caps.TimeCap != 0 {
			merged.TimeCap = caps.TimeCap
		}
		out[kind] = merged
	}
	return out
}

// appendLabel returns labels with want appended when it is absent, so
// the caller's list keeps its order and never doubles a label.
func appendLabel(labels []string, want string) []string {
	if slices.Contains(labels, want) {
		return labels
	}
	return append(labels, want)
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

// agentFor picks the adapter and model to run a job with (§5.1, §8):
// the agent the lease named in the grant — never the job's own harness
// constraint, which is the dispatcher's input. Grants name the adapter
// and model as well, since a runner may offer several of one harness.
// Older engines may name only the harness. Several agents of
// one adapter name (two scripted fakes) pair with that name's adapters
// in advertisement order. A grant naming an agent this runner does not
// offer — or naming none — is an error the job fails with.
func (cfg *Config) agentFor(grant *api.JobGrant) (adapter.Adapter, string, error) {
	offered := cfg.harnessesOffered()
	if grant.Agent == "" {
		return nil, "", fmt.Errorf("the lease named no agent; this runner offers %s", strings.Join(offered, ", "))
	}
	for i, a := range cfg.Agents {
		if !agentNamed(a, grant) {
			continue
		}
		ad := cfg.adapterOfAgent(i)
		if ad == nil {
			return nil, "", fmt.Errorf("agent %s names adapter %q, which the runner does not carry", a.Harness, a.Adapter)
		}
		return ad, a.Model, nil
	}
	named := grant.Agent
	if grant.AgentModel != "" {
		named += "/" + grant.AgentModel
	}
	return nil, "", fmt.Errorf("the lease named agent %q, which this runner does not offer (offered: %s)",
		named, strings.Join(offered, ", "))
}

// agentNamed reports whether the advertised agent is the one the grant
// names. Adapter and model identify a registration exactly. Older engines
// may name the harness alone.
func agentNamed(a api.Agent, grant *api.JobGrant) bool {
	if a.Harness != grant.Agent {
		return false
	}
	if grant.AgentModel != "" && a.Model != grant.AgentModel {
		return false
	}
	return grant.AgentAdapter == "" || a.Adapter == grant.AgentAdapter
}

// effortFor uses the project's effort for foreman turns and the grant's
// mapped effort for task jobs, falling back to the named agent's default.
func (cfg *Config) effortFor(grant *api.JobGrant) string {
	if grant.Kind == "foreman" && grant.ForemanEffort != "" {
		return grant.ForemanEffort
	}
	if grant.Kind != "foreman" && grant.Effort != "" {
		return grant.Effort
	}
	for _, a := range cfg.Agents {
		if agentNamed(a, grant) {
			return a.Effort
		}
	}
	return ""
}

// adapterOfAgent returns the adapter paired with the i-th advertised
// agent. Agents pair with adapters of their adapter name in order: the
// first agent naming a name takes that name's first adapter, the
// second its second — deterministic without extra configuration, and
// the only pairing two same-named agents (two scripted fakes) can
// have. Nil when no adapter of the name exists.
func (cfg *Config) adapterOfAgent(i int) adapter.Adapter {
	var nth int
	for j, a := range cfg.Agents {
		if j >= i {
			break
		}
		if a.Adapter == cfg.Agents[i].Adapter {
			nth++
		}
	}
	seen := 0
	for _, ad := range cfg.Adapters {
		if ad.Name() != cfg.Agents[i].Adapter {
			continue
		}
		if seen == nth {
			return ad
		}
		seen++
	}
	return nil
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
// advertisement, then long-polls leases and runs up to MaxParallel jobs
// at once (one when unset), each in its own goroutine — workspace,
// worker, result contract, check, push — until ctx is done; it polls
// only while a slot is free, and on the way out waits for its jobs.
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
	// One runner process per state directory (§5.1): the runner token
	// names one identity, and a second process on it would replace the
	// first's agents at register while both drove the same worktrees.
	unlock, err := lockStateDir(cfg.StateDir)
	if err != nil {
		return err
	}
	defer unlock()

	if cfg.Ready != nil {
		cfg.Ready()
	}
	// The foreman turns' MCP configs die with their turns (§12.2); a
	// runner killed mid-turn leaves its behind, token and all. The tree
	// goes now, before any new turn's config joins it.
	sweepMCPConfigs(ctx, cfg.StateDir, cfg.Log)
	// Worktrees a crashed runner left behind are swept whole: this
	// runner holds the state directory's lock, so whatever is there now
	// is dead — its jobs' leases lapsed with the crash — and a
	// session-stable path (§5.1) is rebuilt from its branch by the job
	// that leases next. The mirrors' records of them are pruned by the
	// next set-up. The extra checkouts beside them (§5.8) are read-only,
	// so they are made writable first.
	if err := removeAllWritableErr(filepath.Join(cfg.StateDir, "worktrees")); err != nil {
		cfg.Log.WarnContext(ctx, "sweep the worktrees a crashed runner left", "err", err)
	}
	// Likewise the jobs' private setup cache copies (§5.7).
	removeAllWritable(setupJobsRoot(cfg.StateDir))
	client := api.NewClient(cfg.Socket, cfg.Token)
	registered := false
	backoff := leaseBackoffMin

	// The slot semaphore (§8.1): one token per job the runner may run at
	// once, --max-parallel of them, one when unset. The loop takes a
	// token before it polls and hands it to the job it leases, so it
	// never holds a lease it has no slot to run; a poll that brings
	// nothing gives it back. Every job runs in its own goroutine, on
	// its own jobRun.
	slots := make(chan struct{}, max(cfg.MaxParallel, 1))
	var jobs sync.WaitGroup
	// The broken-agent probe loop (§8 "Health") starts once the runner
	// is registered, and keeps asking the engine for due probes for the
	// runner's whole life: an engine that restarts answers "unknown
	// runner" until the lease loop re-registers, which is no reason to
	// stop probing.
	var probes sync.WaitGroup
	probesStarted := false
	// However Run ends, the jobs end with it: the context they run
	// under is cancelled and waited out, so a lease the runner stops
	// serving lapses (§5.6) and no job outlives the runner's lock.
	runCtx, stopJobs := context.WithCancel(ctx)
	defer func() {
		stopJobs()
		jobs.Wait()
		probes.Wait()
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !registered {
			if err := client.Register(ctx, cfg.Agents, cfg.MaxParallel); err != nil {
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
			// The detached starter waits for this record, rather than trusting
			// a log line or a live pid before registration has actually succeeded.
			if err := runnerstate.PublishParallel(cfg.StateDir, cfg.Agents, max(1, cfg.MaxParallel), cfg.AgentSpecs...); err != nil {
				return fmt.Errorf("record runner agents: %w", err)
			}
			registered = true
			backoff = leaseBackoffMin
			cfg.Log.InfoContext(ctx, "runner registered", "socket", cfg.Socket, "agents", len(cfg.Agents))
			if !probesStarted {
				probesStarted = true
				probes.Add(1)
				go func() {
					defer probes.Done()
					probeLoop(runCtx, &cfg, client)
				}()
			}
		}

		// Poll only while a slot is free; a full runner waits here for
		// a job to end, or for shutdown.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		grant, err := client.Lease(ctx, cfg.LeaseWait, cfg.harnessesOffered(), cfg.Labels)
		if err != nil || grant == nil {
			<-slots
		}
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
		jobs.Add(1)
		go func() {
			defer func() {
				<-slots
				jobs.Done()
			}()
			r.execute(runCtx)
		}()
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
