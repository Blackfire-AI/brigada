package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/defaults"
	"github.com/Blackfire-AI/brigada/internal/prompt"
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
	query := "SELECT id, repo_url FROM projects WHERE archived_at IS NULL"
	var args []any
	if project != 0 {
		query += " AND id = ?"
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

// StatusRoleInstructions describes one effective role text. A nil comparison
// means its baseline or the client's current file digest is unknown.
type StatusRoleInstructions struct {
	ProjectID          int64  `json:"project_id"`
	RepoURL            string `json:"repo_url"`
	Role               string `json:"role"`
	State              string `json:"state"`
	Source             string `json:"source,omitempty"`
	Bytes              int    `json:"bytes"`
	Digest             string `json:"digest,omitempty"`
	GeneratedFrom      string `json:"generated_from,omitempty"`
	Edited             *bool  `json:"edited"`
	ChangedSinceImport *bool  `json:"changed_since_import"`
	Text               string `json:"text,omitempty"`
}

type StatusSkill struct {
	ProjectID   int64  `json:"project_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Digest      string `json:"digest"`
	TimesUsed   int    `json:"times_used"`
	TreeDigest  string `json:"tree_digest,omitempty"`
	// ChangedSinceImport compares the client's current file and tree
	// digests with the imported ones; nil when the client sent none.
	ChangedSinceImport *bool `json:"changed_since_import"`
}

func statusGuidanceTx(tx *sql.Tx, project int64, withText bool) ([]StatusRoleInstructions, []StatusSkill, error) {
	q := "SELECT id, repo_url FROM projects WHERE archived_at IS NULL"
	var args []any
	if project != 0 {
		q += " AND id = ?"
		args = append(args, project)
	}
	rows, err := tx.Query(q+" ORDER BY id", args...)
	if err != nil {
		return nil, nil, err
	}
	type proj struct {
		id   int64
		repo string
	}
	var projects []proj
	for rows.Next() {
		var p proj
		if err = rows.Scan(&p.id, &p.repo); err != nil {
			rows.Close()
			return nil, nil, err
		}
		projects = append(projects, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	var roles []StatusRoleInstructions
	var skills []StatusSkill
	for _, p := range projects {
		for _, role := range RoleKeys {
			ri, err := roleInstructionTx(tx, p.id, p.repo, role)
			if err != nil {
				return nil, nil, err
			}
			s := StatusRoleInstructions{ProjectID: p.id, RepoURL: RedactRepoURL(p.repo), Role: role, State: ri.State, Source: ri.Source}
			if ri.State != RoleStateNotConfigured {
				// A role not configured runs nothing: its state is all
				// status shows (§5.9), no size, digest or provenance.
				s.Bytes, s.Digest = ri.Bytes, ri.Digest
			}
			if ri.Source == SourceGenerated {
				// A generated row names the defaults version it was
				// seeded from, as recorded; never this binary's.
				s.GeneratedFrom = ri.Version
				if baseline, err := defaults.ArchiveRoleText(s.GeneratedFrom, role); err == nil {
					edited := instructionsDigest(ri.Text) != instructionsDigest(baseline)
					s.Edited = &edited
				}
			}
			if withText {
				s.Text = prompt.RoleInstructionsSection(role, ri.Text)
			}
			roles = append(roles, s)
		}
		sr, err := tx.Query(`SELECT s.name, s.description, s.digest, s.tree_digest,
   (SELECT count(*) FROM tasks t JOIN plans p ON p.id = t.plan_id
    WHERE p.project_id = s.project_id AND t.skills != '' AND EXISTS
    (SELECT 1 FROM json_each(t.skills) j WHERE json_extract(j.value, '$.name') = s.name))
   FROM skills s WHERE s.project_id = ? ORDER BY s.name`, p.id)
		if err != nil {
			return nil, nil, err
		}
		for sr.Next() {
			s := StatusSkill{ProjectID: p.id}
			if err := sr.Scan(&s.Name, &s.Description, &s.Digest, &s.TreeDigest, &s.TimesUsed); err != nil {
				sr.Close()
				return nil, nil, err
			}
			skills = append(skills, s)
		}
		err = sr.Err()
		sr.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return roles, skills, nil
}
