package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The committed, nonsecret project configuration (design §13.1): the
// client reads .brigada/project.yaml and sends its bytes; the engine
// parses and validates them and never reads a repo path itself (product
// §5 rule 1). Schema 1 holds the keys below and nothing else.

// ProjectConfigPath is where the file lives, relative to the repo root.
const ProjectConfigPath = ".brigada/project.yaml"

// ProjectConfigSchema is the one schema version this build reads.
const ProjectConfigSchema = 1

// maxProjectConfigBytes bounds the content the engine takes: a settings
// file of a few keys is far below it.
const maxProjectConfigBytes = 64 << 10

const (
	configKeySchema = "schema"
	configKeyRepo   = "repo"
	// configKeyInstructionsFile names a repo-relative markdown file the
	// client reads at import (§5.9); the engine stores its text under
	// SettingInstructions and never the path to read.
	configKeyInstructionsFile = "instructions_file"
)

// ConfigHash is the hash of a project.yaml's bytes, as the store records
// it for an import and as a client compares it for status.
func ConfigHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// ConfigHashWith is ConfigHash for a file with an instructions_file: the
// hash covers the yaml and the text read from that path, so a change to
// the text alone counts as a changed file. With nil text it is ConfigHash.
func ConfigHashWith(content []byte, instructions *string) string {
	if instructions == nil {
		return ConfigHash(content)
	}
	return ConfigHash(append(append([]byte{}, content...), "\x00instructions\x00"+*instructions...))
}

// ProjectConfig is a parsed, validated schema-1 file: the repo identity
// it names and the settings it sets, each already in the stored shape.
type ProjectConfig struct {
	// Repo is the canonical identity the file names.
	Repo string
	// InstructionsFile is the repo-relative path the file names under
	// instructions_file, "" when it sets none. The client reads it; the
	// text rides beside the file's bytes at import.
	InstructionsFile string
	// Values holds the stored JSON of each setting the file sets.
	Values map[string]json.RawMessage
}

func configErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidSetting, ProjectConfigPath, fmt.Sprintf(format, args...))
}

// yamlValue is one top-level entry of the file's YAML subset.
type yamlValue struct {
	line   int
	scalar string
	quoted bool
	list   []string
	isList bool
	// flow: the list was written inline, [a, b].
	flow bool
	// blockOpen: "key:" with nothing after it, so a block list may follow.
	blockOpen bool
}

var yamlKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// configSettingKeys are the project settings schema 1 of the shared file
// may carry (§13.1): the approval rules (the task count, the plan budget,
// the classes and protected paths), the gates and land rules (§6.3),
// the default check, the worker tool policy, the environment step (§5.7),
// the extra repositories (§5.8) and the foreman session's rotation
// threshold (§4.2), and nothing else. It is its own
// list, never SettingKeys: a setting added for the local store (the wake
// budget, for one) does not become shared-file policy by being editable.
var configSettingKeys = []string{SettingApprovalMaxTasks, SettingApprovalBudget, SettingApprovalClasses, SettingApprovalProtectedPaths,
	SettingGatePlan, SettingGateLand, SettingGatePush, SettingApprovalTiers, SettingApprovalRestarted, SettingLandTarget, SettingAfterLand, SettingLandReady,
	SettingDefaultCheck, SettingWorkerTools, SettingSetup, SettingSetupCacheKeyFiles, SettingSetupTimeout,
	SettingExtraRepos, SettingForemanRotateTokens, SettingPlanFiles}

// ParseProjectConfig parses and validates a project.yaml. Anything but
// schema 1's keys is refused with the key named: an unknown key, an
// unknown schema version, a value of the wrong type, a repo that is not
// in canonical form. The YAML is a strict subset (top-level keys with a
// scalar, a flow list or a block list), the same hand-rolled kind the
// plan files use, so the engine takes no dependency for it.
func ParseProjectConfig(content []byte) (ProjectConfig, error) {
	if len(content) > maxProjectConfigBytes {
		return ProjectConfig{}, configErr("%d bytes is over the %d byte limit", len(content), maxProjectConfigBytes)
	}
	entries, order, err := parseYAMLSubset(string(content))
	if err != nil {
		return ProjectConfig{}, err
	}
	sv, ok := entries[configKeySchema]
	if !ok {
		return ProjectConfig{}, configErr("%s is required (want %s: %d)", configKeySchema, configKeySchema, ProjectConfigSchema)
	}
	if sv.isList || sv.quoted || sv.blockOpen {
		return ProjectConfig{}, configErr("%s must be the integer %d", configKeySchema, ProjectConfigSchema)
	}
	ver, err := strconv.Atoi(sv.scalar)
	if err != nil {
		return ProjectConfig{}, configErr("%s must be the integer %d, got %q", configKeySchema, ProjectConfigSchema, sv.scalar)
	}
	if ver != ProjectConfigSchema {
		return ProjectConfig{}, configErr("unknown %s version %d; this brigada reads %s %d", configKeySchema, ver, configKeySchema, ProjectConfigSchema)
	}
	valid := append([]string{configKeySchema, configKeyRepo, configKeyInstructionsFile}, configSettingKeys...)
	for _, k := range order {
		if slicesContains(valid, k) {
			continue
		}
		// A local-only setting (a key of `project set`, not of the
		// shared file) is refused by name, so the fix is on the line:
		// the file is shared with every clone, and these keys are one
		// engine's own.
		if slicesContains(SettingKeys, k) {
			return ProjectConfig{}, configErr("%s (line %d) is a local-only setting of this engine's store, not a key of the shared file; set it with %s",
				k, entries[k].line, localSettingHint(k))
		}
		return ProjectConfig{}, configErr("unknown key %q (line %d); valid keys: %s", k, entries[k].line, strings.Join(valid, ", "))
	}
	cfg := ProjectConfig{Values: map[string]json.RawMessage{}}
	rv, ok := entries[configKeyRepo]
	if !ok {
		return ProjectConfig{}, configErr("%s is required: the canonical URL of the repository", configKeyRepo)
	}
	if rv.isList || rv.blockOpen || rv.scalar == "" {
		return ProjectConfig{}, configErr("%s must be a canonical repo URL string", configKeyRepo)
	}
	canon, err := Canonicalize(rv.scalar)
	if err != nil {
		return ProjectConfig{}, configErr("%s: %v", configKeyRepo, err)
	}
	if rv.scalar != canon {
		// The file is shared: it names the identity, never a transport URL
		// (which may carry a user name) and never an arbitrary local path.
		// The one path form allowed is the canonical file remote, file:<abs>.
		return ProjectConfig{}, configErr("%s must be written in its canonical form %q (host[:port]/path, or file:<absolute path> for a file remote)", configKeyRepo, canon)
	}
	cfg.Repo = canon
	if iv, ok := entries[configKeyInstructionsFile]; ok {
		if iv.isList || iv.blockOpen || strings.TrimSpace(iv.scalar) == "" {
			return ProjectConfig{}, configErr("%s (line %d) must be a repo-relative path string; omit the key for no instructions", configKeyInstructionsFile, iv.line)
		}
		if err := checkInstructionsPath(iv.scalar); err != nil {
			return ProjectConfig{}, configErr("%s (line %d): %v", configKeyInstructionsFile, iv.line, err)
		}
		cfg.InstructionsFile = iv.scalar
	}
	for _, k := range configSettingKeys {
		v, ok := entries[k]
		if !ok {
			continue
		}
		text, err := settingText(k, v)
		if err != nil {
			return ProjectConfig{}, err
		}
		raw, err := parseSettingValue(k, text)
		if err != nil {
			return ProjectConfig{}, configErr("%s: %s", k, strings.TrimPrefix(err.Error(), ErrInvalidSetting.Error()+": "))
		}
		cfg.Values[k] = raw
	}
	return cfg, nil
}

// checkInstructionsPath refuses an instructions_file that is not a plain
// repo-relative path (§5.9). That it stays inside the repository after
// symlinks, and names a regular file, only the client can check.
func checkInstructionsPath(p string) error {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.Contains(p, `\`) || strings.ContainsRune(p, 0) {
		return fmt.Errorf("%q must be a repo-relative path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%q must stay inside the repository", p)
		}
	}
	return nil
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// localSettingHint names the command that sets one local-only setting,
// so a refusal of it in project.yaml says what to run instead.
func localSettingHint(key string) string {
	if key == SettingForemanWakesPerDay {
		return "`brigada budget --repo <url> --wakes N`"
	}
	return "`brigada project set " + key + "=<value>`"
}

// settingText turns a file value into the command-line spelling the shared
// setting parser takes, after the type checks the file adds.
func settingText(key string, v yamlValue) (string, error) {
	if v.blockOpen {
		return "", configErr("%s (line %d) has no value; omit the key to leave it at its default", key, v.line)
	}
	switch key {
	case SettingApprovalMaxTasks, SettingSetupTimeout, SettingForemanRotateTokens:
		if v.isList || v.quoted {
			return "", configErr("%s (line %d) must be an integer", key, v.line)
		}
		return nonEmpty(key, v, v.scalar)
	case SettingDefaultCheck, SettingWorkerTools, SettingSetup,
		SettingGatePlan, SettingGateLand, SettingGatePush, SettingApprovalRestarted, SettingLandTarget, SettingAfterLand, SettingLandReady,
		SettingApprovalBudget, SettingPlanFiles:
		if v.isList {
			return "", configErr("%s (line %d) must be a string, not a list", key, v.line)
		}
		return nonEmpty(key, v, v.scalar)
	default: // the list keys
		items := v.list
		if !v.isList {
			if v.scalar == "" {
				return "", configErr("%s (line %d) has no value; omit the key to leave it at its default", key, v.line)
			}
			return v.scalar, nil
		}
		if len(items) == 0 {
			return "", configErr("%s (line %d) is an empty list; omit the key to leave it at its default", key, v.line)
		}
		for _, it := range items {
			if strings.Contains(it, settingListSep) {
				return "", configErr("%s item %q holds %q, which separates list items", key, it, settingListSep)
			}
			if key == SettingApprovalProtectedPaths && (strings.HasPrefix(it, "/") || strings.HasPrefix(it, "~") || strings.Contains(it, "..")) {
				return "", configErr("%s item %q must be a repo-relative glob, not a local path", key, it)
			}
		}
		return strings.Join(items, settingListSep), nil
	}
}

func nonEmpty(key string, v yamlValue, s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", configErr("%s (line %d) is empty; omit the key to leave it at its default", key, v.line)
	}
	return s, nil
}

// parseYAMLSubset reads the file's YAML subset: comments, blank lines, an
// optional leading '---', and top-level `key: value` entries whose value
// is a scalar (plain, 'single' or "double" quoted), a flow list [a, b],
// or a block list of `- item` lines below the key. Nested maps, anchors,
// tags, multi-line scalars and duplicate keys are refused.
func parseYAMLSubset(text string) (map[string]yamlValue, []string, error) {
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")
	entries := map[string]yamlValue{}
	var order []string
	cur := ""
	for i, raw := range strings.Split(text, "\n") {
		n := i + 1
		line := strings.TrimRight(raw, "\r")
		if strings.ContainsRune(line, '\t') {
			return nil, nil, configErr("line %d holds a tab; indent with spaces", n)
		}
		line = stripYAMLComment(line)
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if trim == "---" && len(order) == 0 {
			continue
		}
		if trim == "---" || trim == "..." {
			return nil, nil, configErr("line %d: one document per file", n)
		}
		if item, ok := strings.CutPrefix(trim, "-"); ok && (item == "" || strings.HasPrefix(item, " ")) {
			if cur == "" {
				return nil, nil, configErr("line %d: a list item outside a key", n)
			}
			v := entries[cur]
			if v.flow || (!v.blockOpen && !v.isList) {
				return nil, nil, configErr("line %d: key %q already has a value; a list item cannot follow it", n, cur)
			}
			v.blockOpen, v.isList = false, true
			s, _, err := parseYAMLScalar(strings.TrimSpace(item), n)
			if err != nil {
				return nil, nil, err
			}
			v.list = append(v.list, s)
			entries[cur] = v
			continue
		}
		if line[0] == ' ' {
			return nil, nil, configErr("line %d: nested structures are not supported; schema %d is flat", n, ProjectConfigSchema)
		}
		key, rest, ok := strings.Cut(trim, ":")
		if !ok || !yamlKeyRe.MatchString(key) || (rest != "" && rest[0] != ' ') {
			return nil, nil, configErr("line %d: want `key: value`", n)
		}
		if _, dup := entries[key]; dup {
			return nil, nil, configErr("key %q appears twice (line %d)", key, n)
		}
		rest = strings.TrimSpace(rest)
		v := yamlValue{line: n}
		switch {
		case rest == "":
			v.blockOpen = true
		case rest[0] == '[':
			items, err := parseYAMLFlowList(rest, n)
			if err != nil {
				return nil, nil, err
			}
			v.isList, v.list = true, items
			v.flow = true
		default:
			s, q, err := parseYAMLScalar(rest, n)
			if err != nil {
				return nil, nil, err
			}
			v.scalar, v.quoted = s, q
		}
		entries[key] = v
		order = append(order, key)
		cur = key
	}
	return entries, order, nil
}

// stripYAMLComment drops a '#' comment: one at the line start, or after
// whitespace and outside quotes.
func stripYAMLComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			if i == 0 || line[i-1] == ' ' || line[i-1] == '[' || line[i-1] == ',' || line[i-1] == '-' {
				quote = c
			}
		case c == '#' && (i == 0 || line[i-1] == ' '):
			return line[:i]
		}
	}
	return line
}

// parseYAMLScalar reads one scalar; quoted reports whether it was quoted.
func parseYAMLScalar(s string, n int) (string, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false, configErr("line %d: an empty list item", n)
	}
	switch s[0] {
	case '"':
		var out string
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return "", false, configErr("line %d: bad double-quoted string", n)
		}
		return out, true, nil
	case '\'':
		if len(s) < 2 || s[len(s)-1] != '\'' {
			return "", false, configErr("line %d: unterminated single-quoted string", n)
		}
		inner := s[1 : len(s)-1]
		if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
			return "", false, configErr("line %d: bad single-quoted string", n)
		}
		return strings.ReplaceAll(inner, "''", "'"), true, nil
	case '{', '&', '*', '!', '|', '>', '%', '@', '`', '[', ']', ',':
		return "", false, configErr("line %d: %q starts a YAML form this file does not use; quote the value", n, string(s[0]))
	}
	if strings.Contains(s, ": ") {
		return "", false, configErr("line %d: a plain value holds \": \"; quote it", n)
	}
	return s, false, nil
}

// parseYAMLFlowList reads [a, "b", 'c'] — scalars only, on one line.
func parseYAMLFlowList(s string, n int) ([]string, error) {
	if !strings.HasSuffix(s, "]") {
		return nil, configErr("line %d: a flow list must close with ] on the same line", n)
	}
	inner := s[1 : len(s)-1]
	var items []string
	var quote byte
	start := 0
	flush := func(end int) error {
		part := strings.TrimSpace(inner[start:end])
		if part == "" && end == len(inner) && len(items) > 0 {
			return nil // trailing comma
		}
		item, _, err := parseYAMLScalar(part, n)
		if err != nil {
			return err
		}
		items = append(items, item)
		return nil
	}
	if strings.TrimSpace(inner) == "" {
		return nil, nil
	}
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ',':
			if err := flush(i); err != nil {
				return nil, err
			}
			start = i + 1
		}
	}
	if err := flush(len(inner)); err != nil {
		return nil, err
	}
	return items, nil
}

// --- provenance -----------------------------------------------------

// settingsProv is the projects.settings_prov column: the hash of the last
// import and the values it set. A key counts as imported only while the
// settings row still holds exactly the value the import set; any other
// value under it is a local override.
type settingsProv struct {
	Hash     string                     `json:"hash,omitempty"`
	Imported map[string]json.RawMessage `json:"imported,omitempty"`
}

func parseProv(raw string) (settingsProv, error) {
	p := settingsProv{Imported: map[string]json.RawMessage{}}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return settingsProv{}, err
		}
	}
	if p.Imported == nil {
		p.Imported = map[string]json.RawMessage{}
	}
	return p, nil
}

func (p settingsProv) encode() (string, error) {
	if p.Hash == "" && len(p.Imported) == 0 {
		return "", nil
	}
	b, err := json.Marshal(p)
	return string(b), err
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// isImported reports whether the row's current value for key is the one
// the last import set.
func (p settingsProv) isImported(key string, cur json.RawMessage) bool {
	imp, ok := p.Imported[key]
	return ok && compactJSON(imp) == compactJSON(cur)
}

// toolScopeText renders a parsed worker tool scope in its stored and
// tools= spelling: the patterns joined by "+", and "none" for the
// explicitly empty scope.
func toolScopeText(tools []string) string {
	if len(tools) == 0 {
		return "none"
	}
	return strings.Join(tools, settingListSep)
}

// rawString reads a stored JSON string.
func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if raw == nil || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// rawInt reads a stored JSON number.
func rawInt(raw json.RawMessage) (int, bool) {
	var n int
	if raw == nil || json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

// renderSettingRaw shows a stored value the way project show does.
func renderSettingRaw(key string, raw json.RawMessage) string {
	if raw == nil {
		return "default"
	}
	if key == SettingInstructions {
		s, _ := rawString(raw)
		return instructionsSummary(s)
	}
	if key == SettingExtraRepos {
		var l []ExtraRepo
		if json.Unmarshal(raw, &l) == nil {
			if len(l) == 0 {
				return "none"
			}
			items := make([]string, len(l))
			for i, x := range l {
				items[i] = extraRepoText(x)
			}
			return strings.Join(items, settingListSep)
		}
	}
	if s, ok := rawString(raw); ok {
		return s
	}
	var l stringList
	if json.Unmarshal(raw, &l) == nil && raw[0] == '[' {
		if len(l) == 0 {
			return "none"
		}
		return strings.Join(l, settingListSep)
	}
	return compactJSON(raw)
}

// renderConflictRaw shows a value in a conflict: an instruction text in
// full, since the user resolves the conflict by comparing both texts
// (§5.9); every other key as renderSettingRaw does.
func renderConflictRaw(key string, raw json.RawMessage) string {
	if key == SettingInstructions {
		if s, ok := rawString(raw); ok {
			return s
		}
	}
	return renderSettingRaw(key, raw)
}

// --- import ----------------------------------------------------------

// ConfigChange is one key an import touches: Old is the store's value,
// New the file's (or "default" where the import resets the key).
type ConfigChange struct {
	Key string `json:"key"`
	Old string `json:"old"`
	New string `json:"new"`
}

// ImportResult is what an import did, or — with NeedsConfirm — would do.
type ImportResult struct {
	// Hash is the hash of the content imported.
	Hash string `json:"hash"`
	// NeedsConfirm: a first import found hand-set values the file would
	// replace (Diffs); nothing was written and the caller must repeat the
	// import with Accept set.
	NeedsConfirm bool `json:"needs_confirm,omitempty"`
	// Applied lists the keys whose store value the import changed, hand-set
	// replacements accepted on a first import included.
	Applied []ConfigChange `json:"applied,omitempty"`
	// Conflicts lists the local overrides that kept their value against a
	// different one in the file: Old is the local value, New the file's.
	Conflicts []ConfigChange `json:"conflicts,omitempty"`
	// Diffs lists, on a first import, the hand-set values the file differs
	// from: Old is the hand-set value, New the file's.
	Diffs []ConfigChange `json:"diffs,omitempty"`
	// Settings is the project's settings after the import; zero while
	// NeedsConfirm.
	Settings *ProjectSettings `json:"settings,omitempty"`
}

// ImportProjectConfig imports a project.yaml's content for the repo
// (§13.1). The content is validated whole first; the file's repo must be
// the canonical identity of repoURL, the origin the client read. A file
// change over a value that is still the previous import's applies; over
// a local override it does not, and the conflict is returned; a first
// import over hand-set values returns them as Diffs and writes nothing
// until it is repeated with accept. Validation, settings and provenance
// land in one transaction.
func (e *Engine) ImportProjectConfig(ctx context.Context, repoURL string, content []byte, accept bool) (ImportResult, error) {
	return e.ImportProjectConfigWith(ctx, repoURL, content, nil, accept)
}

// ImportProjectConfigWith is ImportProjectConfig for a file that names an
// instructions_file (§5.9): instructions is the text the client read from
// that path, never a path for the engine to read. A file that names one
// with no text sent is refused; an empty text is an unset value. The
// text is bounded like a `project set` of it, and a refusal leaves the
// previous value in effect.
func (e *Engine) ImportProjectConfigWith(ctx context.Context, repoURL string, content []byte, instructions *string, accept bool) (ImportResult, error) {
	if repoURL == "" {
		return ImportResult{}, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	canon, err := Canonicalize(repoURL)
	if err != nil {
		return ImportResult{}, fmt.Errorf("%w: %v", ErrInvalidSetting, err)
	}
	cfg, err := ParseProjectConfig(content)
	if err != nil {
		return ImportResult{}, err
	}
	if cfg.Repo != canon {
		return ImportResult{}, configErr("repo %q is not this repository's origin %q; refusing to import another repository's configuration", cfg.Repo, canon)
	}
	switch {
	case cfg.InstructionsFile != "" && instructions == nil:
		return ImportResult{}, configErr("%s names %q but no text was sent: the client reads the file and sends its content", configKeyInstructionsFile, cfg.InstructionsFile)
	case cfg.InstructionsFile == "" && instructions != nil:
		return ImportResult{}, configErr("instructions text was sent but the file sets no %s", configKeyInstructionsFile)
	case instructions != nil && strings.TrimSpace(*instructions) != "":
		if err := ValidateInstructions(*instructions); err != nil {
			return ImportResult{}, configErr("%s: %s", cfg.InstructionsFile, strings.TrimPrefix(err.Error(), ErrInvalidSetting.Error()+": "))
		}
		raw, err := json.Marshal(*instructions)
		if err != nil {
			return ImportResult{}, fmt.Errorf("encode the instructions: %w", err)
		}
		cfg.Values[SettingInstructions] = raw
	}
	// The hash covers the yaml and the instruction text, so a change to the
	// text alone shows as a changed import.
	hash := ConfigHashWith(content, instructions)
	res := ImportResult{Hash: hash}
	err = e.store.Update(ctx, func(tx *sql.Tx) error {
		res = ImportResult{Hash: hash}
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		var rawSettings, rawProv, shownRepo string
		err := lookupProjectTx(tx, repoURL, "id, settings, settings_prov, repo_url", &project, &rawSettings, &rawProv, &shownRepo)
		born := false
		switch {
		case errors.Is(err, sql.ErrNoRows):
			born = true
			rawSettings, rawProv, shownRepo = "", "", canon
		case err != nil:
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
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
		first := prov.Hash == ""
		newImported := map[string]json.RawMessage{}
		for _, k := range SettingKeys {
			fv, inFile := cfg.Values[k]
			cur, has := row[k]
			imp := has && prov.isImported(k, cur)
			if !inFile {
				if imp {
					// The file dropped a key the previous import set and
					// nobody overrode: back to the default.
					res.Applied = append(res.Applied, ConfigChange{k, renderSettingRaw(k, cur), "default"})
					delete(row, k)
				}
				continue
			}
			switch {
			case !has:
				row[k] = fv
				newImported[k] = fv
				res.Applied = append(res.Applied, ConfigChange{k, "default", renderSettingRaw(k, fv)})
			case compactJSON(cur) == compactJSON(fv):
				if imp {
					newImported[k] = fv
				}
			case imp:
				row[k] = fv
				newImported[k] = fv
				res.Applied = append(res.Applied, ConfigChange{k, renderSettingRaw(k, cur), renderSettingRaw(k, fv)})
			case first:
				res.Diffs = append(res.Diffs, ConfigChange{k, renderSettingRaw(k, cur), renderSettingRaw(k, fv)})
				if accept {
					row[k] = fv
					newImported[k] = fv
					res.Applied = append(res.Applied, ConfigChange{k, renderSettingRaw(k, cur), renderSettingRaw(k, fv)})
				}
			default:
				res.Conflicts = append(res.Conflicts, ConfigChange{k, renderConflictRaw(k, cur), renderConflictRaw(k, fv)})
			}
		}
		if len(res.Diffs) > 0 && !accept {
			// Nothing is written before the user accepts the replacement.
			res.NeedsConfirm = true
			res.Applied = nil
			return nil
		}
		prov.Hash, prov.Imported = hash, newImported
		b, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("encode the settings of project %d: %w", project, err)
		}
		pb, err := prov.encode()
		if err != nil {
			return fmt.Errorf("encode the provenance of project %d: %w", project, err)
		}
		if born {
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, "", "", now)
			if err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE projects SET archived_at = NULL, settings = ?, settings_prov = ?, updated_at = ? WHERE id = ?", string(b), pb, fmtTime(now), project); err != nil {
			return fmt.Errorf("store the settings of project %d: %w", project, err)
		}
		v, err := settingsViewTx(tx, project, shownRepo, e.cfg.ForemanWakeBudget)
		if err != nil {
			return err
		}
		res.Settings = &v
		return nil
	})
	if err != nil {
		return ImportResult{}, fmt.Errorf("import the project configuration of %s: %w", RedactRepoURL(repoURL), err)
	}
	return res, nil
}
