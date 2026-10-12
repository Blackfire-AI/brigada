package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Heartbeat reports the runner is still on the job and returns ok with
// a new expiry, or why not (§12.3): cancelled first, then gone for a
// job that ended (lost, not gone, when this very call's sweep just
// hard-capped the lease the caller holds), lost for a lease id that is
// no longer the job's, stale_epoch once a recover fenced the caller
// out (§6.4), and lost past the hard cap. An expired lease nobody
// re-leased is revived: the job goes back to leased with the accrual
// clock restarted.
func (e *Engine) Heartbeat(ctx context.Context, jobID int64, leaseID string) (Answer, time.Time, error) {
	var ans Answer
	var expires time.Time
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		rep, err := sweepTx(tx, e.cfg, now)
		if err != nil {
			return err
		}
		ans, expires, err = e.heartbeatTx(tx, jobID, leaseID, rep, now)
		if err != nil {
			return fmt.Errorf("heartbeat job %d: %w", jobID, err)
		}
		return nil
	})
	return ans, expires, err
}

func (e *Engine) heartbeatTx(tx *sql.Tx, jobID int64, leaseID string, rep SweepReport, now time.Time) (Answer, time.Time, error) {
	job, err := loadJobTx(tx, jobID)
	if err != nil {
		return "", time.Time{}, err
	}
	switch {
	case job == nil:
		return AnsGone, time.Time{}, nil
	case job.Status == statusCancelled:
		return AnsCancelled, time.Time{}, nil
	case isTerminal(job.Status):
		if job.LeaseID == leaseID && slices.Contains(rep.HardCapped, jobID) {
			return AnsLost, time.Time{}, nil
		}
		return AnsGone, time.Time{}, nil
	case job.LeaseID == "" || job.LeaseID != leaseID:
		return AnsLost, time.Time{}, nil
	}
	epoch, err := epochTx(tx)
	if err != nil {
		return "", time.Time{}, err
	}
	if job.LeaseEpoch < epoch {
		return AnsStaleEpoch, time.Time{}, nil
	}
	// Past the hard cap the lease is lost and the job fails with the cap
	// named (§14). The sweep at the top of this call usually gets here
	// first; this is the same rule for a job the sweep could not fail.
	if limit, ok := e.cfg.HardCaps[job.Kind]; ok && limit > 0 && job.FirstLeased != "" {
		if first, err := parseTime(job.FirstLeased); err == nil && now.Sub(first) > limit {
			accrue := accrualFrom(job.LastLeased, first.Add(limit))
			if err := failJobTx(tx, job.ID, capError(job.Kind, limit), accrue, now); err != nil {
				return "", time.Time{}, err
			}
			return AnsLost, time.Time{}, nil
		}
	}
	expires := now.Add(e.cfg.LeaseTTL)
	if job.Status == statusQueued {
		// The lease expired and nobody re-leased: revive it. last_leased
		// restarts here and only here, so the revived lease's wall time
		// is banked from this moment, not from the lapsed lease.
		if _, err := tx.Exec(`UPDATE jobs SET status = 'leased', last_leased = ?, lease_expires_at = ?,
			queued_at = NULL, updated_at = ? WHERE id = ?`,
			fmtTime(now), fmtTime(expires), fmtTime(now), job.ID); err != nil {
			return "", time.Time{}, fmt.Errorf("revive job %d: %w", job.ID, err)
		}
	} else if _, err := tx.Exec(`UPDATE jobs SET lease_expires_at = ?, updated_at = ? WHERE id = ?`,
		fmtTime(expires), fmtTime(now), job.ID); err != nil {
		return "", time.Time{}, fmt.Errorf("extend job %d: %w", job.ID, err)
	}
	return AnsOK, expires, nil
}

// Completion is a runner's answer for a job (§5.4, §12.3): exactly one
// of Result or Error, the artifacts the run produced, and the harness
// session the run ended on — written back so the next session-mode job
// on the key resumes it (§4.2).
type Completion struct {
	Result    string
	Error     string
	SessionID string
	Artifacts []Artifact
}

// Artifact is one stored artifact reference (§9): the session
// transcript after a job, the log of a check the runner ran.
type Artifact struct {
	Kind   string // 'transcript' or 'check_log'
	Path   string
	Size   int64
	SHA256 string
}

// Complete finishes a job as done or failed. It is accepted while the
// lease id matches, even after the lease expired and the job went back
// to queued, as long as nobody re-leased it: a runner returning from an
// outage delivers its answer instead of forcing a re-run (§12.3). It
// is idempotent per lease id, answers gone when the job was finished by
// someone else, lost after a re-lease, and cancelled when the job was
// cancelled.
func (e *Engine) Complete(ctx context.Context, jobID int64, leaseID string, c Completion) (Answer, error) {
	if (c.Result == "") == (c.Error == "") {
		return "", fmt.Errorf("complete job %d: exactly one of result or error is required", jobID)
	}
	var ans Answer
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := sweepTx(tx, e.cfg, now); err != nil {
			return err
		}
		a, err := e.completeTx(tx, jobID, leaseID, c, now)
		if err != nil {
			return fmt.Errorf("complete job %d: %w", jobID, err)
		}
		ans = a
		return nil
	})
	return ans, err
}

func (e *Engine) completeTx(tx *sql.Tx, jobID int64, leaseID string, c Completion, now time.Time) (Answer, error) {
	job, err := loadJobTx(tx, jobID)
	if err != nil {
		return "", err
	}
	switch {
	case job == nil:
		return AnsGone, nil
	case job.Status == statusCancelled:
		return AnsCancelled, nil
	case isTerminal(job.Status):
		if job.DoneLease == leaseID {
			return AnsAlready, nil
		}
		return AnsGone, nil
	case job.LeaseID == "" || job.LeaseID != leaseID:
		return AnsLost, nil
	}
	epoch, err := epochTx(tx)
	if err != nil {
		return "", err
	}
	if job.LeaseEpoch < epoch {
		return AnsStaleEpoch, nil
	}

	status := statusDone
	result, err := json.Marshal(struct {
		Result    string `json:"result"`
		SessionID string `json:"session_id,omitempty"`
	}{c.Result, c.SessionID})
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	if c.Error != "" {
		status = statusFailed
		result, err = json.Marshal(struct {
			Error string `json:"error"`
		}{c.Error})
		if err != nil {
			return "", fmt.Errorf("encode error: %w", err)
		}
	}
	accrue := accrualFrom(job.LastLeased, now)
	if _, err := tx.Exec(`UPDATE jobs SET status = ?, result = ?, done_lease = ?,
		lease_expires_at = NULL, last_leased = NULL, queued_at = NULL,
		leased_seconds = leased_seconds + ?, updated_at = ?
		WHERE id = ?`, status, string(result), leaseID, accrue, fmtTime(now), job.ID); err != nil {
		return "", fmt.Errorf("finish job %d: %w", job.ID, err)
	}
	for _, a := range c.Artifacts {
		if _, err := tx.Exec(`INSERT INTO artifacts (job_id, kind, path, size, sha256, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, job.ID, a.Kind, a.Path, a.Size, a.SHA256, fmtTime(now)); err != nil {
			return "", fmt.Errorf("store artifact for job %d: %w", job.ID, err)
		}
	}
	if c.SessionID != "" {
		if err := upsertSessionTx(tx, job.Key, job.LeaseOwner, c.SessionID, now); err != nil {
			return "", err
		}
	}
	return AnsOK, nil
}

// Release gives a job back (§12.3): queued again with the attempt
// refunded and the lease cleared — a late heartbeat or complete from
// the releaser is lost — while the releaser does not see the job again
// until the exclusion lapses. The caller must be the lease's owner: a
// release under anyone else's name is lost, so released_by and the
// exclusion window can never be pinned on a runner that does not hold
// the lease. The reason is kept, bounded to 500 bytes. Idempotent per
// lease id.
func (e *Engine) Release(ctx context.Context, owner, jobID int64, leaseID, reason string) (Answer, error) {
	var ans Answer
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := sweepTx(tx, e.cfg, now); err != nil {
			return err
		}
		a, err := e.releaseTx(tx, owner, jobID, leaseID, reason, now)
		if err != nil {
			return fmt.Errorf("release job %d: %w", jobID, err)
		}
		ans = a
		return nil
	})
	return ans, err
}

func (e *Engine) releaseTx(tx *sql.Tx, owner, jobID int64, leaseID, reason string, now time.Time) (Answer, error) {
	job, err := loadJobTx(tx, jobID)
	if err != nil {
		return "", err
	}
	switch {
	case job == nil:
		return AnsGone, nil
	case job.Status == statusCancelled:
		return AnsCancelled, nil
	case leaseID != "" && job.ReleasedLease == leaseID:
		return AnsAlready, nil
	case isTerminal(job.Status):
		return AnsGone, nil
	case job.LeaseID == "" || job.LeaseID != leaseID:
		return AnsLost, nil
	case job.LeaseOwner != owner:
		// The lease id matches but the caller is not its holder: losing
		// here keeps released_by — and with it the exclusion window —
		// from landing on a runner the job was never leased to.
		return AnsLost, nil
	}
	epoch, err := epochTx(tx)
	if err != nil {
		return "", err
	}
	if job.LeaseEpoch < epoch {
		return AnsStaleEpoch, nil
	}
	accrue := accrualFrom(job.LastLeased, now)
	if _, err := tx.Exec(`UPDATE jobs SET status = 'queued', lease_id = NULL, lease_owner = NULL,
		lease_expires_at = NULL, lease_epoch = NULL, attempts = max(attempts - 1, 0),
		queued_at = ?, released_by = ?, released_until = ?, release_note = ?, released_lease = ?,
		last_leased = NULL, leased_seconds = leased_seconds + ?, updated_at = ?
		WHERE id = ?`,
		fmtTime(now), owner, fmtTime(now.Add(e.cfg.ReleaseExclude)), boundNote(reason), leaseID,
		accrue, fmtTime(now), job.ID); err != nil {
		return "", fmt.Errorf("requeue job %d: %w", job.ID, err)
	}
	return AnsOK, nil
}

// Cancel stops a live job (§12.1): it goes to cancelled whatever lease
// is running on it, and the key's follow-up flag clears. The runner
// learns on its next heartbeat or complete. Cancel is not fenced: it
// carries no lease, so a client may cancel through a recover (§6.4).
func (e *Engine) Cancel(ctx context.Context, jobID int64) (Answer, error) {
	var ans Answer
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := sweepTx(tx, e.cfg, now); err != nil {
			return err
		}
		job, err := loadJobTx(tx, jobID)
		if err != nil {
			return err
		}
		switch {
		case job == nil:
			ans = AnsGone
		case job.Status == statusCancelled:
			ans = AnsAlready
		case isTerminal(job.Status):
			ans = AnsGone
		default:
			ans, err = cancelTx(tx, job, now)
		}
		if err != nil {
			return fmt.Errorf("cancel job %d: %w", jobID, err)
		}
		return nil
	})
	return ans, err
}

// cancelTx ends a live job as cancelled, banking whatever the lease in
// force has run up.
func cancelTx(tx *sql.Tx, job *jobRow, now time.Time) (Answer, error) {
	accrue := accrualFrom(job.LastLeased, now)
	if _, err := tx.Exec(`UPDATE jobs SET status = 'cancelled', followup = 0, lease_expires_at = NULL,
		last_leased = NULL, queued_at = NULL, leased_seconds = leased_seconds + ?, updated_at = ?
		WHERE id = ?`, accrue, fmtTime(now), job.ID); err != nil {
		return "", fmt.Errorf("cancel job %d: %w", job.ID, err)
	}
	return AnsOK, nil
}

// BumpFenceEpoch raises the store-wide fence epoch (§6.4), for recover:
// from here on, every lease, heartbeat or complete that references the
// pre-recovery epoch is refused, so a late runner cannot write into the
// rebuilt store.
func (e *Engine) BumpFenceEpoch(ctx context.Context) (int, error) {
	var epoch int64
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := sweepTx(tx, e.cfg, e.cfg.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE meta SET fence_epoch = fence_epoch + 1 WHERE id = 1"); err != nil {
			return fmt.Errorf("bump fence_epoch: %w", err)
		}
		var err error
		epoch, err = epochTx(tx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("bump fence epoch: %w", err)
	}
	return int(epoch), nil
}

// PlanWorkerSeconds sums the leased wall time the plan's jobs have
// banked: the number the per-plan worker-minutes budget caps (§14).
// Foreman turns belong to no task, so they are not worker time.
func (e *Engine) PlanWorkerSeconds(ctx context.Context, planID int64) (time.Duration, error) {
	var secs int64
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := sweepTx(tx, e.cfg, e.cfg.Now()); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT COALESCE(SUM(j.leased_seconds), 0)
			FROM jobs j JOIN tasks t ON t.id = j.task_id
			WHERE t.plan_id = ?`, planID).Scan(&secs); err != nil {
			return fmt.Errorf("sum worker time of plan %d: %w", planID, err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("plan worker seconds: %w", err)
	}
	return time.Duration(secs) * time.Second, nil
}
