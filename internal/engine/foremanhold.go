package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// foremanHolds answers, per project, whether the foreman will handle a
// task parked at needs_foreman (§10): the queue holds only what a rule
// or the foreman gave the user, so a task the foreman holds stays out of
// it. It reads and never writes — the limit's expiry is read, not
// cleared — and caches the project-wide facts for one listing.
type foremanHolds struct {
	projects map[int64]foremanFacts
}

// foremanFacts are the project-wide reads the rule shares between its
// tasks.
type foremanFacts struct {
	limited  bool // the foreman hit its limit and the reset has not passed
	attached bool // a live attach hold keeps a chat on the foreman
	turnLive bool // a foreman job of the project is queued, leased or running
}

func newForemanHolds() *foremanHolds {
	return &foremanHolds{projects: make(map[int64]foremanFacts)}
}

func (h *foremanHolds) facts(tx *sql.Tx, project int64, now time.Time) (foremanFacts, error) {
	if f, ok := h.projects[project]; ok {
		return f, nil
	}
	var f foremanFacts
	var state sql.NullString
	if err := tx.QueryRow("SELECT foreman_limited FROM projects WHERE id = ?", project).Scan(&state); err != nil {
		return f, fmt.Errorf("read the limited state of project %d: %w", project, err)
	}
	switch {
	case !state.Valid || state.String == "":
	case state.String == foremanLimitUnknown:
		f.limited = true
	default:
		if t, err := parseTime(state.String); err == nil && t.After(now) {
			f.limited = true
		}
	}
	var held int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM attach_holds WHERE project_id = ? AND state = 'live' AND expires_at > ?`,
		project, fmtTime(now)).Scan(&held); err != nil {
		return f, fmt.Errorf("read the attach holds of project %d: %w", project, err)
	}
	f.attached = held > 0
	live, err := liveJobTx(tx, fmt.Sprintf("foreman:%d", project))
	if err != nil {
		return f, err
	}
	f.turnLive = live != nil
	h.projects[project] = f
	return f, nil
}

// task reports whether the foreman holds a task parked at needs_foreman
// with no land-environment cause, whose latest parking event is event
// (§10). It holds the task when the event has an inbox row, the foreman
// is not limited, and either a live attach hold sits on the project —
// whatever the row's state, since the chat's events tool consumes rows —
// or the row is pending or delivered and a foreman turn is live. Every
// other case leaves the task to the user: no row (a plan rule's or the
// plain stale-base park), a consumed row (a done turn left the task
// parked), and the endings that leave no live turn — no harness, a spent
// budget, a stalled wake, a spent outage, a cancelled turn.
func (h *foremanHolds) task(tx *sql.Tx, project, event int64, now time.Time) (bool, error) {
	if event == 0 {
		return false, nil
	}
	var row string
	err := tx.QueryRow(`SELECT state FROM foreman_inbox WHERE event_id = ?`, event).Scan(&row)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the inbox row of event %d: %w", event, err)
	}
	f, err := h.facts(tx, project, now)
	if err != nil {
		return false, err
	}
	if f.limited {
		return false, nil
	}
	if f.attached {
		return true, nil
	}
	return (row == inboxPending || row == inboxDelivered) && f.turnLive, nil
}

// announceUnattendedTx names the tasks the user must now look at because
// the foreman will not (§10): for each task of the project parked at
// needs_foreman with no land-environment cause, whose parking event has
// an inbox row, that the foreman does not hold and that no task_unattended
// has announced since that parking event, one task_unattended event. It is
// a plain log record like plan_verdict_unanswered — in no inbox, waking no
// turn — and the notification the park no longer raises rides on it. A
// park with no inbox row (a plan rule's hold) raises nothing, as before.
func (e *Engine) announceUnattendedTx(tx *sql.Tx, project int64, now time.Time) error {
	kinds := "'" + strings.Join(needsYouEventKinds, "', '") + "'"
	rows, err := tx.Query(fmt.Sprintf(`SELECT t.id, e.id, e.kind, e.payload
		FROM tasks t
		JOIN plans p ON p.id = t.plan_id
		JOIN events e ON e.id = (
			SELECT e2.id FROM events e2
			WHERE e2.task_id = t.id AND e2.kind IN (%s)
			ORDER BY e2.id DESC LIMIT 1)
		JOIN foreman_inbox i ON i.event_id = e.id
		WHERE p.project_id = ? AND t.state = ? AND t.land_env_cause = ''
			AND NOT EXISTS (SELECT 1 FROM events u
				WHERE u.kind = ? AND u.task_id = t.id AND u.id > e.id)
		ORDER BY t.id`, kinds), project, taskNeedsForeman, evTaskUnattended)
	if err != nil {
		return fmt.Errorf("list the unannounced parked tasks of project %d: %w", project, err)
	}
	type parked struct {
		task, event   int64
		kind, payload string
	}
	var found []parked
	for rows.Next() {
		var p parked
		if err := rows.Scan(&p.task, &p.event, &p.kind, &p.payload); err != nil {
			rows.Close()
			return fmt.Errorf("list the unannounced parked tasks of project %d: %w", project, err)
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list the unannounced parked tasks of project %d: %w", project, err)
	}
	rows.Close()
	holds := newForemanHolds()
	for _, p := range found {
		held, err := holds.task(tx, project, p.event, now)
		if err != nil {
			return err
		}
		if held {
			continue
		}
		if _, err := appendEventTx(tx, evTaskUnattended, project, p.task, 0,
			eventPayload{Status: taskNeedsForeman, Reason: needsYouReason(p.kind, p.payload), ParkKind: p.kind}, now); err != nil {
			return err
		}
	}
	return nil
}
