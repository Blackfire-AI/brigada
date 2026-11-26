package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The start-up trailer-landing repair pass (§7.1): before the trailer
// scan validated candidates, a reused task id could wedge a task as
// landed over an unrelated commit — the oldest of many commits carrying
// the id, from a previous store's history — and a done task with a
// landing record refuses restart, cancel and retire, so its real work
// never reached the land target: silent data loss. The pass brings the
// records to the invariant the scan now enforces: every landing recorded
// from a trailer names the task's own approved work, or it is gone.

// RepairFalseTrailerLandings is the start-of-engine pass (§7.1): in one
// store transaction, after the sweep as every recovery call does, it
// reads every landings row of source 'trailer' and validates its stored
// trailer_branch and trailer_head against the task's recorded result
// branch and approved head exactly as recordLandedTx validates a
// candidate. A row that fails — a trailer naming another branch or an
// unapproved head, a task with no recorded branch or approved head, or a
// pre-validation row whose trailer columns are NULL and cannot be
// validated — is deleted, and the land rule re-runs for its task, so its
// real land job queues or the gate holds. The task stays done. One
// history event per repaired task names the dropped commit and the
// reason; no inbox row is written and no foreman woken, as the reopen
// pass. A second run repairs nothing: what survives it validated. It
// returns the repaired task ids.
func (e *Engine) RepairFalseTrailerLandings(ctx context.Context) ([]int64, error) {
	var repaired []int64
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT task_id, commit_sha, COALESCE(trailer_branch, ''), COALESCE(trailer_head, '')
			FROM landings WHERE source = 'trailer' ORDER BY task_id`)
		if err != nil {
			return fmt.Errorf("list the trailer landings: %w", err)
		}
		type trailerLanding struct {
			task                 int64
			commit, branch, head string
		}
		var landings []trailerLanding
		for rows.Next() {
			var l trailerLanding
			if err := rows.Scan(&l.task, &l.commit, &l.branch, &l.head); err != nil {
				rows.Close()
				return fmt.Errorf("list the trailer landings: %w", err)
			}
			landings = append(landings, l)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list the trailer landings: %w", err)
		}
		rows.Close()
		for _, l := range landings {
			var project int64
			var taskBranch string
			err := tx.QueryRow(`SELECT p.project_id, COALESCE(t.branch, '') FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`,
				l.task).Scan(&project, &taskBranch)
			if errors.Is(err, sql.ErrNoRows) {
				continue // the task is gone; its row went with it
			}
			if err != nil {
				return fmt.Errorf("read task %d: %w", l.task, err)
			}
			approvedHead, err := approvedHeadTx(tx, l.task)
			if err != nil {
				return err
			}
			var reason string
			switch {
			case l.branch == "" || l.head == "":
				reason = "the record carries no trailer branch@head to validate"
			case taskBranch == "" || approvedHead == "":
				reason = "the task records no branch or approved head to match"
			case l.branch != taskBranch || l.head != approvedHead:
				reason = fmt.Sprintf("the trailer %s@%s is not the task's approved work %s@%s",
					l.branch, l.head, taskBranch, approvedHead)
			}
			if reason == "" {
				continue
			}
			if _, err := tx.Exec(`DELETE FROM landings WHERE task_id = ?`, l.task); err != nil {
				return fmt.Errorf("drop the false landing of task %d: %w", l.task, err)
			}
			// The task stays done; re-running the land rule queues its
			// real land job or holds it at the gate (§7.1).
			if err := e.maybeLandTaskTx(tx, l.task, now); err != nil {
				return err
			}
			if _, err := appendEventTx(tx, evTaskLandingRepaired, project, l.task, 0,
				eventPayload{Commit: l.commit, Reason: reason}, now); err != nil {
				return err
			}
			repaired = append(repaired, l.task)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("repair the trailer landings: %w", err)
	}
	return repaired, nil
}
