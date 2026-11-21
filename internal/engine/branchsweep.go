package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// branchSweepPayload is the stored form of a sweep job's deletable set.
// The job has no task row of its own; its prompt field carries this internal
// record instead of a worker prompt.
type branchSweepPayload struct {
	TaskIDs []int64 `json:"task_ids"`
}

// QueueTaskBranchSweeps computes each active project's deletable task ids at
// engine start and queues one runner-executed sweep per non-empty set (§13.2).
// A live job on the project's sweep key coalesces the repeat; a job that
// already ended leaves the next start to queue whatever remains.
func (e *Engine) QueueTaskBranchSweeps(ctx context.Context) (int, error) {
	queued := 0
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		rows, err := tx.Query(`SELECT id, settings FROM projects WHERE archived_at IS NULL ORDER BY id`)
		if err != nil {
			return fmt.Errorf("list projects for the task-branch sweep: %w", err)
		}
		type project struct {
			id       int64
			settings string
		}
		var projects []project
		for rows.Next() {
			var p project
			if err := rows.Scan(&p.id, &p.settings); err != nil {
				rows.Close()
				return fmt.Errorf("list projects for the task-branch sweep: %w", err)
			}
			projects = append(projects, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list projects for the task-branch sweep: %w", err)
		}
		rows.Close()

		for _, p := range projects {
			var settings struct {
				Retention *int `json:"branch_retention_days"`
			}
			if err := json.Unmarshal([]byte(p.settings), &settings); err != nil {
				return fmt.Errorf("read the branch retention of project %d: %w", p.id, err)
			}
			retention := defaultBranchRetentionDays
			if settings.Retention != nil {
				retention = *settings.Retention
			}
			ids, err := deletableTaskBranchIDsTx(tx, p.id, retention, now)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				continue
			}
			payload, err := json.Marshal(branchSweepPayload{TaskIDs: ids})
			if err != nil {
				return fmt.Errorf("encode the task-branch sweep of project %d: %w", p.id, err)
			}
			sr, err := e.submitTx(tx, SubmitParams{
				Key:       fmt.Sprintf("sweep:%d", p.id),
				ProjectID: p.id,
				Kind:      KindSweep,
				Mode:      ModeFresh,
				Prompt:    string(payload),
				Labels:    []string{"git"},
			}, now)
			if err != nil {
				return fmt.Errorf("queue the task-branch sweep of project %d: %w", p.id, err)
			}
			if sr.Answer == AnsCreated {
				queued++
			}
		}
		return nil
	})
	return queued, err
}

// deletableTaskBranchIDsTx lists landed tasks unconditionally, and cancelled
// or retired tasks whose retention window measured from settled_at has passed.
func deletableTaskBranchIDsTx(tx *sql.Tx, project int64, retention int, now time.Time) ([]int64, error) {
	rows, err := tx.Query(`SELECT t.id, t.state, t.settled_at,
		EXISTS (SELECT 1 FROM landings l WHERE l.task_id = t.id)
		FROM tasks t JOIN plans p ON p.id = t.plan_id
		WHERE p.project_id = ? ORDER BY t.id`, project)
	if err != nil {
		return nil, fmt.Errorf("list the task-branch candidates of project %d: %w", project, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var state string
		var settled sql.NullString
		var landed bool
		if err := rows.Scan(&id, &state, &settled, &landed); err != nil {
			return nil, fmt.Errorf("list the task-branch candidates of project %d: %w", project, err)
		}
		if landed {
			ids = append(ids, id)
			continue
		}
		if state != taskCancelled && state != taskRetired || !settled.Valid {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, settled.String)
		if err != nil {
			return nil, fmt.Errorf("read when task %d settled: %w", id, err)
		}
		if !at.Add(time.Duration(retention) * 24 * time.Hour).After(now) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the task-branch candidates of project %d: %w", project, err)
	}
	return ids, nil
}

// sweepLeaseGrantTx hands the runner its project's transport URL and the
// deletable set stored with the job. No worker or agent takes part.
func (e *Engine) sweepLeaseGrantTx(tx *sql.Tx, j *jobRow, owner int64, leaseID string, expires time.Time) (*LeaseGrant, error) {
	var payload branchSweepPayload
	if err := json.Unmarshal([]byte(j.Prompt), &payload); err != nil || len(payload.TaskIDs) == 0 {
		return nil, fmt.Errorf("read the task-branch sweep of job %d: invalid deletable set", j.ID)
	}
	var repo string
	if err := tx.QueryRow(`SELECT COALESCE(NULLIF(transport_url, ''), repo_url) FROM projects WHERE id = ?`, j.ProjectID).Scan(&repo); err != nil {
		return nil, fmt.Errorf("read the repo of task-branch sweep job %d: %w", j.ID, err)
	}
	return &LeaseGrant{
		JobID:          j.ID,
		LeaseOwner:     owner,
		ProjectID:      j.ProjectID,
		Kind:           j.Kind,
		Mode:           j.Mode,
		Key:            j.Key,
		LeaseID:        leaseID,
		LeaseExpiresAt: expires,
		RepoURL:        repo,
		SweepTaskIDs:   payload.TaskIDs,
		Attempt:        j.Attempts + 1,
	}, nil
}
