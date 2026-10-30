package store

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/defaults"
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
	58: widenPlanReviewChecks,
	59: widenPlanNeedsForemanCheck,
	64: seedRoleInstructions,
	71: widenTaskStates,
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

// The checks 0058 widens for the plan review: the jobs kind CHECK and its
// pairing CHECK admit a plan_review job (no task, like a standalone
// review), and the plans state CHECK admits the reviewing state a plan
// under review sits in.
const (
	jobsKindCheckPlanReview    = "CHECK (kind IN ('work', 'fix', 'review', 'foreman', 'land', 'plan_review'))"
	jobsPairingCheckPlanReview = "CHECK ((kind = 'foreman') = (task_id IS NULL) OR kind IN ('review', 'plan_review'))"
	plansStateCheck            = "CHECK (state IN ('draft', 'pending_approval', 'active', 'done', 'cancelled'))"
	plansStateCheckReviewing   = "CHECK (state IN ('draft', 'pending_approval', 'reviewing', 'active', 'done', 'cancelled'))"
)

// The CHECK 0059 widens for the parked plan: the plans state CHECK also
// admits needs_foreman, where a plan-review changes verdict with a round
// left waits for the foreman's answer (§6.2).
const plansStateCheckNeedsForeman = "CHECK (state IN ('draft', 'pending_approval', 'reviewing', 'needs_foreman', 'active', 'done', 'cancelled'))"

// widenPlanNeedsForemanCheck lets the plans table hold a parked plan, by
// the same in-place edit 0058 used for reviewing.
func widenPlanNeedsForemanCheck(ctx context.Context, tx *sql.Tx) error {
	return editTableCheck(ctx, tx, "plans", plansStateCheckReviewing, plansStateCheckNeedsForeman)
}

// The tasks state CHECK 0001 put on the table, and the one that also
// admits a retired task (§6.2): retire settles a task that will not
// land, and the word reads apart from cancelled in the store, the
// history and the plan file.
const (
	taskStateCheck        = "CHECK (state IN ('pending', 'ready', 'running', 'checking', 'reviewing', 'done', 'needs_foreman', 'needs_user', 'failed', 'cancelled'))"
	taskStateCheckRetired = "CHECK (state IN ('pending', 'ready', 'running', 'checking', 'reviewing', 'done', 'needs_foreman', 'needs_user', 'failed', 'cancelled', 'retired'))"
)

// widenTaskStates lets the tasks table hold a retired task, by the same
// in-place edit 0058 and 0059 used for the plans table.
func widenTaskStates(ctx context.Context, tx *sql.Tx) error {
	return editTableCheck(ctx, tx, "tasks", taskStateCheck, taskStateCheckRetired)
}

// widenPlanReviewChecks lets the jobs table hold a plan_review job and
// the plans table hold a reviewing plan, by the same in-place edits.
// Each jobs edit replaces whichever wording the store holds: a store
// this binary migrated carries the earlier edits' (0023's, 0050's),
// while one whose earlier migrations were applied by their SQL alone —
// a test fixture, a jumped version — still holds 0001's original CHECK.
func widenPlanReviewChecks(ctx context.Context, tx *sql.Tx) error {
	if err := editTableCheckEither(ctx, tx, "jobs", []string{jobsKindCheckLand, jobsKindCheck}, jobsKindCheckPlanReview); err != nil {
		return err
	}
	if err := editTableCheckEither(ctx, tx, "jobs", []string{jobsPairingCheckRelaxed, jobsPairingCheck}, jobsPairingCheckPlanReview); err != nil {
		return err
	}
	return editTableCheck(ctx, tx, "plans", plansStateCheck, plansStateCheckReviewing)
}

// editTableCheckEither is editTableCheck over the wordings an earlier
// edit may or may not have left: the first found is replaced.
func editTableCheckEither(ctx context.Context, tx *sql.Tx, table string, froms []string, to string) error {
	for _, from := range froms {
		if err := editTableCheck(ctx, tx, table, from, to); err == nil {
			return nil
		}
	}
	return fmt.Errorf("edit the %s table: none of the earlier CHECK wordings was found", table)
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

// seedRoleInstructions is 0064's data half: every project gains one row
// per role. A project with an instructions setting keeps it as its
// worker text, with the setting's own provenance (imported when the
// last import set that value, else local). Every role the project has
// no text for is seeded with the embedded default, provenance
// "generated from <version>, by upgrade": source 'generated' with the
// defaults version in settings_version, so the exact default a project
// runs is pinned at the upgrade and stays even when later defaults
// change.
func seedRoleInstructions(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id, settings, settings_prov FROM projects")
	if err != nil {
		return fmt.Errorf("list the projects for the role seed: %w", err)
	}
	type proj struct {
		id   int64
		text string // the worker text, "" when none
		// imported provenance of the worker text, from settings_prov
		source, version string
	}
	var projs []proj
	for rows.Next() {
		var p proj
		var raw, prov string
		if err := rows.Scan(&p.id, &raw, &prov); err != nil {
			rows.Close()
			return fmt.Errorf("read a project for the role seed: %w", err)
		}
		row := map[string]json.RawMessage{}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &row); err != nil {
				rows.Close()
				return fmt.Errorf("parse the settings of project %d: %w", p.id, err)
			}
		}
		if cur, ok := row["instructions"]; ok {
			var s string
			if json.Unmarshal(cur, &s) == nil && s != "" {
				p.text = s
				p.source = "local"
				if v, ok := importedValue(prov, "instructions", cur); ok {
					p.source, p.version = "imported", v
				}
			}
		}
		projs = append(projs, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list the projects for the role seed: %w", err)
	}
	rows.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, p := range projs {
		for _, role := range roleNames {
			text, source, version := p.text, p.source, p.version
			if role != "worker" || source == "" {
				// Every role a project has no text for — the three
				// others always, the worker when no setting sets one —
				// is seeded from the embedded default.
				text, source, version = "", "generated", defaults.Version
				b, err := defaults.LiveRoleText(role)
				if err != nil {
					return fmt.Errorf("seed the %s role: %w", role, err)
				}
				text = b
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO role_instructions (project_id, role, text, source, settings_version, digest, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				p.id, role, text, source, version, defaults.Digest(text), now); err != nil {
				return fmt.Errorf("seed the %s role of project %d: %w", role, p.id, err)
			}
		}
	}
	return nil
}

// roleNames is the role set of role_instructions, in table-CHECK order.
var roleNames = []string{"foreman", "plan_review", "worker", "reviewer"}

// importedValue reports the settings version the last import set key
// to: the provenance's hash when its imported map holds exactly cur.
func importedValue(prov, key string, cur json.RawMessage) (string, bool) {
	if prov == "" {
		return "", false
	}
	var p struct {
		Hash     string                     `json:"hash"`
		Imported map[string]json.RawMessage `json:"imported"`
	}
	if err := json.Unmarshal([]byte(prov), &p); err != nil {
		return "", false
	}
	imp, ok := p.Imported[key]
	if !ok || p.Hash == "" {
		return "", false
	}
	var a, b bytes.Buffer
	if json.Compact(&a, imp) != nil || json.Compact(&b, cur) != nil {
		return "", false
	}
	return p.Hash, a.String() == b.String()
}
