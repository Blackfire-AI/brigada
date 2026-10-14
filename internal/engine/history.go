package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/history"
)

// The history page's bounds (§9): small enough that one read of the
// timeline stays one read, large enough that a job's whole story — its
// events and the messages they produced — lands on a page or two.
const (
	defaultHistoryLimit = 100
	maxHistoryLimit     = 500
)

// HistoryQuery is one read of the timeline (§9): the filters and the
// page. Every filter is optional; ProjectID and TaskID narrow to one
// project or task, JobID to one job's whole story, Since is the cursor
// the previous page ended on, and Limit bounds the page (kept between
// 1 and 500, default 100). Last asks for the newest page instead of the
// next one: the filters and Limit still apply, Since must be empty, and
// the page has no next cursor — to read further back, narrow with the
// task or job filter, or raise the limit.
type HistoryQuery struct {
	ProjectID int64
	TaskID    int64
	JobID     int64
	Since     string
	Limit     int
	Last      bool
}

// History reads one page of the timeline (§9): events and messages
// merged, oldest first, as one story. Like every engine call it sweeps
// first, so the log it reads is the rules' current word. The next
// cursor is empty when the timeline is exhausted. In Last mode the page
// is the newest Limit items, still oldest first within it, and the next
// cursor is always empty: there is no forward continuation from the
// newest page. Read further back by narrowing to a task or job, or by
// raising the limit. Last cannot continue from a cursor.
func (e *Engine) History(ctx context.Context, q HistoryQuery) (history.Page, error) {
	if q.Last && q.Since != "" {
		return history.Page{}, fmt.Errorf("history: last cannot continue from a cursor")
	}
	if q.Limit <= 0 {
		q.Limit = defaultHistoryLimit
	}
	if q.Limit > maxHistoryLimit {
		q.Limit = maxHistoryLimit
	}
	cursor, ok := history.ParseCursor(q.Since)
	if q.Since != "" && !ok {
		return history.Page{}, fmt.Errorf("history: malformed cursor")
	}

	var page history.Page
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := e.sweepTx(tx, e.cfg.Now()); err != nil {
			return err
		}
		events, err := historyEventsTx(tx, q, cursor)
		if err != nil {
			return err
		}
		messages, err := historyMessagesTx(tx, q, cursor)
		if err != nil {
			return err
		}
		items := history.Merge(events, messages)
		if len(items) > q.Limit {
			if q.Last {
				items = items[len(items)-q.Limit:]
			} else {
				items = items[:q.Limit]
				page.NextCursor = history.CursorOf(items[q.Limit-1])
			}
		}
		page.Items = items
		return nil
	})
	if err != nil {
		return history.Page{}, fmt.Errorf("history: %w", err)
	}
	return page, nil
}

// historyEventsTx reads the page's events, oldest first — newest first in
// Last mode, where Merge re-sorts and no cursor applies. The cursor
// carries the last item of the previous page — of either type — so an
// event continues only strictly past it: an event at the cursor's own
// instant sorts before a message there, and past-page events at that
// instant were already shown.
func historyEventsTx(tx *sql.Tx, q HistoryQuery, cursor history.CursorKey) ([]history.Event, error) {
	where := []string{"1 = 1"}
	var args []any
	if q.ProjectID != 0 {
		where = append(where, "project_id = ?")
		args = append(args, q.ProjectID)
	}
	if q.TaskID != 0 {
		where = append(where, "task_id = ?")
		args = append(args, q.TaskID)
	}
	if q.JobID != 0 {
		where = append(where, "job_id = ?")
		args = append(args, q.JobID)
	}
	if cursor.Type != "" {
		if cursor.Type == history.TypeEvent {
			where = append(where, "(created_at > ? OR (created_at = ? AND id > ?))")
			args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
		} else {
			where = append(where, "created_at > ?")
			args = append(args, cursor.CreatedAt)
		}
	}
	order, limit := "created_at, id", q.Limit+1
	if q.Last {
		order, limit = "created_at DESC, id DESC", q.Limit
	}
	rows, err := tx.Query(`SELECT id, kind, COALESCE(project_id, 0), COALESCE(task_id, 0),
		COALESCE(job_id, 0), payload, created_at
		FROM events WHERE `+strings.Join(where, " AND ")+`
		ORDER BY `+order+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	var out []history.Event
	for rows.Next() {
		var ev history.Event
		var payload string
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.ProjectID, &ev.TaskID, &ev.JobID, &payload, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// historyMessagesTx reads the page's messages, oldest first — newest
// first in Last mode, like the events. A task or
// job filter reaches a message through the event that produced it —
// that reference is what ties a conversation item to the story it
// belongs to (§9). Past the cursor's instant a message continues at
// that instant too: messages sort after events there, so the ones at
// the cursor's time may still be unshown.
func historyMessagesTx(tx *sql.Tx, q HistoryQuery, cursor history.CursorKey) ([]history.Message, error) {
	where := []string{"1 = 1"}
	var args []any
	if q.ProjectID != 0 {
		where = append(where, "m.project_id = ?")
		args = append(args, q.ProjectID)
	}
	if q.TaskID != 0 {
		where = append(where, "EXISTS (SELECT 1 FROM events e WHERE e.id = m.event_id AND e.task_id = ?)")
		args = append(args, q.TaskID)
	}
	if q.JobID != 0 {
		where = append(where, "EXISTS (SELECT 1 FROM events e WHERE e.id = m.event_id AND e.job_id = ?)")
		args = append(args, q.JobID)
	}
	if cursor.Type != "" {
		if cursor.Type == history.TypeMessage {
			where = append(where, "(m.created_at > ? OR (m.created_at = ? AND m.id > ?))")
			args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
		} else {
			where = append(where, "m.created_at >= ?")
			args = append(args, cursor.CreatedAt)
		}
	}
	order, limit := "m.created_at, m.id", q.Limit+1
	if q.Last {
		order, limit = "m.created_at DESC, m.id DESC", q.Limit
	}
	rows, err := tx.Query(`SELECT m.id, m.project_id, COALESCE(m.event_id, 0), m.kind, m.author, m.content, m.created_at
		FROM messages m WHERE `+strings.Join(where, " AND ")+`
		ORDER BY `+order+` LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	defer rows.Close()
	var out []history.Message
	for rows.Next() {
		var m history.Message
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.EventID, &m.Kind, &m.Author, &m.Content, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("read messages: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
