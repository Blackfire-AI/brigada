package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/defaults"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// Per-role instruction texts (design §5.9): the project's texts for the
// four roles a prompt is composed for. Each lives in its own
// role_instructions row with its provenance — source, settings version
// and digest — beside the text, the way a task records its copy.

// The roles a project holds an instruction text for.
const (
	RoleForeman    = "foreman"
	RolePlanReview = "plan_review"
	RoleWorker     = "worker"
	RoleReviewer   = "reviewer"
)

// Roles lists the role names, in display order.
var Roles = []string{RoleForeman, RolePlanReview, RoleWorker, RoleReviewer}

// The three states of a role's text (§5.9): not configured — no user
// value, so the shipped default, or the upgrade's seed of it, applies;
// empty by choice — the user set no text and none applies; file — a
// text the user set or imported.
const (
	RoleTextNotConfigured = "not configured"
	RoleTextEmptyByChoice = "empty by choice"
	RoleTextFile          = "file"
)

// MaxRoleTextBytes bounds one role's instruction text (§5.9), the same
// starting value as the project instructions: above it a set or an
// import is refused, never truncated.
const MaxRoleTextBytes = 8192

// ErrRoleTextKept reports an import the caller could see refused: a
// local override stands under the role's text, keeps its value, and the
// user resolves the two (§5.9: neither wins silently). The record
// returned beside it is the kept local one.
var ErrRoleTextKept = errors.New("a local override kept its value")

// RoleInstructions is one role's instruction text and its provenance.
// Text is the effective text: the default when the role is not
// configured, nothing when it is empty by choice, the stored text when
// it is a file.
type RoleInstructions struct {
	Role    string `json:"role"`
	State   string `json:"state"`
	Text    string `json:"text,omitempty"`
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
	Bytes   int    `json:"bytes"`
	Digest  string `json:"digest,omitempty"`
}

// roleTextMeta is the provenance a role_instructions row records beside
// its text (role_instructions.meta).
type roleTextMeta struct {
	// Source is where the text came from: SourceLocal, SourceImported,
	// or the 0064 seed's 'generated from <version>, by upgrade'.
	Source string `json:"source"`
	// Version is the settings version the text was resolved from: the
	// defaults version for a seeded text, the import hash for an
	// imported one; empty for a local override.
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest"`
	Bytes   int    `json:"bytes"`
}

// ValidateRoleText checks a role text the way the engine does: a known
// role, valid UTF-8, no NUL, at most MaxRoleTextBytes. The refusal
// names the role, the size and the limit.
func ValidateRoleText(role, text string) error {
	if !slices.Contains(Roles, role) {
		return fmt.Errorf("%w: unknown role %q; valid roles: %s", ErrInvalidSetting, role, strings.Join(Roles, ", "))
	}
	if len(text) > MaxRoleTextBytes {
		return fmt.Errorf("%w: %s instructions are %d bytes, over the %d byte limit; shorten them (nothing is truncated)",
			ErrInvalidSetting, role, len(text), MaxRoleTextBytes)
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: %s instructions are not valid UTF-8", ErrInvalidSetting, role)
	}
	if strings.ContainsRune(text, 0) {
		return fmt.Errorf("%w: %s instructions hold a NUL byte", ErrInvalidSetting, role)
	}
	return nil
}

// RoleInstructions reads one role's instruction text for the repo's
// project, with its state and provenance. A repo with no project has
// none: nil, nil.
func (e *Engine) RoleInstructions(ctx context.Context, repoURL, role string) (*RoleInstructions, error) {
	if !slices.Contains(Roles, role) {
		return nil, fmt.Errorf("%w: unknown role %q; valid roles: %s", ErrInvalidSetting, role, strings.Join(Roles, ", "))
	}
	if repoURL == "" {
		return nil, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	var out *RoleInstructions
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		out, err = roleInstructionsTx(tx, project, role)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the %s instructions of the project of %s: %w", role, RedactRepoURL(repoURL), err)
	}
	return out, nil
}

// roleInstructionsTx reads one role's effective instruction text for a
// project, with its state and provenance: the stored row, or the live
// embedded default when the project has none.
func roleInstructionsTx(tx *sql.Tx, project int64, role string) (*RoleInstructions, error) {
	var text, rawMeta string
	err := tx.QueryRow("SELECT text, meta FROM role_instructions WHERE project_id = ? AND role = ?", project, role).Scan(&text, &rawMeta)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not configured: no row, so the live embedded default
		// applies (§5.9).
		live, err := defaults.LiveRoleText(role)
		if err != nil {
			return nil, err
		}
		return &RoleInstructions{Role: role, State: RoleTextNotConfigured, Text: live, Source: SourceDefault,
			Version: defaults.Version, Bytes: len(live), Digest: instructionsDigest(live)}, nil
	case err != nil:
		return nil, fmt.Errorf("read the %s instructions of project %d: %w", role, project, err)
	}
	var meta roleTextMeta
	if rawMeta != "" {
		if err := json.Unmarshal([]byte(rawMeta), &meta); err != nil {
			return nil, fmt.Errorf("parse the %s instructions of project %d: %w", role, project, err)
		}
	}
	out := &RoleInstructions{Role: role, Text: text, Source: meta.Source, Version: meta.Version, Bytes: meta.Bytes, Digest: meta.Digest}
	switch {
	case strings.HasPrefix(meta.Source, store.RoleSourceSeededPrefix),
		meta.Source == "" && text == "":
		// The upgrade's seed is no user value: the role is not
		// configured, and the seeded default stays in effect.
		out.State = RoleTextNotConfigured
	case text == "":
		out.State = RoleTextEmptyByChoice
	default:
		out.State = RoleTextFile
	}
	return out, nil
}

// SetRoleInstructions sets one role's instruction text for the repo's
// project, from a file the client read or text the user gave. An empty
// text is the empty-by-choice state, not a return to the default. A
// refusal — over the bound, not UTF-8, a NUL — names the role and
// truncates nothing: the previous value stays in effect.
func (e *Engine) SetRoleInstructions(ctx context.Context, repoURL, role, text string) (*RoleInstructions, error) {
	if err := ValidateRoleText(role, text); err != nil {
		return nil, err
	}
	if repoURL == "" {
		return nil, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	if _, cerr := Canonicalize(repoURL); cerr != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSetting, cerr)
	}
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, "", "", now)
			if err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		if err := activeProjectTx(tx, project); err != nil {
			return err
		}
		meta, err := json.Marshal(roleTextMeta{Source: SourceLocal, Digest: instructionsDigest(text), Bytes: len(text)})
		if err != nil {
			return fmt.Errorf("encode the %s instructions of project %d: %w", role, project, err)
		}
		if _, err := tx.Exec(`INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, ?, ?, ?)
			ON CONFLICT (project_id, role) DO UPDATE SET text = excluded.text, meta = excluded.meta`,
			project, role, text, string(meta)); err != nil {
			return fmt.Errorf("store the %s instructions of project %d: %w", role, project, err)
		}
		_, err = appendEventTx(tx, evProjectSettingSet, project, 0, 0,
			eventPayload{Reason: fmt.Sprintf("instructions %s: %s", role, instructionsSummary(text))}, now)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInvalidSetting) {
			return nil, err
		}
		return nil, fmt.Errorf("set the %s instructions of the project of %s: %w", role, RedactRepoURL(repoURL), err)
	}
	ri, err := e.RoleInstructions(ctx, repoURL, role)
	if err != nil {
		return nil, err
	}
	if ri == nil {
		return nil, fmt.Errorf("read back the %s instructions the set just wrote", role)
	}
	return ri, nil
}

// roleImportVersion is the settings version an import records beside a
// role's text: the hash of the imported content with the role and its
// text, so a change to either shows as a changed import — the same
// shape of hash the project-config import versions its file with.
func roleImportVersion(role, text string) string {
	return ConfigHash([]byte("\x00" + role + "\x00" + text))
}

// ImportRoleInstructions imports a role's instruction text (§5.9): the
// text a file held, read and sent by the client, never a path for the
// engine to read. The text is bounded like a set of it, and a refusal
// — over the bound, not UTF-8, a NUL — truncates nothing and leaves the
// previous value in effect. An applied import records the imported
// source and the import's hash as the settings version, and a changed
// import over a still-imported value applies; an empty text is an unset
// value, taking the role back to not configured. A local override wins:
// the import is refused with ErrRoleTextKept and the kept local record
// returned beside it.
func (e *Engine) ImportRoleInstructions(ctx context.Context, repoURL, role, text string) (*RoleInstructions, error) {
	if !slices.Contains(Roles, role) {
		return nil, fmt.Errorf("%w: unknown role %q; valid roles: %s", ErrInvalidSetting, role, strings.Join(Roles, ", "))
	}
	if repoURL == "" {
		return nil, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	if _, cerr := Canonicalize(repoURL); cerr != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSetting, cerr)
	}
	if text != "" {
		if err := ValidateRoleText(role, text); err != nil {
			return nil, err
		}
	}
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, "", "", now)
			if err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		if err := activeProjectTx(tx, project); err != nil {
			return err
		}
		var cur, curMeta string
		err = tx.QueryRow("SELECT text, meta FROM role_instructions WHERE project_id = ? AND role = ?", project, role).Scan(&cur, &curMeta)
		switch {
		case errors.Is(err, sql.ErrNoRows), err == nil:
		default:
			return fmt.Errorf("read the %s instructions of project %d: %w", role, project, err)
		}
		if err == nil && curMeta != "" {
			var meta roleTextMeta
			if err := json.Unmarshal([]byte(curMeta), &meta); err != nil {
				return fmt.Errorf("parse the %s instructions of project %d: %w", role, project, err)
			}
			if meta.Source == SourceLocal {
				// A local override wins over the file (§13.1): the
				// import is refused whole and the local value stays.
				return ErrRoleTextKept
			}
		}
		switch {
		case text == "":
			// An empty file is an unset value (§5.9): the role returns
			// to not configured, the shipped default applying again.
			if _, err := tx.Exec("DELETE FROM role_instructions WHERE project_id = ? AND role = ?", project, role); err != nil {
				return fmt.Errorf("unset the %s instructions of project %d: %w", role, project, err)
			}
		default:
			meta, err := json.Marshal(roleTextMeta{Source: SourceImported, Version: roleImportVersion(role, text),
				Digest: instructionsDigest(text), Bytes: len(text)})
			if err != nil {
				return fmt.Errorf("encode the %s instructions of project %d: %w", role, project, err)
			}
			if _, err := tx.Exec(`INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, ?, ?, ?)
				ON CONFLICT (project_id, role) DO UPDATE SET text = excluded.text, meta = excluded.meta`,
				project, role, text, string(meta)); err != nil {
				return fmt.Errorf("import the %s instructions of project %d: %w", role, project, err)
			}
		}
		_, err = appendEventTx(tx, evProjectSettingSet, project, 0, 0,
			eventPayload{Reason: fmt.Sprintf("instructions %s: %s (imported)", role, instructionsSummary(text))}, now)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrRoleTextKept) {
			kept, kerr := e.RoleInstructions(ctx, repoURL, role)
			if kerr != nil || kept == nil {
				return nil, err
			}
			return kept, fmt.Errorf("%w: %s instructions keep the local text %s; resolve the two",
				ErrRoleTextKept, role, instructionsSummary(kept.Text))
		}
		if errors.Is(err, ErrInvalidSetting) {
			return nil, err
		}
		return nil, fmt.Errorf("import the %s instructions of the project of %s: %w", role, RedactRepoURL(repoURL), err)
	}
	ri, err := e.RoleInstructions(ctx, repoURL, role)
	if err != nil {
		return nil, err
	}
	if ri == nil {
		return nil, fmt.Errorf("read back the %s instructions the import just wrote", role)
	}
	return ri, nil
}
