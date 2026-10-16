package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
		"agents", "artifacts", "attach_holds", "events", "foreman_inbox", "hold_tokens",
		"idempotency", "jobs", "lease_tokens", "messages", "meta", "outcomes", "plan_mirror_dirty", "plans",
		"projects", "questions", "runners", "sessions", "task_deps", "tasks", "tokens",
	}
	got := queryStrings(t, s.db, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if !slices.Equal(got, want) {
		t.Errorf("tables after a fresh open:\n got %v\nwant %v", got, want)
	}
	triggers := queryStrings(t, s.db, "SELECT name FROM sqlite_master WHERE type = 'trigger' ORDER BY name")
	if want := []string{"events_no_delete", "events_no_update", "plan_mirror_dep_insert", "plan_mirror_plan_insert",
		"plan_mirror_plan_update", "plan_mirror_task_insert", "plan_mirror_task_update"}; !slices.Equal(triggers, want) {
		t.Errorf("triggers after a fresh open: got %v, want %v", triggers, want)
	}
}

// schemaVersion is the version this binary's migrations reach, derived
// from the migration list so the next migration doesn't break these tests.
func schemaVersion(t *testing.T) int {
	t.Helper()
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(migs) == 0 || migs[len(migs)-1].version != len(migs) {
		t.Fatalf("migrations are not numbered 1..%d without gaps", len(migs))
	}
	return len(migs)
}

func TestReopenUpToDateStoreIsNoOp(t *testing.T) {
	path, s := openFile(t)
	want := schemaVersion(t)
	if v, err := s.userVersion(context.Background()); err != nil || v != want {
		t.Fatalf("user_version after the first open = %d, %v; want %d", v, err, want)
	}
	insertProject(t, s.db)
	s.Close()

	// Re-applying a migration would fail on the existing tables, so a
	// clean reopen is itself the proof that nothing ran again.
	again, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if v, err := again.userVersion(context.Background()); err != nil || v != want {
		t.Errorf("user_version after reopen = %d, %v; want %d", v, err, want)
	}
	if n := queryInt(t, again.db, "SELECT COUNT(*) FROM projects"); n != 1 {
		t.Errorf("projects after reopen = %d, want 1", n)
	}
}

// TestOpenUpgradesAV1Store builds the store a binary that only knows
// 0001 would leave behind, then opens it with this binary and expects
// the later migrations to land without touching the existing rows.
func TestOpenUpgradesAV1Store(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	v1, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read 0001: %v", err)
	}
	if _, err := db.Exec(string(v1)); err != nil {
		t.Fatalf("apply 0001 by hand: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	project := insertProject(t, db)
	plan := insertPlan(t, db, project)
	task := insertTask(t, db, plan)
	job := insertJob(t, db, project, task, fmt.Sprintf("task:%d", task), "done")
	runner := insertRunner(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	if v, err := s.userVersion(context.Background()); err != nil || v != schemaVersion(t) {
		t.Errorf("user_version after upgrade = %d, %v; want %d", v, err, schemaVersion(t))
	}
	if got := queryStrings(t, s.db, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'idempotency'"); len(got) != 1 {
		t.Errorf("idempotency table after upgrade = %v, want it created", got)
	}
	got := queryStrings(t, s.db, "SELECT name FROM pragma_table_info('jobs')")
	for _, want := range []string{
		"first_leased", "last_leased", "queued_at", "lease_epoch",
		"leased_seconds", "harness", "labels", "restart",
		"released_by", "released_until", "release_note",
		"released_lease", "done_lease", "cap_held_at", "cap_held_layer", "cap_held_key",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("jobs after upgrade has no %s column", want)
		}
	}
	if cols := queryStrings(t, s.db, "SELECT name FROM pragma_table_info('runners')"); !slices.Contains(cols, "max_parallel") {
		t.Errorf("runners after upgrade has no max_parallel column: %v", cols)
	}
	if n := queryInt(t, s.db, "SELECT max_parallel FROM runners WHERE id = ?", runner); n != 0 {
		t.Errorf("v1 runner max_parallel = %d, want the 0 default", n)
	}
	if status := queryString(t, s.db, "SELECT status FROM jobs WHERE id = ?", job); status != "done" {
		t.Errorf("v1 job after upgrade = %q, want done untouched", status)
	}
	if n := queryInt(t, s.db, "SELECT leased_seconds FROM jobs WHERE id = ?", job); n != 0 {
		t.Errorf("v1 job leased_seconds = %d, want the 0 default", n)
	}
}

// TestNotifyCursorBackfillOnUpgrade builds the store a binary that
// predates the notify_cursor migration would leave behind — every
// migration before it, with a log full of events nobody was ever
// notified about — then opens it with this binary. The cursor migration
// must start the cursor at the log's end, not at 0: at 0, the first
// engine start after the upgrade would replay the whole history as
// notification banners (§10 promises a restart never re-notifies old
// events). The boundary is derived from the migration's position in
// the list, not hard-coded numbers, so renumbering — the placeholder
// swap at merge included — cannot silently date the test.
func TestNotifyCursorBackfillOnUpgrade(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var before, after int
	for _, m := range migs {
		if m.name == "notify_cursor" {
			before, after = m.version-1, m.version
			break
		}
	}
	if after == 0 {
		t.Fatal("notify_cursor migration not found in allMigrations")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	// The versions are gapless 1..N, so the versions before the cursor
	// migration are exactly the first `before` entries.
	for _, m := range migs[:before] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply %04d_%s by hand: %v", m.version, m.name, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(before)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	// A history of the kinds that notify: two parkings and a plan done.
	for _, kind := range []string{"task_check_failed", "plan_done", "task_needs_help"} {
		exec(t, db, `INSERT INTO events (kind, payload, created_at) VALUES (?, '{}', ?)`, kind, ts)
	}
	maxID := queryInt(t, db, "SELECT MAX(id) FROM events")
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	// Open applies every migration from the cursor's on, so the store
	// ends at the newest version, whichever migrations follow the cursor.
	s := openStore(t, path)
	if v, err := s.userVersion(context.Background()); err != nil || v != len(migs) {
		t.Errorf("user_version after upgrade = %d, %v; want the newest %d", v, err, len(migs))
	}
	if got := queryInt(t, s.db, "SELECT notify_cursor FROM meta WHERE id = 1"); got != maxID {
		t.Errorf("notify_cursor after upgrade = %d, want the log's end %d", got, maxID)
	}
}

// TestEffortMigrationKeepsTheRows builds the store of a binary that
// predates the effort migration — a project, a runner and an agent — and
// opens it with this binary: both new columns exist, default to unset and
// the old rows keep their values.
func TestEffortMigrationKeepsTheRows(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var before int
	for _, m := range migs {
		if m.name == "effort" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("effort migration not found in allMigrations")
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
	insertProject(t, db)
	runner := insertID(t, db, "INSERT INTO runners (name, created_at, last_seen_at) VALUES ('r', ?, ?)", ts, ts)
	exec(t, db, `INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
		VALUES (?, 'claude', 'claude', 'opus', 'claude', 'claude', '[]', ?, ?)`, runner, ts, ts)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	if got := queryString(t, s.db, "SELECT foreman_effort FROM projects"); got != "" {
		t.Errorf("foreman_effort of an old project = %q, want unset", got)
	}
	if got := queryString(t, s.db, "SELECT effort FROM agents"); got != "" {
		t.Errorf("effort of an old agent = %q, want unset", got)
	}
	if got := queryString(t, s.db, "SELECT model FROM agents"); got != "opus" {
		t.Errorf("model of the old agent = %q, want opus", got)
	}
	exec(t, s.db, "UPDATE projects SET foreman_effort = 'high'")
	exec(t, s.db, "UPDATE agents SET effort = 'max'")
}

// TestJobModelMigrationBackfillsLiveForemanTurns builds the store of a
// binary that predates the job-model migration, with foreman turns in each
// state, and opens it with this binary: a live turn on the project's own
// harness (or on none) takes the project's model; a live turn on another
// harness has no recoverable model and keeps none, so the engine holds it;
// a finished turn is left alone.
func TestJobModelMigrationBackfillsLiveForemanTurns(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var before int
	for _, m := range migs {
		if m.name == "job_model" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("job_model migration not found in allMigrations")
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
	project := insertProject(t, db) // claude/sonnet
	turn := func(key, status, harness string) int64 {
		return insertID(t, db, `INSERT INTO jobs (project_id, kind, key, mode, prompt, status, harness, created_at, updated_at)
			VALUES (?, 'foreman', ?, 'session', 'p', ?, ?, ?, ?)`, project, key, status, harness, ts, ts)
	}
	own := turn("foreman:own", "queued", "claude")
	none := turn("foreman:none", "queued", "")
	leased := turn("foreman:leased", "leased", "claude")
	other := turn("foreman:other", "queued", "codex")
	finished := turn("foreman:finished", "done", "claude")
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	for _, tc := range []struct {
		name string
		job  int64
		want string
	}{
		{"queued on the project's harness", own, "sonnet"},
		{"queued with no harness", none, "sonnet"},
		{"leased on the project's harness", leased, "sonnet"},
		{"queued on another harness", other, ""},
		{"finished", finished, ""},
	} {
		if got := queryString(t, s.db, "SELECT model FROM jobs WHERE id = ?", tc.job); got != tc.want {
			t.Errorf("%s: model = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestReviewWithoutTaskMigrationKeepsTheRows builds the store of a
// binary that predates migration 0023 — a job with an artifact and an
// event hanging off it — and opens it with this binary: the CHECK edit
// must not cascade into the jobs' dependents, and a pooled connection
// must see the relaxed rule at once.
func TestReviewWithoutTaskMigrationKeepsTheRows(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var before int
	for _, m := range migs {
		if m.name == "review_without_task" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("review_without_task migration not found in allMigrations")
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
	project := insertProject(t, db)
	plan := insertPlan(t, db, project)
	task := insertTask(t, db, plan)
	job := insertJob(t, db, project, task, fmt.Sprintf("task:%d", task), "done")
	exec(t, db, "INSERT INTO artifacts (job_id, kind, path, size, sha256, created_at) VALUES (?, 'transcript', 'p', 1, 'h', ?)", job, ts)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	if n := queryInt(t, s.db, "SELECT COUNT(*) FROM artifacts WHERE job_id = ?", job); n != 1 {
		t.Errorf("artifacts after the upgrade = %d, want the job's 1", n)
	}
	exec(t, s.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, branch, created_at, updated_at)
		VALUES (?, NULL, 'review', 'review:1:x', 'fresh', 'p', 'queued', 'x', ?, ?)`, project, ts, ts)
	if _, err := s.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'work', 'work-without-task', 'fresh', 'p', 'queued', ?, ?)`, project, ts, ts); err == nil {
		t.Error("a work job without a task was accepted after the upgrade")
	}
	// The edit survives a reopen: the schema on disk holds it.
	s.Close()
	again := openStore(t, path)
	if n := queryInt(t, again.db, "SELECT COUNT(*) FROM jobs WHERE kind = 'review' AND task_id IS NULL"); n != 1 {
		t.Errorf("task-less reviews after reopen = %d, want 1", n)
	}
}

// TestForemanInboxBackfillOnUpgrade builds the store a binary that
// predates the foreman_inbox migration would leave behind — with tasks
// already parked at needs_foreman by the old log — then opens it with
// this binary. The migration must give every parked task one pending row
// pointing at its latest parking event (a later event of another kind is
// not it), so the first engine start after the upgrade wakes for parks
// the old log already holds. The boundary is derived from the
// migration's position in the list, as in TestNotifyCursorBackfillOnUpgrade.
func TestForemanInboxBackfillOnUpgrade(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var before int
	for _, m := range migs {
		if m.name == "foreman_inbox" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("foreman_inbox migration not found in allMigrations")
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
	project := insertProject(t, db)
	plan := insertPlan(t, db, project)
	// The parked task: an older question, the failure that parked it, and
	// a later non-parking event — the row must point at the latest
	// parking event, not the log's latest word.
	parked := insertTask(t, db, plan)
	exec(t, db, "UPDATE tasks SET state = 'needs_foreman' WHERE id = ?", parked)
	exec(t, db, `INSERT INTO events (kind, project_id, task_id, payload, created_at)
		VALUES ('task_needs_help', ?, ?, '{"question":"which file?"}', ?)`, project, parked, ts)
	parkedAt := insertID(t, db, `INSERT INTO events (kind, project_id, task_id, payload, created_at)
		VALUES ('task_failed', ?, ?, '{"reason":"no result block"}', ?)`, project, parked, ts)
	exec(t, db, `INSERT INTO events (kind, project_id, task_id, payload, created_at)
		VALUES ('task_running', ?, ?, '{}', ?)`, project, parked, ts)
	// A finished task's judgment events get no row: nothing waits on it.
	finished := insertTask(t, db, plan)
	exec(t, db, "UPDATE tasks SET state = 'done' WHERE id = ?", finished)
	exec(t, db, `INSERT INTO events (kind, project_id, task_id, payload, created_at)
		VALUES ('task_failed', ?, ?, '{"reason":"done long ago"}', ?)`, project, finished, ts)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	if v, err := s.userVersion(context.Background()); err != nil || v != len(migs) {
		t.Errorf("user_version after upgrade = %d, %v; want the newest %d", v, err, len(migs))
	}
	got := queryStrings(t, s.db, `SELECT event_id || ':' || state || ':' || COALESCE(job_id, 0) || ':' || attempts
		FROM foreman_inbox ORDER BY event_id`)
	if want := []string{fmt.Sprintf("%d:pending:0:0", parkedAt)}; !slices.Equal(got, want) {
		t.Errorf("foreman_inbox after upgrade = %v, want exactly %v", got, want)
	}
}

func TestUpdateCommitsAndRollsBack(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO runners (name, created_at) VALUES ('kept', ?)`, ts)
		return err
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	wantErr := errors.New("rolled back on purpose")
	if err := s.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO runners (name, created_at) VALUES ('dropped', ?)`, ts); err != nil {
			t.Fatalf("insert inside update: %v", err)
		}
		return wantErr
	}); err == nil {
		t.Fatal("update swallowed fn's error")
	} else if !errors.Is(err, wantErr) {
		t.Errorf("update error = %v, want fn's error passed through", err)
	}
	var names []string
	if err := s.View(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT name FROM runners ORDER BY name")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	if want := []string{"kept"}; !slices.Equal(names, want) {
		t.Errorf("runners after the rollback = %v, want %v", names, want)
	}
}

func TestNewerUserVersionIsRefused(t *testing.T) {
	path, s := openFile(t)
	exec(t, s.db, "PRAGMA user_version = "+strconv.Itoa(schemaVersion(t)+1))
	s.Close()

	reopened, err := Open(context.Background(), path)
	if err == nil {
		reopened.Close()
		t.Fatal("opening a store from a newer brigada succeeded; want refusal")
	}
	if !errors.Is(err, ErrTooNew) {
		t.Errorf("error = %v; want it to wrap ErrTooNew", err)
	}
	for _, want := range []string{
		fmt.Sprintf("user_version %d", schemaVersion(t)+1),
		fmt.Sprintf("latest known migration %d", schemaVersion(t)),
	} {
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
			"INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)",
			[]any{f.plan, f.task, bad}},
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

	// A work or fix job always belongs to a task: leaving it out is
	// refused. A review may go without one: the standalone review of
	// `brigada review --branch` (migration 0023).
	for _, kind := range []string{"work", "fix"} {
		if _, err := f.db.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
			VALUES (?, NULL, ?, ?, 'fresh', 'p', 'queued', ?, ?)`, f.project, kind, fmt.Sprintf("%s-without-task", kind), ts, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s job without a task: err = %v; want a CHECK failure", kind, err)
		}
	}
	exec(t, f.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, NULL, 'review', 'review-without-task', 'fresh', 'p', 'queued', ?, ?)`, f.project, ts, ts)
	// ...but a review with a task is as before, and the edit left the
	// foreman rule alone.
	exec(t, f.db, `INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
		VALUES (?, ?, 'review', 'review-with-task', 'fresh', 'p', 'queued', ?, ?)`, f.project, f.task, ts, ts)

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

	const ins = "INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)"
	if _, err := f.db.Exec(ins, f.plan, f.task, f.task); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("self-edge: err = %v; want a CHECK failure", err)
	}
	exec(t, f.db, ins, f.plan, f.task, other)
	if _, err := f.db.Exec(ins, f.plan, f.task, other); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("duplicate edge: err = %v; want a UNIQUE failure", err)
	}
	// The reverse edge is a distinct edge, not a duplicate.
	exec(t, f.db, ins, f.plan, other, f.task)
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
