package lifecycle

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// TestFlushDispatchSkipsWhileInFlight pins the tick's rule (§10): at
// most one delivery in flight — a start that finds one running is
// skipped, not queued, and never runs beside it.
func TestFlushDispatchSkipsWhileInFlight(t *testing.T) {
	log := slog.Default()
	d := &flushDispatch{}
	release := make(chan struct{})
	var started, running atomic.Int32

	fn := func(context.Context) error {
		started.Add(1)
		running.Add(1)
		defer running.Add(-1)
		<-release
		return nil
	}

	ctx := context.Background()
	if !d.start(ctx, fn, log) {
		t.Fatal("the first start was refused with nothing in flight")
	}
	time.Sleep(20 * time.Millisecond) // let the delivery reach its wait

	for i := 0; i < 3; i++ {
		if d.start(ctx, fn, log) {
			t.Error("a start while a delivery runs launched another instead of being skipped")
		}
		if n := running.Load(); n != 1 {
			t.Fatalf("%d deliveries running, want exactly one", n)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d deliveries started, want 1: a skipped flush is not queued", n)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for !d.start(ctx, fn, log) {
		if time.Now().After(deadline) {
			t.Fatal("the dispatch never took a new delivery after the first returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFlushDispatchStopBoundedByGrace: a delivery that ignores its
// context cannot hold stopBy past the deadline — it is abandoned, and
// the wait ends there.
func TestFlushDispatchStopBoundedByGrace(t *testing.T) {
	log := slog.Default()
	d := &flushDispatch{}
	block := make(chan struct{})
	fn := func(context.Context) error {
		<-block // ignores its ctx: ends only when the test says so
		return nil
	}
	if !d.start(context.Background(), fn, log) {
		t.Fatal("start refused with nothing in flight")
	}
	time.Sleep(20 * time.Millisecond)

	begin := time.Now()
	if d.stopBy(context.Background(), log, begin.Add(50*time.Millisecond)) {
		t.Error("stopBy reports the delivery ended, want it abandoned")
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("stopBy waited %v on a delivery that ignores its context, want the grace plus slack", elapsed)
	}
	close(block)
}

// TestFlushAtExitSkipsTheHookWhenDeliveryAteTheGrace pins the single
// shared deadline (§13): a delivery that outlives the grace consumes it
// whole, the pre-exit hook is skipped — never given a second grace
// beside the first — and the return comes within one grace.
func TestFlushAtExitSkipsTheHookWhenDeliveryAteTheGrace(t *testing.T) {
	log := slog.Default()
	d := &flushDispatch{}
	deliveryRunning := make(chan struct{})
	blockDelivery := make(chan struct{})
	delivery := func(context.Context) error {
		close(deliveryRunning)
		<-blockDelivery
		return nil
	}
	if !d.start(context.Background(), delivery, log) {
		t.Fatal("start refused with nothing in flight")
	}
	<-deliveryRunning

	var hooked atomic.Bool
	blockHook := make(chan struct{})
	hook := func(context.Context) error {
		hooked.Store(true)
		<-blockHook
		return nil
	}

	begin := time.Now()
	flushAtExit(context.Background(), log, 100*time.Millisecond, d, hook)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("flushAtExit took %v, want at most one grace plus slack", elapsed)
	}
	if hooked.Load() {
		t.Error("the pre-exit hook ran after the delivery spent the grace, want it skipped")
	}
	close(blockDelivery)
	close(blockHook)
}

// TestFlushAtExitAbandonsTheHook: with no delivery in flight, the hook
// gets the whole grace and one that ignores its context is abandoned at
// its end.
func TestFlushAtExitAbandonsTheHook(t *testing.T) {
	log := slog.Default()
	d := &flushDispatch{}
	block := make(chan struct{})
	hook := func(context.Context) error {
		<-block
		return nil
	}

	begin := time.Now()
	flushAtExit(context.Background(), log, 100*time.Millisecond, d, hook)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("flushAtExit took %v on a hook that ignores its context, want the grace plus slack", elapsed)
	}
	close(block)
}
