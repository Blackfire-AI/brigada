package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AskUser records a foreman question separately from user-originated messages.
// The harness presents it in chat; answering never follows from tool output.
func (e *Engine) AskUser(ctx context.Context, project int64, text string) (int64, error) {
	if strings.TrimSpace(text) == "" || len(text) > UserMessageLimit {
		return 0, fmt.Errorf("%w: a question must contain 1..%d bytes", ErrInvalidMessage, UserMessageLimit)
	}
	var id int64
	err := e.updateGuarded(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT INTO questions (project_id,question,created_at) VALUES (?,?,?)`, project, text, fmtTime(now))
		if err != nil {
			return err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return err
		}
		event, err := appendEventTx(tx, "foreman_question", project, 0, 0, eventPayload{Question: text, QuestionID: id}, now)
		if err != nil {
			return err
		}
		return appendMessageTx(tx, project, event, "foreman_question", "foreman", text, now)
	})
	return id, err
}
