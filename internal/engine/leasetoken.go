package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// leaseTokenEntropy is the entropy of a lease-scoped foreman token's
// secret: 32 random bytes, hex-encoded — the strength of the sha256 the
// store keeps of it, as strong a secret as the token file's.
const leaseTokenEntropy = 32

// newLeaseToken mints the default lease-token secret: 32 random bytes
// as hex, unique for the life of the store.
func newLeaseToken() (string, error) {
	b := make([]byte, leaseTokenEntropy)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint lease token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// hashToken is how every bearer secret becomes its stored form: sha256
// hex, the same rule the token file's sync keeps (§6.4).
func hashToken(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

// mintLeaseTokenTx mints the lease-scoped foreman token of a foreman
// turn's lease (§12.1, §12.2): a random secret whose hash is stored with
// the project, job and lease, expiring with the lease. The secret
// itself comes back exactly once, for the grant — never a log, an event
// or an error message — and lives nowhere but the runner's memory.
func (e *Engine) mintLeaseTokenTx(tx *sql.Tx, j *jobRow, leaseID string, expires, now time.Time) (string, error) {
	secret, err := e.cfg.NewLeaseToken()
	if err != nil {
		return "", fmt.Errorf("mint the lease token of job %d: %w", j.ID, err)
	}
	if _, err := tx.Exec(`INSERT INTO lease_tokens (token_hash, project_id, job_id, lease_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(secret), j.ProjectID, j.ID, leaseID, fmtTime(expires), fmtTime(now)); err != nil {
		return "", fmt.Errorf("store the lease token of job %d: %w", j.ID, err)
	}
	return secret, nil
}

// revokeLeaseTokensTx deletes the job's lease tokens — the revocation
// half of the lease-scoped foreman token: whatever ends the job or its
// lease takes the tokens with it, in the same transaction as the state
// change. A task job holds none, so for it this is a no-op.
func revokeLeaseTokensTx(tx *sql.Tx, jobID int64) error {
	if _, err := tx.Exec("DELETE FROM lease_tokens WHERE job_id = ?", jobID); err != nil {
		return fmt.Errorf("revoke the lease tokens of job %d: %w", jobID, err)
	}
	return nil
}

// revokeAllLeaseTokensTx empties the lease-token table for a fence-epoch
// bump (§6.4): every lease the bump fences out takes its token with it,
// and the leases that follow mint fresh ones under the new epoch.
func revokeAllLeaseTokensTx(tx *sql.Tx) error {
	if _, err := tx.Exec("DELETE FROM lease_tokens"); err != nil {
		return fmt.Errorf("revoke the lease tokens: %w", err)
	}
	return nil
}

// extendLeaseTokensTx moves a job's lease tokens' expiry with the
// lease's own: a heartbeat that keeps the lease alive keeps the foreman
// token alive the same way. A job with no tokens — a task job, or a
// turn whose lapsed lease already lost its token to the sweep —
// updates nothing.
func extendLeaseTokensTx(tx *sql.Tx, jobID int64, expires time.Time) error {
	if _, err := tx.Exec("UPDATE lease_tokens SET expires_at = ? WHERE job_id = ?",
		fmtTime(expires), jobID); err != nil {
		return fmt.Errorf("extend the lease tokens of job %d: %w", jobID, err)
	}
	return nil
}

// LeaseTokenScope resolves the secret of a lease-scoped foreman token to
// the project and job it was minted for: what the API's authentication
// asks about a bearer secret the token file does not know. ok is false
// for a secret nobody minted or one past its expiry — the expiry is
// checked here, at authentication time, so a token dies at its lease's
// expiry even before the sweep has collected the row.
func (e *Engine) LeaseTokenScope(ctx context.Context, secret string) (project, job int64, ok bool, err error) {
	err = e.store.View(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT project_id, job_id FROM lease_tokens
			WHERE token_hash = ? AND expires_at > ?`, hashToken(secret), fmtTime(e.cfg.Now())).
			Scan(&project, &job)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // unknown or expired: not an error, just not ours
		}
		if err != nil {
			return fmt.Errorf("read the lease token: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	return project, job, ok, nil
}
