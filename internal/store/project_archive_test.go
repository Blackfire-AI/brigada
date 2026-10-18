package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
)

func TestProjectArchiveMigrationPreservesHistory(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := 0
	for _, migration := range migs {
		if migration.name == "project_archive" {
			before = migration.version - 1
		}
	}
	if before == 0 {
		t.Fatal("project_archive migration missing")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migs[:before] {
		exec(t, db, migration.sql)
	}
	exec(t, db, "PRAGMA user_version = "+strconv.Itoa(before))
	project := insertProject(t, db)
	event := insertID(t, db, `INSERT INTO events (project_id, kind, payload, created_at) VALUES (?, 'old_event', '{}', ?)`, project, ts)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st := openStore(t, path)
	if got := queryInt(t, st.db, "SELECT archived_at IS NULL FROM projects WHERE id = ?", project); got != 1 {
		t.Fatal("existing project was not active after migration")
	}
	exec(t, st.db, "UPDATE projects SET archived_at = ? WHERE id = ?", ts, project)
	if got := queryInt(t, st.db, "SELECT COUNT(*) FROM events WHERE id = ? AND project_id = ?", event, project); got != 1 {
		t.Fatal("archive changed history")
	}
	if rows := queryStrings(t, st.db, "PRAGMA foreign_key_check"); len(rows) != 0 {
		t.Fatalf("foreign key violations: %v", rows)
	}
}
