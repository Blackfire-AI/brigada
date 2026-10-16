package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Project instructions (design §5.9): the project's one instruction text
// is a setting (SettingInstructions); a task copies the effective text at
// creation, with its provenance, so a later change to the setting never
// alters a recorded task and a recorded prompt is reproducible.

// instructionsMeta is the provenance a task records beside its copy of the
// text (tasks.instructions_meta).
type instructionsMeta struct {
	// Source is where the text came from: SourceLocal or SourceImported.
	Source string `json:"source"`
	// Hash is the hash of the imported project.yaml (plus text) the value
	// came from; it is the settings version of an imported value.
	Hash   string `json:"hash,omitempty"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

// projectInstructionsTx reads a project's effective instruction text and
// its provenance; "" with a zero meta when the project has none.
func projectInstructionsTx(tx *sql.Tx, project int64) (string, instructionsMeta, error) {
	var raw, rawProv string
	if err := tx.QueryRow("SELECT settings, settings_prov FROM projects WHERE id = ?", project).Scan(&raw, &rawProv); err != nil {
		return "", instructionsMeta{}, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	if raw == "" {
		return "", instructionsMeta{}, nil
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		return "", instructionsMeta{}, fmt.Errorf("parse the settings of project %d: %w", project, err)
	}
	cur, ok := row[SettingInstructions]
	if !ok {
		return "", instructionsMeta{}, nil
	}
	text, ok := rawString(cur)
	if !ok || text == "" {
		return "", instructionsMeta{}, nil
	}
	prov, err := parseProv(rawProv)
	if err != nil {
		return "", instructionsMeta{}, fmt.Errorf("parse the provenance of project %d: %w", project, err)
	}
	meta := instructionsMeta{Source: SourceLocal, Digest: instructionsDigest(text), Bytes: len(text)}
	if prov.isImported(SettingInstructions, cur) {
		meta.Source, meta.Hash = SourceImported, prov.Hash
	}
	return text, meta, nil
}

// stampTaskInstructionsTx resolves the project's instructions onto a task
// at creation (§5.9): the effective text, copied into the task row with
// its source, version and digest. A project with none leaves the task's
// columns empty.
func stampTaskInstructionsTx(tx *sql.Tx, project, taskID int64, now time.Time) error {
	text, meta, err := projectInstructionsTx(tx, project)
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode the instructions of task %d: %w", taskID, err)
	}
	if _, err := tx.Exec("UPDATE tasks SET instructions = ?, instructions_meta = ?, updated_at = ? WHERE id = ?",
		text, string(mb), fmtTime(now), taskID); err != nil {
		return fmt.Errorf("record the instructions of task %d: %w", taskID, err)
	}
	return nil
}

// taskInstructionsTx reads the instruction text a task recorded at
// creation, "" when it has none: every prompt of the task's jobs and
// reviews uses it, whatever the project setting says now.
func taskInstructionsTx(tx *sql.Tx, taskID int64) (string, error) {
	var text string
	if err := tx.QueryRow("SELECT instructions FROM tasks WHERE id = ?", taskID).Scan(&text); err != nil {
		return "", fmt.Errorf("read the instructions of task %d: %w", taskID, err)
	}
	return text, nil
}

// StatusInstructions is how a project's instructions show in status
// (§5.9): the source, version, size and digest, and the full text only
// when the caller asked for it.
type StatusInstructions struct {
	ProjectID int64  `json:"project_id"`
	RepoURL   string `json:"repo_url"`
	Source    string `json:"source"`
	// Version is the hash of the import the text came from; empty for a
	// local override.
	Version string `json:"version,omitempty"`
	Bytes   int    `json:"bytes"`
	Digest  string `json:"digest"`
	Text    string `json:"text,omitempty"`
}

// statusInstructionsTx lists the projects in scope that have instructions.
func statusInstructionsTx(tx *sql.Tx, project int64, withText bool) ([]StatusInstructions, error) {
	query := "SELECT id, repo_url FROM projects"
	var args []any
	if project != 0 {
		query += " WHERE id = ?"
		args = append(args, project)
	}
	rows, err := tx.Query(query+" ORDER BY id", args...)
	if err != nil {
		return nil, fmt.Errorf("list the projects for the instructions: %w", err)
	}
	type proj struct {
		id   int64
		repo string
	}
	var projs []proj
	for rows.Next() {
		var p proj
		if err := rows.Scan(&p.id, &p.repo); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the projects for the instructions: %w", err)
		}
		projs = append(projs, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list the projects for the instructions: %w", err)
	}
	rows.Close()
	var out []StatusInstructions
	for _, p := range projs {
		text, meta, err := projectInstructionsTx(tx, p.id)
		if err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		si := StatusInstructions{ProjectID: p.id, RepoURL: RedactRepoURL(p.repo), Source: meta.Source, Version: meta.Hash, Bytes: meta.Bytes, Digest: meta.Digest}
		if withText {
			si.Text = text
		}
		out = append(out, si)
	}
	return out, nil
}
