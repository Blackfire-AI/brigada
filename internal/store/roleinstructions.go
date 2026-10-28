package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/defaults"
)

// RoleSourceSeededPrefix marks a role-instructions row the 0064 upgrade
// seeded from the embedded defaults: no user value stands behind it, so
// the engine reads the role as not configured, with the seeded text —
// the default of the version the provenance names — in effect.
const RoleSourceSeededPrefix = "generated from "

// seedRoleInstructions is the Go half of migration 0064: it seeds every
// project's per-role instruction texts. Each role the project has no
// text for gets the embedded default of the shipped version, recorded
// with the provenance 'generated from <version>, by upgrade' so a later
// diff can name what the text was; a project whose instructions setting
// (§5.9's one text) already holds text keeps it as its worker text,
// carrying its setting provenance over, and gains the other three roles.
func seedRoleInstructions(ctx context.Context, tx *sql.Tx) error {
	texts := map[string]string{}
	for _, role := range defaults.Roles {
		text, err := defaults.LiveRoleText(role)
		if err != nil {
			return err
		}
		texts[role] = text
	}
	seededMeta := func(text string) string {
		return fmt.Sprintf(`{"source":%q,"version":%q,"digest":%q,"bytes":%d}`,
			RoleSourceSeededPrefix+defaults.Version+", by upgrade", defaults.Version, defaults.Digest(text), len(text))
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, settings, settings_prov FROM projects")
	if err != nil {
		return fmt.Errorf("list the projects for the role instructions: %w", err)
	}
	type project struct {
		id         int64
		workerText string
		workerMeta string
	}
	var projs []project
	for rows.Next() {
		var p project
		var settings, prov string
		if err := rows.Scan(&p.id, &settings, &prov); err != nil {
			rows.Close()
			return fmt.Errorf("read a project for the role instructions: %w", err)
		}
		p.workerText, p.workerMeta = carriedWorkerText(settings, prov)
		projs = append(projs, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list the projects for the role instructions: %w", err)
	}
	rows.Close()
	for _, p := range projs {
		for _, role := range defaults.Roles {
			text, meta := texts[role], seededMeta(texts[role])
			if role == "worker" && p.workerText != "" {
				text, meta = p.workerText, p.workerMeta
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, ?, ?, ?)",
				p.id, role, text, meta); err != nil {
				return fmt.Errorf("seed the %s instructions of project %d: %w", role, p.id, err)
			}
		}
	}
	return nil
}

// carriedWorkerText reads a project's old instructions setting, the one
// text of §5.9 before the roles arrived, and returns it with the
// provenance it keeps as the worker row's: the source the settings row
// and its import record give it, with the import's hash as the version.
// An unset or empty setting returns ""s, which seed the default instead.
func carriedWorkerText(settings, prov string) (string, string) {
	var row map[string]json.RawMessage
	if settings != "" && json.Unmarshal([]byte(settings), &row) == nil {
		if cur, ok := row["instructions"]; ok {
			if text, ok := rawText(cur); ok && text != "" {
				meta := fmt.Sprintf(`{"source":"local","digest":%q,"bytes":%d}`, defaults.Digest(text), len(text))
				var p struct {
					Hash     string                     `json:"hash"`
					Imported map[string]json.RawMessage `json:"imported"`
				}
				if json.Unmarshal([]byte(prov), &p) == nil {
					if imp, ok := p.Imported["instructions"]; ok && compactText(imp) == compactText(cur) {
						meta = fmt.Sprintf(`{"source":"imported","version":%q,"digest":%q,"bytes":%d}`, p.Hash, defaults.Digest(text), len(text))
					}
				}
				return text, meta
			}
		}
	}
	return "", ""
}

// rawText reads a stored JSON string.
func rawText(raw json.RawMessage) (string, bool) {
	var s string
	if raw == nil || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// compactText renders a stored JSON value compactly, for value equality.
func compactText(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}
