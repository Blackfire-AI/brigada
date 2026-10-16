package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The project settings the user can edit (§6.3 "Configurable per
// project"). One read/write path serves the CLI and the approval limit:
// SetProjectSettings validates a value with the parser below and stores
// it in the shape approvalSettings reads, so a value the CLI accepts is
// one the limit understands.
const (
	SettingApprovalMaxTasks       = "approval_max_tasks"
	SettingApprovalClasses        = "approval_classes"
	SettingApprovalProtectedPaths = "approval_protected_paths"
	SettingDefaultCheck           = "default_check"
)

// SettingKeys are the editable keys, in the order show prints them.
var SettingKeys = []string{SettingApprovalMaxTasks, SettingApprovalClasses, SettingApprovalProtectedPaths, SettingDefaultCheck}

// settingListSep separates the items of a list value on the command line.
const settingListSep = "+"

// ErrInvalidSetting marks a refusal the caller caused: an unknown key or
// a value the key does not take. The API turns it into a 400.
var ErrInvalidSetting = errors.New("invalid project setting")

// SettingValue is one key's effective value: Value is the +-separated
// (or integer) rendering, Default says the project does not override it.
type SettingValue struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Default bool   `json:"default"`
}

// ProjectSettings is a project's editable settings, every key with its
// effective value.
type ProjectSettings struct {
	ProjectID int64          `json:"project_id"`
	RepoURL   string         `json:"repo_url"`
	Settings  []SettingValue `json:"settings"`
}

var classWord = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseSettingValue validates one value and returns the JSON the
// settings row stores for it. An empty value returns nil: the key is
// removed and the default applies again.
// Every refusal names the valid keys.
func parseSettingValue(key, value string) (json.RawMessage, error) {
	switch key {
	case SettingApprovalMaxTasks, SettingApprovalClasses, SettingApprovalProtectedPaths, SettingDefaultCheck:
	default:
		return nil, fmt.Errorf("%w: unknown key %q; valid keys: %s", ErrInvalidSetting, key, strings.Join(SettingKeys, ", "))
	}
	raw, err := parseKnownValue(key, value)
	if err != nil {
		return nil, fmt.Errorf("%w; valid keys: %s", err, strings.Join(SettingKeys, ", "))
	}
	return raw, nil
}

// parseKnownValue validates the value of a key already known to be valid.
func parseKnownValue(key, value string) (json.RawMessage, error) {
	if value == "" {
		return nil, nil
	}
	if key == SettingDefaultCheck {
		return json.Marshal(strings.TrimSpace(value))
	}
	if key == SettingApprovalMaxTasks {
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("%w: %s takes an integer (0 = every plan waits, negative = off), got %q",
				ErrInvalidSetting, key, value)
		}
		return json.RawMessage(strconv.Itoa(n)), nil
	}
	var items []string
	for _, item := range strings.Split(value, settingListSep) {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("%w: %s is a %s-separated list with no empty items, got %q",
				ErrInvalidSetting, key, settingListSep, value)
		}
		if key == SettingApprovalClasses && !classWord.MatchString(item) {
			return nil, fmt.Errorf("%w: %s item %q is not a class name (letters, digits, _ . -)",
				ErrInvalidSetting, key, item)
		}
		if key == SettingApprovalProtectedPaths {
			if _, err := filepath.Match(item, "x"); err != nil {
				return nil, fmt.Errorf("%w: %s item %q is not a valid glob: %v", ErrInvalidSetting, key, item, err)
			}
		}
		items = append(items, item)
	}
	b, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", key, err)
	}
	return b, nil
}

// ValidateSettingValue reports whether the key and value would be
// accepted, so the CLI can refuse before it contacts the engine.
func ValidateSettingValue(key, value string) error {
	_, err := parseSettingValue(key, value)
	return err
}

// settingsViewTx renders the project's settings row: each key's
// effective value and whether the row overrides it.
func settingsViewTx(tx *sql.Tx, project int64, repo string) (ProjectSettings, error) {
	var raw string
	if err := tx.QueryRow("SELECT settings FROM projects WHERE id = ?", project).Scan(&raw); err != nil {
		return ProjectSettings{}, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	present := map[string]json.RawMessage{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &present); err != nil {
			return ProjectSettings{}, fmt.Errorf("parse the settings of project %d: %w", project, err)
		}
	}
	eff, err := projectApprovalTx(tx, project)
	if err != nil {
		return ProjectSettings{}, err
	}
	list := func(l []string) string {
		if len(l) == 0 {
			return "none"
		}
		return strings.Join(l, settingListSep)
	}
	out := ProjectSettings{ProjectID: project, RepoURL: repo}
	for _, k := range SettingKeys {
		sv := SettingValue{Key: k}
		_, overridden := present[k]
		sv.Default = !overridden
		switch k {
		case SettingApprovalMaxTasks:
			sv.Value = strconv.Itoa(*eff.MaxTasks)
		case SettingApprovalClasses:
			sv.Value = list(eff.Classes)
		case SettingApprovalProtectedPaths:
			sv.Value = list(eff.ProtectedPaths)
		case SettingDefaultCheck:
			sv.Value = eff.DefaultCheck
			if sv.Value == "" {
				sv.Value = "none"
			}
		}
		out.Settings = append(out.Settings, sv)
	}
	return out, nil
}

// ProjectSettings reads the project's settings; nil when the repo has
// no project.
func (e *Engine) ProjectSettings(ctx context.Context, repoURL string) (*ProjectSettings, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("repo URL is required (product §5 rule 6: repos are addressed by URL)")
	}
	var out *ProjectSettings
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := e.sweepTx(tx, e.cfg.Now()); err != nil {
			return err
		}
		var project int64
		var canon string
		err := lookupProjectTx(tx, repoURL, "id, repo_url", &project, &canon)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		v, err := settingsViewTx(tx, project, canon)
		if err != nil {
			return err
		}
		out = &v
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the settings of the project of %s: %w", RedactRepoURL(repoURL), err)
	}
	return out, nil
}

// SetProjectSettings applies key=value pairs to the repo's project, all
// or nothing: every pair is validated first. An empty value resets the
// key to its default. Keys the settings row holds that this call does
// not name are kept. A repo with no project gets one, born by setup like
// the foreman choice's. Returns the settings as they now stand.
func (e *Engine) SetProjectSettings(ctx context.Context, repoURL string, values map[string]string) (ProjectSettings, error) {
	if repoURL == "" {
		return ProjectSettings{}, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	if len(values) == 0 {
		return ProjectSettings{}, fmt.Errorf("%w: no key=value given; valid keys: %s", ErrInvalidSetting, strings.Join(SettingKeys, ", "))
	}
	canon, cerr := Canonicalize(repoURL)
	if cerr != nil {
		return ProjectSettings{}, fmt.Errorf("%w: %v", ErrInvalidSetting, cerr)
	}
	parsed := map[string]json.RawMessage{}
	for k, v := range values {
		raw, err := parseSettingValue(k, v)
		if err != nil {
			return ProjectSettings{}, err
		}
		parsed[k] = raw
	}
	var out ProjectSettings
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		var rawSettings, shownRepo string
		err := lookupProjectTx(tx, repoURL, "id, settings, repo_url", &project, &rawSettings, &shownRepo)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, "", "", now)
			if err != nil {
				return err
			}
			rawSettings, shownRepo = "", canon
		case err != nil:
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		row := map[string]json.RawMessage{}
		if rawSettings != "" {
			if err := json.Unmarshal([]byte(rawSettings), &row); err != nil {
				return fmt.Errorf("parse the settings of project %d: %w", project, err)
			}
		}
		for k, raw := range parsed {
			if raw == nil {
				delete(row, k)
			} else {
				row[k] = raw
			}
		}
		b, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("encode the settings of project %d: %w", project, err)
		}
		if _, err := tx.Exec("UPDATE projects SET settings = ?, updated_at = ? WHERE id = ?", string(b), fmtTime(now), project); err != nil {
			return fmt.Errorf("store the settings of project %d: %w", project, err)
		}
		out, err = settingsViewTx(tx, project, shownRepo)
		return err
	})
	if err != nil {
		return ProjectSettings{}, fmt.Errorf("set the settings of the project of %s: %w", RedactRepoURL(repoURL), err)
	}
	return out, nil
}
