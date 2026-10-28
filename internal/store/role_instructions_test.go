package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/defaults"
)

// roleRow reads one role_instructions row.
func roleRow(t *testing.T, db *sql.DB, project int64, role string) (text, source, settingsVersion, digest string) {
	t.Helper()
	if err := db.QueryRow("SELECT text, source, settings_version, digest FROM role_instructions WHERE project_id = ? AND role = ?", project, role).
		Scan(&text, &source, &settingsVersion, &digest); err != nil {
		t.Fatalf("read the %s text of project %d: %v", role, project, err)
	}
	return
}

// TestRoleInstructionsMigrationSeeds builds the store of a binary that
// predates the role_instructions migration, with three projects — one
// whose instructions setting holds a local text, one whose holds a text
// the last import set, one without — and opens it with this binary:
// every project gains all four per-role texts. A role with no text
// takes the embedded default, provenance 'generated from <version>, by
// upgrade'; the instructed projects keep their setting as the worker
// text with its provenance (the imported one with its settings
// version) and gain the other three.
func TestRoleInstructionsMigrationSeeds(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var before int
	for _, m := range migs {
		if m.name == "role_instructions" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("role_instructions migration not found in allMigrations")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	for _, m := range migs[:before] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply %04d_%s by hand: %v", m.version, m.name, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(before)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	clean := insertProject(t, db)
	local := insertID(t, db, `INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES ('https://example.invalid/other.git', 'main', ?, 'claude', 'sonnet', ?, ?)`,
		`{"instructions":"private project guidance"}`, ts, ts)
	imported := insertID(t, db, `INSERT INTO projects (repo_url, default_branch, settings, settings_prov, foreman_harness, foreman_model, created_at, updated_at)
		VALUES ('https://example.invalid/shared.git', 'main', ?, ?, 'claude', 'sonnet', ?, ?)`,
		`{"instructions":"shared rules"}`, `{"hash":"imp0rthash","imported":{"instructions":"shared rules"}}`, ts, ts)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM role_instructions"); got != 12 {
		t.Fatalf("rows after upgrade = %d, want 4 roles x 3 projects", got)
	}
	generated := defaults.GeneratedProvenance(defaults.Version)
	for _, role := range defaults.Roles {
		want, err := defaults.RoleText(role)
		if err != nil {
			t.Fatal(err)
		}
		text, source, version, digest := roleRow(t, s.db, clean, role)
		if text != want || source != generated || digest != defaults.Digest(want) {
			t.Errorf("clean project %s = %d bytes, %q, %q; want the generated default", role, len(text), source, digest)
		}
		if role == "worker" {
			// The local setting stays the worker text with its provenance.
			text, source, version, digest = roleRow(t, s.db, local, role)
			if text != "private project guidance" || source != "local" || version != "" || digest != defaults.Digest(text) {
				t.Errorf("local project worker = %d bytes, %q, %q, %q; want the setting kept with its provenance", len(text), source, version, digest)
			}
			// The imported one keeps the import's source and settings version.
			text, source, version, digest = roleRow(t, s.db, imported, role)
			if text != "shared rules" || source != "imported" || version != "imp0rthash" || digest != defaults.Digest(text) {
				t.Errorf("imported project worker = %d bytes, %q, %q, %q; want the setting kept with its import provenance", len(text), source, version, digest)
			}
			continue
		}
		text, source, version, digest = roleRow(t, s.db, local, role)
		if text != want || source != generated || digest != defaults.Digest(want) {
			t.Errorf("local project %s = %d bytes, %q, %q; want the generated default", role, len(text), source, digest)
		}
		text, source, version, digest = roleRow(t, s.db, imported, role)
		if text != want || source != generated || digest != defaults.Digest(want) {
			t.Errorf("imported project %s = %d bytes, %q, %q; want the generated default", role, len(text), source, digest)
		}
	}
}

// TestRoleInstructionsSchema is the shape the engine reads: one row per
// project and role, the role set pinned, a project deleted with its
// rows.
func TestRoleInstructionsSchema(t *testing.T) {
	s := openMemory(t)
	project := insertProject(t, s.db)
	exec(t, s.db, `INSERT INTO role_instructions (project_id, role, text, source, digest, updated_at)
		VALUES (?, 'foreman', 'f', 'generated from 1.0.0, by upgrade', 'd', ?)`, project, ts)
	if _, err := s.db.Exec(`INSERT INTO role_instructions (project_id, role, text, source, digest, updated_at)
		VALUES (?, 'chef', 'c', 's', 'd', ?)`, project, ts); err == nil {
		t.Errorf("a role outside the set was accepted")
	}
	if _, err := s.db.Exec(`INSERT INTO role_instructions (project_id, role, text, source, digest, updated_at)
		VALUES (?, 'foreman', 'again', 's', 'd', ?)`, project, ts); err == nil {
		t.Errorf("a second row for one role was accepted")
	}
	exec(t, s.db, "DELETE FROM projects WHERE id = ?", project)
	if got := queryInt(t, s.db, "SELECT COUNT(*) FROM role_instructions"); got != 0 {
		t.Errorf("rows after the project died = %d, want the cascade", got)
	}
}
