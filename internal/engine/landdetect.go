package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LandedCommit is one landing candidate the runner detected (§7.1): a
// commit on the project's landing branch — the land target when one is
// set, else the default branch — that carries the task's Brigada-Task
// trailer, whatever made the commit. Branch and Head are the commit's
// Brigada-Branch trailer split at its last '@', the branch@head form
// landing.Message writes; "" when the commit carries no such trailer.
type LandedCommit struct {
	TaskID int64  `json:"task_id"`
	Commit string `json:"commit"`
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head,omitempty"`
}

// RecordLanded records landings the runner detected from the landing
// branch's trailers — the land target when one is set, else the default
// branch (§7.1): a hand squash, or a brigada land in the
// user's clone, whose commits the land job never made. Each entry is one
// candidate commit, reported newest first per task. A candidate is
// recorded only when its Brigada-Branch trailer names the task's own
// work: the task's recorded result branch and the head its last done
// author job pushed — so a reused task id whose ancient trailer sits on
// an unrelated commit records nothing. The first candidate of a task
// that passes is written as the landing record — the engine's "landed"
// mark, carrying the commit and the trailer — unless one already exists,
// so a task the land job landed is never recorded twice; the task's
// later candidates are refused. A landed record from either source
// counts: the land rule, a dependent's wait and the status all read the
// one table.
func (e *Engine) RecordLanded(ctx context.Context, repoURL, branch string, landed []LandedCommit) error {
	if repoURL == "" {
		return fmt.Errorf("repo URL is required (product §5 rule 4: repos are addressed by URL)")
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
	// tail both on the landing branch, each by its own trailer — must
	// have the tail's row written before the head's landing re-runs the
	// rule on the tail, or the rule would queue a land job for a task
	// that is already on the branch.
	var newLanded []int64
	// Candidates arrive newest first per task; once one of a task's
	// candidates is acted on, the rest are refused (§7.1).
	recorded := map[int64]bool{}
	for _, lc := range landed {
		if lc.TaskID <= 0 || lc.Commit == "" {
			continue
		}
		if recorded[lc.TaskID] {
			continue
		}
		var taskProject int64
		var state, taskBranch string
		err := tx.QueryRow(`SELECT p.project_id, t.state, COALESCE(t.branch, '') FROM tasks t JOIN plans p ON p.id = t.plan_id WHERE t.id = ?`,
			lc.TaskID).Scan(&taskProject, &state, &taskBranch)
		if errors.Is(err, sql.ErrNoRows) {
			continue // no such task; a trailer for another project's id is not ours
		}
		if err != nil {
			return fmt.Errorf("read the project of task %d: %w", lc.TaskID, err)
		}
		if taskProject != project {
			continue // the trailer names another project's task
		}
		if state == taskRetired {
			// A retired task never lands by any path (§6.2): its
			// trailer on the branch is a fact of the repo, not a
			// landing to record.
			continue
		}
		// The candidate must be this task's own work (§7.1): its trailer
		// branch is the task's recorded result branch and its trailer
		// head is the head the task's last done author job pushed, both
		// full shas compared exactly. A commit with no Brigada-Branch
		// trailer, or a task with no recorded branch or approved head,
		// cannot be validated and records nothing — a reused id's ancient
		// trailer on an unrelated commit fails here, never wedging the
		// task as landed over work that never reached the branch.
		if lc.Branch == "" || lc.Head == "" || taskBranch == "" {
			continue
		}
		approvedHead, err := approvedHeadTx(tx, lc.TaskID)
		if err != nil {
			return err
		}
		if approvedHead == "" || lc.Branch != taskBranch || lc.Head != approvedHead {
			continue
		}
		recorded[lc.TaskID] = true
		// A landing on the branch clears a held hold even when a record
		// already exists: a hold left beside a record must not survive.
		if err := e.clearLandHoldTx(tx, project, lc.TaskID, now); err != nil {
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
		// someone else's landing, settled by whoever wrote it. The
		// trailer's branch and head go with it, for the start-up repair
		// pass to validate the record by (§7.1).
		r, err := tx.Exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, source, trailer_branch, trailer_head, created_at)
			VALUES (?, ?, ?, ?, 'pushed', 'trailer', ?, ?, ?) ON CONFLICT (task_id) DO NOTHING`,
			lc.TaskID, project, branch, lc.Commit, lc.Branch, lc.Head, fmtTime(now))
		if err != nil {
			return fmt.Errorf("record the landing of task %d: %w", lc.TaskID, err)
		}
		if n, err := r.RowsAffected(); err != nil {
			return fmt.Errorf("record the landing of task %d: %w", lc.TaskID, err)
		} else if n == 0 {
			continue
		}
		// The record settles the landing, so a batch reservation ends
		// with it (§7.1).
		if err := releaseLandMemberTx(tx, lc.TaskID); err != nil {
			return err
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
		// A wait_landed dependent held past this task's landing now has
		// its work on the target, so its barrier releases (§6.1).
		if err := e.releaseDependentsTx(tx, project, id, now); err != nil {
			return err
		}
	}
	// A settled task was live work its plan waited on (#133), and the
	// release above may have moved a dependent of it — a landing
	// queued, or a hold at the land gate, which keeps that plan open
	// until the user releases it — so the close runs now, after the
	// releases moved every dependent, never between the records the
	// loop above writes.
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

// settleLandedTx brings a task whose commit is on the landing branch —
// the land target when one is set, else the default branch — to
// its landed end state (§7.1): a landing not yet pushed — waiting at the
// push gate, or left to the user under gate_push never — becomes pushed
// at the commit that carries the trailer, and a task parked at
// needs_foreman returns to done with its needs-you line gone, its
// dependents released as a finished task's are and its plan closed when
// it was the last live work. The caller has validated the landing, so
// whatever parked the task — a failed landing or push, a review without
// a verdict or with changes, spent rounds, a failure — the work is on the
// branch and the parking is moot; only a worker's question still awaiting
// an answer stays the foreman's: its latest parking event is task_needs_help,
// and an answer or a restart moves the task out of needs_foreman, so a
// task still parked there with that event is still waiting. It reports whether
// anything changed, so the caller re-runs the land rule for the dependents.
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
	// A question awaiting its answer is the foreman's still; any other
	// parking is moot once the validated work is on the branch.
	kinds := "'" + strings.Join(needsYouEventKinds, "', '") + "'"
	var kind string
	err := tx.QueryRow(`SELECT kind FROM events WHERE task_id = ? AND kind IN (`+kinds+`) ORDER BY id DESC LIMIT 1`,
		taskID).Scan(&kind)
	if err == nil && kind == evTaskNeedsHelp {
		return changed, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
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
