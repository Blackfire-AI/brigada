package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// TestOpenReplacesExcludeAgentWithTheSet builds the store a binary that
// stops at 0080 leaves behind, with an in-flight review job carrying the
// single exclusion and a settled job carrying none, and expects 0081 to
// replace exclude_agent with the exclude_agents set: the in-flight
// exclusion backfilled as a one-element array, the empty set on every
// other row, the old column gone and the fix round's pin untouched: a
// store built from the prior migrations, opened by the new code, comes
// up correct.
func TestOpenReplacesExcludeAgentWithTheSet(t *testing.T) {
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
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &n); err != nil || n > 80 {
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
	if _, err := db.Exec("PRAGMA user_version = 80"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	const seed = "'2026-11-20T10:00:00Z'"
	for _, stmt := range []string{
		`INSERT INTO projects (id, repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (1, 'https://example.invalid/brigada.git', 'main', '{}', 'claude', 'sonnet', ` + seed + `, ` + seed + `)`,
		`INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
			VALUES (1, 1, 'a plan', 'active', ` + seed + `, ` + seed + `)`,
		`INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, created_at, updated_at)
			VALUES (4, 1, 'a task', 'the spec', 'implement', 'reviewing', 'main', ` + seed + `, ` + seed + `)`,
		`INSERT INTO runners (id, name, created_at, last_seen_at) VALUES (1, 'runner-1', ` + seed + `, ` + seed + `)`,
		`INSERT INTO agents (id, runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
			VALUES (7, 1, 'fake', 'first', 'first-1', 'google', 'google', '["review"]', ` + seed + `, ` + seed + `)`,
		`INSERT INTO jobs (id, project_id, task_id, kind, key, mode, prompt, status, exclude_agent, created_at, updated_at)
			VALUES (1, 1, 4, 'review', 'task:4', 'fresh', 'the review', 'queued', 7, ` + seed + `, ` + seed + `)`,
		`INSERT INTO jobs (id, project_id, task_id, kind, key, mode, prompt, status, require_agent, created_at, updated_at)
			VALUES (2, 1, 4, 'fix', 'task:4', 'session', 'the fix', 'done', 7, ` + seed + `, ` + seed + `)`,
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

	for _, tt := range []struct {
		job  int64
		want string
	}{
		{1, "[7]"}, // the in-flight exclusion, backfilled as a one-element set
		{2, "[]"},  // no exclusion: the empty set
	} {
		var got string
		if err := s.db.QueryRow("SELECT exclude_agents FROM jobs WHERE id = ?", tt.job).Scan(&got); err != nil {
			t.Fatalf("read the set of job %d: %v", tt.job, err)
		}
		if got != tt.want {
			t.Errorf("exclude_agents of job %d = %q, want %q", tt.job, got, tt.want)
		}
	}
	var pin int64
	if err := s.db.QueryRow("SELECT require_agent FROM jobs WHERE id = 2").Scan(&pin); err != nil || pin != 7 {
		t.Errorf("require_agent of the fix = %d (%v), want the pin untouched at 7", pin, err)
	}
	if _, err := s.db.Exec("SELECT exclude_agent FROM jobs"); err == nil {
		t.Error("the single exclude_agent column survived the migration")
	}
}
