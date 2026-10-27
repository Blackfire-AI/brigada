package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/caps"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/notify"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// defaultIdle is how long the engine stays up with nothing to do (§13):
// half an hour, tunable by config and flag.
const defaultIdle = 30 * time.Minute

// flushGrace bounds the notification flush at shutdown (§13): the hook
// is the §10 work's to fill in; whatever it does, it cannot hold the
// engine's exit hostage.
const flushGrace = 10 * time.Second

// Config is the engine role's own settings (§13): where its state lives
// and when it may idle out. Paths left zero derive from StateDir.
type Config struct {
	StateDir  string
	StorePath string
	Socket    string
	LockPath  string
	TokenFile string

	// Idle is how long with no live jobs and no connected requests the
	// engine tolerates before it exits (§13); zero keeps 30 minutes.
	Idle time.Duration
	// Caps are the concurrency caps and cost weights the config file
	// named (§8.1); the zero value caps nothing.
	Caps caps.Limits
	// Notify selects the local notifier (§10): notify.ModeAuto picks
	// the OS tool on PATH, notify.ModeNone silences notifications; the
	// empty value keeps auto.
	Notify string
	// NotifyWebhook is the optional webhook URL (§10): one JSON POST
	// per delivered notification, beside the OS banner. Empty = none.
	// The URL may carry a token, so it is never logged and never
	// reaches the status or the history.
	NotifyWebhook string
	// SnapshotDir is where the engine snapshots its store daily (§6.4);
	// empty takes no snapshots.
	SnapshotDir string
	// Rounds are the config file's default round caps; unnamed keys keep
	// the engine defaults.
	Rounds RoundsConfig
	// Review is the config file's default tier and effort of a review;
	// unnamed keys keep the engine defaults.
	Review engine.ReviewConfig
	// TaskAutoRetry controls transient work retries; nil keeps the default, on.
	TaskAutoRetry *bool
	// ForemanWakeBudget is the default daily foreman wake budget of every
	// project (§4.3); 0 keeps the built-in default of 20. A project's
	// foreman_wakes_per_day setting overrides it.
	ForemanWakeBudget int
	// DefaultReviewer is the harness a standalone review goes to when its
	// author's family cannot be told; empty = none.
	DefaultReviewer string
	// IdleTick is how often busyness is checked; zero derives a tick
	// from Idle, clamped to [200ms, 30s].
	IdleTick time.Duration
	// Now is the idle rule's clock; tests inject one and drive it. Nil
	// is real time.
	Now func() time.Time
	// FlushNotifications is the §10 hook, called once before an idle
	// exit with unsent notifications to deliver; nil is "nothing to
	// flush yet".
	FlushNotifications func(ctx context.Context) error
	// FlushGrace bounds every shutdown wait on the notification flush
	// and on a delivery still in flight (§13): past it the engine
	// leaves without the wait, with a log line. Zero keeps 10s.
	FlushGrace time.Duration
	// Notifier overrides the notifier the notify mode resolves (§10):
	// the tests inject one; production leaves it nil.
	Notifier notify.Notifier
	// Log is the engine's logger; nil uses slog's default.
	Log *slog.Logger
}

func (c *Config) resolve() (Paths, error) {
	if c.StateDir == "" {
		return Paths{}, fmt.Errorf("engine: a state directory is required")
	}
	p := PathsOf(c.StateDir)
	if c.StorePath != "" {
		p.Store = c.StorePath
	}
	if c.Socket != "" {
		p.Socket = c.Socket
	}
	if c.LockPath != "" {
		p.Lock = c.LockPath
	}
	if c.TokenFile != "" {
		p.Tokens = c.TokenFile
	}
	if err := CheckSocketPath(p.Socket); err != nil {
		return Paths{}, err
	}
	if c.Idle <= 0 {
		c.Idle = defaultIdle
	}
	if c.FlushGrace <= 0 {
		c.FlushGrace = flushGrace
	}
	if c.IdleTick <= 0 {
		c.IdleTick = min(max(c.Idle/4, 200*time.Millisecond), 30*time.Second)
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return p, nil
}

// Run is the engine role (§13): it takes the single-instance lock, opens
// the store, records itself as the engine, and serves the API on the
// socket until ctx ends, serving fails, or it has been idle long enough.
// On the way out it flushes notifications (§10's hook), removes the
// socket and releases the lock, in that order.
func Run(ctx context.Context, cfg Config) error {
	p, err := cfg.resolve()
	if err != nil {
		return err
	}
	log := cfg.Log
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return fmt.Errorf("engine: create the state directory %s: %w", p.StateDir, err)
	}
	lock, err := AcquireLock(p.Lock)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := EnsureTokenFile(p.Tokens); err != nil {
		return err
	}

	started := cfg.Now()
	st, err := store.Open(ctx, p.Store)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	defer st.Close()
	roundCaps := cfg.Rounds.Caps()
	eng := engine.New(st, engine.Config{Now: cfg.Now, SweepInterval: cfg.IdleTick, Caps: cfg.Caps, PlanFiles: true, SnapshotDir: cfg.SnapshotDir, TaskAutoRetry: cfg.TaskAutoRetry, ForemanWakeBudget: cfg.ForemanWakeBudget, DefaultReviewer: cfg.DefaultReviewer, ExactRoundCaps: &roundCaps, Review: cfg.Review})
	// Project identities are canonical repo URLs (design §13.1): rows
	// written in an older form are rewritten once, before anything reads
	// them. Rows that cannot be, or that name one repository twice, stay
	// as they are and are said so here and in the status.
	canon, err := eng.CanonicalizeProjects(ctx)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	if canon.Rewritten > 0 {
		log.InfoContext(ctx, "project identities rewritten to canonical repo URLs", "projects", canon.Rewritten)
	}
	for repo, ids := range canon.Collisions {
		log.WarnContext(ctx, "projects name the same repository and were left as they are", "repo", repo, "projects", ids)
	}
	for id, why := range canon.Failed {
		log.WarnContext(ctx, "a project's repo URL cannot be canonicalized and was left as it is", "project", id, "why", why)
	}
	// A push still running when the engine stops finishes before the
	// store closes (defers run last-in first); what it misses stays marked
	// in the store for the next run's catch-up below.
	defer eng.WaitPlanFiles()
	eng.SyncPlanFiles()
	// A snapshot due since the last run is taken now, before the API
	// serves; the tick below keeps the daily pace.
	if path, err := eng.SnapshotIfDue(ctx); err != nil {
		log.WarnContext(ctx, "store snapshot failed", "err", err)
	} else if path != "" {
		log.InfoContext(ctx, "store snapshot taken", "path", path)
	}
	if err := eng.RecordInstance(ctx, int64(os.Getpid()), started); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	// Recovery (§13): judgment events that parked a task while no engine
	// ran get their foreman turn now — after the sweep, as every engine
	// call does it, and before the API serves a soul.
	if err := eng.RecoverForemanWakes(ctx); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	srv, err := api.New(ctx, st, eng, log, p.Tokens)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}

	// §10's delivery loop: the local notifier the config selected and a
	// loop that walks the event log once per event. The pre-exit hook
	// (§13) flushes whatever a tick has not delivered yet; a caller's
	// own FlushNotifications still wins, the tests inject one.
	notifier := notify.Resolve(cfg.Notify)
	if cfg.Notifier != nil {
		notifier = cfg.Notifier
	}
	nloop := notify.NewLoop(st, notifier, log)
	if cfg.NotifyWebhook != "" {
		nloop = nloop.WithWebhook(&notify.Webhook{URL: cfg.NotifyWebhook})
	}
	if cfg.FlushNotifications == nil {
		cfg.FlushNotifications = nloop.Flush
	}
	log.InfoContext(ctx, "notifications", "notifier", notify.NameOf(notifier), "webhook", cfg.NotifyWebhook != "")

	// Serving stops when stopServe is called; Listen then removes the
	// socket it owns. The context is deliberately WithoutCancel: the
	// engine's own shutdown, not the caller's exit, takes the listener
	// down, so the socket is removed even from a dying parent's context.
	serveCtx, stopServe := context.WithCancel(context.WithoutCancel(ctx))
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Listen(serveCtx, p.Socket) }()

	busy := func() bool {
		if n, err := eng.LiveJobs(ctx); err != nil {
			// A store that cannot be read is not an idle engine: stay up
			// and let the failure surface in the requests it causes.
			log.WarnContext(ctx, "idle check: count live jobs failed", "err", err)
			return true
		} else if n > 0 {
			return true
		}
		// An attached chat is a live session on this engine (§13): its
		// hold's token and events tool answer here, so the engine of an
		// attach that is still beating stays up however quiet the chat.
		if n, err := eng.LiveAttaches(ctx); err != nil {
			log.WarnContext(ctx, "idle check: count live attach holds failed", "err", err)
			return true
		} else if n > 0 {
			return true
		}
		return srv.InFlight() > 0
	}
	mon := newIdleMonitor(cfg.Idle, busy, started)
	ticker := time.NewTicker(cfg.IdleTick)
	defer ticker.Stop()

	log.InfoContext(ctx, "engine up", "pid", os.Getpid(), "socket", p.Socket, "store", p.Store, "idle_after", cfg.Idle)

	// §10's delivery runs off the main loop: a tick starts at most one
	// flush in its own goroutine, and a tick that finds one still
	// running skips it — no queue, no second concurrent flush — so a
	// notifier that never returns cannot stall the sweep, the snapshot
	// or the idle exit. Shutdown cancels the goroutine's context and
	// waits for it only within the grace.
	var (
		deliverStop context.CancelFunc
		deliverDone chan struct{}
	)
	startDelivery := func() {
		dctx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		deliverStop, deliverDone = stop, done
		go func() {
			defer close(done)
			if err := nloop.Flush(dctx); err != nil && dctx.Err() == nil {
				log.Warn("deliver notifications failed", "err", err)
			}
		}()
	}
	// stopDelivery cancels an in-flight delivery and waits for it only
	// within the grace: a notifier that ignores its context cannot hold
	// the exit — the goroutine is abandoned with a log line.
	stopDelivery := func() {
		if deliverDone == nil {
			return
		}
		deliverStop()
		select {
		case <-deliverDone:
		case <-time.After(cfg.FlushGrace):
			log.Warn("notification delivery still in flight at shutdown; abandoning it")
		}
	}
	for {
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "engine shutting down")
			stopDelivery()
			return stopEngine(stopServe, serveErr)
		case err := <-serveErr:
			stopServe()
			stopDelivery()
			if err != nil {
				return fmt.Errorf("engine: %w", err)
			}
			return nil
		case <-ticker.C:
			// The periodic sweep keeps the store honest without a call
			// (§13): leases and attach holds lapse on time, with no new
			// event to say so.
			if deliverDone != nil {
				select {
				case <-deliverDone:
					// The last delivery finished; reap it and start a
					// fresh one below.
					deliverStop()
					deliverStop, deliverDone = nil, nil
				default:
				}
			}
			if deliverDone != nil {
				log.DebugContext(ctx, "a notification delivery is still in flight; skipping this flush")
			} else {
				startDelivery()
			}
			if _, err := eng.Sweep(ctx); err != nil {
				log.WarnContext(ctx, "sweep failed", "err", err)
			}
			if path, err := eng.SnapshotIfDue(ctx); err != nil {
				log.WarnContext(ctx, "store snapshot failed", "err", err)
			} else if path != "" {
				log.InfoContext(ctx, "store snapshot taken", "path", path)
			}
			now := cfg.Now()
			mon.poll(now)
			if !mon.shouldExit(now) {
				continue
			}
			log.InfoContext(ctx, "engine idle; shutting down", "idle_for", now.Sub(mon.lastBusy).Round(time.Second))
			stopDelivery()
			if cfg.FlushNotifications != nil {
				// The hook runs in its own goroutine and the engine
				// stops waiting at the grace: a flush that ignores its
				// context is abandoned with a log line, never an exit
				// held hostage (§13).
				flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.FlushGrace)
				done := make(chan error, 1)
				go func() { done <- cfg.FlushNotifications(flushCtx) }()
				select {
				case err := <-done:
					if err != nil {
						log.WarnContext(ctx, "flush notifications at shutdown failed", "err", err)
					}
				case <-time.After(cfg.FlushGrace):
					log.WarnContext(ctx, "flush notifications at shutdown outlived the grace; abandoning it")
				}
				cancel()
			}
			return stopEngine(stopServe, serveErr)
		}
	}
}

// stopEngine takes the listener down and waits for the socket to be
// removed before returning, so a caller that starts the next engine at
// once finds no leftover to reason about (§13).
func stopEngine(stopServe context.CancelFunc, serveErr <-chan error) error {
	stopServe()
	<-serveErr
	return nil
}

// idleMonitor is §13's idle rule as a unit: when the engine last had
// something to do, measured on the injected clock the tests drive. The
// engine is idle exactly when it has been busy never in the last
// idleAfter — live jobs, connected requests, all of it — and the
// monitor's owner decides what "busy" reads.
type idleMonitor struct {
	idleAfter time.Duration
	busy      func() bool
	lastBusy  time.Time
}

func newIdleMonitor(idleAfter time.Duration, busy func() bool, now time.Time) *idleMonitor {
	return &idleMonitor{idleAfter: idleAfter, busy: busy, lastBusy: now}
}

// poll records whether the engine had something to do at now.
func (m *idleMonitor) poll(now time.Time) {
	if m.busy() {
		m.lastBusy = now
	}
}

// shouldExit reports whether the engine has now been idle long enough.
func (m *idleMonitor) shouldExit(now time.Time) bool {
	return now.Sub(m.lastBusy) >= m.idleAfter
}
