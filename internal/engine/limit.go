package engine

import (
	"database/sql"
	"fmt"
	"time"
)

// foremanLimitUnknown is the limited state's value when the limit error
// named no reset time (§4.4): it holds until the user switches the foreman.
const foremanLimitUnknown = "unknown"

// foremanLimitHitTx marks the project's foreman limited when its turn
// completed with a limit error (§4.4): the state on the project, a
// foreman_limited event, and — through the event — the user's
// notification (§10), all in the completion's transaction. Only the
// foreman's own key marks it: a task job's limit is the dispatcher's
// business, not the foreman's. The reset time is kept when it parses and
// lies ahead; anything else — empty, malformed, already passed — is
// unknown, which stops the wake until the user chooses.
func foremanLimitHitTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	if job.Kind != KindForeman || job.Key != fmt.Sprintf("foreman:%d", job.ProjectID) ||
		c.Error == "" || c.ErrorKind != "limit" {
		return nil
	}
	state := foremanLimitUnknown
	var payload eventPayload
	if until, err := time.Parse(time.RFC3339, c.LimitUntil); err == nil && until.After(now) {
		state = fmtTime(until)
		payload.Until = state
	}
	if _, err := tx.Exec("UPDATE projects SET foreman_limited = ?, updated_at = ? WHERE id = ?",
		state, fmtTime(now), job.ProjectID); err != nil {
		return fmt.Errorf("mark the foreman of project %d limited: %w", job.ProjectID, err)
	}
	_, err := appendEventTx(tx, evForemanLimited, job.ProjectID, 0, job.ID, payload, now)
	return err
}

// foremanLimitedTx reads the project's limited state (§4.4): limited
// reports whether it holds now, until the reset time ("" when unknown).
// A reset time that has passed is cleared here, in the caller's
// transaction, and reads as not limited — the lazy expiry every wake
// takes.
func foremanLimitedTx(tx *sql.Tx, project int64, now time.Time) (limited bool, until string, err error) {
	var state sql.NullString
	if err := tx.QueryRow("SELECT foreman_limited FROM projects WHERE id = ?", project).Scan(&state); err != nil {
		return false, "", fmt.Errorf("read the limited state of project %d: %w", project, err)
	}
	if !state.Valid || state.String == "" {
		return false, "", nil
	}
	if state.String == foremanLimitUnknown {
		return true, "", nil
	}
	if t, perr := parseTime(state.String); perr == nil && t.After(now) {
		return true, state.String, nil
	}
	if _, err := tx.Exec("UPDATE projects SET foreman_limited = NULL, updated_at = ? WHERE id = ?",
		fmtTime(now), project); err != nil {
		return false, "", fmt.Errorf("clear the expired limit of project %d: %w", project, err)
	}
	return false, "", nil
}
