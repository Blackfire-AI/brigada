package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

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
