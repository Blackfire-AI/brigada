package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/defaults"
)

// buildPreRoleInstructions opens a raw store holding every migration
// before role_instructions, with user_version set to match, at a fresh
// path the caller seeds old data into.
func buildPreRoleInstructions(t *testing.T) (string, *sql.DB) {
	t.Helper()
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	before := 0
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
	return path, db
}

// TestRoleInstructionsMigrationSeedsOldData builds the store of a binary
// that predates the role_instructions migration — three projects: one
// with a locally set instructions setting, one whose instructions the
// last import set, one with none — and opens it with this binary. Every
// project gains all four roles; a project's instructions setting is
// kept as its worker text with its provenance; every role with no text
// is seeded from the embedded default, provenance generated from the
// defaults version, by upgrade.
func TestRoleInstructionsMigrationSeedsOldData(t *testing.T) {
	path, db := buildPreRoleInstructions(t)
	ins := func(repo, settings, prov string) int64 {
		return insertID(t, db, `INSERT INTO projects (repo_url, default_branch, settings, settings_prov, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (?, 'main', ?, ?, 'claude', 'sonnet', ?, ?)`, repo, settings, prov, ts, ts)
	}
	local := ins("https://example.invalid/local.git", `{"instructions":"local rules"}`, "")
	imported := ins("https://example.invalid/imported.git", `{"instructions":"file rules"}`, `{"hash":"abc123","imported":{"instructions":"file rules"}}`)
	plain := ins("https://example.invalid/plain.git", `{}`, "")
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	if v, err := s.userVersion(context.Background()); err != nil || v != len(migs) {
		t.Errorf("user_version after upgrade = %d, %v; want the newest %d", v, err, len(migs))
	}
	if n := queryInt(t, s.db, "SELECT COUNT(*) FROM role_instructions"); n != 12 {
		t.Errorf("role_instructions holds %d rows, want 4 roles x 3 projects", n)
	}
	row := func(project int64, role string) (text, source, version, digest string) {
		t.Helper()
		if err := s.db.QueryRow("SELECT text, source, settings_version, digest FROM role_instructions WHERE project_id = ? AND role = ?", project, role).
			Scan(&text, &source, &version, &digest); err != nil {
			t.Fatalf("read %s of project %d: %v", role, project, err)
		}
		return
	}
	if text, source, version, digest := row(local, "worker"); text != "local rules" || source != "local" || version != "" || digest != defaults.Digest("local rules") {
		t.Errorf("local worker row = (%q, %q, %q, %q), want the kept setting as local text", text, source, version, digest)
	}
	if text, source, version, digest := row(imported, "worker"); text != "file rules" || source != "imported" || version != "abc123" || digest != defaults.Digest("file rules") {
		t.Errorf("imported worker row = (%q, %q, %q, %q), want the setting kept with its import hash", text, source, version, digest)
	}
	wantWorker, err := defaults.LiveRoleText("worker")
	if err != nil {
		t.Fatalf("live worker default: %v", err)
	}
	if text, source, version, digest := row(plain, "worker"); text != wantWorker || source != "generated" || version != defaults.Version || digest != defaults.Digest(wantWorker) {
		t.Errorf("plain worker row = (%d bytes, %q, %q, %q), want the embedded default generated from %s", len(text), source, version, digest, defaults.Version)
	}
	wantForeman, err := defaults.LiveRoleText("foreman")
	if err != nil {
		t.Fatalf("live foreman default: %v", err)
	}
	for _, project := range []int64{local, imported, plain} {
		if text, source, version, digest := row(project, "foreman"); text != wantForeman || source != "generated" || version != defaults.Version || digest != defaults.Digest(wantForeman) {
			t.Errorf("foreman row of project %d = (%d bytes, %q, %q, %q), want the embedded default generated from %s", project, len(text), source, version, digest, defaults.Version)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO role_instructions (project_id, role, text, source, settings_version, digest, updated_at)
		VALUES (?, 'boss', '', 'local', '', '', ?)`, plain, ts); err == nil {
		t.Errorf("an unknown role was accepted")
	}
	if _, err := s.db.Exec(`INSERT INTO role_instructions (project_id, role, text, source, settings_version, digest, updated_at)
		VALUES (?, 'reviewer', '', 'magic', '', '', ?)`, plain, ts); err == nil {
		t.Errorf("an unknown source was accepted")
	}
}
