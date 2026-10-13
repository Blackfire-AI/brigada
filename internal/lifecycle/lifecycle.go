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
	eng := engine.New(st, engine.Config{Now: cfg.Now, Caps: cfg.Caps})
	if err := eng.RecordInstance(ctx, int64(os.Getpid()), started); err != nil {
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
	nloop := notify.NewLoop(st, notifier, log)
	if cfg.FlushNotifications == nil {
		cfg.FlushNotifications = nloop.Flush
	}
	log.InfoContext(ctx, "notifications", "notifier", notify.NameOf(notifier))

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
		return srv.InFlight() > 0
	}
	mon := newIdleMonitor(cfg.Idle, busy, started)
	ticker := time.NewTicker(cfg.IdleTick)
	defer ticker.Stop()

	log.InfoContext(ctx, "engine up", "pid", os.Getpid(), "socket", p.Socket, "store", p.Store, "idle_after", cfg.Idle)
	for {
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "engine shutting down")
			return stopEngine(stopServe, serveErr)
		case err := <-serveErr:
			stopServe()
			if err != nil {
				return fmt.Errorf("engine: %w", err)
			}
			return nil
		case <-ticker.C:
			if err := nloop.Flush(ctx); err != nil {
				log.WarnContext(ctx, "deliver notifications failed", "err", err)
			}
			now := cfg.Now()
			mon.poll(now)
			if !mon.shouldExit(now) {
				continue
			}
			log.InfoContext(ctx, "engine idle; shutting down", "idle_for", now.Sub(mon.lastBusy).Round(time.Second))
			if cfg.FlushNotifications != nil {
				flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushGrace)
				if err := cfg.FlushNotifications(flushCtx); err != nil {
					log.WarnContext(ctx, "flush notifications at shutdown failed", "err", err)
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
