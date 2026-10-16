package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const DecisionNoteLimit = 16 << 10
const evDecisionNote = "decision_note"
const authorInteractive = "foreman-interactive"

// DecisionNote journals reasoning for the separate headless foreman. Notes
// never become user messages and do not themselves request a headless wake.
func (e *Engine) DecisionNote(ctx context.Context, project int64, text string) (int64, error) {
	if strings.TrimSpace(text) == "" || len(text) > DecisionNoteLimit {
		return 0, fmt.Errorf("%w: a decision note must contain 1..%d bytes of text", ErrInvalidMessage, DecisionNoteLimit)
	}
	var event int64
	err := e.updateGuarded(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var err error
		event, err = appendEventTx(tx, evDecisionNote, project, 0, 0,
			eventPayload{Text: text, Source: authorInteractive}, now)
		if err != nil {
			return err
		}
		return appendMessageTx(tx, project, event, evDecisionNote, authorInteractive, text, now)
	})
	return event, err
}

// decisionNotesTx reads journal order rather than clock time: even notes
// written at precisely the same timestamp as acquire have a stable boundary.
func decisionNotesTx(tx *sql.Tx, project, since int64) ([]string, int64, error) {
	rows, err := tx.Query(`SELECT event_id, content FROM messages
		WHERE project_id = ? AND kind = ? AND event_id > ? ORDER BY event_id`, project, evDecisionNote, since)
	if err != nil {
		return nil, since, err
	}
	defer rows.Close()
	notes := []string{}
	last := since
	for rows.Next() {
		var text string
		if err := rows.Scan(&last, &text); err != nil {
			return nil, since, err
		}
		notes = append(notes, "decision_note by "+authorInteractive+":\n"+text)
	}
	return notes, last, rows.Err()
}

// lastHeldEventTx prefers the bridge's previous acquisition, falling back
// only to recorded harness metadata, never to an unverified session hint.
// The preceding acquire is the boundary: include the reasoning recorded
// during that working turn as well as anything journaled while it was away.
func lastHeldEventTx(tx *sql.Tx, req HoldAcquire) (int64, error) {
	var hold int64
	err := tx.QueryRow(`SELECT id FROM interactive_holds WHERE project_id = ? AND bridge_instance = ?
		ORDER BY id DESC LIMIT 1`, req.Project, req.BridgeInstance).Scan(&hold)
	if err == sql.ErrNoRows && req.HarnessSessionID != "" {
		err = tx.QueryRow(`SELECT id FROM interactive_holds WHERE project_id = ? AND harness_session_id = ?
			ORDER BY id DESC LIMIT 1`, req.Project, req.HarnessSessionID).Scan(&hold)
	}
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var event int64
	err = tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events WHERE project_id = ? AND kind = ?
		AND json_extract(payload, '$.hold') = ?`, req.Project, evHoldAcquired, hold).Scan(&event)
	return event, err
}
