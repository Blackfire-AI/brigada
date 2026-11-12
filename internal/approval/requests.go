package approval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// The durable side of the authority: a pending request is a row of the
// store's approval_requests table (migration 0075), so a challenge stays
// single-use across an engine restart. The in-memory spent set of the
// Authority is the fast path; the row's state is the truth.

// ErrRefused marks a request the user refused, and ErrNoRequest one the
// store does not hold.
var (
	ErrRefused   = errors.New("the request was refused")
	ErrNoRequest = errors.New("no such approval request")
)

// Request issues a challenge for act and stores it as a waiting request.
// The caller shows the challenge's Message to the user and later hands the
// request's id and the user's signature to Approve.
func (a *Authority) Request(ctx context.Context, st *store.Store, act Act, ttl time.Duration) (int64, Challenge, error) {
	c, err := a.Issue(act, ttl)
	if err != nil {
		return 0, Challenge{}, err
	}
	row, err := st.CreateApprovalRequest(ctx, act.Kind, act.ID, c.Nonce, c.Expires, a.now())
	if err != nil {
		return 0, Challenge{}, err
	}
	return row.ID, c, nil
}

func challengeOf(r store.ApprovalRequest) Challenge { return RequestChallenge(r) }

// RequestChallenge rebuilds the challenge a stored request carries.
func RequestChallenge(r store.ApprovalRequest) Challenge {
	return Challenge{Act: Act{Kind: r.Kind, ID: r.Identity}, Nonce: r.Nonce, Expires: r.Expires}
}

// Approve verifies sig for the stored request id and act, and spends the
// request. A request that is signed, refused or expired is refused at once
// with the reason, before any signature is looked at; a signature that
// does not verify leaves the request waiting, for the user to try again
// until it expires. On success the request is signed, holding the
// signature, and the label of the enrolled key that made it is returned.
func (a *Authority) Approve(ctx context.Context, st *store.Store, id int64, act Act, sig []byte) (string, error) {
	row, err := st.ApprovalRequest(ctx, id, a.now())
	if errors.Is(err, store.ErrApprovalNotFound) {
		return "", fmt.Errorf("refusing %s: %w: %d", act, ErrNoRequest, id)
	}
	if err != nil {
		return "", fmt.Errorf("refusing %s: %w", act, err)
	}
	switch row.State {
	case store.ApprovalSigned:
		return "", fmt.Errorf("refusing %s: %w", act, ErrReused)
	case store.ApprovalRefused:
		return "", fmt.Errorf("refusing %s: %w", act, ErrRefused)
	case store.ApprovalExpired:
		return "", fmt.Errorf("refusing %s: %w", act, ErrExpired)
	}
	c := challengeOf(row)
	label, err := a.Verify(c, act, sig)
	if err != nil {
		return "", err
	}
	won, err := st.SignApprovalRequest(ctx, id, string(sig), label, a.now())
	if err != nil {
		return "", fmt.Errorf("refusing %s: %w", act, err)
	}
	if !won {
		// Another engine process spent it, or it expired meanwhile.
		return "", fmt.Errorf("refusing %s: %w", act, ErrReused)
	}
	return label, nil
}

// Refuse records the user's no on a waiting request; the challenge is spent
// too, so a signature made for it later approves nothing.
func (a *Authority) Refuse(ctx context.Context, st *store.Store, id int64) error {
	row, err := st.ApprovalRequest(ctx, id, a.now())
	if errors.Is(err, store.ErrApprovalNotFound) {
		return fmt.Errorf("%w: %d", ErrNoRequest, id)
	}
	if err != nil {
		return err
	}
	if _, err := st.RefuseApprovalRequest(ctx, id, a.now()); err != nil {
		return err
	}
	return a.MarkSpent(challengeOf(row))
}

// LoadSpent marks every signed or refused, still unexpired request of the
// store spent in memory: the engine calls it at start, so the authority's
// reuse detection holds what the last run decided.
func (a *Authority) LoadSpent(ctx context.Context, st *store.Store) error {
	rows, err := st.SpentApprovalRequests(ctx, a.now())
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := a.MarkSpent(challengeOf(r)); err != nil {
			return err
		}
	}
	return nil
}
