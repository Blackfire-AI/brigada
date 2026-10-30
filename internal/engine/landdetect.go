package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LandedCommit is one landing the runner detected (§7.1): a commit on
// the project's default branch that carries the task's Brigada-Task
// trailer, whatever made the commit.
type LandedCommit struct {
	TaskID int64  `json:"task_id"`
	Commit string `json:"commit"`
}

// RecordLanded records landings the runner detected from the default
// branch's trailers (§7.1): a hand squash, or a brigada land in the
// user's clone, whose commits the land job never made. Each entry whose
// task belongs to the project repoURL names is written as a landing
// record — the engine's "landed" mark, carrying the commit — unless one
// already exists, so a task the land job landed is never recorded twice.
// A landed record from either source counts: the land rule, a
// dependent's wait and the status all read the one table.
func (e *Engine) RecordLanded(ctx context.Context, repoURL, branch string, landed []LandedCommit) error {
	if repoURL == "" {
		return fmt.Errorf("repo URL is required (product §5 rule 6: repos are addressed by URL)")
	}
	if branch == "" {
		return fmt.Errorf("the default branch is required to record a landing")
	}
	return e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		return e.recordLandedTx(tx, repoURL, branch, landed, now)
	})
}

// recordLandedTx applies RecordLanded in its caller's transaction.
func (e *Engine) recordLandedTx(tx *sql.Tx, repoURL, branch string, landed []LandedCommit, now time.Time) error {
	var project int64
	err := lookupProjectTx(tx, repoURL, "id", &project)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no project for repo %s", RedactRepoURL(repoURL))
	}
	if err != nil {
		return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
	}
	// Every landing the detection reports is written before any
	// dependent is released: a chain landed together by hand — head and
	// tail both on the default branch, each by its own trailer — must
	// have the tail's row written before the head's landing re-runs the
	// rule on the tail, or the rule would queue a land job for a task
	// that is already on the branch.
	var newLanded []int64
	for _, lc := range landed {
		if lc.TaskID <= 0 || lc.Commit == "" {
			continue
		}
		var taskProject int64
		err := tx.QueryRow(`SELECT p.project_id FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`,
			lc.TaskID).Scan(&taskProject)
		if errors.Is(err, sql.ErrNoRows) {
			continue // no such task; a trailer for another project's id is not ours
		}
		if err != nil {
			return fmt.Errorf("read the project of task %d: %w", lc.TaskID, err)
		}
		if taskProject != project {
			continue // the trailer names another project's task
		}
		// A landing on the branch clears a held hold even when a record
		// already exists: a hold left beside a record must not survive.
		if err := clearLandHoldTx(tx, lc.TaskID); err != nil {
			return err
		}
		// Idempotent: a task the land job landed already has a record, and
		// one recorded by an earlier detection has one too.
		existing, err := landingTx(tx, lc.TaskID)
		if err != nil {
			return err
		}
		if existing != nil {
			// The commit is on the branch, so a push still waiting for
			// its gate, or a task parked after a rejected push, is done.
			freed, err := e.settleLandedTx(tx, lc.TaskID, lc.Commit, existing.Pushed != "pushed", now)
			if err != nil {
				return err
			}
			if freed {
				newLanded = append(newLanded, lc.TaskID)
			}
			continue
		}
		// The read above saw no record; the conflict clause keeps the
		// write idempotent all the same, and a row it did not write is
		// someone else's landing, settled by whoever wrote it.
		r, err := tx.Exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, source, created_at)
			VALUES (?, ?, ?, ?, 'pushed', 'trailer', ?) ON CONFLICT (task_id) DO NOTHING`,
			lc.TaskID, project, branch, lc.Commit, fmtTime(now))
		if err != nil {
			return fmt.Errorf("record the landing of task %d: %w", lc.TaskID, err)
		}
		if n, err := r.RowsAffected(); err != nil {
			return fmt.Errorf("record the landing of task %d: %w", lc.TaskID, err)
		} else if n == 0 {
			continue
		}
		if _, err := e.settleLandedTx(tx, lc.TaskID, lc.Commit, false, now); err != nil {
			return err
		}
		newLanded = append(newLanded, lc.TaskID)
	}
	for _, id := range newLanded {
		// A dependent that waited on this task's landing now has it
		// landed, so its own landing may proceed (§7.1).
		if err := e.releaseDependentLandsTx(tx, id, now); err != nil {
			return err
		}
	}
	// A settled task was live work its plan waited on (#133), and the
	// release above may have ended a dependent held at the land gate —
	// a hold with no job, which lets its plan close under "waits at a
	// gate" — so the close runs now, after the releases moved every
	// dependent, never between the records the loop above writes.
	seenPlan := map[int64]bool{}
	for _, id := range newLanded {
		var plan int64
		if err := tx.QueryRow("SELECT plan_id FROM tasks WHERE id = ?", id).Scan(&plan); err != nil {
			return fmt.Errorf("read the plan of task %d: %w", id, err)
		}
		if seenPlan[plan] {
			continue
		}
		seenPlan[plan] = true
		if _, err := e.finishPlanIfClosedTx(tx, project, plan, id, 0, now); err != nil {
			return err
		}
	}
	// A landing the branch holds proves the environment the failed
	// landings blamed (issue #116): their causes end, so a recurrence
	// wakes anew.
	if len(newLanded) > 0 {
		if err := e.recoverLandEnvCausesTx(tx, project, now); err != nil {
			return err
		}
	}
	return nil
}

// settleLandedTx brings a task whose commit is on the default branch to
// its landed end state (§7.1): a landing not yet pushed — waiting at the
// push gate, or left to the user under gate_push never — becomes pushed
// at the commit that carries the trailer, and a task parked at needs_foreman by a failed landing
// or push returns to done with its needs-you line gone, its dependents
// released as a finished task's are. It reports whether anything changed,
// so the caller re-runs the land rule for the dependents.
func (e *Engine) settleLandedTx(tx *sql.Tx, taskID int64, commit string, unpushed bool, now time.Time) (bool, error) {
	changed := false
	if unpushed {
		if _, err := tx.Exec(`UPDATE landings SET pushed = 'pushed', commit_sha = ?, push_cmd = '' WHERE task_id = ? AND pushed <> 'pushed'`,
			commit, taskID); err != nil {
			return false, fmt.Errorf("record the push of task %d: %w", taskID, err)
		}
		changed = true
	}
	var state string
	var project, plan int64
	if err := tx.QueryRow(`SELECT t.state, p.project_id, t.plan_id FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`,
		taskID).Scan(&state, &project, &plan); err != nil {
		return false, fmt.Errorf("read task %d: %w", taskID, err)
	}
	if state != taskNeedsForeman {
		return changed, nil
	}
	// Only a parking by a failed landing moves: a task parked for any
	// other reason is the foreman's still.
	kinds := "'" + strings.Join(needsYouEventKinds, "', '") + "'"
	var kind string
	err := tx.QueryRow(`SELECT kind FROM events WHERE task_id = ? AND kind IN (`+kinds+`) ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && kind != evTaskLandFailed) {
		return changed, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the parking of task %d: %w", taskID, err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`, taskDone, fmtTime(now), taskID); err != nil {
		return false, fmt.Errorf("finish task %d: %w", taskID, err)
	}
	if err := e.releaseDependentsTx(tx, project, taskID, now); err != nil {
		return false, err
	}
	if _, err := e.finishPlanIfClosedTx(tx, project, plan, taskID, 0, now); err != nil {
		return false, err
	}
	return true, nil
}
