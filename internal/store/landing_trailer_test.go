package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestOpenAddsLandingTrailerColumns builds the store a binary that stops
// at 0079 leaves behind, with a trailer landing row in it, and expects
// 0080 to add the nullable trailer columns with the old row intact and
// NULL in them: a store built from the prior migrations, opened by the
// new code, comes up correct.
func TestOpenAddsLandingTrailerColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &n); err != nil || n > 79 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s by hand: %v", e.Name(), err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 79"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	const seed = "'2026-11-20T10:00:00Z'"
	for _, stmt := range []string{
		`INSERT INTO projects (id, repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (1, 'https://example.invalid/brigada.git', 'main', '{}', 'claude', 'sonnet', ` + seed + `, ` + seed + `)`,
		`INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
			VALUES (1, 1, 'a plan', 'done', ` + seed + `, ` + seed + `)`,
		`INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, created_at, updated_at)
			VALUES (4, 1, 'a task', 'the spec', 'implement', 'done', 'main', ` + seed + `, ` + seed + `)`,
		`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, source, created_at)
			VALUES (4, 1, 'main', 'a7b61ba4', 'pushed', 'trailer', ` + seed + `)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v (%s)", err, stmt)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()

	rows, err := s.db.Query(`SELECT commit_sha, trailer_branch, trailer_head FROM landings WHERE task_id = 4`)
	if err != nil {
		t.Fatalf("read the landing: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the old landing row is gone")
	}
	var commit string
	var branch, head sql.NullString
	if err := rows.Scan(&commit, &branch, &head); err != nil {
		t.Fatalf("scan the landing: %v", err)
	}
	if commit != "a7b61ba4" {
		t.Errorf("commit_sha = %q, want the old row's a7b61ba4", commit)
	}
	if branch.Valid || head.Valid {
		t.Errorf("trailer columns on the old row = %q, %q, want NULL", branch.String, head.String)
	}
}
