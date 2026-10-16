package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDecisionNotesMigrationPreservesHistory(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	before := 0
	for _, m := range migs {
		if m.name == "decision_notes" {
			before = m.version - 1
			break
		}
	}
	if before == 0 {
		t.Fatal("decision_notes migration missing")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs[:before] {
		exec(t, db, m.sql)
	}
	exec(t, db, "PRAGMA user_version = "+strconv.Itoa(before))
	project := insertProject(t, db)
	event := insertID(t, db, `INSERT INTO events (project_id, kind, payload, created_at) VALUES (?, 'old_event', '{}', ?)`, project, ts)
	message := insertID(t, db, `INSERT INTO messages (project_id, event_id, kind, author, content, created_at) VALUES (?, ?, 'old_message', 'foreman', 'keep this', ?)`, project, event, ts)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, path)
	if got := queryString(t, s.db, `SELECT content FROM messages WHERE id = ? AND project_id = ? AND event_id = ? AND kind = 'old_message' AND author = 'foreman' AND created_at = ?`, message, project, event, ts); got != "keep this" {
		t.Fatalf("old message changed: %q", got)
	}
	if cursor := queryInt(t, s.db, "SELECT decision_note_cursor FROM projects WHERE id = ?", project); cursor != 0 {
		t.Fatalf("initial cursor = %d", cursor)
	}
	exec(t, s.db, `INSERT INTO messages (project_id, kind, author, content, created_at) VALUES (?, 'decision_note', 'foreman-interactive', 'new note', ?)`, project, ts)
	if _, err := s.db.Exec(`INSERT INTO messages (project_id, kind, author, content, created_at) VALUES (?, 'decision_note', 'bogus', 'bad', ?)`, project, ts); err == nil {
		t.Fatal("author constraint was lost")
	}
	if rows := queryStrings(t, s.db, "PRAGMA foreign_key_check"); len(rows) != 0 {
		t.Fatalf("foreign key violations: %v", rows)
	}
}
