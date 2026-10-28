package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/defaults"
)

// openBeforeRoleInstructions builds, at a path of its own, the store a
// binary that predates 0064 would leave behind: every earlier migration
// applied, user_version set to the one before it. Old data is the
// caller's to insert before the returned handle is closed and reopened.
func openBeforeRoleInstructions(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return openBeforeMigration(t, "role_instructions")
}

// openBeforeMigration builds the store a binary that predates the named
// migration would leave behind, as openBeforeRoleInstructions does.
func openBeforeMigration(t *testing.T, name string) (string, *sql.DB) {
	t.Helper()
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var before int
	for _, m := range migs {
		if m.name == name {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatalf("%s migration not found in allMigrations", name)
	}
	path := t.TempDir() + "/store.db"
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	for _, m := range migs[:before] {
		exec(t, db, m.sql)
	}
	exec(t, db, "PRAGMA user_version = "+strconv.Itoa(before))
	return path, db
}

// insertOldProject adds a project with the given settings and
// provenance, the shape the role-instructions migration reads.
func insertOldProject(t *testing.T, db *sql.DB, settings, prov string) int64 {
	t.Helper()
	return insertID(t, db, `INSERT INTO projects (repo_url, default_branch, settings, settings_prov, foreman_harness, foreman_model, created_at, updated_at)
		VALUES (?, 'main', ?, ?, 'claude', 'sonnet', ?, ?)`,
		fmt.Sprintf("https://example.invalid/p%d.git", queryInt(t, db, "SELECT COUNT(*) FROM projects")+1), settings, prov, ts, ts)
}

func roleRow(t *testing.T, db *sql.DB, project int64, role string) (string, string) {
	t.Helper()
	var text, meta string
	if err := db.QueryRow("SELECT text, meta FROM role_instructions WHERE project_id = ? AND role = ?", project, role).Scan(&text, &meta); err != nil {
		t.Fatalf("read the %s row of project %d: %v", role, project, err)
	}
	return text, meta
}

// TestRoleInstructionsMigrationSeedsOldData: opening a pre-0064 store
// seeds every project's four role rows. A project with an instructions
// setting keeps its text as the worker row, with the setting's
// provenance carried over (a local override or the import that set it);
// every role with no text gets the embedded default, recorded as
// 'generated from <version>, by upgrade'.
func TestRoleInstructionsMigrationSeedsOldData(t *testing.T) {
	path, db := openBeforeRoleInstructions(t)
	local := insertOldProject(t, db, `{"instructions":"local rules"}`, "")
	imported := insertOldProject(t, db, `{"instructions":"file rules"}`, `{"hash":"cafe01","imported":{"instructions":"file rules"}}`)
	empty := insertOldProject(t, db, `{"approval_max_tasks":-1}`, "")
	none := insertOldProject(t, db, `{}`, "")
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	seeded := RoleSourceSeededPrefix + defaults.Version + ", by upgrade"
	for _, project := range []struct {
		id         int64
		workerText string
		workerMeta string
	}{
		{local, "local rules", `{"source":"local","digest":"` + defaults.Digest("local rules") + `","bytes":11}`},
		{imported, "file rules", `{"source":"imported","version":"cafe01","digest":"` + defaults.Digest("file rules") + `","bytes":10}`},
	} {
		text, meta := roleRow(t, s.db, project.id, "worker")
		if text != project.workerText {
			t.Errorf("project %d: worker text = %q, want the setting's %q", project.id, text, project.workerText)
		}
		if meta != project.workerMeta {
			t.Errorf("project %d: worker meta = %s, want %s", project.id, meta, project.workerMeta)
		}
	}
	for _, project := range []int64{local, imported, empty, none} {
		if n := queryInt(t, s.db, "SELECT COUNT(*) FROM role_instructions WHERE project_id = ?", project); n != 4 {
			t.Errorf("project %d: role rows = %d, want one per role (4)", project, n)
		}
		for _, role := range defaults.Roles {
			if role == "worker" && project <= imported {
				continue // the two above keep their setting's text
			}
			want, err := defaults.LiveRoleText(role)
			if err != nil {
				t.Fatal(err)
			}
			text, meta := roleRow(t, s.db, project, role)
			if text != want {
				t.Errorf("project %d, role %s: seeded text is not the default", project, role)
			}
			wantMeta := fmt.Sprintf(`{"source":%q,"version":%q,"digest":%q,"bytes":%d}`,
				seeded, defaults.Version, defaults.Digest(want), len(want))
			if meta != wantMeta {
				t.Errorf("project %d, role %s: seeded meta = %s, want %s", project, role, meta, wantMeta)
			}
		}
	}
	// A reopen re-runs nothing: the rows stand at one per role.
	s.Close()
	again := openStore(t, path)
	if n := queryInt(t, again.db, "SELECT COUNT(*) FROM role_instructions"); n != 16 {
		t.Errorf("role rows after reopen = %d, want the 16 seeded (4 projects x 4 roles)", n)
	}
}

// TestRoleInstructionsTableRules: the primary key refuses a second row
// for one role, the role CHECK admits exactly the four roles, and the
// project reference is enforced.
func TestRoleInstructionsTableRules(t *testing.T) {
	f := newFixture(t)
	exec(t, f.db, `INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, 'worker', 'w', '')`, f.project)
	if _, err := f.db.Exec(`INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, 'worker', 'again', '')`, f.project); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("second worker row: err = %v, want a UNIQUE failure", err)
	}
	if _, err := f.db.Exec(`INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, 'operator', 'o', '')`, f.project); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("unknown role: err = %v, want a CHECK failure", err)
	}
	if _, err := f.db.Exec(`INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, 'reviewer', 'r', '')`, f.project+1000); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("row without its project: err = %v, want a FOREIGN KEY failure", err)
	}
	if n := queryInt(t, f.db, "SELECT COUNT(*) FROM role_instructions"); n != 1 {
		t.Errorf("role rows = %d, want 1", n)
	}
	// A project this binary created gets no rows: the seed ran at the
	// migration, on the projects that existed then.
	fresh := insertID(t, f.db, `INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES ('https://example.invalid/fresh.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, ts, ts)
	if n := queryInt(t, f.db, "SELECT COUNT(*) FROM role_instructions WHERE project_id = ?", fresh); n != 0 {
		t.Errorf("a new project was seeded %d rows; seeding belongs to the migration", n)
	}
}
