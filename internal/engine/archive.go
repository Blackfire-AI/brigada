package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrProjectArchived marks a repository removed from the engine. Its stored
// identity and history remain available, and registration restores that row.
var ErrProjectArchived = errors.New("project was removed")

// ErrProjectBusy means removal needs the caller's explicit cancellation choice.
var ErrProjectBusy = errors.New("project has live jobs or active plans")

// activeProjectTx is the shared gate for work entering a project, whether the
// caller addressed it by repository URL, project ID, or a task/plan ID.
func activeProjectTx(tx *sql.Tx, project int64) error {
	var repo string
	var archived sql.NullString
	if err := tx.QueryRow("SELECT repo_url, archived_at FROM projects WHERE id = ?", project).Scan(&repo, &archived); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no such project %d", ErrNoProject, project)
		}
		return fmt.Errorf("read project %d: %w", project, err)
	}
	if archived.Valid {
		return &archivedProjectError{repo: RedactRepoURL(repo)}
	}
	return nil
}

type archivedProjectError struct{ repo string }

func (e *archivedProjectError) Error() string {
	return fmt.Sprintf("project %s was removed; run brigada init in it to bring it back", e.repo)
}
func (e *archivedProjectError) Unwrap() error { return ErrProjectArchived }

// ArchiveProject removes a repository from active service without deleting its
// history. With cancel, live jobs and unfinished plans end through the same
// cancellation paths used by job cancellation and plan rejection. Removal and
// cancellation are atomic, including ending a live attach hold.
func (e *Engine) ArchiveProject(ctx context.Context, repoURL string, cancel bool) (Answer, error) {
	var answer Answer
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		var archived sql.NullString
		if err := lookupProjectTx(tx, repoURL, "id, archived_at", &project, &archived); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no project for repo %s", ErrNoProject, RedactRepoURL(repoURL))
			}
			return err
		}
		if archived.Valid {
			answer = AnsAlready
			return nil
		}
		// Collect IDs before cancellation changes the rows being enumerated.
		ids := func(query string) ([]int64, error) {
			rows, err := tx.Query(query, project)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []int64
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return nil, err
				}
				out = append(out, id)
			}
			return out, rows.Err()
		}
		plans, err := ids("SELECT id FROM plans WHERE project_id = ? AND state IN ('draft', 'pending_approval', 'reviewing', 'active') ORDER BY id")
		if err != nil {
			return err
		}
		jobs, err := ids("SELECT id FROM jobs WHERE project_id = ? AND status IN ('queued', 'leased', 'running') ORDER BY id")
		if err != nil {
			return err
		}
		if !cancel && (len(plans) > 0 || len(jobs) > 0) {
			return fmt.Errorf("%w: project %s has %d live jobs and %d unfinished plans; use cancel to remove it and cancel them", ErrProjectBusy, RedactRepoURL(repoURL), len(jobs), len(plans))
		}
		// Close the work gate before cancellation emits events. Background
		// wake evaluation then leaves those events pending for registration.
		if _, err := tx.Exec("UPDATE projects SET archived_at = ?, updated_at = ? WHERE id = ?", fmtTime(now), fmtTime(now), project); err != nil {
			return err
		}
		for _, plan := range plans {
			if err := e.cancelPlanTx(tx, project, plan, evPlanCancelled, now); err != nil {
				return err
			}
		}
		for _, id := range jobs {
			job, err := loadJobTx(tx, id)
			if err != nil {
				return err
			}
			if job != nil && !isTerminal(job.Status) {
				if _, err := e.cancelTx(tx, job, "project removed", now); err != nil {
					return err
				}
			}
		}
		attaches, err := ids("SELECT id FROM attach_holds WHERE project_id = ? AND state = 'live'")
		if err != nil {
			return err
		}
		for _, hold := range attaches {
			if err := e.expireAttachHoldTx(tx, hold, project, now); err != nil {
				return err
			}
		}
		if _, err := appendEventTx(tx, evProjectArchived, project, 0, 0, eventPayload{}, now); err != nil {
			return err
		}
		answer = AnsOK
		return nil
	})
	return answer, err
}
