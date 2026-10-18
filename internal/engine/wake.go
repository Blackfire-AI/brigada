package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/foreman"
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

// WakeOutcome is what one wake evaluation did (§4.2): woke a turn, or
// why the pending inbox stays pending. `brigada ask` reports it, so a
// user message never queues in silence (§4.2 item 3).
type WakeOutcome string

const (
	WakeWoken       WakeOutcome = "woken"           // a foreman turn was submitted carrying the inbox
	WakeNothing     WakeOutcome = "nothing_pending" // the inbox was empty
	WakeAttached    WakeOutcome = "attached"        // a person is attached; the session reads the inbox through events
	WakeHeld        WakeOutcome = "held"            // the user's own session holds the slot (§4.6); it reads the inbox through events
	WakeTurnRunning WakeOutcome = "turn_running"    // a turn is live; its completion re-evaluates
	WakeNoForeman   WakeOutcome = "no_foreman"      // the project names no foreman harness
	WakeStalled     WakeOutcome = "stalled"         // failing turns carried the inbox too often
	WakeBudgetSpent WakeOutcome = "budget_spent"    // the day's wake budget is spent (§4.3)
	WakeDuplicate   WakeOutcome = "duplicate"       // a user message repeating one sent moments ago (§12.1); nothing new was queued
	WakeLimited     WakeOutcome = "limited"         // the foreman hit its subscription limit and has not reset (§4.4)
)

// ForemanInbox is the foreman's pending inbox as it reaches a session
// (§4.2): the judgment events, rendered as event lines — data — and the
// user's messages (§4.2 item 3), kept apart because they are the one
// input that is a request rather than data (§1.4).
type ForemanInbox struct {
	Events       []string              `json:"events"`
	UserMessages []foreman.UserMessage `json:"user_messages"`
}

// Empty reports whether the inbox holds nothing at all.
func (in ForemanInbox) Empty() bool {
	return len(in.Events) == 0 && len(in.UserMessages) == 0
}

// wakeForemanTx evaluates one wake for the project, its outcome left to
// wakeOutcomeTx's callers that report it.
func (e *Engine) wakeForemanTx(tx *sql.Tx, project int64, now time.Time) error {
	_, err := e.wakeOutcomeTx(tx, project, now)
	// Background events must not resurrect a removed project or break a sweep.
	if errors.Is(err, ErrProjectArchived) {
		return nil
	}
	return err
}

// wakeOutcomeTx evaluates one wake for the project (§4.2 item 2, §4.3):
// with pending inbox rows, a foreman harness, no live turn on the key, a
// run of failed turns that has not stalled, and a wake budget with room,
// it submits a foreman turn carrying every pending event verbatim and
// marks them delivered to that job. Anything that says no — no harness,
// a live turn, the limit, the stall, the budget — leaves the events pending; a live
// turn's completion re-evaluates, and `brigada status` shows the parked
// tasks either way.
func (e *Engine) wakeOutcomeTx(tx *sql.Tx, project int64, now time.Time) (WakeOutcome, error) {
	if err := activeProjectTx(tx, project); err != nil {
		return "", err
	}
	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM foreman_inbox WHERE project_id = ? AND state = ?`,
		project, inboxPending).Scan(&pending); err != nil {
		return "", fmt.Errorf("read the foreman inbox of project %d: %w", project, err)
	}
	if pending == 0 {
		return WakeNothing, nil
	}
	// An interactive hold has the slot (§4.6): the session reads the
	// inbox through its `events` tool, and the hold's end re-evaluates
	// the wake, after the credential is revoked and the fence bumped.
	var interactive int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM interactive_holds WHERE project_id = ? AND state = 'live'`,
		project).Scan(&interactive); err != nil {
		return "", fmt.Errorf("read the interactive holds of project %d: %w", project, err)
	}
	if interactive > 0 {
		return WakeHeld, nil
	}
	// An attached chat holds the foreman (§4.2 item 1): while a hold
	// lives, no headless turn is submitted — the events wait for the
	// attached session, which reads them through its `events` tool. The
	// sweep at every call's top has already expired what lapsed.
	var held int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM attach_holds WHERE project_id = ? AND state = 'live' AND expires_at > ?`,
		project, fmtTime(now)).Scan(&held); err != nil {
		return "", fmt.Errorf("read the attach holds of project %d: %w", project, err)
	}
	if held > 0 {
		return WakeAttached, nil
	}
	var harness, model string
	err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", project).
		Scan(&harness, &model)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("no such project %d", project)
	case err != nil:
		return "", fmt.Errorf("read the foreman of project %d: %w", project, err)
	}
	if harness == "" || model == "" {
		// Nobody to wake: the foreman is the user's own choice (§4.1), and
		// a project that never named one keeps its events pending until
		// someone does.
		return WakeNoForeman, nil
	}
	key := fmt.Sprintf("foreman:%d", project)
	live, err := liveJobTx(tx, key)
	if err != nil {
		return "", err
	}
	if live != nil {
		// The turn's completion re-evaluates (§4.2): events that arrived
		// while it ran get the next turn then, not a second job now.
		return WakeTurnRunning, nil
	}
	// A limited foreman is not woken into the same wall (§4.4): the inbox
	// waits for the reset or the user's switch. A reset time that passed
	// is cleared here and the wake goes on.
	limited, _, err := foremanLimitedTx(tx, project, now)
	if err != nil {
		return "", err
	}
	if limited {
		return WakeLimited, nil
	}
	stalled, err := foremanWakeStalledTx(tx, project)
	if err != nil {
		return "", err
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
			return "", fmt.Errorf("read the last wake-stalled event of project %d: %w", project, err)
		}
		var woke int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND project_id = ? AND id > ?`,
			evForemanWoken, project, lastStall.Int64).Scan(&woke); err != nil {
			return "", fmt.Errorf("read the woken events of project %d: %w", project, err)
		}
		if woke > 0 {
			if _, err := appendEventTx(tx, evForemanWakeStalled, project, 0, 0,
				eventPayload{Count: foremanWakeStallThreshold}, now); err != nil {
				return "", err
			}
		}
		return WakeStalled, nil
	}
	// The day's budget counts every foreman job of the project since UTC
	// midnight — the turns this engine wakes and a person's manual submits
	// alike (§4.3) — so a hand-submitted turn spends the same budget it
	// bounds. The budget itself is the project's foreman_wakes_per_day
	// setting, else the engine's configured default.
	dayStart, dayEnd := wakeDayBounds(now)
	dayStartText := fmtTime(dayStart)
	var wakes int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = ? AND created_at >= ?`,
		project, KindForeman, dayStartText).Scan(&wakes); err != nil {
		return "", fmt.Errorf("count the day's foreman turns of project %d: %w", project, err)
	}
	carriesUser, err := pendingUserMessagesTx(tx, project)
	if err != nil {
		return "", err
	}
	budget, _, err := e.foremanWakeBudgetTx(tx, project)
	if err != nil {
		return "", err
	}
	// A wake that carries a user message is not charged (§4.3, §4.2 item
	// 3): the budget bounds the foreman reacting on its own, and a user
	// asking is as attended as a chat.
	if wakes >= budget && carriesUser == 0 {
		// The budget is spent (§4.3): at most one event says so per
		// project per budget window, carrying the count and the window's
		// end — the notification and the needs-you entry read it (§10) —
		// and the events stay pending for the next day or a raise.
		var spent int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND project_id = ? AND created_at >= ?`,
			evForemanWakeBudgetSpent, project, dayStartText).Scan(&spent); err != nil {
			return "", fmt.Errorf("read the budget-spent events of project %d: %w", project, err)
		}
		if spent == 0 {
			if _, err := appendEventTx(tx, evForemanWakeBudgetSpent, project, 0, 0,
				eventPayload{Count: int64(budget), Until: fmtTime(dayEnd)}, now); err != nil {
				return "", err
			}
		}
		return WakeBudgetSpent, nil
	}
	inbox, err := pendingInboxTx(tx, project)
	if err != nil {
		return "", err
	}
	res, err := e.submitForemanTurnTx(tx, project, inbox, now)
	if err != nil {
		return "", err
	}
	if res.Answer != AnsCreated {
		// A live turn appeared inside this transaction: its completion
		// re-evaluates, so the events stay pending rather than ride two.
		return WakeTurnRunning, nil
	}
	// hold_id clears with the delivery's move (0014): a row an expired
	// hold returned keeps its hold the way a returned row keeps its job,
	// and the turn that takes it must not carry the dead hold's name.
	if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ?, job_id = ?, hold_id = NULL WHERE project_id = ? AND state = ?`,
		inboxDelivered, res.JobID, project, inboxPending); err != nil {
		return "", fmt.Errorf("deliver the inbox of project %d to job %d: %w", project, res.JobID, err)
	}
	if _, err := appendEventTx(tx, evForemanWoken, project, 0, res.JobID,
		eventPayload{Job: res.JobID, Count: int64(len(inbox.Events) + len(inbox.UserMessages))}, now); err != nil {
		return "", err
	}
	return WakeWoken, nil
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

// The foreman_inbox table's rows are judgment events and, since §4.2
// item 3, user messages too: one row per event either way, told apart
// by the event's kind (0013's header predates the messages).
//
// pendingInboxTx renders the project's pending inbox (§4.2): the user's
// messages (§4.2 item 3) verbatim, apart, and the judgment events as the
// wake prompt's event lines (§4.2 item 2): one per event — the kind, the
// task id and title, and the event's own words as the log holds them,
// bounded only by what the payload already holds — so the foreman reads
// what the log holds, not the needs-you queue's one-line abridgement
// (§10). The title alone is flattened to one line: a newline there would
// break the event's own line, while the payload's words stay verbatim.
func pendingInboxTx(tx *sql.Tx, project int64) (ForemanInbox, error) {
	return inboxRowsTx(tx, project, inboxPending, 0)
}

// inboxRowsTx renders the project's inbox rows in one state, optionally
// only those a given job carried (job 0 = any): pending ones for a wake,
// delivered ones for a status that names what a live turn carries.
func inboxRowsTx(tx *sql.Tx, project int64, state string, job int64) (ForemanInbox, error) {
	rows, err := tx.Query(`SELECT e.kind, COALESCE(e.task_id, 0), COALESCE(t.title, ''), e.payload,
		COALESCE(pp.id, 0), COALESCE(pp.title, '')
		FROM foreman_inbox i
		JOIN events e ON e.id = i.event_id
		LEFT JOIN tasks t ON t.id = e.task_id
		LEFT JOIN plans pp ON pp.id = CASE WHEN json_valid(e.payload) THEN json_extract(e.payload, '$.plan') END
		WHERE i.project_id = ? AND i.state = ? AND (? = 0 OR i.job_id = ?)
		ORDER BY e.id`, project, state, job, job)
	if err != nil {
		return ForemanInbox{}, fmt.Errorf("read the pending events of project %d: %w", project, err)
	}
	inbox := ForemanInbox{Events: []string{}, UserMessages: []foreman.UserMessage{}}
	for rows.Next() {
		var kind, payload string
		var task int64
		var title, planTitle string
		var plan int64
		if err := rows.Scan(&kind, &task, &title, &payload, &plan, &planTitle); err != nil {
			rows.Close()
			return ForemanInbox{}, fmt.Errorf("read the pending events of project %d: %w", project, err)
		}
		if kind == evUserMessage {
			// The user's words go apart, verbatim: a request, never an
			// event line (§1.4, §4.2 item 3).
			var p eventPayload
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				rows.Close()
				return ForemanInbox{}, fmt.Errorf("read a user message of project %d: %w", project, err)
			}
			inbox.UserMessages = append(inbox.UserMessages, foreman.UserMessage{Source: p.Source, Text: p.Text})
			continue
		}
		text := "the plan is done"
		if kind != evPlanDone {
			text = wakeEventText(kind, payload)
		} else {
			// The finished plan by its own id and title — a completion
			// an amendment or an approval caused names no task — and
			// the done report's account of what never ran or failed.
			if plan != 0 && task == 0 {
				text += fmt.Sprintf(" (plan %d %q)", plan, strings.Join(strings.Fields(planTitle), " "))
			}
			if why := wakeEventText(kind, payload); why != kind {
				text += "; " + why
			}
		}
		inbox.Events = append(inbox.Events, fmt.Sprintf("%s task %d %s: %s",
			kind, task, strings.Join(strings.Fields(title), " "), text))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ForemanInbox{}, fmt.Errorf("read the pending events of project %d: %w", project, err)
	}
	rows.Close()
	return inbox, nil
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
	if t, ok := checkEventText(kind, p); ok {
		text = t
	}
	switch kind {
	case evTaskNeedsHelp:
		text = p.Question
	case evReviewChanges:
		text = p.Findings
	case evTaskDiscoveries:
		text = p.Discoveries
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
