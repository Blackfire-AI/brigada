package store

import (
	"context"
	"strings"
	"time"
)

// IsBusy reports whether err is SQLite refusing the database to the
// caller: SQLITE_BUSY or SQLITE_LOCKED, the "database is locked"
// family, as another connection held the write lock past the busy
// timeout. A bounded retry can ride that out; anything else cannot.
// The driver's error text is matched unwrapped, since every wrapping
// on the way up (fmt.Errorf's %w included) keeps the text.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// busyAttempts is how many times RetryBusy runs fn, the first attempt
// included; busyFirstBackoff is where its doubling backoff starts.
const (
	busyAttempts     = 5
	busyFirstBackoff = 25 * time.Millisecond
)

// RetryBusy runs fn and re-runs it while it fails busy (IsBusy), with
// the backoff doubling from busyFirstBackoff and bounded by
// busyAttempts. A fn that fails any other way, or succeeds, ends the
// retry with that result; a context that runs out ends it with the
// last busy error. fn must be safe to re-run — a failed transaction
// rolled back, so its effects are gone — and it should begin on a
// context that does not cancel under it, or the retry inherits the
// very failure it is meant to ride out.
func RetryBusy(ctx context.Context, fn func() error) error {
	backoff := busyFirstBackoff
	var err error
	for i := 0; i < busyAttempts; i++ {
		if i > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return err
			case <-timer.C:
			}
			backoff *= 2
		}
		if err = fn(); err == nil || !IsBusy(err) {
			return err
		}
	}
	return err
}
