package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		// Idempotent: a task the land job landed already has a record, and
		// one recorded by an earlier detection has one too.
		var have int
		if err := tx.QueryRow("SELECT COUNT(*) FROM landings WHERE task_id = ?", lc.TaskID).Scan(&have); err != nil {
			return fmt.Errorf("read the landing of task %d: %w", lc.TaskID, err)
		}
		if have > 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, source, created_at)
			VALUES (?, ?, ?, ?, 'pushed', 'trailer', ?)`,
			lc.TaskID, project, branch, lc.Commit, fmtTime(now)); err != nil {
			return fmt.Errorf("record the landing of task %d: %w", lc.TaskID, err)
		}
		// A dependent that waited on this task's landing now has it
		// landed, so its own landing may proceed (§7.1).
		if err := e.releaseDependentLandsTx(tx, lc.TaskID, now); err != nil {
			return err
		}
	}
	return nil
}
