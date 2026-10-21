package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// The schema migrations, one numbered file each, embedded so the single
// binary carries them and the store stays self-contained (design §1.5).
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one numbered schema change. version is the PRAGMA
// user_version the store reaches once the migration has committed.
type migration struct {
	version int
	name    string
	sql     string
}

// allMigrations returns the embedded migrations in order. The numbering
// must start at 1 with no gaps or duplicates: user_version doubles as a
// count of applied migrations, which is what lets a binary tell an old
// store from one from the future.
func allMigrations() ([]migration, error) {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var migs []migration
	for _, file := range files {
		base := strings.TrimSuffix(path.Base(file), ".sql")
		num, name, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: file name must be <number>_<name>.sql", file)
		}
		version, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: file name must be <number>_<name>.sql", file)
		}
		content, err := migrationFS.ReadFile(file)
		if err != nil {
			return nil, err
		}
		migs = append(migs, migration{version: version, name: name, sql: string(content)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i, m := range migs {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..%d in order; %04d_%s is out of place", len(migs), m.version, m.name)
		}
	}
	return migs, nil
}

// ErrTooNew reports a store whose user_version is newer than the newest
// migration this binary carries. An older binary must refuse such a store
// outright rather than guess its way through a newer schema.
var ErrTooNew = errors.New("store schema is newer than this binary")

// migrate brings the store up to this binary's newest migration. An
// up-to-date store is left untouched; a store from the future is refused
// with an error wrapping ErrTooNew.
func (s *Store) migrate(ctx context.Context) error {
	migs, err := allMigrations()
	if err != nil {
		return err
	}
	version, err := s.userVersion(ctx)
	if err != nil {
		return err
	}
	if version > len(migs) {
		return fmt.Errorf("%w: user_version %d, latest known migration %d; upgrade brigada first", ErrTooNew, version, len(migs))
	}
	for _, m := range migs[version:] {
		if err := s.apply(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// apply runs one migration in its own transaction and sets user_version
// on the same commit, so a failure anywhere leaves the store exactly as
// it was. The version is read again inside the transaction: another
// process may have migrated concurrently while this Open waited on the
// write lock, and re-applying would then fail on the existing tables.
func (s *Store) apply(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
	}
	defer tx.Rollback() // a no-op once the commit below succeeds
	version, err := userVersionTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
	}
	switch {
	case version >= m.version: // someone migrated concurrently
		return tx.Commit()
	case version != m.version-1:
		return fmt.Errorf("migration %04d_%s: store is at version %d, want %d; the store changed under us", m.version, m.name, version, m.version-1)
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
	}
	if edit := schemaEdits[m.version]; edit != nil {
		if err := edit(ctx, tx); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.version)); err != nil {
		return fmt.Errorf("migration %04d_%s: set user_version: %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %04d_%s: commit: %w", m.version, m.name, err)
	}
	return nil
}

// userVersion reads PRAGMA user_version, the store's schema version.
func (s *Store) userVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return version, nil
}

// userVersionTx is userVersion inside a migration's transaction.
func userVersionTx(ctx context.Context, tx *sql.Tx) (int, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return version, nil
}

// schemaEdits are the migrations whose change SQLite's ALTER TABLE
// cannot express, run in the migration's transaction right after its
// SQL, by migration number.
var schemaEdits = map[int]func(ctx context.Context, tx *sql.Tx) error{
	23: relaxJobsTaskCheck,
	36: widenArtifactKinds,
	50: widenJobKinds,
}

// jobsPairingCheck is the CHECK 0001 put on jobs: foreman jobs have no
// task and every other job has one. jobsPairingCheckRelaxed lets a
// review job go without a task too.
const (
	jobsPairingCheck        = "CHECK ((kind = 'foreman') = (task_id IS NULL))"
	jobsPairingCheckRelaxed = "CHECK ((kind = 'foreman') = (task_id IS NULL) OR kind = 'review')"
)

// relaxJobsTaskCheck edits the jobs table's CHECK in place, by the
// procedure the SQLite documentation gives for changing a constraint
// without rebuilding the table (a rebuild drops the table, and with
// foreign keys enforced that cascades into the artifacts): edit the
// table's sql in the schema table and bump the schema version, so every
// connection re-reads the schema.
func relaxJobsTaskCheck(ctx context.Context, tx *sql.Tx) error {
	return editTableCheck(ctx, tx, "jobs", jobsPairingCheck, jobsPairingCheckRelaxed)
}

// The artifacts kind CHECK 0001 put on the table, and the one that also
// admits the setup log (§5.7).
const (
	artifactKindCheck      = "CHECK (kind IN ('transcript', 'check_log'))"
	artifactKindCheckSetup = "CHECK (kind IN ('transcript', 'check_log', 'setup_log'))"
)

// widenArtifactKinds lets the artifacts table hold a setup log, by the
// same in-place edit.
func widenArtifactKinds(ctx context.Context, tx *sql.Tx) error {
	return editTableCheck(ctx, tx, "artifacts", artifactKindCheck, artifactKindCheckSetup)
}

// The jobs kind CHECK 0001 put on the table, and the one that also
// admits the land job (§7.1).
const (
	jobsKindCheck     = "CHECK (kind IN ('work', 'fix', 'review', 'foreman'))"
	jobsKindCheckLand = "CHECK (kind IN ('work', 'fix', 'review', 'foreman', 'land'))"
)

// widenJobKinds lets the jobs table hold a land job, by the same in-place
// edit. The pairing CHECK 0023 relaxed stays as it is: a land job always
// belongs to a task, so the pairing holds for it without change.
func widenJobKinds(ctx context.Context, tx *sql.Tx) error {
	return editTableCheck(ctx, tx, "jobs", jobsKindCheck, jobsKindCheckLand)
}

// editTableCheck replaces one constraint's text in a table's stored sql.
func editTableCheck(ctx context.Context, tx *sql.Tx, table, from, to string) error {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA writable_schema = ON"); err != nil {
		return fmt.Errorf("allow schema edits: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE sqlite_master SET sql = replace(sql, ?, ?)
		WHERE type = 'table' AND name = ? AND instr(sql, ?) > 0`,
		from, to, table, from)
	if err != nil {
		return fmt.Errorf("edit the %s table: %w", table, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("edit the %s table: %q was not found (rows %d, err %v)", table, from, n, err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA schema_version = "+strconv.Itoa(version+1)); err != nil {
		return fmt.Errorf("bump schema_version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA writable_schema = OFF"); err != nil {
		return fmt.Errorf("close schema edits: %w", err)
	}
	return nil
}
