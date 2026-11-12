package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The states of an approval request (migration 0075).
const (
	ApprovalWaiting = "waiting"
	ApprovalSigned  = "signed"
	ApprovalRefused = "refused"
	ApprovalExpired = "expired"
)

// ErrApprovalNotFound reports a request id the store does not hold.
var ErrApprovalNotFound = errors.New("no such approval request")

// approvalTime is how the table keeps times: UTC, to the second.
const approvalTime = "2006-01-02T15:04:05Z"

// ApprovalRequest is one row of approval_requests: the act the challenge
// was issued for, its nonce and expiry, the request's state and, once
// signed, the signature that satisfied it and the enrolled key's label.
type ApprovalRequest struct {
	ID        int64
	Kind      string
	Identity  string
	Nonce     string
	Expires   time.Time
	Created   time.Time
	State     string
	Signature string
	KeyLabel  string
	Decided   time.Time // zero while waiting
}

const approvalColumns = `id, kind, identity, nonce, expires_at, created_at, state,
	COALESCE(signature, ''), COALESCE(key_label, ''), COALESCE(decided_at, '')`

func scanApproval(row interface{ Scan(...any) error }) (ApprovalRequest, error) {
	var r ApprovalRequest
	var expires, created, decided string
	if err := row.Scan(&r.ID, &r.Kind, &r.Identity, &r.Nonce, &expires, &created, &r.State, &r.Signature, &r.KeyLabel, &decided); err != nil {
		return ApprovalRequest{}, err
	}
	var err error
	if r.Expires, err = time.Parse(approvalTime, expires); err != nil {
		return ApprovalRequest{}, fmt.Errorf("approval request %d: expires_at: %w", r.ID, err)
	}
	if r.Created, err = time.Parse(approvalTime, created); err != nil {
		return ApprovalRequest{}, fmt.Errorf("approval request %d: created_at: %w", r.ID, err)
	}
	if decided != "" {
		if r.Decided, err = time.Parse(approvalTime, decided); err != nil {
			return ApprovalRequest{}, fmt.Errorf("approval request %d: decided_at: %w", r.ID, err)
		}
	}
	return r, nil
}

// CreateApprovalRequest stores a waiting request for the challenge of one
// act, with the expiry the challenge carries.
func (s *Store) CreateApprovalRequest(ctx context.Context, kind, identity, nonce string, expires, now time.Time) (ApprovalRequest, error) {
	var out ApprovalRequest
	err := s.Update(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO approval_requests (kind, identity, nonce, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
			kind, identity, nonce, expires.UTC().Format(approvalTime), now.UTC().Format(approvalTime))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out, err = scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approval_requests WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return ApprovalRequest{}, fmt.Errorf("create the approval request: %w", err)
	}
	return out, nil
}

// ApprovalRequest reads one request. A waiting request whose expiry has
// passed is settled to expired first, so it reads as expired and never as
// waiting.
func (s *Store) ApprovalRequest(ctx context.Context, id int64, now time.Time) (ApprovalRequest, error) {
	var out ApprovalRequest
	err := s.Update(ctx, func(tx *sql.Tx) error {
		if err := expireApprovals(ctx, tx, now); err != nil {
			return err
		}
		var err error
		out, err = scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approval_requests WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d", ErrApprovalNotFound, id)
		}
		return err
	})
	if err != nil && !errors.Is(err, ErrApprovalNotFound) {
		return ApprovalRequest{}, fmt.Errorf("read approval request %d: %w", id, err)
	}
	return out, err
}

// expireApprovals settles every waiting request past its expiry.
func expireApprovals(ctx context.Context, tx *sql.Tx, now time.Time) error {
	at := now.UTC().Format(approvalTime)
	_, err := tx.ExecContext(ctx,
		`UPDATE approval_requests SET state = 'expired', decided_at = ? WHERE state = 'waiting' AND expires_at <= ?`, at, at)
	return err
}

// SignApprovalRequest spends the request: it moves a waiting, unexpired
// request to signed, keeping the signature and the signing key's label.
// It reports whether this call did it. Only one call ever does, however
// many race or restart in between: the UPDATE takes a waiting row alone.
func (s *Store) SignApprovalRequest(ctx context.Context, id int64, signature, keyLabel string, now time.Time) (bool, error) {
	at := now.UTC().Format(approvalTime)
	var won bool
	err := s.Update(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE approval_requests SET state = 'signed', signature = ?, key_label = ?, decided_at = ?
			 WHERE id = ? AND state = 'waiting' AND expires_at > ?`, signature, keyLabel, at, id, at)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		won = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("sign approval request %d: %w", id, err)
	}
	return won, nil
}

// RefuseApprovalRequest moves a waiting request to refused; it reports
// whether it was waiting.
func (s *Store) RefuseApprovalRequest(ctx context.Context, id int64, now time.Time) (bool, error) {
	var won bool
	err := s.Update(ctx, func(tx *sql.Tx) error {
		if err := expireApprovals(ctx, tx, now); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE approval_requests SET state = 'refused', decided_at = ? WHERE id = ? AND state = 'waiting'`,
			now.UTC().Format(approvalTime), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		won = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("refuse approval request %d: %w", id, err)
	}
	return won, nil
}

// SpentApprovalRequests lists the signed and refused requests whose
// challenge has not yet expired: the ones an authority starting up must
// treat as spent.
func (s *Store) SpentApprovalRequests(ctx context.Context, now time.Time) ([]ApprovalRequest, error) {
	var out []ApprovalRequest
	err := s.View(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT `+approvalColumns+` FROM approval_requests WHERE state IN ('signed', 'refused') AND expires_at > ? ORDER BY id`,
			now.UTC().Format(approvalTime))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanApproval(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list spent approval requests: %w", err)
	}
	return out, nil
}

// WaitingApprovalRequests lists the requests still waiting, oldest first,
// after settling the ones past their expiry to expired.
func (s *Store) WaitingApprovalRequests(ctx context.Context, now time.Time) ([]ApprovalRequest, error) {
	var out []ApprovalRequest
	err := s.Update(ctx, func(tx *sql.Tx) error {
		if err := expireApprovals(ctx, tx, now); err != nil {
			return err
		}
		var err error
		out, err = WaitingApprovalRequestsTx(tx, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list waiting approval requests: %w", err)
	}
	return out, nil
}

// WaitingApprovalRequestsTx is the read-only half, for a status view: the
// waiting requests whose expiry has not passed, oldest first. A request
// past its expiry that nothing has settled yet is left out, as it reads
// as expired.
func WaitingApprovalRequestsTx(tx *sql.Tx, now time.Time) ([]ApprovalRequest, error) {
	rows, err := tx.Query(`SELECT `+approvalColumns+` FROM approval_requests WHERE state = 'waiting' AND expires_at > ? ORDER BY id`,
		now.UTC().Format(approvalTime))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApprovalRequest
	for rows.Next() {
		r, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
