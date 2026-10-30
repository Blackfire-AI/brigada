package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ImportProject is one whole import (§13.1): the project.yaml's settings
// — skills_dir included — the worker file's text as the instructions
// setting, every other role file the nested instructions map names (a
// key the file dropped moves its role to not configured), and the skills
// under skills_dir, all in one store transaction. A refusal or an error
// at any step rolls the whole import back and every previous value stays
// in effect (§5.9: an import is one step, never a half-applied one); the
// API reports the error as it does a config refusal alone. A local
// override that keeps its text is not a refusal: the conflict is
// reported in the result and the rest of the import commits.
//
// instructions is the worker file's text, read by the client from
// instructions_file or the nested map's worker entry — whichever the file
// names, and roles[worker] stands in when only the map names it. roles
// holds each other role's file text. skills, when not nil, replaces the
// project's set; an empty set drops them all.
func (e *Engine) ImportProject(ctx context.Context, repoURL string, content []byte, instructions *string, roles map[string]string, skills *[]Skill, accept bool) (ImportResult, error) {
	if repoURL == "" {
		return ImportResult{}, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	for _, role := range RoleKeys {
		if text, ok := roles[role]; ok {
			if err := ValidateRoleText(role, text); err != nil {
				return ImportResult{}, err
			}
		}
	}
	for role := range roles {
		if err := ValidateRoleText(role, ""); err != nil {
			return ImportResult{}, err
		}
	}
	var skillTexts []Skill
	var skillNames []string
	if skills != nil {
		var err error
		skillTexts, skillNames, err = validateSkills(*skills)
		if err != nil {
			return ImportResult{}, err
		}
	}
	// The worker's file is the instructions setting (§5.9): its text
	// rides the project.yaml import itself, so the setting and the
	// worker row stay one text.
	if text, ok := roles[RoleWorker]; ok && instructions == nil {
		instructions = &text
	}
	imp, err := parseProjectImport(repoURL, content, instructions)
	if err != nil {
		return ImportResult{}, err
	}
	var res ImportResult
	err = e.store.Update(ctx, func(tx *sql.Tx) error {
		res, err = e.importProjectConfigTx(tx, repoURL, imp, accept)
		if err != nil {
			return fmt.Errorf("import the project configuration of %s: %w", RedactRepoURL(repoURL), err)
		}
		if res.NeedsConfirm {
			// Nothing is written before the user accepts the replacement;
			// the caller repeats the import with accept set.
			return nil
		}
		configured := map[string]bool{}
		for role := range roles {
			configured[role] = true
		}
		removals, err := e.unimportRoleFilesTx(tx, repoURL, configured)
		if err != nil {
			if errors.Is(err, ErrInvalidSetting) {
				return err
			}
			return fmt.Errorf("unimport the role instructions of %s: %w", RedactRepoURL(repoURL), err)
		}
		for _, rm := range removals {
			// A role whose key the file dropped moves to not
			// configured; a local override under it keeps its text
			// and shows as the conflict (§5.9).
			if rm.Kept != nil {
				res.Conflicts = append(res.Conflicts, rm.Change())
			} else {
				res.Applied = append(res.Applied, rm.Change())
			}
		}
		for _, role := range RoleKeys {
			text, ok := roles[role]
			if !ok || role == RoleWorker {
				continue
			}
			kept, err := e.importRoleFileTx(tx, repoURL, imp.canon, role, text)
			switch {
			case errors.Is(err, ErrRoleTextKept):
				// The local text stays; the conflict shows both texts (§5.9).
				old := "local override"
				if kept != nil {
					old = kept.Text
				}
				res.Conflicts = append(res.Conflicts, ConfigChange{Key: "instructions." + role, Old: strconv.Quote(old), New: strconv.Quote(text)})
			case errors.Is(err, ErrInvalidSetting):
				return err
			case err != nil:
				return fmt.Errorf("import the %s instructions of the project of %s: %w", role, RedactRepoURL(repoURL), err)
			}
		}
		if skills != nil {
			if err := e.importSkillsTx(tx, repoURL, skillTexts, skillNames); err != nil {
				if errors.Is(err, ErrInvalidSetting) {
					return err
				}
				return fmt.Errorf("import the skills of the project of %s: %w", RedactRepoURL(repoURL), err)
			}
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, err
	}
	return res, nil
}
