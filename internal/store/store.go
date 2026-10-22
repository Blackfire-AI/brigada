package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the pure-Go SQLite driver, so brigada stays one static binary
)

// busyTimeout is how long a connection waits on a database lock before
// giving up: long enough for a writer to finish, short enough that a
// genuinely stuck store surfaces.
const busyTimeout = 5 * time.Second

// Store is a handle to the engine's SQLite database: one file holding all
// authoritative state (design §11), opened with the pragmas the engine
// relies on and migrated to the newest schema this binary knows.
type Store struct {
	db   *sql.DB
	path string
}

// dsn builds the connection string for path. The pragmas ride in the DSN
// so every pooled connection gets them, not just the first: write-ahead
// logging, enforced foreign keys and the busy timeout. Transactions begin
// immediately, which serializes writers up front instead of deadlocking
// them mid-transaction.
func dsn(path string) string {
	return fmt.Sprintf("%s?_txlock=immediate&_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)",
		path, busyTimeout.Milliseconds())
}

// Open opens the SQLite database at path, creating the file when missing,
// and migrates the schema as far as this binary knows.
//
// The path ":memory:" opens a private in-memory database on a single
// pooled connection — a second connection would see a fresh, empty
// database — which is what tests want. The journal-mode pragma is a
// harmless no-op there: an in-memory database cannot use write-ahead
// logging.
//
// A path containing '?' or '#' is refused: dsn appends the pragmas as
// query parameters, so either character would be read as a delimiter and
// the pragmas would silently not apply.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("open store %s: path must not contain '?' or '#': dsn appends the pragmas as query parameters, so either would be read as a delimiter", path)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	return s, nil
}

// Close closes the store's database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// IsBusy reports whether err is SQLite's SQLITE_BUSY — "database is
// locked" — the error a writer meets when another writer held the
// database past the busy timeout the DSN already waits with. A caller
// whose whole transaction can safely run again may back off and retry;
// the failed transaction rolled back whole, so the retry starts clean.
// The check reads the message, not a driver type, so it keeps working
// through every wrapping the error passes through.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// Path is the store file's path as opened — ":memory:" for a private
// in-memory store — so callers can place the engine's other files
// (artifact blobs, §9) beside the one database.
func (s *Store) Path() string {
	return s.path
}

// Update runs fn in one transaction on the store, committing when fn
// returns nil and rolling back when it does not. The connection string
// already makes every transaction begin immediately (see dsn), so fn
// holds the database's write lock from its first statement: two callers
// cannot interleave inside fn, whatever pool connection each rides on
// and however many processes share the file.
func (s *Store) Update(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.withTx(ctx, fn)
}

// View is Update named for reads: fn runs in the same single immediate
// transaction, so what it sees is one consistent snapshot of the store.
func (s *Store) View(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.withTx(ctx, fn)
}

func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback() // a no-op once nothing was written
		return err
	}
	return tx.Commit()
}
