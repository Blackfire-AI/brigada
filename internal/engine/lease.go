package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// LeaseGrant is a job plus the lease that took it: what a runner needs
// to run the job and to speak about it afterwards (§12.3). The task
// fields — RepoURL, BaseRef, CheckCmd — are what the runner's workspace
// and check are made of (§5.1, §5.5); Attempt is the lease count after
// this one, so a re-dispatched attempt can take a fresh branch name
// (§6.4).
type LeaseGrant struct {
	JobID      int64
	LeaseOwner int64 // the runner the lease belongs to
	ProjectID  int64
	TaskID     int64 // 0 for a foreman turn
	Kind       Kind
	Mode       Mode
	Key        string
	Prompt     string
	LeaseID    string
	// LeaseExpiresAt is when the lease lapses without a heartbeat.
	LeaseExpiresAt time.Time

	Harness  string // the job's harness constraint; '' = any (§8)
	RepoURL  string // the task's own repo, else the project's (§6.1)
	BaseRef  string // the ref the worktree starts from
	CheckCmd string // the check the runner runs after a done (§5.5); '' = none
	Attempt  int64  // leases taken on the job, this one included
	// ResumeSessionID is the harness session the key's last run ended
	// on (§4.2), for a session-mode job to resume; empty when the key
	// never ran or the job is fresh.
	ResumeSessionID string
}

// Lease hands the caller the oldest queued job it may run, or nil when
// there is none (§12.3, §8). A job is offered to owner only if the
// owner offers its harness and all its labels, is not the runner that
// just released it (until the exclusion lapses), and — for a
// session-mode job — is not a different runner than the one the key's
// live session sits on.
func (e *Engine) Lease(ctx context.Context, owner int64, harnesses, labels []string) (*LeaseGrant, error) {
	var grant *LeaseGrant
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := sweepTx(tx, e.cfg, now); err != nil {
			return err
		}
		g, err := e.leaseTx(tx, owner, harnesses, labels, now)
		if err != nil {
			return fmt.Errorf("lease for runner %d: %w", owner, err)
		}
		grant = g
		return nil
	})
	return grant, err
}

func (e *Engine) leaseTx(tx *sql.Tx, owner int64, harnesses, labels []string, now time.Time) (*LeaseGrant, error) {
	epoch, err := epochTx(tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT ` + jobColumns + ` FROM jobs WHERE status = 'queued' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list queued jobs: %w", err)
	}
	var queued []*jobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("list queued jobs: %w", err)
		}
		queued = append(queued, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list queued jobs: %w", err)
	}
	rows.Close()

	nowStr := fmtTime(now)
	for _, j := range queued {
		// A job that lost its runner MaxAttempts times fails here rather
		// than being offered again — at lease time, not in the sweep, so
		// a heartbeat can still revive a healthy third lease (§3). This
		// only fires when some runner asks for work; until then the job
		// simply sits queued, and the queue TTL — not this rule — is
		// what frees its key.
		if e.cfg.MaxAttempts > 0 && j.Attempts >= int64(e.cfg.MaxAttempts) {
			reason := fmt.Sprintf("runner lost %d times", e.cfg.MaxAttempts)
			if err := failJobTx(tx, j.ID, reason, 0, now); err != nil {
				return nil, err
			}
			if _, err := appendEventTx(tx, evJobAttemptsExceeded, j.ProjectID, j.TaskID, j.ID,
				eventPayload{Status: statusFailed, Reason: reason}, now); err != nil {
				return nil, err
			}
			continue
		}
		if j.Harness != "" && !slices.Contains(harnesses, j.Harness) {
			continue
		}
		if !labelsCover(labels, jobLabels(j)) {
			continue
		}
		// A job this owner released goes to other runners first; the
		// exclusion lapses so a lone runner can retry (§12.3).
		if j.ReleasedBy == owner && j.ReleasedUntil != "" && j.ReleasedUntil > nowStr {
			continue
		}
		if j.Mode == ModeSession {
			free, err := affinityFreeTx(tx, j.Key, owner)
			if err != nil {
				return nil, err
			}
			if !free {
				continue
			}
		}

		leaseID, err := e.cfg.NewLeaseID(owner)
		if err != nil {
			return nil, err
		}
		expires := now.Add(e.cfg.LeaseTTL)
		_, err = tx.Exec(`UPDATE jobs SET status = 'leased', lease_id = ?, lease_owner = ?, lease_expires_at = ?,
			lease_epoch = ?, fence_token = attempts + 1, attempts = attempts + 1,
			first_leased = COALESCE(first_leased, ?), last_leased = ?, queued_at = NULL,
			released_by = NULL, released_until = NULL, released_lease = NULL, updated_at = ?
			WHERE id = ?`,
			leaseID, owner, fmtTime(expires), epoch, nowStr, nowStr, nowStr, j.ID)
		if err != nil {
			return nil, fmt.Errorf("lease job %d: %w", j.ID, err)
		}
		if j.Mode == ModeSession {
			// The session now sits on this runner: later session jobs on
			// the key are invisible to the others (§8).
			if err := upsertSessionTx(tx, j.Key, owner, "", now); err != nil {
				return nil, err
			}
		}
		if _, err := appendEventTx(tx, evJobLeased, j.ProjectID, j.TaskID, j.ID,
			eventPayload{Lease: leasePrefix(leaseID), Owner: owner, Attempt: j.Attempts + 1}, now); err != nil {
			return nil, err
		}
		repo, baseRef, checkCmd, err := taskInfoTx(tx, j)
		if err != nil {
			return nil, err
		}
		resume := ""
		if j.Mode == ModeSession {
			// What the key's last run ended on, if it left a session id
			// behind (§4.2); empty for the key's first session job.
			var sid sql.NullString
			if err := tx.QueryRow("SELECT harness_session_id FROM sessions WHERE key = ?", j.Key).Scan(&sid); err == nil && sid.Valid {
				resume = sid.String
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("read the session of %s: %w", j.Key, err)
			}
		}
		return &LeaseGrant{
			JobID:          j.ID,
			LeaseOwner:     owner,
			ProjectID:      j.ProjectID,
			TaskID:         j.TaskID,
			Kind:           j.Kind,
			Mode:           j.Mode,
			Key:            j.Key,
			Prompt:         j.Prompt,
			LeaseID:        leaseID,
			LeaseExpiresAt: expires,

			Harness:         j.Harness,
			RepoURL:         repo,
			BaseRef:         baseRef,
			CheckCmd:        checkCmd,
			Attempt:         j.Attempts + 1,
			ResumeSessionID: resume,
		}, nil
	}
	return nil, nil // nothing this runner may take
}

// taskInfoTx reads the workspace fields of a worker job's task: the
// repo URL (the task's own, else the project's, §6.1) and the base ref,
// plus the check command the runner runs after a done (§5.5). A foreman
// turn belongs to no task and gets empty fields.
func taskInfoTx(tx *sql.Tx, j *jobRow) (repoURL, baseRef, checkCmd string, err error) {
	if j.TaskID == 0 {
		return "", "", "", nil
	}
	err = tx.QueryRow(`SELECT COALESCE(NULLIF(t.repo_url, ''), p.repo_url), t.base_ref, COALESCE(t.check_cmd, '')
		FROM tasks t JOIN plans pl ON pl.id = t.plan_id JOIN projects p ON p.id = pl.project_id
		WHERE t.id = ?`, j.TaskID).
		Scan(&repoURL, &baseRef, &checkCmd)
	if err != nil {
		return "", "", "", fmt.Errorf("read task %d: %w", j.TaskID, err)
	}
	return repoURL, baseRef, checkCmd, nil
}
