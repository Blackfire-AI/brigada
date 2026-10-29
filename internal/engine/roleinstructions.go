package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Per-role instruction texts (design §5.9): beside the worker text the
// instructions setting holds, each role has a text of its own, stored
// one row per project and role. The row's provenance says where the
// text came from; a missing row is not configured — no text applies,
// for at run time the engine never falls back to a text in the binary
// (§5.9 'Three states, no hidden fallback'); a row with an empty local
// text is an empty-by-choice mark.

// The roles a project can set an instruction text for.
const (
	RoleForeman    = "foreman"
	RolePlanReview = "plan_review"
	RoleWorker     = "worker"
	RoleReviewer   = "reviewer"
)

// RoleKeys are the roles, in the order show prints them.
var RoleKeys = []string{RoleForeman, RolePlanReview, RoleWorker, RoleReviewer}

// The three states of a role text (§5.9): not configured (no row: no
// text applies), empty by choice (a row whose local text is empty: no
// text applies), and file (a row holding a text).
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
	// Text is the effective text: the row's; empty for a role not
	// configured or an empty-by-choice row, for at run time the engine
	// never falls back to a text in the binary (§5.9).
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

// roleInstructionTx reads one role's effective text and provenance. A
// role with no row is not configured (§5.9 'Three states, no hidden
// fallback'): no text of its own, the source reading default, and at
// run time the engine never falls back to a text in the binary.
func roleInstructionTx(tx *sql.Tx, project int64, repo, role string) (RoleInstruction, error) {
	var text, source, version, digest string
	err := tx.QueryRow("SELECT text, source, settings_version, digest FROM role_instructions WHERE project_id = ? AND role = ?", project, role).
		Scan(&text, &source, &version, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return RoleInstruction{
			ProjectID: project, RepoURL: repo, Role: role,
			State: RoleStateNotConfigured, Source: SourceDefault,
			Bytes: 0, Digest: instructionsDigest(""),
		}, nil
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
	if (source == SourceLocal || source == SourceImported) && text == "" {
		// A local mark, or an empty role file an import read (§5.9):
		// no text applies.
		out.State = RoleStateEmptyByChoice
		out.Digest = instructionsDigest("")
	}
	out.Text = text
	return out, nil
}

// storedRoleTextTx reads one role's text as a prompt carries it (§5.9):
// the stored row — the user's text, an import's, or the upgrade's seed
// of the default — and "" when the project has no row at all: a
// not-configured role runs on the contract alone, never a text in the
// binary.
func storedRoleTextTx(tx *sql.Tx, project int64, role string) (string, error) {
	ri, err := roleInstructionTx(tx, project, "", role)
	if err != nil {
		return "", err
	}
	return ri.Text, nil
}

// ValidateRoleText checks a role text the way a set or an import does:
// a known role, then validateRoleText's bounds. A client calls it to
// refuse before it sends; the engine checks again.
func ValidateRoleText(role, text string) error {
	if !validRole(role) {
		return fmt.Errorf("%w: unknown role %q; valid roles: %s", ErrInvalidSetting, role, strings.Join(RoleKeys, ", "))
	}
	return validateRoleText(role, text)
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
// gone with it; a generated or local one stays. An empty worker file
// (emptyFile) is the one exception: the file is read as the empty-by-choice
// state (§5.9), so an empty imported row stands for it.
func syncWorkerInstructionTx(tx *sql.Tx, project int64, row, imported map[string]json.RawMessage, hash string, replacedLocal, emptyFile bool, now time.Time) error {
	raw, ok := row[SettingInstructions]
	if !ok {
		if _, err := tx.Exec("DELETE FROM role_instructions WHERE project_id = ? AND role = ? AND source = ?", project, RoleWorker, SourceImported); err != nil {
			return fmt.Errorf("clear the imported worker instructions of project %d: %w", project, err)
		}
		if !emptyFile {
			return nil
		}
		var source string
		err := tx.QueryRow("SELECT source FROM role_instructions WHERE project_id = ? AND role = ?", project, RoleWorker).Scan(&source)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read the worker instructions of project %d: %w", project, err)
		case source == SourceLocal && !replacedLocal:
			return nil // the user's own mark or text wins
		}
		return upsertRoleInstructionTx(tx, project, RoleWorker, "", SourceImported, hash, now)
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

// RoleFileRemoval is one role an import moved to not configured, or one
// whose local override stood against the move (§5.9): exactly one of
// Removed and Kept is set.
type RoleFileRemoval struct {
	Role string
	// Removed is the imported record that was deleted.
	Removed *RoleInstruction
	// Kept is the local record that kept its text against the dropped
	// key, the ErrRoleTextKept case of a role file the file no longer
	// names.
	Kept *RoleInstruction
}

// Change is the import-result line of one removal: an applied move to
// not configured, or the conflict of a local override that kept its
// text while the file dropped the role's key.
func (r RoleFileRemoval) Change() ConfigChange {
	if r.Kept != nil {
		return ConfigChange{Key: "instructions." + r.Role, Old: strconv.Quote(r.Kept.Text), New: RoleStateNotConfigured}
	}
	old := "none"
	if r.Removed != nil {
		old = instructionsSummary(r.Removed.Text)
	}
	return ConfigChange{Key: "instructions." + r.Role, Old: old, New: RoleStateNotConfigured}
}

// UnimportRoleFiles moves to not configured (§5.9 'Three states, no
// hidden fallback') every non-worker role whose key the request no
// longer configures: configured holds the roles the request configures —
// the keys project.yaml's instructions map still names — and any other
// role's imported row is removed, so dropping a key reaches not
// configured at the next import instead of pinning the last import's
// text forever. The worker is not this function's: its text is the
// instructions setting and imports with project.yaml itself
// (ImportProjectConfigWith), which removes its imported row the same
// way. A local override wins (§13.1): the row keeps its text and is
// returned as Kept, the caller's conflict, as ErrRoleTextKept is for a
// text the file still carries. The upgrade's seed of a default (source
// generated) is no import: it stands until an import owns the role, so
// a project that upgraded without rerunning init keeps its guidance.
func (e *Engine) UnimportRoleFiles(ctx context.Context, repoURL string, configured map[string]bool) ([]RoleFileRemoval, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	var out []RoleFileRemoval
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // nothing was ever imported; there is no row to remove
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		for _, role := range RoleKeys {
			if role == RoleWorker || configured[role] {
				continue
			}
			cur, err := roleInstructionTx(tx, project, "", role)
			if err != nil {
				return err
			}
			switch cur.Source {
			case SourceDefault:
				// No row: already not configured.
			case SourceLocal:
				// A local override wins over the key the file dropped:
				// its text stays and the import reports the conflict.
				out = append(out, RoleFileRemoval{Role: role, Kept: &cur})
			case SourceGenerated:
				// The upgrade's seed, never an import: it stands.
			default:
				if _, err := tx.Exec("DELETE FROM role_instructions WHERE project_id = ? AND role = ?", project, role); err != nil {
					return fmt.Errorf("clear the imported %s instructions of project %d: %w", role, project, err)
				}
				if _, err := appendEventTx(tx, evRoleInstructionSet, project, 0, 0,
					eventPayload{Reason: fmt.Sprintf("%s instructions removed, key gone from the imported file", role)}, now); err != nil {
					return err
				}
				out = append(out, RoleFileRemoval{Role: role, Removed: &cur})
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalidSetting) {
			return nil, err
		}
		return nil, fmt.Errorf("unimport the role instructions of %s: %w", RedactRepoURL(repoURL), err)
	}
	return out, nil
}

// ErrRoleTextKept reports a role-file import refused because a local
// override stands under the role's text: it keeps its value and the
// user resolves the two (§5.9: neither wins silently). The record
// returned beside it is the kept local one.
var ErrRoleTextKept = errors.New("a local override kept its value")

// roleImportVersion is the settings version an import of a role file
// records: the hash of the role and its text, so a change to either
// shows as a changed import.
func roleImportVersion(role, text string) string {
	return ConfigHash([]byte("\x00" + role + "\x00" + text))
}

// ImportRoleFile imports the text of a role file project.yaml's nested
// instructions map names (§5.9), read and sent by the client, never a
// path for the engine to read. It covers the foreman, plan_review and
// reviewer roles; the worker's file is the instructions setting and
// imports with project.yaml itself (ImportProjectConfigWith), so the
// setting and the worker row stay one text. The text is bounded like a
// set of it and never truncated. An applied import records the imported
// source and its hash as the settings version; an empty file is the
// empty-by-choice state, imported. A local override wins: the import is
// refused with ErrRoleTextKept and the kept local record returned.
func (e *Engine) ImportRoleFile(ctx context.Context, repoURL, role, text string) (*RoleInstruction, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	if err := ValidateRoleText(role, text); err != nil {
		return nil, err
	}
	if role == RoleWorker {
		return nil, fmt.Errorf("%w: the worker file imports as the instructions setting, with project.yaml", ErrInvalidSetting)
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
		cur, err := roleInstructionTx(tx, project, shownRepo, role)
		if err != nil {
			return err
		}
		if cur.Source == SourceLocal {
			// A local override wins over the file: the import is
			// refused whole and the local value stays.
			out = &cur
			return ErrRoleTextKept
		}
		if err := upsertRoleInstructionTx(tx, project, role, text, SourceImported, roleImportVersion(role, text), now); err != nil {
			return err
		}
		if _, err := appendEventTx(tx, evRoleInstructionSet, project, 0, 0,
			eventPayload{Reason: fmt.Sprintf("%s instructions imported, %d bytes", role, len(text))}, now); err != nil {
			return err
		}
		v, err := roleInstructionTx(tx, project, shownRepo, role)
		if err != nil {
			return err
		}
		out = &v
		return nil
	})
	switch {
	case errors.Is(err, ErrRoleTextKept):
		return out, fmt.Errorf("%w: %s instructions keep the local text %s; resolve the two",
			ErrRoleTextKept, role, instructionsSummary(out.Text))
	case errors.Is(err, ErrInvalidSetting):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("import the %s instructions of the project of %s: %w", role, RedactRepoURL(repoURL), err)
	}
	return out, nil
}
