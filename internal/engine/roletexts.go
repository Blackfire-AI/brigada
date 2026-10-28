package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/defaults"
)

// The per-role instruction texts (design §5.9): one text per project and
// role — foreman, plan_review, worker and reviewer — stored in
// role_instructions with its source, settings version and digest. The
// worker text is the project's instructions setting materialized; the
// other three roles are the foreman's, the plan review's and the
// reviewer's texts. A role with no row, or a row an upgrade generated,
// is not configured: the embedded default applies.

// The roles a per-role instruction text exists for.
const (
	RoleForeman    = "foreman"
	RolePlanReview = "plan_review"
	RoleWorker     = "worker"
	RoleReviewer   = "reviewer"
)

// RoleTexts is the roles a per-role instruction text exists for, in order.
var RoleTexts = []string{RoleForeman, RolePlanReview, RoleWorker, RoleReviewer}

// The three states a role text can be in (§5.9): no row — the embedded
// default applies; a row the user emptied on purpose; a row with a text.
const (
	RoleTextNotConfigured = "not configured"
	RoleTextEmptyByChoice = "empty by choice"
	RoleTextFile          = "file"
)

// RoleTextView is one role's stored text with its provenance.
type RoleTextView struct {
	Role string `json:"role"`
	// State is one of RoleTextNotConfigured, RoleTextEmptyByChoice,
	// RoleTextFile.
	State string `json:"state"`
	Text  string `json:"text,omitempty"`
	// Source is where the text came from: an upgrade's generated
	// provenance, a local override, or a file's import record.
	Source string `json:"source,omitempty"`
	// Version is the settings version (the import hash) the text was
	// resolved at; empty for a local value.
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Bytes   int    `json:"bytes"`
}

// ValidateRoleText checks a role text the way SetRoleText does: a known
// role, at most MaxInstructionsBytes of valid UTF-8 with no NUL. The
// refusal names the role, and an oversize text its size and the limit;
// nothing is ever truncated.
func ValidateRoleText(role, text string) error {
	if !slices.Contains(RoleTexts, role) {
		return fmt.Errorf("%w: unknown role %q; valid roles: %s", ErrInvalidSetting, role, strings.Join(RoleTexts, ", "))
	}
	if len(text) > MaxInstructionsBytes {
		return fmt.Errorf("%w: %s text is %d bytes, over the %d byte limit; shorten it (nothing is truncated)",
			ErrInvalidSetting, role, len(text), MaxInstructionsBytes)
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: %s text is not valid UTF-8", ErrInvalidSetting, role)
	}
	if strings.ContainsRune(text, 0) {
		return fmt.Errorf("%w: %s text holds a NUL byte", ErrInvalidSetting, role)
	}
	return nil
}

// roleTextRow is one row of role_instructions.
type roleTextRow struct {
	text    string
	source  string
	version string
	digest  string
}

// roleTextRowsTx reads a project's role texts, empty texts included.
func roleTextRowsTx(tx *sql.Tx, project int64) (map[string]roleTextRow, error) {
	rows, err := tx.Query("SELECT role, text, source, settings_version, digest FROM role_instructions WHERE project_id = ?", project)
	if err != nil {
		return nil, fmt.Errorf("read the role texts of project %d: %w", project, err)
	}
	defer rows.Close()
	out := map[string]roleTextRow{}
	for rows.Next() {
		role, row := "", roleTextRow{}
		if err := rows.Scan(&role, &row.text, &row.source, &row.version, &row.digest); err != nil {
			return nil, fmt.Errorf("read the role texts of project %d: %w", project, err)
		}
		out[role] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the role texts of project %d: %w", project, err)
	}
	return out, nil
}

// roleTextView renders one stored row as its state and view.
func roleTextView(role string, row roleTextRow) RoleTextView {
	v := RoleTextView{Role: role, Text: row.text, Source: row.source, Version: row.version, Digest: row.digest, Bytes: len(row.text)}
	switch {
	case defaults.IsGeneratedProvenance(row.source):
		v.State = RoleTextNotConfigured
	case row.text == "":
		v.State = RoleTextEmptyByChoice
	default:
		v.State = RoleTextFile
	}
	return v
}

// roleTextUpsert writes one role's row, the worker's included.
const roleTextUpsert = `INSERT INTO role_instructions (project_id, role, text, source, settings_version, digest, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (project_id, role) DO UPDATE SET
		text = excluded.text, source = excluded.source, settings_version = excluded.settings_version,
		digest = excluded.digest, updated_at = excluded.updated_at`

// syncWorkerTextTx materializes the instructions setting into the
// worker's role row after either changes (§5.9): the setting stays the
// worker text's one source of truth and the row its stored copy, so the
// task resolution below and the role views never disagree. An unset
// setting removes the row; an empty-by-choice marker survives only
// until the setting next changes.
func syncWorkerTextTx(tx *sql.Tx, project int64, now time.Time) error {
	text, meta, err := projectInstructionsTx(tx, project)
	if err != nil {
		return err
	}
	if text == "" {
		if _, err := tx.Exec("DELETE FROM role_instructions WHERE project_id = ? AND role = ?", project, RoleWorker); err != nil {
			return fmt.Errorf("materialize the worker text of project %d: %w", project, err)
		}
		return nil
	}
	if _, err := tx.Exec(roleTextUpsert, project, RoleWorker, text, meta.Source, meta.Hash, meta.Digest, fmtTime(now)); err != nil {
		return fmt.Errorf("materialize the worker text of project %d: %w", project, err)
	}
	return nil
}

// SetRoleText sets or imports one role's instruction text (§5.9). A set
// passes the text with source "" (recorded as a local override); an
// import passes the file's text, a source naming it and the settings
// version (the import hash) it came from. The text is bounded at
// MaxInstructionsBytes, must be valid UTF-8 with no NUL, and is never
// truncated: a refusal leaves the previous value in effect. An empty
// text is a state of its own — empty by choice — not a reset; use
// ResetRoleText for that.
//
// The worker role's text is the instructions setting: a call through
// here writes the setting as a local override — an empty text unsets
// it — so `project set instructions=`, a project.yaml import and this
// call all move one effective text. Unlike the setting, the row keeps
// the provenance the caller passed, as every role's row does: an
// import through this API records its source and settings version
// while the effective text still resolves through the setting.
func (e *Engine) SetRoleText(ctx context.Context, repoURL, role, text, source, settingsVersion string) (RoleTextView, error) {
	if repoURL == "" {
		return RoleTextView{}, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	if err := ValidateRoleText(role, text); err != nil {
		return RoleTextView{}, err
	}
	if source == "" {
		source = SourceLocal
	}
	if _, err := Canonicalize(repoURL); err != nil {
		return RoleTextView{}, fmt.Errorf("%w: %v", ErrInvalidSetting, err)
	}
	var out RoleTextView
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		if errors.Is(err, sql.ErrNoRows) {
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, "", "", now)
		}
		if err != nil {
			return err
		}
		if err := activeProjectTx(tx, project); err != nil {
			return err
		}
		if role == RoleWorker {
			// One source of truth for the text: the setting takes it as a
			// local override — an empty text unsets it — while the row keeps
			// the provenance the caller passed, like every other role's.
			var rawSettings, rawProv string
			if err := tx.QueryRow("SELECT settings, settings_prov FROM projects WHERE id = ?", project).Scan(&rawSettings, &rawProv); err != nil {
				return fmt.Errorf("read the settings of project %d: %w", project, err)
			}
			parsed := map[string]json.RawMessage{SettingInstructions: nil}
			if text != "" {
				raw, err := json.Marshal(text)
				if err != nil {
					return fmt.Errorf("encode the instructions of project %d: %w", project, err)
				}
				parsed[SettingInstructions] = raw
			}
			if err := applySettingsRowTx(tx, project, rawSettings, rawProv, parsed, now); err != nil {
				return err
			}
			if text == "" {
				// The setting cannot hold an empty value; the row keeps the
				// empty-by-choice state alone, and no settings version names
				// an emptiness.
				settingsVersion = ""
			}
			digest := instructionsDigest(text)
			if _, err := tx.Exec(roleTextUpsert, project, RoleWorker, text, source, settingsVersion, digest, fmtTime(now)); err != nil {
				return fmt.Errorf("store the worker text of the project of %s: %w", RedactRepoURL(repoURL), err)
			}
			out = roleTextView(RoleWorker, roleTextRow{text: text, source: source, version: settingsVersion, digest: digest})
			return nil
		}
		digest := instructionsDigest(text)
		if _, err := tx.Exec(roleTextUpsert, project, role, text, source, settingsVersion, digest, fmtTime(now)); err != nil {
			return fmt.Errorf("store the %s text of the project of %s: %w", role, RedactRepoURL(repoURL), err)
		}
		out = roleTextView(role, roleTextRow{text: text, source: source, version: settingsVersion, digest: digest})
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalidSetting) {
			return RoleTextView{}, err
		}
		return RoleTextView{}, fmt.Errorf("set the %s text of the project of %s: %w", role, RedactRepoURL(repoURL), err)
	}
	return out, nil
}

// ResetRoleText removes one role's text: the role is not configured
// again and the embedded default applies. For the worker the
// instructions setting is reset with it.
func (e *Engine) ResetRoleText(ctx context.Context, repoURL, role string) error {
	if repoURL == "" {
		return fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	if !slices.Contains(RoleTexts, role) {
		return fmt.Errorf("%w: unknown role %q; valid roles: %s", ErrInvalidSetting, role, strings.Join(RoleTexts, ", "))
	}
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		if role == RoleWorker {
			var rawSettings, rawProv string
			if err := tx.QueryRow("SELECT settings, settings_prov FROM projects WHERE id = ?", project).Scan(&rawSettings, &rawProv); err != nil {
				return fmt.Errorf("read the settings of project %d: %w", project, err)
			}
			if err := applySettingsRowTx(tx, project, rawSettings, rawProv, map[string]json.RawMessage{SettingInstructions: nil}, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM role_instructions WHERE project_id = ? AND role = ?", project, role); err != nil {
			return fmt.Errorf("reset the %s text of the project of %s: %w", role, RedactRepoURL(repoURL), err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("reset the %s text of the project of %s: %w", role, RedactRepoURL(repoURL), err)
	}
	return nil
}

// ProjectRoleTexts lists every role's text state for the repo's
// project, in RoleTexts order; nil when the repo has no project.
func (e *Engine) ProjectRoleTexts(ctx context.Context, repoURL string) ([]RoleTextView, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("repo URL is required (product §5 rule 6: repos are addressed by URL)")
	}
	var out []RoleTextView
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := e.sweepTx(tx, e.cfg.Now()); err != nil {
			return err
		}
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		rows, err := roleTextRowsTx(tx, project)
		if err != nil {
			return err
		}
		for _, role := range RoleTexts {
			if row, ok := rows[role]; ok {
				out = append(out, roleTextView(role, row))
			} else {
				out = append(out, RoleTextView{Role: role, State: RoleTextNotConfigured})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the role texts of the project of %s: %w", RedactRepoURL(repoURL), err)
	}
	return out, nil
}
