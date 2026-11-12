package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"database/sql"
)

const approvalNonce = "0123456789abcdef0123456789abcdef"

func TestApprovalRequestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openMemory(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	expires := now.Add(5 * time.Minute)

	req, err := s.CreateApprovalRequest(ctx, "plan", "12", approvalNonce, expires, now)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != ApprovalWaiting || req.Kind != "plan" || req.Identity != "12" || req.Nonce != approvalNonce || !req.Expires.Equal(expires) {
		t.Fatalf("created = %+v", req)
	}
	won, err := s.SignApprovalRequest(ctx, req.ID, "armored signature", "me", now.Add(time.Minute))
	if err != nil || !won {
		t.Fatalf("sign = %v, %v; want it won", won, err)
	}
	got, err := s.ApprovalRequest(ctx, req.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ApprovalSigned || got.Signature != "armored signature" || got.KeyLabel != "me" || got.Decided.IsZero() {
		t.Fatalf("signed = %+v", got)
	}
	if _, err := s.ApprovalRequest(ctx, req.ID+99, now); !errors.Is(err, ErrApprovalNotFound) {
		t.Errorf("unknown id: err = %v, want ErrApprovalNotFound", err)
	}
}

func TestSpentApprovalRequestIsRefusedASecondTime(t *testing.T) {
	ctx := context.Background()
	path, s := openFile(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	req, err := s.CreateApprovalRequest(ctx, "push", "p1", approvalNonce, now.Add(5*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.SignApprovalRequest(ctx, req.ID, "sig", "me", now); err != nil || !won {
		t.Fatalf("first sign = %v, %v", won, err)
	}
	if won, err := s.SignApprovalRequest(ctx, req.ID, "sig", "me", now); err != nil || won {
		t.Fatalf("second sign = %v, %v; want refused", won, err)
	}
	// A restart: the row, not memory, holds the spent state.
	s.Close()
	again := openStore(t, path)
	if won, err := again.SignApprovalRequest(ctx, req.ID, "sig", "me", now); err != nil || won {
		t.Fatalf("sign after reopen = %v, %v; want refused", won, err)
	}
	if won, err := again.RefuseApprovalRequest(ctx, req.ID, now); err != nil || won {
		t.Fatalf("refuse of a signed request = %v, %v; want no change", won, err)
	}
	spent, err := again.SpentApprovalRequests(ctx, now)
	if err != nil || len(spent) != 1 || spent[0].ID != req.ID {
		t.Fatalf("spent = %+v, %v", spent, err)
	}
}

func TestExpiredApprovalRequestReadsAsExpired(t *testing.T) {
	ctx := context.Background()
	s := openMemory(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	req, err := s.CreateApprovalRequest(ctx, "task", "t1", approvalNonce, now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	late := now.Add(time.Minute)
	if won, err := s.SignApprovalRequest(ctx, req.ID, "sig", "me", late); err != nil || won {
		t.Fatalf("sign at the expiry = %v, %v; want refused", won, err)
	}
	got, err := s.ApprovalRequest(ctx, req.ID, late)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ApprovalExpired || got.Signature != "" {
		t.Fatalf("past its expiry = %+v, want expired and unsigned", got)
	}
}

func TestRefusedApprovalRequestCannotBeSigned(t *testing.T) {
	ctx := context.Background()
	s := openMemory(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	req, _ := s.CreateApprovalRequest(ctx, "import", "i1", approvalNonce, now.Add(time.Minute), now)
	if won, err := s.RefuseApprovalRequest(ctx, req.ID, now); err != nil || !won {
		t.Fatalf("refuse = %v, %v", won, err)
	}
	if won, err := s.SignApprovalRequest(ctx, req.ID, "sig", "me", now); err != nil || won {
		t.Fatalf("sign of a refused request = %v, %v; want refused", won, err)
	}
}

func TestApprovalRequestChecks(t *testing.T) {
	s := openMemory(t)
	now := time.Now()
	if _, err := s.CreateApprovalRequest(context.Background(), "merge", "1", approvalNonce, now, now); err == nil {
		t.Error("an unknown act kind was stored")
	}
	if _, err := s.CreateApprovalRequest(context.Background(), "plan", "1", "short", now, now); err == nil {
		t.Error("a short nonce was stored")
	}
}

// A store built from the migrations before approval_requests, opened by
// this binary, gains the table and works.
func TestMigrationAddsApprovalRequests(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var before int
	for _, m := range migs {
		if m.name == "approval_requests" {
			before = m.version - 1
		}
	}
	if before == 0 {
		t.Fatal("approval_requests migration not found")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs[:before] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply %04d_%s by hand: %v", m.version, m.name, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(before)); err != nil {
		t.Fatal(err)
	}
	var n int
	err = db.QueryRow(`SELECT count(*) FROM approval_requests`).Scan(&n)
	if err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("the table exists before its migration: %v", err)
	}
	db.Close()

	s := openStore(t, path)
	if v, err := s.userVersion(context.Background()); err != nil || v != len(migs) {
		t.Errorf("user_version after upgrade = %d, %v; want %d", v, err, len(migs))
	}
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	req, err := s.CreateApprovalRequest(context.Background(), "plan", "1", approvalNonce, now.Add(time.Minute), now)
	if err != nil || req.State != ApprovalWaiting {
		t.Fatalf("create after upgrade = %+v, %v", req, err)
	}
}
