package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The foreman inbox's states (§4.2): pending while no turn has carried
// the event, delivered once a foreman turn was woken with it, consumed
// when a turn completed done having carried it.
const (
	inboxPending   = "pending"
	inboxDelivered = "delivered"
	inboxConsumed  = "consumed"
)

// foremanWakeStallThreshold is how many failing turns may carry one
// event before the engine stops re-delivering it (§4.2): the count is
// per row, so an event that arrives mid-run keeps the wake answering it,
// and the events that stalled stay pending for a person to read in
// `brigada status`.
const foremanWakeStallThreshold = 3

// inboxEventTx appends one judgment event and its inbox row together
// (§4.2): the event is the log's record, the row what a wake delivers.
func inboxEventTx(tx *sql.Tx, kind string, project, task, job int64, payload eventPayload, now time.Time) error {
	id, err := appendEventTx(tx, kind, project, task, job, payload, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO foreman_inbox (event_id, project_id, state) VALUES (?, ?, ?)`,
		id, project, inboxPending); err != nil {
		return fmt.Errorf("inbox the %s event: %w", kind, err)
	}
	return nil
}

// wakeForemanTx evaluates one wake for the project (§4.2 item 2, §4.3):
// with pending inbox rows, a foreman harness, no live turn on the key, a
// run of failed turns that has not stalled, and a wake budget with room,
// it submits a foreman turn carrying every pending event verbatim and
// marks them delivered to that job. Anything that says no — no harness,
// a live turn, the stall, the budget — leaves the events pending; a live
// turn's completion re-evaluates, and `brigada status` shows the parked
// tasks either way.
func (e *Engine) wakeForemanTx(tx *sql.Tx, project int64, now time.Time) error {
	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM foreman_inbox WHERE project_id = ? AND state = ?`,
		project, inboxPending).Scan(&pending); err != nil {
		return fmt.Errorf("read the foreman inbox of project %d: %w", project, err)
	}
	if pending == 0 {
		return nil
	}
	// An attached chat holds the foreman (§4.2 item 1): while a hold
	// lives, no headless turn is submitted — the events wait for the
	// attached session, which reads them through its `events` tool. The
	// sweep at every call's top has already expired what lapsed.
	var held int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM attach_holds WHERE project_id = ? AND state = 'live' AND expires_at > ?`,
		project, fmtTime(now)).Scan(&held); err != nil {
		return fmt.Errorf("read the attach holds of project %d: %w", project, err)
	}
	if held > 0 {
		return nil
	}
	var harness, model string
	err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", project).
		Scan(&harness, &model)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("no such project %d", project)
	case err != nil:
		return fmt.Errorf("read the foreman of project %d: %w", project, err)
	}
	if harness == "" || model == "" {
		// Nobody to wake: the foreman is the user's own choice (§4.1), and
		// a project that never named one keeps its events pending until
		// someone does.
		return nil
	}
	key := fmt.Sprintf("foreman:%d", project)
	live, err := liveJobTx(tx, key)
	if err != nil {
		return err
	}
	if live != nil {
		// The turn's completion re-evaluates (§4.2): events that arrived
		// while it ran get the next turn then, not a second job now.
		return nil
	}
	stalled, err := foremanWakeStalledTx(tx, project)
	if err != nil {
		return err
	}
	if stalled {
		// Named once per episode of failures: the wake a later evaluation
		// would announce again is the one already sitting in the log. A
		// new episode — a fresh event woke another turn since the last
		// stall event — is announced again, so a run that stalls twice is
		// heard of twice.
		var lastStall sql.NullInt64
		if err := tx.QueryRow(`SELECT MAX(id) FROM events WHERE kind = ? AND project_id = ?`,
			evForemanWakeStalled, project).Scan(&lastStall); err != nil {
			return fmt.Errorf("read the last wake-stalled event of project %d: %w", project, err)
		}
		var woke int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND project_id = ? AND id > ?`,
			evForemanWoken, project, lastStall.Int64).Scan(&woke); err != nil {
			return fmt.Errorf("read the woken events of project %d: %w", project, err)
		}
		if woke > 0 {
			if _, err := appendEventTx(tx, evForemanWakeStalled, project, 0, 0,
				eventPayload{Count: foremanWakeStallThreshold}, now); err != nil {
				return err
			}
		}
		return nil
	}
	// The day's budget counts every foreman job of the project since UTC
	// midnight — the turns this engine wakes and a person's manual submits
	// alike (§4.3) — so a hand-submitted turn spends the same budget it
	// bounds.
	dayStart := fmtTime(now.Truncate(24 * time.Hour))
	var wakes int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = ? AND created_at >= ?`,
		project, KindForeman, dayStart).Scan(&wakes); err != nil {
		return fmt.Errorf("count the day's foreman turns of project %d: %w", project, err)
	}
	if wakes >= e.cfg.ForemanWakeBudget {
		// The budget is spent (§4.3): at most one event says so per project
		// per day, and the events stay pending for the next day or a raise.
		var spent int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND project_id = ? AND created_at >= ?`,
			evForemanWakeBudgetSpent, project, dayStart).Scan(&spent); err != nil {
			return fmt.Errorf("read the budget-spent events of project %d: %w", project, err)
		}
		if spent == 0 {
			if _, err := appendEventTx(tx, evForemanWakeBudgetSpent, project, 0, 0,
				eventPayload{Count: int64(e.cfg.ForemanWakeBudget)}, now); err != nil {
				return err
			}
		}
		return nil
	}
	lines, err := pendingEventLinesTx(tx, project)
	if err != nil {
		return err
	}
	res, err := e.submitForemanTurnTx(tx, project, lines, now)
	if err != nil {
		return err
	}
	if res.Answer != AnsCreated {
		// A live turn appeared inside this transaction: its completion
		// re-evaluates, so the events stay pending rather than ride two.
		return nil
	}
	// hold_id clears with the delivery's move (0014): a row an expired
	// hold returned keeps its hold the way a returned row keeps its job,
	// and the turn that takes it must not carry the dead hold's name.
	if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ?, job_id = ?, hold_id = NULL WHERE project_id = ? AND state = ?`,
		inboxDelivered, res.JobID, project, inboxPending); err != nil {
		return fmt.Errorf("deliver the inbox of project %d to job %d: %w", project, res.JobID, err)
	}
	_, err = appendEventTx(tx, evForemanWoken, project, 0, res.JobID,
		eventPayload{Job: res.JobID, Count: int64(len(lines))}, now)
	return err
}

// foremanWakeStalledTx reports whether the project's wake has stalled
// (§4.2): every pending row has been carried by foremanWakeStallThreshold
// failing turns — its attempts count — so waking again would only loop.
// A row that arrived since, with fewer attempts, keeps the wake
// answering: nobody has read it enough times to give up on it.
func foremanWakeStalledTx(tx *sql.Tx, project int64) (bool, error) {
	var least sql.NullInt64
	if err := tx.QueryRow(`SELECT MIN(attempts) FROM foreman_inbox WHERE project_id = ? AND state = ?`,
		project, inboxPending).Scan(&least); err != nil {
		return false, fmt.Errorf("read the pending inbox of project %d: %w", project, err)
	}
	return least.Valid && least.Int64 >= foremanWakeStallThreshold, nil
}

// pendingEventLinesTx renders the project's pending inbox rows as the
// wake prompt's event lines (§4.2 item 2): one per event — the kind, the
// task id and title, and the event's own words as the log holds them,
// bounded only by what the payload already holds — so the foreman reads
// what the log holds, not the needs-you queue's one-line abridgement
// (§10). The title alone is flattened to one line: a newline there would
// break the event's own line, while the payload's words stay verbatim.
func pendingEventLinesTx(tx *sql.Tx, project int64) ([]string, error) {
	rows, err := tx.Query(`SELECT e.kind, COALESCE(e.task_id, 0), COALESCE(t.title, ''), e.payload
		FROM foreman_inbox i
		JOIN events e ON e.id = i.event_id
		LEFT JOIN tasks t ON t.id = e.task_id
		WHERE i.project_id = ? AND i.state = ?
		ORDER BY e.id`, project, inboxPending)
	if err != nil {
		return nil, fmt.Errorf("read the pending events of project %d: %w", project, err)
	}
	lines := []string{}
	for rows.Next() {
		var kind, payload string
		var task int64
		var title string
		if err := rows.Scan(&kind, &task, &title, &payload); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the pending events of project %d: %w", project, err)
		}
		text := "the plan is done"
		if kind != evPlanDone {
			text = wakeEventText(kind, payload)
		}
		lines = append(lines, fmt.Sprintf("%s task %d %s: %s",
			kind, task, strings.Join(strings.Fields(title), " "), text))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the pending events of project %d: %w", project, err)
	}
	rows.Close()
	return lines, nil
}

// wakeEventText renders one inbox event's own words (§4.2, §9): the
// question a needs_help asked, the findings a review changes carried,
// the reason the rest named — as stored, no flattening, no cut beyond
// what the payload already holds. The kind itself is the text when the
// payload named nothing.
func wakeEventText(kind, payload string) string {
	var p eventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return kind
	}
	text := p.Reason
	switch kind {
	case evTaskNeedsHelp:
		text = p.Question
	case evReviewChanges:
		text = p.Findings
	}
	if text == "" {
		return kind
	}
	return text
}

// foremanReportTx appends a done foreman turn's report to the user
// (§3, §10): the final message the turn ended saying, kept as the event
// a notification and `brigada status` read — the turn spoke to the user,
// and the log is where the user's side hears it (§9). The payload holds
// the report alone, bounded like any event's words and never a prompt;
// the turn's own prompt stays on its job. An empty final message is no
// report. A turn that ended any other way — failed, stalled, cancelled
// — reports nothing: its words were not an answer, and its events
// return to pending instead (§4.2). The report is no judgment event, so
// it never lands in the inbox and wakes nothing.
func foremanReportTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	wr, ok := parseWorkerResult(c.Result)
	if !ok || wr.Summary == "" {
		return nil
	}
	_, err := appendEventTx(tx, evForemanReport, job.ProjectID, 0, job.ID,
		eventPayload{Report: boundNote(wr.Summary)}, now)
	return err
}

// foremanFinishedTx settles a finished foreman turn's inbox (§4.2): a
// turn that completed done consumed the events it was woken with; a
// failed or stalled one returns them to pending a carried attempt the
// poorer — the count the wake's stall reads (§5.6: a stalled turn is a
// turn that never answered); a cancelled one returns them untouched,
// because a cancel is the user's own stop, not a failure to answer, and
// its attempts must not grow from it. Either way the key's follow-up
// flag clears. A cancelled turn wakes nothing at once — the user just
// said stop, and an instant re-wake would defy them — so its events
// stay pending for the next judgment event or the start-of-engine
// recovery (§13). Every other ending re-evaluates the wake here, in the
// same transaction as the completion, so the events that arrived during
// the turn, and the ones just returned, reach the next turn.
func (e *Engine) foremanFinishedTx(tx *sql.Tx, job *jobRow, status string, now time.Time) error {
	if job.Kind != KindForeman {
		return nil
	}
	switch status {
	case statusDone:
		if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ? WHERE job_id = ? AND state = ?`,
			inboxConsumed, job.ID, inboxDelivered); err != nil {
			return fmt.Errorf("consume the inbox delivered to job %d: %w", job.ID, err)
		}
	case statusCancelled:
		if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ? WHERE job_id = ? AND state = ?`,
			inboxPending, job.ID, inboxDelivered); err != nil {
			return fmt.Errorf("return the inbox delivered to job %d: %w", job.ID, err)
		}
	default:
		if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ?, attempts = attempts + 1
			WHERE job_id = ? AND state = ?`, inboxPending, job.ID, inboxDelivered); err != nil {
			return fmt.Errorf("return the inbox delivered to job %d: %w", job.ID, err)
		}
	}
	if _, err := tx.Exec("UPDATE jobs SET followup = 0 WHERE id = ?", job.ID); err != nil {
		return fmt.Errorf("clear the follow-up flag of job %d: %w", job.ID, err)
	}
	if status == statusCancelled {
		return nil
	}
	return e.wakeForemanTx(tx, job.ProjectID, now)
}

// RecoverForemanWakes is the start-of-engine recovery (§13): after the
// sweep, every project with pending inbox rows and no live foreman turn
// gets its wake evaluated — an event that parked a task while no engine
// ran does not wait for someone to look at `brigada status`.
func (e *Engine) RecoverForemanWakes(ctx context.Context) error {
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		// Heal first: a delivered row whose job is no longer live belongs
		// to a turn that ended without its settle — a heartbeat's hard cap
		// in a binary before that rule, a crash between the two writes.
		// The row returns to pending, its attempts untouched, and the wake
		// loop below picks the project up again. A row delivered to a live
		// hold is not the heal's: no turn carried it, and the attached
		// session's opening prompt already read it (§4.2 item 1).
		if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ?
			WHERE state = ? AND job_id NOT IN (
				SELECT id FROM jobs WHERE status IN ('queued', 'leased', 'running'))
			AND (hold_id IS NULL OR hold_id NOT IN (
				SELECT id FROM attach_holds WHERE state = 'live' AND expires_at > ?))`,
			inboxPending, inboxDelivered, fmtTime(now)); err != nil {
			return fmt.Errorf("heal the foreman inbox: %w", err)
		}
		rows, err := tx.Query(`SELECT DISTINCT project_id FROM foreman_inbox
			WHERE state = ? AND project_id NOT IN (
				SELECT project_id FROM jobs WHERE kind = ? AND status IN ('queued', 'leased', 'running'))`,
			inboxPending, KindForeman)
		if err != nil {
			return fmt.Errorf("list the projects with pending inbox rows: %w", err)
		}
		var projects []int64
		for rows.Next() {
			var p int64
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return fmt.Errorf("list the projects with pending inbox rows: %w", err)
			}
			projects = append(projects, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list the projects with pending inbox rows: %w", err)
		}
		rows.Close()
		for _, p := range projects {
			if err := e.wakeForemanTx(tx, p, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("recover foreman wakes: %w", err)
	}
	return nil
}
