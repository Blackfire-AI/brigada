package store

import (
	"context"
	"database/sql"
	"fmt"
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
//
// The writer starts from inside the View callback, so the read
// transaction is provably open while the writer commits: had View begun
// immediate, the writer's own immediate begin would sit on the lock
// until the view closed.
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

	started := make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		<-started
		tx, err := other.BeginTx(ctx, nil) // the DSN begins it immediate
		if err != nil {
			writer <- err
			return
		}
		if _, err := tx.Exec("INSERT INTO t (v) VALUES ('w')"); err != nil {
			tx.Rollback()
			writer <- err
			return
		}
		writer <- tx.Commit()
	}()

	err = s.View(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM t").Scan(&n); err != nil {
			return err
		}
		// The read transaction is open here: the writer begins now, and
		// it may commit before this View closes only if the view holds
		// no write lock.
		close(started)
		select {
		case err := <-writer:
			return err
		case <-time.After(2 * time.Second):
			return fmt.Errorf("the writer is still waiting after 2s: the open view holds the write lock")
		}
	})
	if err != nil {
		t.Fatalf("view with a concurrent writer: %v", err)
	}
}
