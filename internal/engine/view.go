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
	// TranscriptID is the job's transcript artifact (§9), the run's own
	// record of itself; 0 until the runner uploaded one.
	TranscriptID int64 `json:"transcript_id,omitempty"`
	// CheckLogID is the job's newest check log artifact (§9); 0 when the
	// job ran no check or left no log.
	CheckLogID int64 `json:"check_log_id,omitempty"`
	// SetupLogID is the job's newest setup log artifact (§5.7); 0 when the
	// job ran no setup.
	SetupLogID int64  `json:"setup_log_id,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// Job reads one job by id (§12.1); nil when there is none. Like every
// engine call it sweeps first, so the status is the rules' current word
// on the job, not a row as it happens to sit. The sweep runs in its own
// short transaction and the read in a read-only one, so the read half of
// a status poll holds no write lock while a lease poll or an upload
// runs; a writer that lands between the two can only make what the read
// sees newer, never staler.
func (e *Engine) Job(ctx context.Context, id int64) (*JobInfo, error) {
	if err := e.store.Update(ctx, func(tx *sql.Tx) error {
		_, err := e.sweepTx(tx, e.cfg.Now())
		return err
	}); err != nil {
		return nil, fmt.Errorf("read job %d: %w", id, err)
	}
	var info *JobInfo
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		j, err := loadJobTx(tx, id)
		if err != nil {
			return err
		}
		var created, updated string
		var check sql.NullString
		var transcript sql.NullInt64
		var checkLog, setupLog int64
		if j != nil {
			if err := tx.QueryRow("SELECT created_at, updated_at, check_result, transcript_id FROM jobs WHERE id = ?", id).
				Scan(&created, &updated, &check, &transcript); err != nil {
				return fmt.Errorf("read timestamps: %w", err)
			}
			if err := tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM artifacts WHERE job_id = ? AND kind = ?", id, ArtifactCheckLog).
				Scan(&checkLog); err != nil {
				return fmt.Errorf("read check log: %w", err)
			}
			if err := tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM artifacts WHERE job_id = ? AND kind = ?", id, ArtifactSetupLog).
				Scan(&setupLog); err != nil {
				return fmt.Errorf("read setup log: %w", err)
			}
		}
		info = jobInfoOf(j)
		if info != nil {
			info.CreatedAt, info.UpdatedAt = created, updated
			info.TranscriptID = transcript.Int64
			info.CheckLogID = checkLog
			info.SetupLogID = setupLog
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
