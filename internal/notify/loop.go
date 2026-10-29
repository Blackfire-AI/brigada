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

// webhookQueue bounds the posts waiting for the webhook worker: room
// for a burst of notifications, small enough that a dead relay's
// backlog is dropped with a log line instead of growing without end.
// The queue is what keeps the webhook off the engine's tick — a full
// one costs the post, never the tick.
const webhookQueue = 32

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
	Count    int64  `json:"count,omitempty"`
}

// hookPost is one notification waiting for its webhook turn: the event
// as the log read it, and the one-line text the banner already showed.
type hookPost struct {
	ev   Event
	body string
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
	h   *Webhook
	hq  chan hookPost
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

// WithWebhook adds the optional webhook sink (§10): every notification
// the banner fires is also queued for one small JSON POST to the URL,
// beside the OS notification, never instead of it. nil h keeps the loop
// local-only. The posts ride a background worker on their own context,
// not the engine's tick: the loop queues and moves on, so a relay that
// hangs, fails or backs up never delays the sweep, the snapshot or the
// idle exit — fire and forget.
func (l *Loop) WithWebhook(h *Webhook) *Loop {
	l.h = h
	l.hq = make(chan hookPost, webhookQueue)
	go l.serveWebhook()
	return l
}

// serveWebhook drains the queue one post at a time, each attempt
// bounded by the webhook's own short timeout and its one retry. A
// failing post is logged — the error's text carries no URL — and
// dropped: the cursor has already advanced past its event, and
// fire-and-forget means no post holds a pass, a tick or the engine's
// exit hostage. What is still queued or in flight when the process
// stops is lost with it.
func (l *Loop) serveWebhook() {
	for p := range l.hq {
		if err := l.h.Post(context.Background(), p.ev, p.body); err != nil {
			l.log.Warn("webhook post failed; dropping it",
				"event", p.ev.ID, "kind", p.ev.Kind, "err", err)
		}
	}
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
			if l.h != nil {
				// The post leaves the pass here: the worker runs it on
				// its own context, so a hung relay delays neither the
				// cursor nor the tick, and a shutdown may cut posts
				// queued or in flight — fire-and-forget accepts that. A
				// full queue drops the post with a log line rather
				// than wait.
				select {
				case l.hq <- hookPost{ev: ev, body: hookText(ev, body)}:
				default:
					l.log.WarnContext(ctx, "webhook queue full; dropping the post",
						"event", ev.ID, "kind", ev.Kind)
				}
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
// mapper has titles without a second query, and its project's repo URL
// for the webhook. Events whose referents are gone still read: LEFT
// JOINs keep the log readable, and the mapper renders what it has.
func eventsAfterTx(tx *sql.Tx, after, limit int64) ([]Event, error) {
	rows, err := tx.Query(`SELECT e.id, e.kind, COALESCE(e.task_id, 0),
		COALESCE(t.plan_id, pp.id, 0), COALESCE(p.title, pp.title, ''),
		COALESCE(ep.repo_url, tp.repo_url, ppj.repo_url, ''), e.created_at, e.payload,
		COALESCE(e.job_id, 0),
		COALESCE((SELECT json_extract(pe.payload, '$.placement') FROM events pe
			WHERE pe.kind = 'job_agent_picked' AND pe.job_id = e.job_id ORDER BY pe.id DESC LIMIT 1), '')
		FROM events e
		LEFT JOIN tasks t ON t.id = e.task_id
		LEFT JOIN plans p ON p.id = t.plan_id
		LEFT JOIN projects ep ON ep.id = e.project_id
		LEFT JOIN projects tp ON tp.id = p.project_id
		LEFT JOIN plans pp ON pp.id = CASE WHEN json_valid(e.payload) THEN json_extract(e.payload, '$.plan') END
		LEFT JOIN projects ppj ON ppj.id = pp.project_id
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
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.TaskID, &ev.PlanID, &ev.PlanTitle, &ev.Project, &ev.At, &payload, &ev.JobID, &ev.Placement); err != nil {
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
// foreman report is its payload's own field, whole: the mapper cuts it
// to the first line — the line the banner shows — and ends a cut with
// the pointer to the history. A payload that names nothing yields the
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
		return p.Report
	}
	if kind == KindForemanLimited {
		return p.Until
	}
	if kind == KindForemanWakeBudgetSpent {
		// "20/20 today, resets <time>": the budget the event's Count
		// holds, spent, and the window's end its Until names.
		if p.Count > 0 && p.Until != "" {
			return oneLine(fmt.Sprintf("%d/%d today, resets %s", p.Count, p.Count, p.Until), bodyLimit)
		}
		if p.Until != "" {
			return "resets " + p.Until
		}
		return ""
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
