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
