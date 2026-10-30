package engine

import (
	"context"
	"time"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// retryBusyTx runs one write transaction under the store's bounded busy
// retry (store.RetryBusy), on a context of the call's own: a request that
// gives up mid-call must not roll the transaction back while its
// statements still run, and the deadline keeps a wedged store from
// holding the call forever — the heartbeat's own rule (calls.go), kept
// over the whole retry, so the call's total wait stays within its one
// deadline (heartbeatDeadline for a heartbeat, writeDeadline for a
// sweep). fn is one transaction, never a sequence: a failed attempt
// rolled back clean, and it resets what it captures before each attempt,
// so only the committed attempt's values come back.
func retryBusyTx(ctx context.Context, deadline time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deadline)
	defer cancel()
	return store.RetryBusy(ctx, func() error { return fn(ctx) })
}

// faultBusy plays the store refusing this transaction the write lock: the
// error a test injected (faultBusyTx), or none. It runs at the end of a
// retried transaction's work, where a busy commit would, so the bounded
// retry and the rollback are exercised deterministically, without
// holding a real lock past the busy timeout.
func (e *Engine) faultBusy() error {
	if e.faultBusyTx == nil {
		return nil
	}
	return e.faultBusyTx()
}
