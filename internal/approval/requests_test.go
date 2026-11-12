package approval

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/store"
)

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestApprovedRequestCannotBeVerifiedAgainAfterRestart(t *testing.T) {
	ctx := context.Background()
	s := newSigner(t)
	keys := NewStore(filepath.Join(t.TempDir(), StoreFile))
	if err := keys.Add(Key{Label: "me", PublicKey: s.public}); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "store.db")
	st := openStore(t, dbPath)
	act := Act{Kind: KindPlan, ID: "4"}

	a := New(keys)
	id, c, err := a.Request(ctx, st, act, 0)
	if err != nil {
		t.Fatal(err)
	}
	sig := s.signChallenge(t, c)
	if label, err := a.Approve(ctx, st, id, act, sig); err != nil || label != "me" {
		t.Fatalf("approve = %q, %v", label, err)
	}
	row, err := st.ApprovalRequest(ctx, id, time.Now())
	if err != nil || row.State != "signed" || row.Signature != string(sig) || row.KeyLabel != "me" {
		t.Fatalf("stored request = %+v, %v", row, err)
	}
	if _, err := a.Approve(ctx, st, id, act, sig); !errors.Is(err, ErrReused) {
		t.Fatalf("second approve: err = %v, want ErrReused", err)
	}

	// The engine restarts: a fresh authority and a reopened store.
	st.Close()
	st2 := openStore(t, dbPath)
	b := New(keys)
	if _, err := b.Approve(ctx, st2, id, act, sig); !errors.Is(err, ErrReused) {
		t.Fatalf("approve after restart: err = %v, want ErrReused", err)
	}
	// Even the in-memory check alone holds once the spent rows are loaded.
	if err := b.LoadSpent(ctx, st2); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(c, act, sig); !errors.Is(err, ErrReused) {
		t.Fatalf("verify after LoadSpent: err = %v, want ErrReused", err)
	}
}

func TestBadSignatureLeavesRequestWaitingAndExpiredRequestReadsExpired(t *testing.T) {
	ctx := context.Background()
	s := newSigner(t)
	a, clock := enrolled(t, s)
	st := openStore(t, ":memory:")
	act := Act{Kind: KindTask, ID: "t9"}
	id, c, err := a.Request(ctx, st, act, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Approve(ctx, st, id, act, []byte("a client token")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("bad signature: err = %v, want ErrBadSignature", err)
	}
	if row, _ := st.ApprovalRequest(ctx, id, *clock); row.State != "waiting" {
		t.Fatalf("state after a bad signature = %q, want waiting", row.State)
	}
	sig := s.signChallenge(t, c)
	*clock = clock.Add(2 * time.Minute)
	if _, err := a.Approve(ctx, st, id, act, sig); !errors.Is(err, ErrExpired) {
		t.Fatalf("late approve: err = %v, want ErrExpired", err)
	}
	if row, _ := st.ApprovalRequest(ctx, id, *clock); row.State != "expired" {
		t.Fatalf("state past expiry = %q, want expired", row.State)
	}
}

func TestRefusedRequestCannotBeApproved(t *testing.T) {
	ctx := context.Background()
	s := newSigner(t)
	a, _ := enrolled(t, s)
	st := openStore(t, ":memory:")
	act := Act{Kind: KindImport, ID: "i2"}
	id, c, err := a.Request(ctx, st, act, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Refuse(ctx, st, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Approve(ctx, st, id, act, s.signChallenge(t, c)); !errors.Is(err, ErrRefused) {
		t.Fatalf("approve of a refused request: err = %v, want ErrRefused", err)
	}
	if _, err := a.Approve(ctx, st, id+50, act, []byte("x")); !errors.Is(err, ErrNoRequest) {
		t.Fatalf("unknown request: err = %v, want ErrNoRequest", err)
	}
}
