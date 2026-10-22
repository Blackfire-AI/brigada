package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestIsBusy covers the busy matcher against the driver's error text,
// wrapped as the engine wraps it, and the errors it must not match.
func TestIsBusy(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	locked := errors.New("database table is locked (5) (SQLITE_LOCKED)")
	for _, err := range []error{
		busy,
		locked,
		fmt.Errorf("record the offer of runner 3: %w", busy),
		fmt.Errorf("lease: %w", fmt.Errorf("sweep hard caps: %w", locked)),
	} {
		if !IsBusy(err) {
			t.Errorf("IsBusy(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		nil,
		errors.New("sql: transaction has already been committed or rolled back"),
		errors.New("no such table: jobs"),
		context.Canceled,
	} {
		if IsBusy(err) {
			t.Errorf("IsBusy(%v) = true, want false", err)
		}
	}
}

// TestRetryBusyRidesOutContention covers the retry: a fn that fails
// busy twice succeeds on the third run, after the backoff waited.
func TestRetryBusyRidesOutContention(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	n := 0
	start := time.Now()
	err := RetryBusy(context.Background(), func() error {
		n++
		if n < 3 {
			return busy
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryBusy: %v", err)
	}
	if n != 3 {
		t.Fatalf("fn ran %d times, want 3", n)
	}
	if waited := time.Since(start); waited < 75*time.Millisecond {
		t.Fatalf("RetryBusy waited %v, want the 25+50 ms backoff", waited)
	}
}

// TestRetryBusyReturnsOtherErrors covers the boundary: an error that is
// not busy goes back at once, unretried.
func TestRetryBusyReturnsOtherErrors(t *testing.T) {
	other := errors.New("no such table: jobs")
	n := 0
	start := time.Now()
	if err := RetryBusy(context.Background(), func() error { n++; return other }); err != other {
		t.Fatalf("RetryBusy error = %v, want %v", err, other)
	}
	if n != 1 {
		t.Fatalf("fn ran %d times, want 1", n)
	}
	if waited := time.Since(start); waited > 10*time.Millisecond {
		t.Fatalf("RetryBusy waited %v on an error it cannot ride out", waited)
	}
}

// TestRetryBusyIsBounded covers the bound: a fn that stays busy runs
// busyAttempts times and no more, and the last busy error goes back.
func TestRetryBusyIsBounded(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	n := 0
	start := time.Now()
	err := RetryBusy(context.Background(), func() error { n++; return busy })
	if err != busy {
		t.Fatalf("RetryBusy error = %v, want the busy error", err)
	}
	if n != busyAttempts {
		t.Fatalf("fn ran %d times, want %d", n, busyAttempts)
	}
	if waited := time.Since(start); waited < 375*time.Millisecond {
		t.Fatalf("RetryBusy waited %v, want the full 25+50+100+200 ms backoff", waited)
	}
}

// TestRetryBusyStopsOnACancelledContext covers the wait's bound: once
// the context is done, no further attempt is made and the last busy
// error goes back.
func TestRetryBusyStopsOnACancelledContext(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	err := RetryBusy(ctx, func() error {
		n++
		cancel()
		return busy
	})
	if err != busy {
		t.Fatalf("RetryBusy error = %v, want the busy error", err)
	}
	if n != 1 {
		t.Fatalf("fn ran %d times, want the one attempt", n)
	}
}
