// The hostile-banner tests run the engine role in-process against a
// notifier that never returns (§10, §13): the delivery runs off the
// main loop — one in flight, a tick that finds it still running skips
// the flush — and no wait on it, or on the shutdown flush, outlives
// the grace.
package lifecycle_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// stuckNotifier is the hostile banner tool: every Notify blocks until
// the test releases it, whatever its context says — a notification
// exec that never returns. The first call announces itself on entered.
type stuckNotifier struct {
	release chan struct{}
	entered chan struct{}
	calls   atomic.Int32
	once    sync.Once
}

func (s *stuckNotifier) Notify(context.Context, string, string) error {
	s.once.Do(func() { close(s.entered) })
	s.calls.Add(1)
	<-s.release
	return nil
}

// Name is the log name.
func (s *stuckNotifier) Name() string { return "stuck" }

// newStuckNotifier wires a stuck notifier whose calls the test's
// cleanup releases.
func newStuckNotifier(t *testing.T) *stuckNotifier {
	t.Helper()
	n := &stuckNotifier{release: make(chan struct{}), entered: make(chan struct{})}
	t.Cleanup(func() { close(n.release) })
	return n
}

// seedHostileBannerStore seeds the one parking event the delivery
// loop's joins resolve, so its flush reaches the notifier.
func seedHostileBannerStore(t *testing.T, ctx context.Context, state string) {
	t.Helper()
	st, err := store.Open(ctx, lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	seedNotifyEvent(t, ctx, st)
	st.Close()
}

// TestRunDeliveryOffTheMainLoop: a notifier that never returns does
// not stop the engine. The tick's delivery runs in its own goroutine —
// a tick that finds it still running skips the flush and goes on with
// the sweep, the snapshot and the idle check — so the engine idles out
// on schedule, and neither the wait on the in-flight delivery nor the
// pre-exit flush outlives the grace.
func TestRunDeliveryOffTheMainLoop(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	ctx := context.Background()
	seedHostileBannerStore(t, ctx, state)
	notifier := newStuckNotifier(t)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- lifecycle.Run(ctx, lifecycle.Config{
			StateDir:   state,
			Notify:     "none",
			Notifier:   notifier,
			Idle:       300 * time.Millisecond,
			IdleTick:   50 * time.Millisecond,
			FlushGrace: 300 * time.Millisecond,
		})
	}()

	select {
	case <-notifier.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery loop never called the notifier")
	}
	// The engine must idle out — sweep, snapshot and idle check all
	// alive — and leave within the bounds: idle (300ms) plus the
	// abandoned in-flight delivery and the abandoned shutdown flush
	// (300ms each) and slack.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run the engine: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck notifier stopped the engine's idle exit or held its shutdown")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the engine left after %v, want it within idle plus two graces", took)
	}
}

// TestRunSkipsFlushWhileDeliveryInFlight: while one delivery is stuck
// in the notifier, later ticks skip the flush — the notifier is not
// called again, nothing queues, nothing runs concurrently — and a
// ctx-cancel shutdown still returns within the grace.
func TestRunSkipsFlushWhileDeliveryInFlight(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seedHostileBannerStore(t, ctx, state)
	notifier := newStuckNotifier(t)

	done := make(chan error, 1)
	go func() {
		done <- lifecycle.Run(ctx, lifecycle.Config{
			StateDir:   state,
			Notify:     "none",
			Notifier:   notifier,
			Idle:       time.Hour, // no idle exit; the test cancels
			IdleTick:   50 * time.Millisecond,
			FlushGrace: 300 * time.Millisecond,
		})
	}()

	select {
	case <-notifier.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery loop never called the notifier")
	}
	// Five ticks pass while the first delivery is stuck: the flush is
	// skipped, so exactly one Notify has run.
	time.Sleep(300 * time.Millisecond)
	if n := notifier.calls.Load(); n != 1 {
		t.Fatalf("the stuck delivery saw %d Notify calls, want 1: a tick must skip the flush, not queue or run it concurrently", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run the engine: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the ctx-cancel shutdown waited on the stuck delivery past the grace")
	}
}
