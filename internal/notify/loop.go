package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// batch is how many events one pass reads past the cursor: bounded, so
// a long-quiet engine catching up does not hold a read open over the
// whole log — the next pass continues exactly where this one stopped.
const batch = 200

// maxPasses caps one Flush's catch-up work (batch per pass). A normal
// pass sees a handful of events; the cap only bites on a pathological
// burst, and the remainder waits for the next tick — nothing is lost,
// the cursor is what remembers.
const maxPasses = 25

// passBudget bounds one pass's notifying: a banner tool may hang for
// its whole exec timeout on every event, and the loop must not let a
// pathological batch hold the engine's tick — or its shutdown — hostage
// for the sum of those hangs.
const passBudget = 30 * time.Second

// eventPayload is the slice of the engine's event payload the mapper
// reads: the reason words for the kinds that notify. Defined here,
// not imported from the engine, to keep this package's reading of the
// log self-contained.
type eventPayload struct {
	Status   string `json:"status,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Question string `json:"question,omitempty"`
	Findings string `json:"findings,omitempty"`
	Report   string `json:"report,omitempty"`
	Until    string `json:"until,omitempty"`
	Harness  string `json:"harness,omitempty"`
	Model    string `json:"model,omitempty"`
	Plan     int64  `json:"plan,omitempty"`
}

// Loop is the delivery loop (§13): it polls the event log for rows
// past a persisted cursor and notifies each mapped event once. The
// cursor lives in the store's meta row, so a restart resumes exactly
// after the last delivered event — never re-notifying old ones, never
// missing new ones — and advances even past events whose notification
// failed, so one broken banner never repeats forever.
type Loop struct {
	st  *store.Store
	n   Notifier
	log *slog.Logger
}

// NewLoop returns a delivery loop over st, delivering through n. A nil
// logger keeps slog's default.
func NewLoop(st *store.Store, n Notifier, log *slog.Logger) *Loop {
	if log == nil {
		log = slog.Default()
	}
	return &Loop{st: st, n: n, log: log}
}

// Flush delivers everything pending, in passes of batch events: read
// the cursor and the events past it, notify the mapped ones — advancing
// the cursor after each event — then read again, until a pass finds
// nothing or the cap or ctx says stop. A failing notifier is logged and
// does not stop the loop; the store is what fails the flush.
func (l *Loop) Flush(ctx context.Context) error {
	for pass := 0; pass < maxPasses; pass++ {
		more, err := l.flushOnce(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return nil
}

// flushOnce runs one pass and reports whether more events may be
// waiting past the new cursor. The cursor advances after each processed
// event — one small write per banner — so a crash mid-pass re-delivers
// at most the in-flight banner, never the whole batch. The pass stops
// early when ctx ends or the pass budget runs out; events not yet
// processed — and a banner the context killed mid-flight, which showed
// nothing — stay past the cursor for the next pass, so stopping never
// drops a notification.
func (l *Loop) flushOnce(ctx context.Context) (bool, error) {
	passCtx, stop := context.WithTimeout(ctx, passBudget)
	defer stop()

	var cursor int64
	var events []Event
	err := l.st.View(passCtx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT COALESCE(notify_cursor, 0) FROM meta WHERE id = 1`).
			Scan(&cursor); err != nil {
			return fmt.Errorf("read the notification cursor: %w", err)
		}
		var err error
		events, err = eventsAfterTx(tx, cursor, batch)
		return err
	})
	if err != nil {
		return false, err
	}

	// The notifications ride no transaction: a banner tool may take its
	// time, and the log is not locked against the engine's own writes
	// while a banner shows. The cursor write after each event is what
	// makes a restart at-least-once, never at-most-once, and bounds the
	// at-least-once window to the single in-flight banner.
	for _, ev := range events {
		if passCtx.Err() != nil {
			// Stopped mid-pass: the cursor is at the last processed
			// event — nothing delivered repeats, nothing unprocessed
			// is skipped.
			return false, passCtx.Err()
		}
		if t, body, ok := Message(ev); ok {
			if err := l.n.Notify(passCtx, t, body); err != nil {
				if passCtx.Err() != nil {
					// Shutdown or the pass budget landed inside this
					// banner's exec: the tool was killed, no banner
					// showed, and advancing would lose the event.
					// Stop the pass without advancing — the cursor
					// stays before it and the next flush delivers it.
					return false, passCtx.Err()
				}
				l.log.WarnContext(ctx, "notification failed; skipping and advancing",
					"event", ev.ID, "kind", ev.Kind, "err", err)
			}
		}
		// The cursor advances past the tool's own failures and ignored
		// kinds alike — they never notify — and past each event before
		// the next one is read, so a kill between banners duplicates
		// nothing already delivered. The write rides a context that
		// survives cancellation, so it lands even while shutting down.
		if err := l.advance(context.WithoutCancel(ctx), ev.ID); err != nil {
			return false, err
		}
	}
	return len(events) == batch, nil
}

// advance persists the cursor at id — never backwards, even if two
// passes somehow raced — in its own short transaction.
func (l *Loop) advance(ctx context.Context, id int64) error {
	return l.st.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE meta SET notify_cursor = MAX(COALESCE(notify_cursor, 0), ?) WHERE id = 1`, id)
		if err != nil {
			return fmt.Errorf("advance the notification cursor to %d: %w", id, err)
		}
		return nil
	})
}

// eventsAfterTx reads at most limit events with id strictly after the
// cursor, oldest first, resolving each event's task and plan so the
// mapper has titles without a second query. Events whose referents are
// gone still read: LEFT JOINs keep the log readable, and the mapper
// renders what it has.
func eventsAfterTx(tx *sql.Tx, after, limit int64) ([]Event, error) {
	rows, err := tx.Query(`SELECT e.id, e.kind, COALESCE(e.task_id, 0),
		COALESCE(t.plan_id, 0), COALESCE(p.title, ''), e.payload
		FROM events e
		LEFT JOIN tasks t ON t.id = e.task_id
		LEFT JOIN plans p ON p.id = t.plan_id
		WHERE e.id > ?
		ORDER BY e.id
		LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read events after %d: %w", after, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var payload string
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.TaskID, &ev.PlanID, &ev.PlanTitle, &payload); err != nil {
			return nil, fmt.Errorf("read events after %d: %w", after, err)
		}
		ev.Reason = reasonOf(ev.Kind, payload)
		ev.Parked = parkedBy(payload)
		if ev.Kind == KindPlanAwaitingApproval {
			// The event names its plan in the payload, not through a task.
			var p eventPayload
			if json.Unmarshal([]byte(payload), &p) == nil {
				ev.PlanID = p.Plan
			}
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read events after %d: %w", after, err)
	}
	return out, nil
}

// parkedBy reports whether the event parked its task: the engine
// stamps the state a parking event left the task in (needs_foreman,
// needs_user) as the payload's status. task_check_failed and
// review_changes also land without parking, as the history's record of
// a failure that a fix round then answers (§3); those never wake the
// user.
func parkedBy(payload string) bool {
	var p eventPayload
	if payload == "" || json.Unmarshal([]byte(payload), &p) != nil {
		return false
	}
	return p.Status == "needs_foreman" || p.Status == "needs_user"
}

// reasonOf picks the payload's words for the kind, the same choice the
// "needs you" queue makes: the worker's question for a needs_help, the
// review's findings for review changes, the named reason — the check's
// tail, the failure's words — for the rest, flattened to one line. A
// foreman report is its payload's own field, kept to its first line —
// the line the banner shows. A payload that names nothing yields the
// empty string and Message falls back to the kind itself.
func reasonOf(kind, payload string) string {
	if payload == "" {
		return ""
	}
	var p eventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	if kind == KindForemanReport {
		return firstLine(p.Report)
	}
	if kind == KindForemanLimited {
		return p.Until
	}
	if kind == KindPlanAwaitingApproval {
		return oneLine(p.Reason, bodyLimit)
	}
	if kind == KindAgentBroken {
		// "harness/model: reason" — the pair `brigada agent reset` takes.
		return oneLine(p.Harness+"/"+p.Model+": "+p.Reason, bodyLimit)
	}
	reason := p.Reason
	switch kind {
	case KindTaskNeedsHelp:
		reason = p.Question
	case KindReviewChanges:
		reason = p.Findings
	}
	return oneLine(reason, bodyLimit)
}
