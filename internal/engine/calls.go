package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Progress is what a runner's heartbeat carries besides its hold on
// the job (§5.6): when the worker last made progress, the last sign of
// life, and the last few progress events. The timestamps land on the
// job's row; the events ride along for the runs the engine will have to
// report (a stall), whose storage arrives with the history.
type Progress struct {
	LastProgressAt time.Time
	LastActivityAt time.Time
	Events         []string
}

// Heartbeat reports the runner is still on the job and returns ok with
// a new expiry, or why not (§12.3): cancelled first, then gone for a
// job that ended (lost, not gone, when this very call's sweep just
// hard-capped the lease the caller holds), lost for a lease id that is
// no longer the job's or one the caller does not hold — the lease id is
// the capability, the owner the identity, and both must agree —
// stale_epoch once a recover fenced the caller out (§6.4), and lost
// past the hard cap. An expired lease nobody re-leased is revived: the
// job goes back to leased with the accrual clock restarted. Progress
// from a heartbeat that answers ok is recorded on the row; the others
// carry no hold, so they record nothing.
func (e *Engine) Heartbeat(ctx context.Context, owner, jobID int64, leaseID string, prog Progress) (Answer, time.Time, error) {
	var ans Answer
	var expires time.Time
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		// The heartbeat shows the runner is here, before the sweep can
		// call it stale (§8 "Health").
		if err := e.runnerSeenTx(tx, owner, now); err != nil {
			return err
		}
		rep, err := e.sweepTx(tx, now)
		if err != nil {
			return err
		}
		ans, expires, err = e.heartbeatTx(tx, owner, jobID, leaseID, prog, rep, now)
		if err != nil {
			return fmt.Errorf("heartbeat job %d: %w", jobID, err)
		}
		return nil
	})
	return ans, expires, err
}

func (e *Engine) heartbeatTx(tx *sql.Tx, owner, jobID int64, leaseID string, prog Progress, rep SweepReport, now time.Time) (Answer, time.Time, error) {
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
	case job.LeaseOwner != owner:
		// The lease id matches but the caller is not its holder: the
		// heartbeat of a runner the job was never leased to is lost, so
		// one runner cannot keep another's lease alive.
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
			reason := capError(job.Kind, limit)
			if err := failJobTx(tx, job.ID, reason, accrue, now); err != nil {
				return "", time.Time{}, err
			}
			if _, err := appendEventTx(tx, evJobHardCapped, job.ProjectID, job.TaskID, job.ID,
				eventPayload{Status: statusFailed, Reason: reason}, now); err != nil {
				return "", time.Time{}, err
			}
			// A foreman turn capped here returns its events and
			// re-evaluates the wake like any ending (§4.2); for the
			// other kinds this is a no-op.
			if err := e.foremanFinishedTx(tx, job, statusFailed, now); err != nil {
				return "", time.Time{}, err
			}
			return AnsLost, time.Time{}, nil
		}
	}
	expires := now.Add(e.cfg.LeaseTTL)
	if err := recordProgressTx(tx, job.ID, prog, now); err != nil {
		return "", time.Time{}, err
	}
	if job.Status == statusQueued {
		// The lease expired and nobody re-leased: revive it. last_leased
		// restarts here and only here, so the revived lease's wall time
		// is banked from this moment, not from the lapsed lease. Any cap
		// hold on the queued stint is over: the job is live again.
		if _, err := tx.Exec(`UPDATE jobs SET status = 'leased', last_leased = ?, lease_expires_at = ?,
				queued_at = NULL, cap_held_at = NULL, cap_held_layer = NULL, cap_held_key = NULL, updated_at = ? WHERE id = ?`,
			fmtTime(now), fmtTime(expires), fmtTime(now), job.ID); err != nil {
			return "", time.Time{}, fmt.Errorf("revive job %d: %w", job.ID, err)
		}
		if _, err := appendEventTx(tx, evLeaseRevived, job.ProjectID, job.TaskID, job.ID,
			eventPayload{Lease: leasePrefix(leaseID)}, now); err != nil {
			return "", time.Time{}, err
		}
	} else if _, err := tx.Exec(`UPDATE jobs SET lease_expires_at = ?, updated_at = ? WHERE id = ?`,
		fmtTime(expires), fmtTime(now), job.ID); err != nil {
		return "", time.Time{}, fmt.Errorf("extend job %d: %w", job.ID, err)
	}
	// The lease-scoped foreman token, when the job carries one, lives
	// and dies with the lease: a beat that extends the lease extends the
	// token to the same expiry (§12.1).
	if err := extendLeaseTokensTx(tx, job.ID, expires); err != nil {
		return "", time.Time{}, err
	}
	return AnsOK, expires, nil
}

// recordProgressTx stores a heartbeat's progress timestamps on the job
// (§5.6): last_progress_at when the worker last made progress,
// last_activity_at for the last sign of life — a heartbeat with no
// progress yet still counts as activity, since the runner is on the
// job. Of the events only one line is stored, the newest that is not a
// tool end: a tool end carries the call's id, not the tool's name, so
// it says nothing to a person. The rest ride the protocol for the
// reports that need them.
func recordProgressTx(tx *sql.Tx, jobID int64, prog Progress, now time.Time) error {
	progress, activity := prog.LastProgressAt, prog.LastActivityAt
	if activity.IsZero() {
		activity = now
	}
	var sets []string
	var args []any
	if !progress.IsZero() {
		sets = append(sets, "last_progress_at = ?")
		args = append(args, fmtTime(progress))
	}
	for i := len(prog.Events) - 1; i >= 0; i-- {
		// The newest event that names something, one short line, for
		// the status views (§10); the rules read only the times. A
		// heartbeat of tool ends alone leaves the last event as it was.
		if strings.HasPrefix(prog.Events[i], "tool_end") {
			continue
		}
		sets = append(sets, "last_event = ?")
		args = append(args, oneLine(prog.Events[i], lastEventLimit))
		break
	}
	sets = append(sets, "last_activity_at = ?", "updated_at = ?")
	args = append(args, fmtTime(activity), fmtTime(now))
	args = append(args, jobID)
	if _, err := tx.Exec("UPDATE jobs SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return fmt.Errorf("record progress on job %d: %w", jobID, err)
	}
	return nil
}

// Completion is a runner's answer for a job (§5.4, §5.5, §12.3):
// exactly one of Result or Error, the check result when the runner ran
// the task's check, and the harness session the run ended on — written
// back so the next session-mode job on the key resumes it (§4.2).
// Artifacts are not the completion's to name: the runner uploads them
// through the artifact endpoint (§9), which stores them itself, so a
// runner can never point the engine's readers at a path of its
// choosing. Outcome, when set, names a run the runner stopped itself
// (§5.6): stalled, or past its class time cap. It rides with a Result
// — the stopped run's outcome block — and ends the job as stalled
// rather than done.
type Completion struct {
	Result      string
	Error       string
	CheckResult string
	SessionID   string
	Outcome     string
	// ErrorKind sorts an Error — "limit" when the CLI hit its
	// subscription limit (§4.4) — and LimitUntil is the reset time then,
	// RFC3339, empty when unknown.
	ErrorKind  string
	LimitUntil string
}

// Complete finishes a job as done or failed — or, when the completion
// carries a stop outcome, as stalled (§5.6). It is accepted while the
// lease id matches and the caller is the lease's holder, even after the
// lease expired and the job went back to queued, as long as nobody
// re-leased it: a runner returning from an outage delivers its answer
// instead of forcing a re-run (§12.3). It is idempotent per lease id,
// answers gone when the job was finished by someone else, lost after a
// re-lease or from a runner that does not hold the lease, and cancelled
// when the job was cancelled.
func (e *Engine) Complete(ctx context.Context, owner, jobID int64, leaseID string, c Completion) (Answer, error) {
	if (c.Result == "") == (c.Error == "") {
		return "", fmt.Errorf("complete job %d: exactly one of result or error is required", jobID)
	}
	if c.Outcome != "" {
		if !isStopOutcome(c.Outcome) {
			return "", fmt.Errorf("complete job %d: outcome must be %s or %s when set, not %q",
				jobID, OutcomeStalled, OutcomeTimeCap, c.Outcome)
		}
		if c.Error != "" {
			return "", fmt.Errorf("complete job %d: an outcome names a stopped run's result, not an error", jobID)
		}
	}
	var ans Answer
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		a, err := e.completeTx(tx, owner, jobID, leaseID, c, now)
		if err != nil {
			return fmt.Errorf("complete job %d: %w", jobID, err)
		}
		ans = a
		return nil
	})
	return ans, err
}

func (e *Engine) completeTx(tx *sql.Tx, owner, jobID int64, leaseID string, c Completion, now time.Time) (Answer, error) {
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
	case job.LeaseOwner != owner:
		// As in release: the lease id is the capability, the owner the
		// identity, and a completion from anyone else is lost.
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
	if c.Outcome != "" {
		status = statusStalled // both stop outcomes end the job as stalled (§5.6)
	}
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
		leased_seconds = leased_seconds + ?, updated_at = ?,
		check_result = COALESCE(NULLIF(?, ''), check_result)
		WHERE id = ?`, status, string(result), leaseID, accrue, fmtTime(now), c.CheckResult, job.ID); err != nil {
		return "", fmt.Errorf("finish job %d: %w", job.ID, err)
	}
	// The turn is over, and its lease-scoped foreman token with it
	// (§12.1): revoked here, in the same transaction as the completion.
	if err := revokeLeaseTokensTx(tx, job.ID); err != nil {
		return "", err
	}
	// Artifacts arrive only through the upload endpoint (§9), which
	// stores the blob itself: a completion carries references to
	// nothing the engine can be pointed at.
	eventID, err := appendEventTx(tx, evJobCompleted, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: completionStatus(c), Lease: leasePrefix(leaseID)}, now)
	if err != nil {
		return "", err
	}
	if c.Result != "" {
		// The run's answer is a conversation item too (§9): what the
		// worker ended saying, kept with the event that ended the job.
		if err := appendMessageTx(tx, job.ProjectID, eventID, msgKindWorkerResult,
			authorWorker, workerResultMessage(c.Result), now); err != nil {
			return "", err
		}
	}
	if c.SessionID != "" && job.Kind != KindReview {
		// A review's session is never the key's resume target (§7): the
		// key holds several sessions in turn — the author's, a
		// cross-family reviewer's — and the fix round a changes verdict
		// queues must resume the author's, whose runner the session's
		// affinity also pins (§8). Writing the reviewer's id here would
		// send that fix round to the reviewer's session instead.
		if err := upsertSessionTx(tx, job.Key, job.LeaseOwner, c.SessionID, now); err != nil {
			return "", err
		}
	}
	// The agent's quota and health follow its completion (§8): a limit
	// cools the account, an auth error or a streak of failures breaks
	// the agent.
	if err := e.agentOutcomeTx(tx, job, c, now); err != nil {
		return "", err
	}
	if job.Kind == KindForeman {
		// A limit hit marks the foreman limited before the inbox settles,
		// so the settle's wake already sees it (§4.4).
		if err := foremanLimitHitTx(tx, job, c, now); err != nil {
			return "", err
		}
		// A done turn reports to the user (§3, §10) before the inbox
		// settles, so the report is in the log ahead of anything the
		// settle's wake appends.
		if status == statusDone {
			if err := foremanReportTx(tx, job, c, now); err != nil {
				return "", err
			}
		}
		// The turn's events are consumed by its completion, never before
		// (§4.2), and the next wake is evaluated in this same transaction.
		return AnsOK, e.foremanFinishedTx(tx, job, status, now)
	}
	if job.TaskID != 0 {
		// The task's state follows its job's completion (§3, §6.1, §7):
		// reviewing behind a passed check, done behind a verdict or a
		// non-implement class, or parked at needs_foreman — each move in
		// this same transaction, each with its event.
		if err := e.taskAfterCompletionTx(tx, job, c, now); err != nil {
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
		if _, err := e.sweepTx(tx, now); err != nil {
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
	// The lease the token was minted for is gone, so the token is too
	// (§12.1); the turn's next lease mints a fresh one.
	if err := revokeLeaseTokensTx(tx, job.ID); err != nil {
		return "", err
	}
	if _, err := appendEventTx(tx, evJobReleased, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Reason: boundNote(reason), Lease: leasePrefix(leaseID), Owner: owner}, now); err != nil {
		return "", err
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
		if _, err := e.sweepTx(tx, now); err != nil {
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
			ans, err = e.cancelTx(tx, job, "", now)
		}
		if err != nil {
			return fmt.Errorf("cancel job %d: %w", jobID, err)
		}
		return nil
	})
	return ans, err
}

// cancelTx ends a live job as cancelled, banking whatever the lease in
// force has run up. Reason names who asked: empty for a plain cancel, or
// the submit that replaced the job with a restart round (§5.6).
func (e *Engine) cancelTx(tx *sql.Tx, job *jobRow, reason string, now time.Time) (Answer, error) {
	accrue := accrualFrom(job.LastLeased, now)
	if _, err := tx.Exec(`UPDATE jobs SET status = 'cancelled', followup = 0, lease_expires_at = NULL,
		last_leased = NULL, queued_at = NULL, leased_seconds = leased_seconds + ?, updated_at = ?
		WHERE id = ?`, accrue, fmtTime(now), job.ID); err != nil {
		return "", fmt.Errorf("cancel job %d: %w", job.ID, err)
	}
	if err := revokeLeaseTokensTx(tx, job.ID); err != nil {
		return "", err
	}
	if _, err := appendEventTx(tx, evJobCancelled, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Reason: reason}, now); err != nil {
		return "", err
	}
	// A cancelled foreman turn returns its events to pending (§4.2), and
	// the wake that follows runs in this same transaction.
	if err := e.foremanFinishedTx(tx, job, statusCancelled, now); err != nil {
		return "", err
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
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE meta SET fence_epoch = fence_epoch + 1 WHERE id = 1"); err != nil {
			return fmt.Errorf("bump fence_epoch: %w", err)
		}
		// Every lease the bump fences out takes its lease-scoped token
		// with it (§6.4, §12.1); leases granted under the new epoch mint
		// fresh ones.
		if err := revokeAllLeaseTokensTx(tx); err != nil {
			return err
		}
		var err error
		epoch, err = epochTx(tx)
		if err != nil {
			return err
		}
		_, err = appendEventTx(tx, evFenceEpochBumped, 0, 0, 0, eventPayload{Epoch: epoch}, now)
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
		if _, err := e.sweepTx(tx, e.cfg.Now()); err != nil {
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
