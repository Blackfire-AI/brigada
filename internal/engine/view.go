package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
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

// Job reads one job by id (§12.1); nil when there is none. The status
// it answers is the rules' current word on the job, not a row as it
// happens to sit — but the word changes only when a time-based rule
// could have bitten since the last write, so the read first asks
// sweepDue whether one could have (a lease past its expiry, a live job
// past its kind's hard cap, a queued job past the queue TTL) and only
// then sweeps, in a write transaction, before reading again. A row
// none of them can touch — a job still inside its lease, or already
// terminal — needs no sweep at all, so the read stays on the deferred
// read-only path and never waits on the store's write lock: a status
// poll cannot be starved by a lease poll or an upload, and it adds no
// writer of its own to their queue. The sweep here is for this job's
// row alone; the store-wide pass (attach holds, stale runners) belongs
// to the calls that write and the idle engine's ticker.
func (e *Engine) Job(ctx context.Context, id int64) (*JobInfo, error) {
	var row *jobRow
	read := func() (*JobInfo, error) {
		var info *JobInfo
		err := e.store.View(ctx, func(tx *sql.Tx) error {
			j, err := loadJobTx(tx, id)
			if err != nil {
				return err
			}
			row = j
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
		return info, err
	}
	info, err := read()
	if err != nil {
		return nil, fmt.Errorf("read job %d: %w", id, err)
	}
	if !e.sweepDue(row, e.cfg.Now()) {
		return info, nil
	}
	if err := e.store.Update(ctx, func(tx *sql.Tx) error {
		_, err := e.sweepTx(tx, e.cfg.Now())
		return err
	}); err != nil {
		return nil, fmt.Errorf("read job %d: %w", id, err)
	}
	if info, err = read(); err != nil {
		return nil, fmt.Errorf("read job %d: %w", id, err)
	}
	return info, nil
}

// sweepDue reports whether the sweep's time rules could have changed
// this job's row since the last write, so a read of it must sweep
// first. The rules that touch a job are its lease's expiry, its kind's
// hard cap measured from the lease it runs on, and the queue TTL of a
// queued job; a terminal or cancelled job is beyond all three, and a
// live one inside its deadlines has nothing to sweep. The test reads
// the very columns the sweep acts on and compares them as the sweep
// does, so any round that could fire trips it — and one that cannot
// leaves the read on the lock-free path.
func (e *Engine) sweepDue(j *jobRow, now time.Time) bool {
	if j == nil {
		return false
	}
	switch j.Status {
	case statusQueued, statusLeased, statusRunning:
	default:
		return false // terminal or cancelled: no sweep round touches it
	}
	nowStr := fmtTime(now)
	if j.Status != statusQueued && j.LeaseExpiresAt != "" && j.LeaseExpiresAt <= nowStr {
		return true // the lease's expiry passed: the requeue round
	}
	if j.LastLeased != "" {
		if limit, ok := e.cfg.HardCaps[j.Kind]; ok && limit > 0 &&
			j.LastLeased <= fmtTime(now.Add(-limit)) {
			return true // past its kind's hard cap from the current lease
		}
	}
	if e.cfg.QueueTTL > 0 && j.Status == statusQueued && j.QueuedAt != "" {
		held := j.CapHeldAt
		if held == "" {
			held = j.QueuedAt
		}
		if maxStr := max(j.QueuedAt, held); maxStr <= fmtTime(now.Add(-e.cfg.QueueTTL)) {
			return true // waited past the queue TTL without a lease
		}
	}
	return false
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
