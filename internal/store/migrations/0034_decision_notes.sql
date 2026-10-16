-- Notes are journal messages. This cursor records delivery to the separate
-- headless session, independently of interactive inbox consumption.
ALTER TABLE projects ADD COLUMN decision_note_cursor INTEGER NOT NULL DEFAULT 0;

-- SQLite cannot extend a CHECK constraint in place. Preserve every history
-- row and its id while adding the interactive foreman's distinct author.
CREATE TABLE messages_with_interactive_author (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    event_id    INTEGER REFERENCES events(id),
    kind        TEXT NOT NULL,
    author      TEXT NOT NULL CHECK (author IN ('user', 'foreman', 'foreman-interactive', 'worker', 'engine')),
    content     TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
INSERT INTO messages_with_interactive_author SELECT * FROM messages;
DROP TABLE messages;
ALTER TABLE messages_with_interactive_author RENAME TO messages;
CREATE INDEX idx_messages_project_time ON messages(project_id, created_at);
CREATE INDEX idx_messages_decision_notes ON messages(project_id, kind, event_id);
