package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ts is the fixed timestamp the tests write; the schema treats timestamps
// as opaque UTC text.
const ts = "2026-10-12T15:36:21Z"

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openMemory(t *testing.T) *Store {
	t.Helper()
	return openStore(t, ":memory:")
}

// openFile opens a store at a fresh path in a temp directory and returns
// the path, for the tests that reopen or inspect the file.
func openFile(t *testing.T) (string, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	return path, openStore(t, path)
}

// exec runs a statement that must succeed.
func exec(t *testing.T, db *sql.DB, query string, args ...any) sql.Result {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	return res
}

// insertID runs an insert that must succeed and returns the row's id.
func insertID(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("insert %q: %v", query, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insert %q: last insert id: %v", query, err)
	}
	return id
}

func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func queryString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s
}

func queryStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("query %q: scan: %v", query, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return out
}

// One valid row per table, so the constraint tests can break exactly one
// rule at a time.

func insertProject(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	return insertID(t, db, `INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES ('https://example.invalid/repo.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, ts, ts)
}

func insertPlan(t *testing.T, db *sql.DB, project int64) int64 {
	t.Helper()
	return insertID(t, db, `INSERT INTO plans (project_id, title, state, created_at, updated_at)
		VALUES (?, 'a plan', 'active', ?, ?)`, project, ts, ts)
}

func insertTask(t *testing.T, db *sql.DB, plan int64) int64 {
	t.Helper()
	return insertID(t, db, `INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
		VALUES (?, 'a task', 'the spec', 'go test ./...', 'implement', 'ready', 'main', '{}', ?, ?)`, plan, ts, ts)
}

func insertRunner(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	return insertID(t, db, `INSERT INTO runners (name, created_at, last_seen_at) VALUES ('a runner', ?, ?)`, ts, ts)
}

func insertAgent(t *testing.T, db *sql.DB, runner int64) int64 {
	t.Helper()
	return insertID(t, db, `INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, health, cooling_until, created_at, updated_at)
		VALUES (?, 'claude', 'claude', 'sonnet', 'anthropic', 'work', '["implement", "review"]', 'ready', NULL, ?, ?)`, runner, ts, ts)
}

// insertJob inserts a worker job; key and status are the test's to pick.
func insertJob(t *testing.T, db *sql.DB, project, task int64, key, status string) int64 {
	t.Helper()
	return insertID(t, db, `INSERT INTO jobs (project_id, task_id, kind, key, agent_id, mode, prompt, status, created_at, updated_at)
		VALUES (?, ?, 'work', ?, NULL, 'fresh', 'the prompt', ?, ?, ?)`, project, task, key, status, ts, ts)
}

// fixture is an in-memory store seeded with one valid chain of rows:
// project, plan, task, job, runner and agent.
type fixture struct {
	*Store
	project, plan, task, job, runner, agent int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{Store: openMemory(t)}
	f.project = insertProject(t, f.db)
	f.plan = insertPlan(t, f.db, f.project)
	f.task = insertTask(t, f.db, f.plan)
	f.runner = insertRunner(t, f.db)
	f.agent = insertAgent(t, f.db, f.runner)
	f.job = insertJob(t, f.db, f.project, f.task, fmt.Sprintf("task:%d", f.task), "queued")
	return f
}

func TestOpenCreatesEveryTable(t *testing.T) {
	path, s := openFile(t)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("open did not create the file: %v", err)
	}
	want := []string{
		"agents", "artifacts", "events", "jobs", "messages", "meta",
		"outcomes", "plans", "projects", "questions", "runners",
		"sessions", "task_deps", "tasks", "tokens",
	}
	got := queryStrings(t, s.db, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if !slices.Equal(got, want) {
		t.Errorf("tables after a fresh open:\n got %v\nwant %v", got, want)
	}
	triggers := queryStrings(t, s.db, "SELECT name FROM sqlite_master WHERE type = 'trigger' ORDER BY name")
	if want := []string{"events_no_delete", "events_no_update"}; !slices.Equal(triggers, want) {
		t.Errorf("triggers after a fresh open: got %v, want %v", triggers, want)
	}
}

func TestReopenUpToDateStoreIsNoOp(t *testing.T) {
	path, s := openFile(t)
	if v, err := s.userVersion(context.Background()); err != nil || v != 1 {
		t.Fatalf("user_version after the first open = %d, %v; want 1", v, err)
	}
	insertProject(t, s.db)
	s.Close()

	// Re-applying 0001 would fail on the existing tables, so a clean
	// reopen is itself the proof that nothing ran again.
	again, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if v, err := again.userVersion(context.Background()); err != nil || v != 1 {
		t.Errorf("user_version after reopen = %d, %v; want 1", v, err)
	}
	if n := queryInt(t, again.db, "SELECT COUNT(*) FROM projects"); n != 1 {
		t.Errorf("projects after reopen = %d, want 1", n)
	}
}

func TestNewerUserVersionIsRefused(t *testing.T) {
	path, s := openFile(t)
	exec(t, s.db, "PRAGMA user_version = 2")
	s.Close()

	reopened, err := Open(context.Background(), path)
	if err == nil {
		reopened.Close()
		t.Fatal("opening a store from a newer brigada succeeded; want refusal")
	}
	if !errors.Is(err, ErrTooNew) {
		t.Errorf("error = %v; want it to wrap ErrTooNew", err)
	}
	for _, want := range []string{"user_version 2", "latest known migration 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestOpenSetsPragmas(t *testing.T) {
	_, file := openFile(t)
	if got := queryString(t, file.db, "PRAGMA journal_mode"); got != "wal" {
		t.Errorf("file journal_mode = %q, want wal", got)
	}
	if got := queryInt(t, file.db, "PRAGMA foreign_keys"); got != 1 {
		t.Errorf("file foreign_keys = %d, want 1", got)
	}
	if got := queryInt(t, file.db, "PRAGMA busy_timeout"); got != int(busyTimeout.Milliseconds()) {
		t.Errorf("file busy_timeout = %d, want %d", got, int(busyTimeout.Milliseconds()))
	}

	// An in-memory database cannot use write-ahead logging; the pragma
	// is a no-op there, and the rest must still hold.
	mem := openMemory(t)
	if got := queryInt(t, mem.db, "PRAGMA foreign_keys"); got != 1 {
		t.Errorf("memory foreign_keys = %d, want 1", got)
	}
	if got := queryInt(t, mem.db, "PRAGMA busy_timeout"); got != int(busyTimeout.Milliseconds()) {
		t.Errorf("memory busy_timeout = %d, want %d", got, int(busyTimeout.Milliseconds()))
	}
}

func TestOpenRefusesQueryAndFragmentChars(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"store?.db", "store#.db"} {
		path := filepath.Join(dir, name)
		s, err := Open(context.Background(), path)
		if err == nil {
			s.Close()
			t.Errorf("open %q succeeded; want refusal", name)
			continue
		}
		if !strings.Contains(err.Error(), "'?' or '#'") {
			t.Errorf("open %q error = %v; want it to mention '?' and '#'", name, err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Errorf("open %q created the file despite refusing", name)
		}
	}

	// ":memory:" contains neither character and stays allowed.
	mem, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Errorf("open :memory:: %v; want it allowed", err)
	} else {
		mem.Close()
	}
}

func TestMemorySchemaSurvivesThePool(t *testing.T) {
	s := openMemory(t)
	insertProject(t, s.db)
	// Every query goes back through the pool; with more than one
	// connection each would hand out a fresh, empty database.
	for i := 0; i < 5; i++ {
		if n := queryInt(t, s.db, "SELECT COUNT(*) FROM projects"); n != 1 {
			t.Fatalf("round %d: projects = %d, want 1", i, n)
		}
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	f := newFixture(t)
	bad := f.project + 1000 // matches no row in any table
	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{"plan without its project",
			"INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (?, 'x', 'draft', ?, ?)",
			[]any{bad, ts, ts}},
		{"task without its plan",
			"INSERT INTO tasks (plan_id, title, spec, class, state, base_ref, caps, created_at, updated_at) VALUES (?, 'x', 's', 'implement', 'ready', 'main', '{}', ?, ?)",
			[]any{bad, ts, ts}},
		{"task_dep without its dependency",
			"INSERT INTO task_deps (task_id, dep_id) VALUES (?, ?)",
			[]any{f.task, bad}},
		{"job without its project",
			"INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at) VALUES (?, ?, 'work', 'fk-job-project', 'fresh', 'p', 'queued', ?, ?)",
			[]any{bad, f.task, ts, ts}},
		{"job without its task",
			"INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at) VALUES (?, ?, 'work', 'fk-job-task', 'fresh', 'p', 'queued', ?, ?)",
			[]any{f.project, bad, ts, ts}},
		{"job without its agent",
			"INSERT INTO jobs (project_id, task_id, kind, key, agent_id, mode, prompt, status, created_at, updated_at) VALUES (?, ?, 'work', 'fk-job-agent', ?, 'fresh', 'p', 'queued', ?, ?)",
			[]any{f.project, f.task, bad, ts, ts}},
		{"agent without its runner",
			"INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at) VALUES (?, 'a', 'h', 'm', 'fam', 'acct', '[]', ?, ?)",
			[]any{bad, ts, ts}},
		{"session without its agent",
			"INSERT INTO sessions (key, agent_id, state, created_at) VALUES ('fk-session', ?, 'active', ?)",
			[]any{bad, ts}},
		{"outcome without its project",
			"INSERT INTO outcomes (project_id, agent_id, class, updated_at) VALUES (?, ?, 'implement', ?)",
			[]any{bad, f.agent, ts}},
		{"outcome without its agent",
			"INSERT INTO outcomes (project_id, agent_id, class, updated_at) VALUES (?, ?, 'implement', ?)",
			[]any{f.project, bad, ts}},
		{"event without its job",
			"INSERT INTO events (kind, project_id, task_id, job_id, created_at) VALUES ('kind', ?, ?, ?, ?)",
			[]any{f.project, f.task, bad, ts}},
		{"message without its project",
			"INSERT INTO messages (project_id, kind, author, content, created_at) VALUES (?, 'chat', 'user', 'hi', ?)",
			[]any{bad, ts}},
		{"message without its event",
			"INSERT INTO messages (project_id, event_id, kind, author, content, created_at) VALUES (?, ?, 'chat', 'user', 'hi', ?)",
			[]any{f.project, bad, ts}},
		{"question without its task",
			"INSERT INTO questions (project_id, task_id, question, created_at) VALUES (?, ?, 'why', ?)",
			[]any{f.project, bad, ts}},
		{"token without its project",
			"INSERT INTO tokens (role, token_hash, project_id, created_at) VALUES ('foreman', 'hash', ?, ?)",
			[]any{bad, ts}},
		{"artifact without its job",
			"INSERT INTO artifacts (job_id, kind, path, size, sha256, created_at) VALUES (?, 'transcript', 'p', 1, 'h', ?)",
			[]any{bad, ts}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.db.Exec(c.query, c.args...)
			if err == nil {
				t.Fatal("insert succeeded; want a foreign key failure")
			}
			if !strings.Contains(err.Error(), "FOREIGN KEY") {
				t.Errorf("error = %v; want a foreign key failure", err)
			}
		})
	}
}

func TestCheckConstraintsRejectBadValues(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{"job kind",
			"INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at) VALUES (?, ?, 'bogus', 'check-kind', 'fresh', 'p', 'queued', ?, ?)",
			[]any{f.project, f.task, ts, ts}},
		{"job mode",
			"INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at) VALUES (?, ?, 'work', 'check-mode', 'bogus', 'p', 'queued', ?, ?)",
			[]any{f.project, f.task, ts, ts}},
		{"job status",
			"INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at) VALUES (?, ?, 'work', 'check-status', 'fresh', 'p', 'bogus', ?, ?)",
			[]any{f.project, f.task, ts, ts}},
		{"plan state",
			"INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (?, 'x', 'bogus', ?, ?)",
			[]any{f.project, ts, ts}},
		{"task state",
			"INSERT INTO tasks (plan_id, title, spec, class, state, base_ref, caps, created_at, updated_at) VALUES (?, 'x', 's', 'implement', 'bogus', 'main', '{}', ?, ?)",
			[]any{f.plan, ts, ts}},
		{"session state",
			"INSERT INTO sessions (key, state, created_at) VALUES ('check-session', 'bogus', ?)",
			[]any{ts}},
		{"agent health",
			"INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, health, created_at, updated_at) VALUES (?, 'a', 'h', 'm', 'fam', 'acct', '[]', 'bogus', ?, ?)",
			[]any{f.runner, ts, ts}},
		{"artifact kind",
			"INSERT INTO artifacts (job_id, kind, path, size, sha256, created_at) VALUES (?, 'bogus', 'p', 1, 'h', ?)",
			[]any{f.job, ts}},
		{"token role",
			"INSERT INTO tokens (role, token_hash, project_id, created_at) VALUES ('bogus', 'hash', NULL, ?)",
			[]any{ts}},
		{"message author",
			"INSERT INTO messages (project_id, kind, author, content, created_at) VALUES (?, 'chat', 'bogus', 'hi', ?)",
			[]any{f.project, ts}},
		{"second meta row",
			"INSERT INTO meta (id, fence_epoch) VALUES (2, 1)",
			nil},
		{"meta fence epoch below one",
			"UPDATE meta SET fence_epoch = 0",
			nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.db.Exec(c.query, c.args...)
			if err == nil {
				t.Fatal("statement succeeded; want a CHECK failure")
			}
			if !strings.Contains(err.Error(), "CHECK") {
				t.Errorf("error = %v; want a CHECK failure", err)
			}
		})
	}
}

func TestJobTaskPresenceFollowsKind(t *testing.T) {
	f := newFixture(t)

	// A foreman turn belongs to no task: giving it one is refused.
	if _, err := f.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, ?, 'foreman', 'foreman-with-task', 'session', 'p', 'queued', ?, ?)`, f.project, f.task, ts, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("foreman job with a task: err = %v; want a CHECK failure", err)
	}

	// A worker job always belongs to a task: leaving it out is refused,
	// for every worker kind.
	for _, kind := range []string{"work", "fix", "review"} {
		if _, err := f.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
			VALUES (?, NULL, ?, ?, 'fresh', 'p', 'queued', ?, ?)`, f.project, kind, fmt.Sprintf("%s-without-task", kind), ts, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s job without a task: err = %v; want a CHECK failure", kind, err)
		}
	}

	// The matched pairs pass: a foreman turn with no task (the fixture's
	// work job already covers a worker with one).
	exec(t, f.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'foreman', ?, 'session', 'wake up', 'queued', ?, ?)`, f.project, fmt.Sprintf("foreman:%d", f.project), ts, ts)
	if n := queryInt(t, f.db, "SELECT COUNT(*) FROM jobs WHERE kind = 'foreman'"); n != 1 {
		t.Errorf("foreman jobs = %d, want 1", n)
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	f := newFixture(t)
	id := insertID(t, f.db, `INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
		VALUES ('task_done', ?, ?, ?, '{"status": "done"}', ?)`, f.project, f.task, f.job, ts)

	if _, err := f.db.Exec("UPDATE events SET kind = 'x' WHERE id = ?", id); err == nil {
		t.Error("UPDATE on events succeeded; want refusal")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE error = %v; want the append-only refusal", err)
	}
	if _, err := f.db.Exec("DELETE FROM events WHERE id = ?", id); err == nil {
		t.Error("DELETE on events succeeded; want refusal")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE error = %v; want the append-only refusal", err)
	}
	if n := queryInt(t, f.db, "SELECT COUNT(*) FROM events"); n != 1 {
		t.Errorf("events after the attempts = %d, want 1", n)
	}
}

func TestAtMostOneLiveJobPerKey(t *testing.T) {
	f := newFixture(t)
	key := fmt.Sprintf("task:%d", f.task)

	// A second live job for the key is refused, whatever live state it
	// would be in.
	for _, status := range []string{"queued", "leased", "running"} {
		_, err := f.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
			VALUES (?, ?, 'fix', ?, 'session', 'p', ?, ?, ?)`, f.project, f.task, key, status, ts, ts)
		if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			t.Errorf("second live job with status %s: err = %v; want a UNIQUE failure", status, err)
		}
	}

	// A finished job frees the key for a new live one (§14: the fix and
	// review rounds follow the work job on the same key).
	exec(t, f.db, "UPDATE jobs SET status = 'done' WHERE id = ?", f.job)
	live := insertJob(t, f.db, f.project, f.task, key, "queued")

	// Which blocks the key again, and a stalled job leaves it free once
	// more: stalled is terminal (§5.6).
	if _, err := f.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, ?, 'fix', ?, 'session', 'p', 'leased', ?, ?)`, f.project, f.task, key, ts, ts); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("third live job while one is live: err = %v; want a UNIQUE failure", err)
	}
	exec(t, f.db, "UPDATE jobs SET status = 'stalled' WHERE id = ?", live)
	insertJob(t, f.db, f.project, f.task, key, "running")

	// A different key may be live at the same time: the foreman turn of
	// the project, which has no task (§4.2).
	exec(t, f.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'foreman', ?, 'session', 'wake up', 'queued', ?, ?)`, f.project, fmt.Sprintf("foreman:%d", f.project), ts, ts)
	if n := queryInt(t, f.db, "SELECT COUNT(*) FROM jobs WHERE status IN ('queued', 'leased', 'running')"); n != 2 {
		t.Errorf("live jobs = %d, want 2 (one per key)", n)
	}
}

func TestTaskDeps(t *testing.T) {
	f := newFixture(t)
	other := insertTask(t, f.db, f.plan)

	if _, err := f.db.Exec("INSERT INTO task_deps (task_id, dep_id) VALUES (?, ?)", f.task, f.task); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("self-edge: err = %v; want a CHECK failure", err)
	}
	exec(t, f.db, "INSERT INTO task_deps (task_id, dep_id) VALUES (?, ?)", f.task, other)
	if _, err := f.db.Exec("INSERT INTO task_deps (task_id, dep_id) VALUES (?, ?)", f.task, other); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("duplicate edge: err = %v; want a UNIQUE failure", err)
	}
	// The reverse edge is a distinct edge, not a duplicate.
	exec(t, f.db, "INSERT INTO task_deps (task_id, dep_id) VALUES (?, ?)", other, f.task)
	if n := queryInt(t, f.db, "SELECT COUNT(*) FROM task_deps"); n != 2 {
		t.Errorf("task_deps rows = %d, want 2", n)
	}
}

func TestMetaFenceEpochStartsAtOne(t *testing.T) {
	f := newFixture(t)
	if got := queryInt(t, f.db, "SELECT fence_epoch FROM meta WHERE id = 1"); got != 1 {
		t.Errorf("fence_epoch = %d, want 1", got)
	}
	if n := queryInt(t, f.db, "SELECT COUNT(*) FROM meta"); n != 1 {
		t.Errorf("meta rows = %d, want the single row", n)
	}
}
