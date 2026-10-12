package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// JobInfo is a job as the API shows it (§12.1): the row's public state.
// The lease's id and owner stay between the engine and the holder; the
// expiry is shown because a waiting client waits on it. Timestamps are
// the store's UTC text as stored.
type JobInfo struct {
	ID             int64           `json:"id"`
	ProjectID      int64           `json:"project_id"`
	TaskID         int64           `json:"task_id"` // 0 for a foreman turn
	Kind           Kind            `json:"kind"`
	Mode           Mode            `json:"mode"`
	Key            string          `json:"key"`
	Status         string          `json:"status"`
	Prompt         string          `json:"prompt"`
	Harness        string          `json:"harness"`
	Labels         []string        `json:"labels"`
	Restart        bool            `json:"restart"`
	Followup       bool            `json:"followup"`
	Attempts       int64           `json:"attempts"`
	LeasedSeconds  int64           `json:"leased_seconds"`
	Result         json.RawMessage `json:"result,omitempty"`
	CheckResult    json.RawMessage `json:"check_result,omitempty"`
	LeaseExpiresAt string          `json:"lease_expires_at,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

// Job reads one job by id (§12.1); nil when there is none. Like every
// engine call it sweeps first, so the status is the rules' current word
// on the job, not a row as it happens to sit.
func (e *Engine) Job(ctx context.Context, id int64) (*JobInfo, error) {
	var info *JobInfo
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := sweepTx(tx, e.cfg, e.cfg.Now()); err != nil {
			return err
		}
		j, err := loadJobTx(tx, id)
		if err != nil {
			return err
		}
		var created, updated string
		var check sql.NullString
		if j != nil {
			if err := tx.QueryRow("SELECT created_at, updated_at, check_result FROM jobs WHERE id = ?", id).
				Scan(&created, &updated, &check); err != nil {
				return fmt.Errorf("read timestamps: %w", err)
			}
		}
		info = jobInfoOf(j)
		if info != nil {
			info.CreatedAt, info.UpdatedAt = created, updated
			if check.Valid && check.String != "" {
				info.CheckResult = json.RawMessage(check.String)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read job %d: %w", id, err)
	}
	return info, nil
}

func jobInfoOf(j *jobRow) *JobInfo {
	if j == nil {
		return nil
	}
	info := &JobInfo{
		ID:             j.ID,
		ProjectID:      j.ProjectID,
		TaskID:         j.TaskID,
		Kind:           j.Kind,
		Mode:           j.Mode,
		Key:            j.Key,
		Status:         j.Status,
		Prompt:         j.Prompt,
		Harness:        j.Harness,
		Labels:         jobLabels(j),
		Restart:        j.Restart,
		Followup:       j.Followup,
		Attempts:       j.Attempts,
		LeasedSeconds:  j.LeasedSeconds,
		LeaseExpiresAt: j.LeaseExpiresAt,
	}
	if j.Result != "" {
		info.Result = json.RawMessage(j.Result)
	}
	return info
}
