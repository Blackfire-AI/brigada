package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestJobSessionCostMigration(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	idx := -1
	for i, m := range migs {
		if m.name == "job_session_cost" {
			idx = i
		}
	}
	if idx <= 0 {
		t.Fatal("no job_session_cost migration")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs[:idx] {
		exec(t, db, m.sql)
	}
	exec(t, db, fmt.Sprintf("PRAGMA user_version = %d", migs[idx-1].version))
	project := insertProject(t, db)
	plan := insertPlan(t, db, project)
	task := insertTask(t, db, plan)
	runner := insertRunner(t, db)
	agent := insertAgent(t, db, runner)
	exec(t, db, `INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, health, created_at, updated_at)
		VALUES (?, 'fake', 'fake', 'm', 'f', 'a', '["implement"]', 'ready', ?, ?)`, runner, ts, ts)
	other := int64(queryInt(t, db, "SELECT MAX(id) FROM agents"))
	mk := func(agentID int64, key, session string, cost float64) int64 {
		id := insertJob(t, db, project, task, key, "done")
		exec(t, db, "UPDATE jobs SET agent_id=?, cost_usd=?, result=? WHERE id=?", agentID, cost,
			`{"result":"x","session_id":"`+session+`"}`, id)
		return id
	}
	a1 := mk(agent, "task:1", "s1", 1.0)
	a2 := mk(agent, "task:2", "s1", 1.5)
	a3 := mk(agent, "task:3", "s1", 1.25) // below the previous total: floors at 0
	b1 := mk(agent, "task:4", "s2", 2.0)
	f1 := mk(other, "task:5", "s1", 9.0)
	f2 := mk(other, "task:6", "s1", 9.5)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st := openStore(t, path)
	val := func(id int64, col string) float64 {
		var v float64
		if err := st.db.QueryRow("SELECT "+col+" FROM jobs WHERE id=?", id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		id            int64
		cost, session float64
	}{{a1, 1.0, 1.0}, {a2, 0.5, 1.5}, {a3, 0, 1.25}, {b1, 2.0, 2.0}} {
		if got := val(c.id, "cost_usd"); got != c.cost {
			t.Errorf("job %d cost_usd = %v, want %v", c.id, got, c.cost)
		}
		if got := val(c.id, "session_cost_usd"); got != c.session {
			t.Errorf("job %d session_cost_usd = %v, want %v", c.id, got, c.session)
		}
	}
	for _, id := range []int64{f1, f2} {
		if queryInt(t, st.db, "SELECT session_cost_usd IS NULL FROM jobs WHERE id=?", id) != 1 {
			t.Errorf("non-claude job %d got a session total", id)
		}
	}
	if val(f1, "cost_usd") != 9.0 || val(f2, "cost_usd") != 9.5 {
		t.Error("non-claude cost changed")
	}
}

func TestJobUsageMigration(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if migs[42].version != 43 || migs[42].name != "job_usage" {
		t.Fatal("usage must be migration 0043")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs[:42] {
		exec(t, db, m.sql)
	}
	exec(t, db, "PRAGMA user_version = 42")
	project := insertProject(t, db)
	plan := insertPlan(t, db, project)
	task := insertTask(t, db, plan)
	job := insertJob(t, db, project, task, "task:old", "done")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st := openStore(t, path)
	for _, column := range []string{"input_tokens", "output_tokens", "cached_input_tokens", "reasoning_tokens", "cost_usd", "wall_seconds"} {
		if queryInt(t, st.db, "SELECT "+column+" IS NULL FROM jobs WHERE id=?", job) != 1 {
			t.Fatalf("old job %s is known", column)
		}
		exec(t, st.db, "UPDATE jobs SET "+column+"=0 WHERE id=?", job)
		if queryInt(t, st.db, "SELECT "+column+" IS NOT NULL AND "+column+"=0 FROM jobs WHERE id=?", job) != 1 {
			t.Fatalf("%s lost known zero", column)
		}
	}
	if queryInt(t, st.db, "SELECT COUNT(*) FROM jobs WHERE id=? AND status='done'", job) != 1 {
		t.Fatal("history changed")
	}
}
