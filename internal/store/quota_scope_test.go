package store

import (
	"context"
	"database/sql"
	"testing"
)

// buildPreQuotaScope opens a raw store holding every migration before
// quota_scope, with user_version set to match, at a fresh path the
// caller seeds old data into.
func buildPreQuotaScope(t *testing.T) (string, *sql.DB) {
	t.Helper()
	return openBeforeMigration(t, "quota_scope")
}

// TestQuotaScopeMigrationBackfillsAndResets builds the store of a binary
// that predates the quota_scope migration, with opencode and
// single-provider agents cooling, and opens it with this binary: every
// agent gains the derived scope — opencode/"provider" from the model's
// first path segment, "opencode/default" for the CLI default, the
// account label for every other harness — and inherited cooling clears
// only on the opencode account whose providers split the old pool.
func TestQuotaScopeMigrationBackfillsAndResets(t *testing.T) {
	path, db := buildPreQuotaScope(t)
	runner := insertID(t, db, "INSERT INTO runners (name, created_at, last_seen_at) VALUES ('r', ?, ?)", ts, ts)
	future := "2026-11-01T00:00:00Z"
	agent := func(adapter, harness, model, family, account, cooling string) int64 {
		t.Helper()
		var cool any
		if cooling != "" {
			cool = cooling
		}
		return insertID(t, db, `INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, health, cooling_until, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '["implement"]', 'ready', ?, ?, ?)`, runner, adapter, harness, model, family, account, cool, ts, ts)
	}
	// One opencode runner's three providers share the old "opencode"
	// account: the split clears their inherited cooling.
	zai := agent("opencode", "opencode", "zai-coding-plan/glm-5.3", "zhipu", "opencode", future)
	openai := agent("opencode", "opencode", "opencode/mimo", "xiaomi", "opencode", future)
	def := agent("opencode", "opencode", "default", "", "opencode", future)
	// A single-provider opencode roster keeps its cooling.
	solo := agent("opencode", "opencode", "deepseek/chat", "deepseek", "solo", future)
	// Non-opencode harnesses keep their account-scoped cooling.
	claude := agent("claude", "claude", "sonnet", "anthropic", "claude", future)
	opus := agent("claude", "claude", "opus", "anthropic", "claude", future)
	if err := db.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	s := openStore(t, path)
	migs, err := allMigrations()
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if v, err := s.userVersion(context.Background()); err != nil || v != len(migs) {
		t.Errorf("user_version after upgrade = %d, %v; want the newest %d", v, err, len(migs))
	}
	scope := func(id int64) string {
		t.Helper()
		return queryString(t, s.db, "SELECT quota_scope FROM agents WHERE id = ?", id)
	}
	cooling := func(id int64) string {
		t.Helper()
		return queryString(t, s.db, "SELECT COALESCE(cooling_until, '') FROM agents WHERE id = ?", id)
	}
	for id, want := range map[int64]string{
		zai:    "opencode/zai-coding-plan",
		openai: "opencode/opencode",
		def:    "opencode/default",
		solo:   "opencode/deepseek",
		claude: "claude",
		opus:   "claude",
	} {
		if got := scope(id); got != want {
			t.Errorf("quota_scope of agent %d = %q, want %q", id, got, want)
		}
	}
	// The split opencode pool's inherited cooling clears; the single
	// provider and the non-opencode harnesses keep theirs.
	for _, id := range []int64{zai, openai, def} {
		if got := cooling(id); got != "" {
			t.Errorf("cooling_until of split agent %d = %q, want cleared", id, got)
		}
	}
	for _, id := range []int64{solo, claude, opus} {
		if got := cooling(id); got != future {
			t.Errorf("cooling_until of kept agent %d = %q, want %q", id, got, future)
		}
	}
}
