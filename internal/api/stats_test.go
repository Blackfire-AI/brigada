package api

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// seedStatsRows registers one agent offering implement and gives it a
// record in project 1 only.
func (e *env) seedStatsRows() {
	e.t.Helper()
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO runners (name, created_at, last_seen_at) VALUES ('stats-runner', ?, ?)`, seedTS, seedTS)
		if err != nil {
			return err
		}
		runner, err := res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.Exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
			VALUES (?, 'claude', 'claude', 'sonnet', 'claude', 'work', '["implement"]', ?, ?)`, runner, seedTS, seedTS)
		if err != nil {
			return err
		}
		agent, err := res.LastInsertId()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
			VALUES (?, ?, 'implement', 3, 2, ?)`, e.project1, agent, seedTS)
		return err
	})
	if err != nil {
		e.t.Fatalf("seed stats rows: %v", err)
	}
}

// TestStatsEndpoint covers /v1/stats (§8): the roles, and the scope
// like status — a client or admin reads every project, a foreman only
// its own, and an explicit ask for another project is refused.
func TestStatsEndpoint(t *testing.T) {
	e := newEnv(t)
	e.seedStatsRows()

	code, _, data := e.do(http.MethodGet, "/v1/stats", "", "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s, want 401", code, data)
	}
	if code, _, data = e.do(http.MethodGet, "/v1/stats", "r1", "", nil); code != http.StatusForbidden {
		t.Fatalf("runner reads stats: %d %s, want 403", code, data)
	}
	if code, _, data = e.do(http.MethodPost, "/v1/stats", "alice", "", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST stats: %d %s, want 405", code, data)
	}

	for _, token := range []string{"alice", "root"} {
		code, _, data = e.do(http.MethodGet, "/v1/stats", token, "", nil)
		if code != http.StatusOK {
			t.Fatalf("%s reads stats: %d %s, want 200", token, code, data)
		}
		stats := decode[[]engine.ProjectStats](t, data)
		if len(stats) != 2 {
			t.Fatalf("%s reads %d projects, want both", token, len(stats))
		}
		rows := stats[0].Rows
		if len(rows) != 1 || rows[0].Harness != "claude" || rows[0].Model != "sonnet" ||
			rows[0].Attempts != 3 || rows[0].Successes != 2 {
			t.Errorf("%s: project 1 rows = %+v, want the claude/sonnet record 2 of 3", token, rows)
		}
		if rows := stats[1].Rows; len(rows) != 1 || rows[0].Attempts != 0 || rows[0].Score != 0.5 {
			t.Errorf("%s: project 2 rows = %+v, want the unrecorded agent at attempts 0", token, rows)
		}
	}

	code, _, data = e.do(http.MethodGet, "/v1/stats?project=2", "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("client asks for project 2: %d %s, want 200", code, data)
	}
	if stats := decode[[]engine.ProjectStats](t, data); len(stats) != 1 || stats[0].ProjectID != e.project2 {
		t.Errorf("project=2 stats = %+v, want project 2 alone", stats)
	}

	// The foreman is walled into its own project.
	code, _, data = e.do(http.MethodGet, "/v1/stats", "foreman1", "", nil)
	if code != http.StatusOK {
		t.Fatalf("foreman reads stats: %d %s, want 200", code, data)
	}
	if stats := decode[[]engine.ProjectStats](t, data); len(stats) != 1 || stats[0].ProjectID != e.project1 {
		t.Errorf("foreman stats = %+v, want its own project alone", stats)
	}
	code, _, data = e.do(http.MethodGet, "/v1/stats?project=2", "foreman1", "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("foreman asks for project 2: %d %s, want 403", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeForbidden {
		t.Errorf("foreman scope: code = %q, want %q", err.Code, codeForbidden)
	}

	code, _, data = e.do(http.MethodGet, "/v1/stats?project=one", "alice", "", nil)
	if code != http.StatusBadRequest {
		t.Errorf("stats?project=one: %d %s, want 400", code, data)
	}
}
