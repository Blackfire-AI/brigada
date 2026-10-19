package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TaskInfo is a task as a client reads it (§12.1): where its change
// lives — the repo, the base it started from, its current result branch
// (the latest attempt's, -r<n> included) — and the job a reader of its
// record starts from. The engine answers which refs; it never runs git
// on a client's behalf (product §5 rule 1).
type TaskInfo struct {
	Tier          string `json:"tier,omitempty"`
	EffectiveTier string `json:"effective_tier,omitempty"`
	Effort        string `json:"effort,omitempty"`
	ID            int64  `json:"id"`
	ProjectID     int64  `json:"project_id"`
	PlanID        int64  `json:"plan_id"`
	Title         string `json:"title"`
	State         string `json:"state"`
	RepoURL       string `json:"repo_url"`
	BaseRef       string `json:"base_ref"`
	Branch        string `json:"branch,omitempty"` // empty until the task has a result branch
	// LatestJobID is the task's newest job of any kind; 0 before the
	// first one was queued.
	LatestJobID int64 `json:"latest_job_id,omitempty"`
}

// Task reads one task by id; nil when there is none.
func (e *Engine) Task(ctx context.Context, id int64) (*TaskInfo, error) {
	var info *TaskInfo
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		t := &TaskInfo{ID: id}
		err := tx.QueryRow(`SELECT pl.project_id, t.plan_id, t.title, t.state, t.tier, t.effective_tier, t.effort,
				COALESCE(NULLIF(t.repo_url, ''), p.repo_url), t.base_ref, COALESCE(t.branch, ''),
				COALESCE((SELECT MAX(j.id) FROM jobs j WHERE j.task_id = t.id), 0)
			FROM tasks t JOIN plans pl ON pl.id = t.plan_id JOIN projects p ON p.id = pl.project_id
			WHERE t.id = ?`, id).
			Scan(&t.ProjectID, &t.PlanID, &t.Title, &t.State, &t.Tier, &t.EffectiveTier, &t.Effort, &t.RepoURL, &t.BaseRef, &t.Branch, &t.LatestJobID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		info = t
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read task %d: %w", id, err)
	}
	return info, nil
}
