package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/adapter"
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
	// The gates and land rules (§6.3). gate_plan is enforced today: ask
	// holds every plan at the plan-approval gate. The others are stored
	// and shown only, until the land job reads them.
	SettingGatePlan          = "gate_plan"
	SettingGateLand          = "gate_land"
	SettingGatePush          = "gate_push"
	SettingApprovalTiers     = "approval_tiers"
	SettingApprovalRestarted = "approval_restarted"
	SettingLandTarget        = "land_target"
	SettingAfterLand         = "after_land"
	SettingDefaultCheck      = "default_check"
	SettingWorkerTools       = "worker_tools"
	// The environment step (§5.7): the command, the repo-relative files
	// whose contents key its cache, and its time bound in minutes. Schema
	// 1 of the shared file is flat, so the three are sibling keys.
	SettingSetup              = "setup"
	SettingSetupCacheKeyFiles = "setup_cache_key_files"
	SettingSetupTimeout       = "setup_timeout_minutes"
	// The project's daily foreman wake budget (§4.3): headless foreman
	// turns per UTC day. Local only — the shared project.yaml refuses
	// it — and set through `brigada budget --wakes N`, which also
	// re-evaluates the waiting wake.
	SettingForemanWakesPerDay = "foreman_wakes_per_day"
	// The project's instruction text for every worker and reviewer
	// (§5.9). Stored as the text itself: the client reads the file named
	// by project.yaml's instructions_file and sends the content.
	SettingInstructions = "instructions"
)

// MaxForemanWakesPerDay bounds a set wake budget (§4.3): a sane ceiling
// on the day's foreman turns, so a typo cannot buy a thousand-turn day.
const MaxForemanWakesPerDay = 1000

// The values a gate takes (§6.3): auto runs on, ask waits for the user;
// the push gate may also be never, which lands locally.
const (
	GateAuto  = "auto"
	GateAsk   = "ask"
	GateNever = "never"
)

// SettingKeys are the editable keys, in the order show prints them.
var SettingKeys = []string{SettingApprovalMaxTasks, SettingApprovalClasses, SettingApprovalProtectedPaths,
	SettingGatePlan, SettingGateLand, SettingGatePush, SettingApprovalTiers, SettingApprovalRestarted, SettingLandTarget, SettingAfterLand,
	SettingDefaultCheck, SettingWorkerTools, SettingSetup, SettingSetupCacheKeyFiles, SettingSetupTimeout,
	SettingExtraRepos, SettingForemanWakesPerDay, SettingInstructions}

// MaxInstructionsBytes bounds the project instruction text (§5.9): a
// starting value, open in §16 item 11. Above it a set or an import is
// refused, never truncated.
const MaxInstructionsBytes = 8192

// ValidateInstructions checks an instruction text the way the engine does:
// valid UTF-8, no NUL, at most MaxInstructionsBytes. The refusal names
// the size and the limit.
func ValidateInstructions(text string) error {
	if len(text) > MaxInstructionsBytes {
		return fmt.Errorf("%w: %s is %d bytes, over the %d byte limit; shorten it (nothing is truncated)",
			ErrInvalidSetting, SettingInstructions, len(text), MaxInstructionsBytes)
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidSetting, SettingInstructions)
	}
	if strings.ContainsRune(text, 0) {
		return fmt.Errorf("%w: %s holds a NUL byte", ErrInvalidSetting, SettingInstructions)
	}
	return nil
}

// instructionsDigest is the SHA-256 of an instruction text, hex.
func instructionsDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// instructionsSummary is how the settings view and the foreman's status
// name a stored text: its size and a short digest, never the text.
func instructionsSummary(text string) string {
	if text == "" {
		return "none"
	}
	return fmt.Sprintf("%d bytes, sha256 %s", len(text), instructionsDigest(text)[:12])
}

// settingListSep separates the items of a list value on the command line.
const settingListSep = "+"

// validBranchName reports whether name is a branch name git takes, by
// check-ref-format's rules spelled in Go: the engine runs no git (product
// §5 rule 1), so the check is its own. A branch additionally cannot begin
// with a dash, which git reads as an option.
func validBranchName(name string) bool {
	if name == "" || name == "@" || strings.HasPrefix(name, "-") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	return true
}

// ErrInvalidSetting marks a refusal the caller caused: an unknown key or
// a value the key does not take. The API turns it into a 400.
var ErrInvalidSetting = errors.New("invalid project setting")

// Where a setting's value comes from (§13.1 precedence): a local override
// the user set, the last imported project.yaml, or the built-in default.
const (
	SourceDefault  = "default"
	SourceLocal    = "local"
	SourceImported = "imported"
)

// SettingValue is one key's effective value: Value is the +-separated
// (or integer) rendering, Default says no override or import sets it.
// Source is its provenance; Hash is the hash of the imported file the
// value came from, set only for an imported value.
type SettingValue struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Default bool   `json:"default"`
	Source  string `json:"source"`
	Hash    string `json:"hash,omitempty"`
}

// ProjectSettings is a project's editable settings, every key with its
// effective value.
type ProjectSettings struct {
	ProjectID int64          `json:"project_id"`
	RepoURL   string         `json:"repo_url"`
	Settings  []SettingValue `json:"settings"`
	// ImportHash is the hash of the project.yaml content last imported;
	// empty when none was.
	ImportHash string `json:"import_hash,omitempty"`
}

var classWord = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseSettingValue validates one value and returns the JSON the
// settings row stores for it. An empty value returns nil: the key is
// removed and the default applies again.
// Every refusal names the valid keys.
func parseSettingValue(key, value string) (json.RawMessage, error) {
	switch key {
	case SettingApprovalMaxTasks, SettingApprovalClasses, SettingApprovalProtectedPaths,
		SettingGatePlan, SettingGateLand, SettingGatePush, SettingApprovalTiers, SettingApprovalRestarted, SettingLandTarget, SettingAfterLand,
		SettingDefaultCheck, SettingWorkerTools,
		SettingSetup, SettingSetupCacheKeyFiles, SettingSetupTimeout, SettingExtraRepos, SettingForemanWakesPerDay, SettingInstructions:
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
	if key == SettingDefaultCheck || key == SettingSetup || key == SettingAfterLand {
		return json.Marshal(strings.TrimSpace(value))
	}
	if key == SettingGatePlan || key == SettingGateLand {
		v := strings.TrimSpace(value)
		if v != GateAuto && v != GateAsk {
			return nil, fmt.Errorf("%w: %s takes %s or %s, got %q", ErrInvalidSetting, key, GateAuto, GateAsk, value)
		}
		return json.Marshal(v)
	}
	if key == SettingGatePush {
		v := strings.TrimSpace(value)
		if v != GateAuto && v != GateAsk && v != GateNever {
			return nil, fmt.Errorf("%w: %s takes %s, %s or %s, got %q", ErrInvalidSetting, key, GateAuto, GateAsk, GateNever, value)
		}
		return json.Marshal(v)
	}
	if key == SettingApprovalRestarted {
		v := strings.TrimSpace(value)
		if v != "true" && v != "false" {
			return nil, fmt.Errorf("%w: %s takes true or false, got %q", ErrInvalidSetting, key, value)
		}
		return json.Marshal(v == "true")
	}
	if key == SettingLandTarget {
		v := strings.TrimSpace(value)
		if !validBranchName(v) {
			return nil, fmt.Errorf("%w: %s takes a branch name, got %q", ErrInvalidSetting, key, value)
		}
		return json.Marshal(v)
	}
	if key == SettingInstructions {
		if strings.TrimSpace(value) == "" {
			return nil, nil // an empty text is an unset value
		}
		if err := ValidateInstructions(value); err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	if key == SettingWorkerTools {
		tools, err := adapter.ParseToolScope(value)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrInvalidSetting, key, err)
		}
		return json.Marshal(toolScopeText(tools))
	}
	if key == SettingApprovalMaxTasks {
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("%w: %s takes an integer (0 = every plan waits, negative = off), got %q",
				ErrInvalidSetting, key, value)
		}
		return json.RawMessage(strconv.Itoa(n)), nil
	}
	if key == SettingSetupTimeout {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: %s takes a positive whole number of minutes, got %q",
				ErrInvalidSetting, key, value)
		}
		return json.RawMessage(strconv.Itoa(n)), nil
	}
	if key == SettingForemanWakesPerDay {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > MaxForemanWakesPerDay {
			return nil, fmt.Errorf("%w: %s takes a positive whole number of foreman wakes per day, at most %d, got %q",
				ErrInvalidSetting, key, MaxForemanWakesPerDay, value)
		}
		return json.RawMessage(strconv.Itoa(n)), nil
	}
	var items []string
	var entries []ExtraRepo
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
		if key == SettingApprovalTiers && item != "light" && item != "standard" && item != "heavy" {
			return nil, fmt.Errorf("%w: %s item %q is not a tier (light, standard, heavy)",
				ErrInvalidSetting, key, item)
		}
		if key == SettingSetupCacheKeyFiles {
			if err := validateCacheKeyFile(item); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidSetting, err)
			}
		}
		if key == SettingExtraRepos {
			x, err := parseExtraRepoItem(item)
			if err != nil {
				return nil, err
			}
			entries = append(entries, x)
			continue
		}
		if key == SettingApprovalProtectedPaths {
			if _, err := filepath.Match(item, "x"); err != nil {
				return nil, fmt.Errorf("%w: %s item %q is not a valid glob: %v", ErrInvalidSetting, key, item, err)
			}
		}
		items = append(items, item)
	}
	if key == SettingExtraRepos {
		// Resolved here, once, so the stored default is canonical and
		// carries the URL the runner fetches from (§5.8).
		resolved, err := resolveExtraRepos(entries)
		if err != nil {
			return nil, err
		}
		return json.Marshal(resolved)
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
// effective value and whether the row overrides it. wakeBudgetDefault
// is the engine-wide wake budget (§4.3) the foreman_wakes_per_day key
// falls back to.
func settingsViewTx(tx *sql.Tx, project int64, repo string, wakeBudgetDefault int) (ProjectSettings, error) {
	var raw, rawProv string
	if err := tx.QueryRow("SELECT settings, settings_prov FROM projects WHERE id = ?", project).Scan(&raw, &rawProv); err != nil {
		return ProjectSettings{}, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	prov, err := parseProv(rawProv)
	if err != nil {
		return ProjectSettings{}, fmt.Errorf("parse the provenance of project %d: %w", project, err)
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
	// land_target's default is the project's default branch (§6.3).
	var defaultBranch string
	if err := tx.QueryRow("SELECT COALESCE(NULLIF(default_branch, ''), ?) FROM projects WHERE id = ?", setupDefaultBranch, project).Scan(&defaultBranch); err != nil {
		return ProjectSettings{}, fmt.Errorf("read the default branch of project %d: %w", project, err)
	}
	list := func(l []string) string {
		if len(l) == 0 {
			return "none"
		}
		return strings.Join(l, settingListSep)
	}
	out := ProjectSettings{ProjectID: project, RepoURL: repo, ImportHash: prov.Hash}
	for _, k := range SettingKeys {
		sv := SettingValue{Key: k, Source: SourceDefault}
		cur, has := present[k]
		sv.Default = !has
		if has {
			sv.Source = SourceLocal
			if prov.isImported(k, cur) {
				sv.Source, sv.Hash = SourceImported, prov.Hash
			}
		}
		switch k {
		case SettingApprovalMaxTasks:
			sv.Value = strconv.Itoa(*eff.MaxTasks)
		case SettingApprovalClasses:
			sv.Value = list(eff.Classes)
		case SettingApprovalProtectedPaths:
			sv.Value = list(eff.ProtectedPaths)
		case SettingGatePlan:
			sv.Value = GateAuto
			if s, ok := rawString(cur); ok && s != "" {
				sv.Value = s
			}
		case SettingGateLand:
			sv.Value = GateAuto
			if s, ok := rawString(cur); ok && s != "" {
				sv.Value = s
			}
		case SettingGatePush:
			sv.Value = GateAsk
			if s, ok := rawString(cur); ok && s != "" {
				sv.Value = s
			}
		case SettingApprovalTiers:
			sv.Value = list(eff.ApprovalTiers)
		case SettingApprovalRestarted:
			sv.Value = strconv.FormatBool(eff.ApprovalRestarted)
		case SettingLandTarget:
			sv.Value = defaultBranch
			if s, ok := rawString(cur); ok && s != "" {
				sv.Value = s
			}
		case SettingAfterLand:
			sv.Value = "none"
			if s, ok := rawString(cur); ok && s != "" {
				sv.Value = s
			}
		case SettingDefaultCheck:
			sv.Value = eff.DefaultCheck
			if sv.Value == "" {
				sv.Value = "none"
			}
		case SettingWorkerTools:
			sv.Value = "agent default"
			if s, ok := rawString(cur); ok {
				sv.Value = s
			}
		case SettingSetup:
			sv.Value = "none"
			if s, ok := rawString(cur); ok && s != "" {
				sv.Value = s
			}
		case SettingSetupCacheKeyFiles:
			sv.Value = "none"
			if has {
				var l stringList
				if json.Unmarshal(cur, &l) == nil && len(l) > 0 {
					sv.Value = list(l)
				}
			}
		case SettingSetupTimeout:
			sv.Value = strconv.Itoa(defaultSetupTimeoutMinutes)
			var n int
			if has && json.Unmarshal(cur, &n) == nil && n > 0 {
				sv.Value = strconv.Itoa(n)
			}
		case SettingExtraRepos:
			sv.Value = "none"
			if has {
				sv.Value = renderSettingRaw(k, cur)
			}
		case SettingForemanWakesPerDay:
			budget := wakeBudgetDefault
			if n, ok := rawInt(cur); ok && n > 0 {
				budget = n
			}
			sv.Value = strconv.Itoa(budget)
		case SettingInstructions:
			text, _ := rawString(cur)
			sv.Value = instructionsSummary(text)
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
		v, err := settingsViewTx(tx, project, canon, e.cfg.ForemanWakeBudget)
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
		var rawSettings, rawProv, shownRepo string
		err := lookupProjectTx(tx, repoURL, "id, settings, settings_prov, repo_url", &project, &rawSettings, &rawProv, &shownRepo)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, "", "", now)
			if err != nil {
				return err
			}
			rawSettings, rawProv, shownRepo = "", "", canon
		case err != nil:
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		if err := activeProjectTx(tx, project); err != nil {
			return err
		}
		row := map[string]json.RawMessage{}
		if rawSettings != "" {
			if err := json.Unmarshal([]byte(rawSettings), &row); err != nil {
				return fmt.Errorf("parse the settings of project %d: %w", project, err)
			}
		}
		prov, err := parseProv(rawProv)
		if err != nil {
			return fmt.Errorf("parse the provenance of project %d: %w", project, err)
		}
		for k, raw := range parsed {
			// A key set here is a local override from now on, even when it
			// holds the value the import set; a reset returns it to the
			// default, which no import owns either.
			delete(prov.Imported, k)
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
		pb, err := prov.encode()
		if err != nil {
			return fmt.Errorf("encode the provenance of project %d: %w", project, err)
		}
		if _, err := tx.Exec("UPDATE projects SET settings = ?, settings_prov = ?, updated_at = ? WHERE id = ?", string(b), pb, fmtTime(now), project); err != nil {
			return fmt.Errorf("store the settings of project %d: %w", project, err)
		}
		out, err = settingsViewTx(tx, project, shownRepo, e.cfg.ForemanWakeBudget)
		return err
	})
	if err != nil {
		return ProjectSettings{}, fmt.Errorf("set the settings of the project of %s: %w", RedactRepoURL(repoURL), err)
	}
	return out, nil
}
