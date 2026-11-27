package engine

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// taskNoteTx reads the task's pending note (§6.2); ” when none.
func taskNoteTx(tx *sql.Tx, taskID int64) (string, error) {
	var note sql.NullString
	if err := tx.QueryRow(`SELECT note FROM tasks WHERE id = ?`, taskID).Scan(&note); err != nil {
		return "", fmt.Errorf("read the note of task %d: %w", taskID, err)
	}
	return note.String, nil
}

// setTaskNoteTx stores a task's pending note (§6.2), replacing the one
// before it; an empty note clears it. A live queued work or fix job of
// the task is the next job to lease, so its stored prompt is rewritten
// to carry the note too (prompt.InsertTaskNote; the job's prompt_meta
// does not hold the note and stays). The set records one task_note_set
// event with the note verbatim and the caller.
func setTaskNoteTx(tx *sql.Tx, project, taskID int64, note, caller string, now time.Time) error {
	nowStr := fmtTime(now)
	if _, err := tx.Exec(`UPDATE tasks SET note = NULLIF(?, ''), updated_at = ? WHERE id = ?`, note, nowStr, taskID); err != nil {
		return fmt.Errorf("set the note of task %d: %w", taskID, err)
	}
	j, err := liveJobTx(tx, fmt.Sprintf("task:%d", taskID))
	if err != nil {
		return err
	}
	if j != nil && j.Status == "queued" && (j.Kind == KindWork || j.Kind == KindFix) {
		if _, err := tx.Exec(`UPDATE jobs SET prompt = ?, updated_at = ? WHERE id = ?`,
			prompt.InsertTaskNote(j.Prompt, note), nowStr, j.ID); err != nil {
			return fmt.Errorf("carry the note of task %d into job %d: %w", taskID, j.ID, err)
		}
	}
	if _, err := appendEventTx(tx, evTaskNoteSet, project, taskID, 0, eventPayload{Note: note, Caller: caller}, now); err != nil {
		return err
	}
	return nil
}

// clearDeliveredNoteTx ends a note's wait (§6.2) when a work or fix job
// is leased: the job's first lease delivers it, as does the lease of a
// retried job whose stored prompt carries the pending note. Review jobs
// never carry it, and a re-lease of a job that did not carry it leaves
// the note for the next job.
func clearDeliveredNoteTx(tx *sql.Tx, j *jobRow, firstLease bool) error {
	if j.TaskID == 0 || (j.Kind != KindWork && j.Kind != KindFix) {
		return nil
	}
	note, err := taskNoteTx(tx, j.TaskID)
	if err != nil || note == "" {
		return err
	}
	if !firstLease && !prompt.HasTaskNote(j.Prompt, note) {
		return nil
	}
	if _, err := tx.Exec(`UPDATE tasks SET note = NULL WHERE id = ?`, j.TaskID); err != nil {
		return fmt.Errorf("clear the note of task %d: %w", j.TaskID, err)
	}
	return nil
}
