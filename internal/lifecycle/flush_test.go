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
// context cannot hold stop past the grace — it is abandoned, and the
// wait ends at the grace.
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
	d.stop(context.Background(), log, 50*time.Millisecond)
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("stop waited %v on a delivery that ignores its context, want the grace plus slack", elapsed)
	}
	close(block)
}
