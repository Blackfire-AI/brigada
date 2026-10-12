package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SubmitParams describes the job to create for a key (§12.1, §14).
type SubmitParams struct {
	// Key is the queue: 'task:<id>' for worker jobs, 'foreman:<project>'
	// for the foreman's turns. At most one live job exists per key.
	Key       string
	ProjectID int64
	TaskID    int64 // 0 for a foreman turn
	Kind      Kind
	Mode      Mode
	Prompt    string
	// Harness and Labels are placement constraints: the harness that
	// must run the job ('' = any) and the labels a runner must offer in
	// full (§8).
	Harness string
	Labels  []string
	// Restart marks the job as a restart round (§5.6): any live job on
	// the key is cancelled first, and the job counts toward the task's
	// restart cap.
	Restart bool
}

// SubmitResult is what Submit did.
type SubmitResult struct {
	Answer Answer
	// JobID is the created job, or the live job a pending submit
	// coalesced behind.
	JobID int64
	// Run is the key's run number when a job was created: how many jobs
	// the key has had, this one included.
	Run int
}

// Submit creates a queued job for the key, or coalesces behind the live
// one (§12.1, §14): while a live job holds the key, nothing is created,
// the live job's follow-up flag is set and the caller learns which job
// to wait for. A submit past the task's round cap creates nothing and
// answers capped. Losing the one-live-job-per-key race to a concurrent
// submit returns the same pending answer, never an error.
func (e *Engine) Submit(ctx context.Context, p SubmitParams) (SubmitResult, error) {
	var res SubmitResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := sweepTx(tx, e.cfg, now); err != nil {
			return err
		}
		r, err := e.submitTx(tx, p, now)
		if err != nil {
			return fmt.Errorf("submit %s: %w", p.Key, err)
		}
		res = r
		return nil
	})
	return res, err
}

func (e *Engine) submitTx(tx *sql.Tx, p SubmitParams, now time.Time) (SubmitResult, error) {
	switch {
	case p.Key == "":
		return SubmitResult{}, fmt.Errorf("key is required")
	case p.Kind == KindForeman && p.TaskID != 0:
		return SubmitResult{}, fmt.Errorf("a foreman turn belongs to no task")
	case p.Kind != KindForeman && p.TaskID == 0:
		return SubmitResult{}, fmt.Errorf("kind %s needs a task", p.Kind)
	}
	capped, err := e.roundCapReached(tx, p)
	if err != nil {
		return SubmitResult{}, err
	}
	if capped {
		return SubmitResult{Answer: AnsCapped}, nil
	}
	if p.Restart {
		// §5.6: a restart replaces whatever is live on the key.
		live, err := liveJobTx(tx, p.Key)
		if err != nil {
			return SubmitResult{}, err
		}
		if live != nil {
			if _, err := cancelTx(tx, live, now); err != nil {
				return SubmitResult{}, err
			}
		}
	}
	labels := p.Labels
	if labels == nil {
		labels = []string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("encode labels: %w", err)
	}
	var run int
	if err := tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE key = ?", p.Key).Scan(&run); err != nil {
		return SubmitResult{}, fmt.Errorf("count jobs for the run number: %w", err)
	}
	run++

	nowStr := fmtTime(now)
	res, err := tx.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status,
		harness, labels, restart, queued_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?)`,
		p.ProjectID, nullIfZero(p.TaskID), string(p.Kind), p.Key, string(p.Mode), p.Prompt,
		p.Harness, string(labelsJSON), boolInt(p.Restart), nowStr, nowStr, nowStr)
	if err != nil {
		if !isUniqueViolation(err) {
			return SubmitResult{}, fmt.Errorf("create job: %w", err)
		}
		// The one-live-job-per-key race is lost: the winner's live job is
		// this submit's answer. The insert doubles as the live-job check,
		// so there is no window between the two for anyone to slip into.
		live, lerr := liveJobTx(tx, p.Key)
		if lerr != nil {
			return SubmitResult{}, lerr
		}
		if live == nil {
			return SubmitResult{}, fmt.Errorf("create job: %w", err)
		}
		if _, uerr := tx.Exec("UPDATE jobs SET followup = 1, updated_at = ? WHERE id = ?", nowStr, live.ID); uerr != nil {
			return SubmitResult{}, fmt.Errorf("set the follow-up flag on job %d: %w", live.ID, uerr)
		}
		return SubmitResult{Answer: AnsPending, JobID: live.ID}, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return SubmitResult{}, fmt.Errorf("create job: last insert id: %w", err)
	}
	return SubmitResult{Answer: AnsCreated, JobID: id, Run: run}, nil
}

// roundCapReached counts the rounds the task has already spent against
// the cap this submit would consume: fix and review rounds as job
// counts, restart rounds as jobs flagged restart (§14). A task's caps
// JSON — {"fix": 3, "review": 3, "restart": 2} — overrides the
// configured defaults key by key.
func (e *Engine) roundCapReached(tx *sql.Tx, p SubmitParams) (bool, error) {
	if p.TaskID == 0 {
		return false, nil
	}
	var which string
	var limit int
	switch {
	case p.Kind == KindFix:
		which, limit = "fix", e.cfg.RoundCaps.Fix
	case p.Kind == KindReview:
		which, limit = "review", e.cfg.RoundCaps.Review
	case p.Restart:
		which, limit = "restart", e.cfg.RoundCaps.Restart
	default:
		return false, nil
	}
	var capsJSON string
	err := tx.QueryRow("SELECT caps FROM tasks WHERE id = ?", p.TaskID).Scan(&capsJSON)
	switch {
	case err == nil:
		var caps map[string]int
		if json.Unmarshal([]byte(capsJSON), &caps) == nil {
			if v, ok := caps[which]; ok {
				limit = v
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		// the insert's foreign key will say this plainly
	default:
		return false, fmt.Errorf("read caps of task %d: %w", p.TaskID, err)
	}
	query := "SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = ?"
	args := []any{p.TaskID, which}
	if which == "restart" {
		query = "SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1"
		args = []any{p.TaskID}
	}
	var count int
	if err := tx.QueryRow(query, args...).Scan(&count); err != nil {
		return false, fmt.Errorf("count %s rounds of task %d: %w", which, p.TaskID, err)
	}
	return count >= limit, nil
}
