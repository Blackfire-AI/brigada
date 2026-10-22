package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestViewDoesNotHoldTheWriteLock pins View's begin: read-only, hence
// deferred, even on the _txlock=immediate DSN. A writer on another
// connection of the write-ahead-logged store must commit while a View
// transaction is open — the reader holds no write lock for it to wait
// on. Before this, every read began immediate too and took SQLite's
// single write lock, so a reader could starve writers past the busy
// timeout and a long writer could fail every concurrent reader.
func TestViewDoesNotHoldTheWriteLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "view.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	s := &Store{db: db, path: path}

	err = s.View(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			return err
		}
		// A second statement keeps the read transaction open while the
		// writer below commits.
		return tx.QueryRow("SELECT COUNT(*) FROM t").Scan(&n)
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		tx, err := other.BeginTx(ctx, nil) // the DSN begins it immediate
		if err != nil {
			done <- err
			return
		}
		if _, err := tx.Exec("INSERT INTO t (v) VALUES ('w')"); err != nil {
			tx.Rollback()
			done <- err
			return
		}
		done <- tx.Commit()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writer locked out by the open view: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer still waiting after 2s: the view holds the write lock")
	}
}
