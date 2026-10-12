package engine

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// LeaseGrant is a job plus the lease that took it: what a runner needs
// to run the job and to speak about it afterwards (§12.3).
type LeaseGrant struct {
	JobID     int64
	ProjectID int64
	TaskID    int64 // 0 for a foreman turn
	Kind      Kind
	Mode      Mode
	Key       string
	Prompt    string
	LeaseID   string
	// LeaseExpiresAt is when the lease lapses without a heartbeat.
	LeaseExpiresAt time.Time
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
		// a heartbeat can still revive a healthy third lease (§3).
		if e.cfg.MaxAttempts > 0 && j.Attempts >= int64(e.cfg.MaxAttempts) {
			if err := failJobTx(tx, j.ID, fmt.Sprintf("runner lost %d times", e.cfg.MaxAttempts), 0, now); err != nil {
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
		return &LeaseGrant{
			JobID:          j.ID,
			ProjectID:      j.ProjectID,
			TaskID:         j.TaskID,
			Kind:           j.Kind,
			Mode:           j.Mode,
			Key:            j.Key,
			Prompt:         j.Prompt,
			LeaseID:        leaseID,
			LeaseExpiresAt: expires,
		}, nil
	}
	return nil, nil // nothing this runner may take
}
