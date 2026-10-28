package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/defaults"
)

// Per-role instruction texts (design §5.9): beside the worker text the
// instructions setting holds, each role has a text of its own, stored
// one row per project and role. The row's provenance says where the
// text came from; a missing row is not configured and the embedded
// default applies; a row with an empty local text is an empty-by-choice
// mark that suppresses the default.

// The roles a project can set an instruction text for.
const (
	RoleForeman    = "foreman"
	RolePlanReview = "plan_review"
	RoleWorker     = "worker"
	RoleReviewer   = "reviewer"
)

// RoleKeys are the roles, in the order show prints them.
var RoleKeys = []string{RoleForeman, RolePlanReview, RoleWorker, RoleReviewer}

// The three states of a role text (§5.9): not configured (no row: the
// embedded default applies), empty by choice (a row whose local text is
// empty: no text applies), and file (a row holding a text).
const (
	RoleStateNotConfigured = "not configured"
	RoleStateEmptyByChoice = "empty by choice"
	RoleStateFile          = "file"
)

// SourceGenerated is the source of a text seeded from the embedded
// defaults — by the role_instructions migration, or a later seeding —
// with the defaults version in the settings-version column.
const SourceGenerated = "generated"

// MaxRoleInstructionBytes bounds one role's text: the same starting
// value as the worker setting's (§5.9), refused above, never truncated.
const MaxRoleInstructionBytes = MaxInstructionsBytes

// RoleInstruction is a role's effective text and its provenance, as a
// read or a set returns it.
type RoleInstruction struct {
	ProjectID int64  `json:"project_id"`
	RepoURL   string `json:"repo_url"`
	Role      string `json:"role"`
	// State is one of RoleStateNotConfigured, RoleStateEmptyByChoice,
	// RoleStateFile.
	State string `json:"state"`
	// Source is where the text came from: default (not configured),
	// generated (seeded from the embedded defaults), local or imported.
	Source string `json:"source"`
	// Version is the settings version the text is pinned to: the import
	// hash for an imported text, the defaults version for a generated
	// one, empty for a local text.
	Version string `json:"version,omitempty"`
	// Provenance is the one-line reading of the source, the way the
	// foreman's status shows it: "generated from <version>, by upgrade"
	// for a seeded text, the source word otherwise.
	Provenance string `json:"provenance"`
	Bytes      int    `json:"bytes"`
	Digest     string `json:"digest"`
	// Text is the effective text: the row's, or the embedded default of
	// a role not configured; empty for an empty-by-choice row.
	Text string `json:"text,omitempty"`
}

func provenanceLine(source, version string) string {
	if source == SourceGenerated {
		return fmt.Sprintf("generated from %s, by upgrade", version)
	}
	return source
}

// validRole reports whether role is one the engine stores a text for.
func validRole(role string) bool {
	for _, r := range RoleKeys {
		if r == role {
			return true
		}
	}
	return false
}

// validateRoleText checks a role text the way ValidateInstructions
// checks the worker setting's: valid UTF-8, no NUL, at most
// MaxRoleInstructionBytes, with the role, the size and the limit in
// every refusal. Nothing is ever truncated.
func validateRoleText(role, text string) error {
	if len(text) > MaxRoleInstructionBytes {
		return fmt.Errorf("%w: %s instruction text is %d bytes, over the %d byte limit; shorten it (nothing is truncated)",
			ErrInvalidSetting, role, len(text), MaxRoleInstructionBytes)
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: %s instruction text is not valid UTF-8", ErrInvalidSetting, role)
	}
	if strings.ContainsRune(text, 0) {
		return fmt.Errorf("%w: %s instruction text holds a NUL byte", ErrInvalidSetting, role)
	}
	return nil
}

// upsertRoleInstructionTx writes one role's row: the text, its source
// and the settings version it is pinned to, and the digest of the text.
func upsertRoleInstructionTx(tx *sql.Tx, project int64, role, text, source, version string, now time.Time) error {
	digest := instructionsDigest(text)
	if _, err := tx.Exec(`INSERT INTO role_instructions (project_id, role, text, source, settings_version, digest, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id, role) DO UPDATE SET
			text = excluded.text, source = excluded.source, settings_version = excluded.settings_version,
			digest = excluded.digest, updated_at = excluded.updated_at`,
		project, role, text, source, version, digest, fmtTime(now)); err != nil {
		return fmt.Errorf("store the %s instructions of project %d: %w", role, project, err)
	}
	return nil
}

// defaultRoleInstruction reads the embedded default of a role as the
// view of a role that is not configured.
func defaultRoleInstruction(project int64, repo, role string) (RoleInstruction, error) {
	text, err := defaults.LiveRoleText(role)
	if err != nil {
		return RoleInstruction{}, fmt.Errorf("read the default %s instructions: %w", role, err)
	}
	return RoleInstruction{
		ProjectID: project, RepoURL: repo, Role: role,
		State: RoleStateNotConfigured, Source: SourceDefault,
		Bytes: len(text), Digest: instructionsDigest(text), Text: text,
	}, nil
}

// roleInstructionTx reads one role's effective text and provenance.
func roleInstructionTx(tx *sql.Tx, project int64, repo, role string) (RoleInstruction, error) {
	var text, source, version, digest string
	err := tx.QueryRow("SELECT text, source, settings_version, digest FROM role_instructions WHERE project_id = ? AND role = ?", project, role).
		Scan(&text, &source, &version, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultRoleInstruction(project, repo, role)
	}
	if err != nil {
		return RoleInstruction{}, fmt.Errorf("read the %s instructions of project %d: %w", role, project, err)
	}
	out := RoleInstruction{
		ProjectID: project, RepoURL: repo, Role: role,
		State: RoleStateFile, Source: source, Version: version,
		Provenance: provenanceLine(source, version),
		Bytes:      len(text), Digest: digest,
	}
	if source == SourceLocal && text == "" {
		out.State = RoleStateEmptyByChoice
		out.Digest = instructionsDigest("")
	}
	out.Text = text
	return out, nil
}

// RoleInstruction reads the repo's effective text for one role, with its
// state, source, settings version, size and digest; nil when the repo
// has no project.
func (e *Engine) RoleInstruction(ctx context.Context, repoURL, role string) (*RoleInstruction, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("repo URL is required (product §5 rule 6: repos are addressed by URL)")
	}
	if !validRole(role) {
		return nil, fmt.Errorf("%w: unknown role %q; valid roles: %s", ErrInvalidSetting, role, strings.Join(RoleKeys, ", "))
	}
	var out *RoleInstruction
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		var project int64
		var repo string
		err := lookupProjectTx(tx, repoURL, "id, repo_url", &project, &repo)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		v, err := roleInstructionTx(tx, project, repo, role)
		if err != nil {
			return err
		}
		out = &v
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the %s instructions of the project of %s: %w", role, RedactRepoURL(repoURL), err)
	}
	return out, nil
}

// SetRoleInstruction sets the repo's text for one role, all or nothing.
// The text is bounded at MaxRoleInstructionBytes, must be valid UTF-8
// with no NUL, and is never truncated: a refusal names the role, the
// size and the limit, and the previous value stays. An empty text is an
// empty-by-choice mark, not a reset: the row stays and suppresses the
// role's default. The worker role mirrors the instructions setting, so
// every reader of the setting sees the same text. A repo with no
// project gets one, born by setup like the foreman choice's.
func (e *Engine) SetRoleInstruction(ctx context.Context, repoURL, role, text string) (*RoleInstruction, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	if !validRole(role) {
		return nil, fmt.Errorf("%w: unknown role %q; valid roles: %s", ErrInvalidSetting, role, strings.Join(RoleKeys, ", "))
	}
	if err := validateRoleText(role, text); err != nil {
		return nil, err
	}
	canon, cerr := Canonicalize(repoURL)
	if cerr != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSetting, cerr)
	}
	var out *RoleInstruction
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		var shownRepo string
		err := lookupProjectTx(tx, repoURL, "id, repo_url", &project, &shownRepo)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, "", "", now)
			if err != nil {
				return err
			}
			shownRepo = canon
		case err != nil:
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		if err := activeProjectTx(tx, project); err != nil {
			return err
		}
		if role == RoleWorker {
			// The worker text is the instructions setting: the set
			// writes both, so the setting's readers and the role's
			// agree. An empty-by-choice mark removes the setting and
			// keeps an empty local row.
			var rawSettings, rawProv string
			if err := tx.QueryRow("SELECT settings, settings_prov FROM projects WHERE id = ?", project).Scan(&rawSettings, &rawProv); err != nil {
				return fmt.Errorf("read the settings of project %d: %w", project, err)
			}
			parsed := map[string]json.RawMessage{SettingInstructions: nil}
			if text != "" {
				raw, err := json.Marshal(text)
				if err != nil {
					return fmt.Errorf("encode the %s instructions: %w", role, err)
				}
				parsed[SettingInstructions] = raw
			}
			if err := applySettingsRowTx(tx, project, rawSettings, rawProv, parsed, now); err != nil {
				return err
			}
			if text == "" {
				if err := upsertRoleInstructionTx(tx, project, role, "", SourceLocal, "", now); err != nil {
					return err
				}
			}
		} else if err := upsertRoleInstructionTx(tx, project, role, text, SourceLocal, "", now); err != nil {
			return err
		}
		if _, err := appendEventTx(tx, evRoleInstructionSet, project, 0, 0,
			eventPayload{Reason: fmt.Sprintf("%s instructions set, %d bytes", role, len(text))}, now); err != nil {
			return err
		}
		v, err := roleInstructionTx(tx, project, shownRepo, role)
		if err != nil {
			return err
		}
		out = &v
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalidSetting) {
			return nil, err
		}
		return nil, fmt.Errorf("set the %s instructions of the project of %s: %w", role, RedactRepoURL(repoURL), err)
	}
	return out, nil
}

// syncWorkerInstructionTx mirrors an import's instructions outcome into
// the worker role row (§5.9). Provenance follows the import even when
// the text is unchanged, so a re-import that moved only other keys still
// dates the row with the new hash. A local row — the user's own text or
// an empty-by-choice mark — is left alone: it wins against the file, as
// a local override does, unless the import explicitly accepted the
// replacement of that hand-set value, in which case the row follows the
// settings. When no instructions value is in effect, an imported row is
// gone with it; a generated or local one stays.
func syncWorkerInstructionTx(tx *sql.Tx, project int64, row, imported map[string]json.RawMessage, hash string, replacedLocal bool, now time.Time) error {
	raw, ok := row[SettingInstructions]
	if !ok {
		if _, err := tx.Exec("DELETE FROM role_instructions WHERE project_id = ? AND role = ? AND source = ?", project, RoleWorker, SourceImported); err != nil {
			return fmt.Errorf("clear the imported worker instructions of project %d: %w", project, err)
		}
		return nil
	}
	if _, imp := imported[SettingInstructions]; !imp {
		return nil // a local override kept its value against the file
	}
	var source string
	err := tx.QueryRow("SELECT source FROM role_instructions WHERE project_id = ? AND role = ?", project, RoleWorker).Scan(&source)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read the worker instructions of project %d: %w", project, err)
	case source == SourceLocal && !replacedLocal:
		return nil // the user's text or empty-by-choice mark wins
	}
	text, ok := rawString(raw)
	if !ok || text == "" {
		return nil
	}
	return upsertRoleInstructionTx(tx, project, RoleWorker, text, SourceImported, hash, now)
}
