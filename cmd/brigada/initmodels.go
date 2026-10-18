package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// name is the resolved model shown in the menu and plan; value is recorded in
// runner/foreman pairs. A CLI default keeps value "default" even when resolved.
// An unresolved default has an empty name. Numbered menu IDs are one-based.
type initModelEntry struct {
	id             int
	name           string
	note           string
	value          string
	isDefault      bool
	isCurrent      bool     // A saved choice, displayed as a tag after the note.
	effortLevels   []string // Source order; Kimi metadata is display-only.
	defaultEffort  string   // Empty if unknown; Claude uses a descriptive default.
	effortFromUser bool     // The default effort comes from the user's CLI config.
}

type initModelSource struct {
	entries       []initModelEntry
	signIn        harnessProbe
	providerCount int // OpenCode only; no provider names or account output retained.
	unknownCount  int // Catalog models omitted because their family is unknown.
	defaultID     int // Entry picked on Enter; 0 means the first.
}

// Notes for aliases live here; the CLI documents no descriptions for them.
var initClaudeAliasNotes = map[string]string{
	"sonnet": "balanced; the usual choice",
	"opus":   "strong; uses your plan's limits faster",
	"haiku":  "fastest; fine for small tasks",
}

var (
	initClaudeModelRow = regexp.MustCompile(`^\s*(?:-\w,\s*)?--model(?:[\s<\[=]|$)`)
	initQuotedWord     = regexp.MustCompile(`'([A-Za-z0-9][A-Za-z0-9._-]*)'`)
	initClaudeContext  = regexp.MustCompile(`(?i)\[([1-9][0-9]*)m\]$`)
)

const initClaudeFileLimit = 4 << 20

// initClaudeAliases reads the aliases from the --model row of claude's root
// help, in its order, plus haiku. Without the row: sonnet, opus, haiku.
func initClaudeAliases(help string) []string {
	var row []string
	lines := strings.Split(help, "\n")
	for i, line := range lines {
		if !initClaudeModelRow.MatchString(line) {
			continue
		}
		row = append(row, line)
		for _, next := range lines[i+1:] {
			if t := strings.TrimSpace(next); t == "" || strings.HasPrefix(t, "-") {
				break
			}
			row = append(row, next)
		}
		break
	}
	var aliases []string
	for _, m := range initQuotedWord.FindAllStringSubmatch(strings.Join(row, " "), -1) {
		if !slices.Contains(aliases, m[1]) {
			aliases = append(aliases, m[1])
		}
	}
	if len(aliases) == 0 {
		return []string{"sonnet", "opus", "haiku"}
	}
	if !slices.Contains(aliases, "haiku") {
		aliases = append(aliases, "haiku")
	}
	return aliases
}

func initClaudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// readInitClaudeFile decodes a capped JSON file into v, which must declare only
// the keys wanted: the files also hold account details. Errors are ignored.
func readInitClaudeFile(path string, v any) {
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, initClaudeFileLimit+1))
	if err != nil || len(data) > initClaudeFileLimit {
		return
	}
	_ = json.Unmarshal(data, v)
}

func initCleanText(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
}

func initClaudeModels(ctx context.Context) initModelSource {
	probe := probeHarness(ctx, "claude")
	help := probe.rootHelp
	probe.rootHelp = ""
	source := initModelSource{signIn: probe}

	var cache struct {
		Models []struct {
			Value       string `json:"value"`
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"additionalModelOptionsCache"`
	}
	dir := initClaudeConfigDir()
	jsonPath := ""
	if configDir := os.Getenv("CLAUDE_CONFIG_DIR"); configDir != "" {
		jsonPath = filepath.Join(configDir, ".claude.json")
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		jsonPath = filepath.Join(home, ".claude.json")
	}
	readInitClaudeFile(jsonPath, &cache)
	var settings struct {
		Model string `json:"model"`
	}
	if dir != "" {
		readInitClaudeFile(filepath.Join(dir, "settings.json"), &settings)
	}

	add := func(name, note string) {
		if _, err := adapter.AgentFamily("claude", name); err != nil {
			return // unidentifiable names are left out; they could not be picked
		}
		source.entries = append(source.entries, initModelEntry{id: len(source.entries) + 1, name: name, value: name, note: note,
			effortLevels: slices.Clone(adapter.EffortLevels("claude")), defaultEffort: "Claude Code's default"})
	}
	aliases := initClaudeAliases(help)
	for _, alias := range aliases {
		note := initClaudeAliasNotes[alias]
		for _, m := range cache.Models {
			if strings.EqualFold(initCleanText(m.Label), alias) {
				if description := initCleanText(m.Description); description != "" {
					note = description
				}
				break
			}
		}
		add(alias, note)
	}
	for _, m := range cache.Models {
		value := m.Value
		if !validInitModelName(value) || slices.Contains(aliases, value) || slices.ContainsFunc(source.entries, func(e initModelEntry) bool { return e.value == value }) {
			continue
		}
		add(value, initClaudeModelNote(value, m.Label, m.Description))
	}

	source.defaultID = 1
	for i := range source.entries {
		entry := &source.entries[i]
		if settings.Model != "" && entry.value == settings.Model {
			source.defaultID = entry.id
			entry.isDefault = true
			return source
		}
	}
	for _, entry := range source.entries {
		if entry.value == "sonnet" {
			source.defaultID = entry.id
		}
	}
	return source
}

func initClaudeModelNote(value, label, description string) string {
	label, description = initCleanText(label), initCleanText(description)
	if context := initClaudeContext.FindStringSubmatch(value); context != nil && label != "" {
		return label + ", " + context[1] + "M context"
	}
	if label == "" {
		return description
	}
	if description == "" {
		return label
	}
	if strings.EqualFold(label, strings.Fields(description)[0]) {
		return description
	}
	return label + ": " + description
}

// codexConfigPath is the config.toml codex reads, or "" without a home.
func codexConfigPath() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, _ := os.UserHomeDir()
		if userHome == "" {
			return ""
		}
		home = filepath.Join(userHome, ".codex")
	}
	return filepath.Join(home, "config.toml")
}

// codexUserEffort is the top-level model_reasoning_effort of the codex config,
// read with the same line reader as the model. Init and config share it.
func codexUserEffort(path string) string {
	if path == "" {
		return ""
	}
	level, _ := readInitModelConfig(path, "model_reasoning_effort", false)
	return level
}

// codexDefaultEffort is the effort codex runs a model at without --effort: the
// user's config value when the model offers it, else the catalog's default.
func codexDefaultEffort(userEffort string, levels []string, catalog string) (effort string, fromUser bool) {
	if userEffort != "" && slices.Contains(levels, userEffort) {
		return userEffort, true
	}
	return catalog, false
}

func initCodexModels(ctx context.Context) initModelSource {
	path := codexConfigPath()
	userEffort := codexUserEffort(path)
	entry := initModelEntry{id: 1, value: "default", isDefault: true}
	if path != "" {
		entry.name, _ = readInitModelConfig(path, "model", false)
	}
	setEffort := func(e *initModelEntry, model initCodexModel) {
		e.effortLevels = model.efforts()
		e.defaultEffort, e.effortFromUser = codexDefaultEffort(userEffort, e.effortLevels, model.DefaultReasoningLevel)
	}
	source := initModelSource{entries: []initModelEntry{entry}, signIn: probeHarness(ctx, "codex")}
	if !source.signIn.installed {
		return source
	}
	help, err := cheapHarnessCommand(ctx, "codex", "--help")
	if err != nil || !helpNamesHarnessCommand(string(help), "codex", "debug") {
		return source
	}
	help, err = cheapHarnessCommand(ctx, "codex", "debug", "--help")
	if err != nil || !helpNamesHarnessCommand(string(help), "codex debug", "models") {
		return source
	}
	models, ok := readInitCodexCatalog(ctx)
	if !ok {
		return source
	}
	slices.SortFunc(models, func(a, b initCodexModel) int {
		if a.Priority < b.Priority {
			return -1
		}
		if a.Priority > b.Priority {
			return 1
		}
		return strings.Compare(a.Slug, b.Slug)
	})
	seen := map[string]bool{}
	for _, model := range models {
		if model.Visibility != "list" || !validInitModelName(model.Slug) || seen[model.Slug] {
			continue
		}
		seen[model.Slug] = true
		if model.Slug == entry.name {
			setEffort(&source.entries[0], model)
			if model.Description != "" {
				source.entries[0].note = model.Description
			}
			continue
		}
		if _, err := adapter.AgentFamily("codex", model.Slug); err != nil {
			source.unknownCount++
			continue
		}
		added := initModelEntry{id: len(source.entries) + 1, name: model.Slug, value: model.Slug, note: model.Description}
		setEffort(&added, model)
		source.entries = append(source.entries, added)
	}
	return source
}

// Decode only menu fields. Catalog metadata may contain unrelated private data.
type initCodexModel struct {
	Slug                     string `json:"slug"`
	Description              string `json:"description"`
	Visibility               string `json:"visibility"`
	Priority                 int    `json:"priority"`
	SupportedReasoningLevels []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
	DefaultReasoningLevel string `json:"default_reasoning_level"`
}

func (m initCodexModel) efforts() []string {
	var levels []string
	for _, level := range m.SupportedReasoningLevels {
		levels = append(levels, level.Effort)
	}
	return levels
}

func readInitCodexCatalog(ctx context.Context) ([]initCodexModel, bool) {
	read := func(args ...string) ([]initCodexModel, bool) {
		out, err := cheapHarnessCommand(ctx, "codex", args...)
		if err != nil {
			return nil, false
		}
		var catalog struct {
			Models []initCodexModel `json:"models"`
		}
		if json.Unmarshal(out, &catalog) != nil || catalog.Models == nil {
			return nil, false
		}
		return catalog.Models, true
	}
	if models, ok := read("debug", "models"); ok {
		return models, true
	}
	// Refresh can fail offline. Use the bundled catalog only when this binary
	// documents the option, and keep both attempts within the cheap timeout.
	help, err := cheapHarnessCommand(ctx, "codex", "debug", "models", "--help")
	if err != nil || !helpNamesCommand(string(help), "--bundled") {
		return nil, false
	}
	return read("debug", "models", "--bundled")
}

func kimiConfigPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".kimi-code", "config.toml")
}

func initKimiModels(ctx context.Context) initModelSource {
	var name string
	var models []initKimiModel
	if path := kimiConfigPath(); path != "" {
		name, models = readInitModelConfig(path, "default_model", true)
	}
	entries := []initModelEntry{{id: 1, name: name, value: "default", isDefault: true}}
	byID := map[string]initKimiModel{}
	for _, model := range models {
		if prev, ok := byID[model.id]; ok {
			if model.display == "" {
				model.display = prev.display
			}
			if model.context == 0 {
				model.context = prev.context
			}
			if model.effortLevels == nil {
				model.effortLevels = prev.effortLevels
			}
			if model.defaultEffort == "" {
				model.defaultEffort = prev.defaultEffort
			}
		}
		byID[model.id] = model
	}
	if detail := byID[name].note(); name != "" && detail != "" {
		entries[0].note = detail
	}
	entries[0].effortLevels = byID[name].effortLevels
	entries[0].defaultEffort = byID[name].defaultEffort
	seen := map[string]bool{name: true}
	for _, model := range models {
		if !seen[model.id] {
			entries = append(entries, initModelEntry{id: len(entries) + 1, name: model.id, value: model.id, note: byID[model.id].note(),
				effortLevels: byID[model.id].effortLevels, defaultEffort: byID[model.id].defaultEffort})
			seen[model.id] = true
		}
	}
	return initModelSource{entries: entries, signIn: probeHarness(ctx, "kimi")}
}

// initKimiModel holds the only keys read from a [models."<id>"] table.
type initKimiModel struct {
	id            string
	display       string
	context       int64
	effortLevels  []string
	defaultEffort string
}

// note reads "<display_name>, <context> context", leaving out missing parts.
func (m initKimiModel) note() string {
	var parts []string
	if m.display != "" {
		parts = append(parts, m.display)
	}
	if c := initContextSize(m.context); c != "" {
		parts = append(parts, c+" context")
	}
	return strings.Join(parts, ", ")
}

// initContextSize accepts catalogs using either decimal or binary units. Exact
// multiples of 1024 use binary units; other counts use decimal units. Round to
// a whole K below 1000K and a whole M above it. Nonpositive counts give "".
func initContextSize(n int64) string {
	if n <= 0 {
		return ""
	}
	unit := int64(1000)
	if n%1024 == 0 {
		unit = 1024
	}
	// Divide before adding to avoid overflowing large catalog counts.
	round := func(unit int64) int64 { return n/unit + (n%unit+unit/2)/unit }
	if round(unit) >= 1000 {
		return strconv.FormatInt(round(unit*unit), 10) + "M"
	}
	return strconv.FormatInt(max(round(unit), 1), 10) + "K"
}

// Scan only the requested top-level assignment and, for Kimi, model table
// names plus their display_name, max_context_size, support_efforts and
// default_effort. Never decode a whole config into a map: other fields may be secrets.
// Unreadable or unrecognized config is a best-effort unresolved CLI default.
func readInitModelConfig(path, key string, tables bool) (name string, models []initKimiModel) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	topLevel := true
	cur := -1 // index in models of the table being read, or -1
	multiline := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		insideString := multiline != ""
		multiline = initConfigMultiline(line, multiline)
		if insideString || multiline != "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			topLevel = false
			cur = -1
			if tables {
				if rest, ok := strings.CutPrefix(line, "[models."); ok {
					if model, suffix, ok := initConfigString(rest); ok && strings.HasPrefix(suffix, "]") && initConfigEnd(strings.TrimSpace(suffix[1:])) && validInitModelName(model) {
						models = append(models, initKimiModel{id: model})
						cur = len(models) - 1
					}
				}
			}
			continue
		}
		if cur >= 0 {
			// Only these menu keys of a model table are read; the rest may
			// hold provider, api key or oauth details.
			lhs, rhs, ok := strings.Cut(line, "=")
			switch {
			case !ok:
			case strings.TrimSpace(lhs) == "display_name":
				if v, suffix, ok := initConfigString(rhs); ok && initConfigEnd(suffix) {
					models[cur].display = initCleanText(v)
				}
			case strings.TrimSpace(lhs) == "max_context_size":
				digits, _, _ := strings.Cut(rhs, "#")
				if n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(digits), "_", ""), 10, 64); err == nil {
					models[cur].context = n
				}
			case strings.TrimSpace(lhs) == "support_efforts":
				models[cur].effortLevels = initConfigEfforts(rhs, scanner)
			case strings.TrimSpace(lhs) == "default_effort":
				if v, suffix, ok := initConfigString(rhs); ok && initConfigEnd(suffix) {
					models[cur].defaultEffort = v
				}
			}
			continue
		}
		if !topLevel {
			continue
		}
		lhs, rhs, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(lhs) != key {
			continue
		}
		if model, suffix, ok := initConfigString(rhs); ok && initConfigEnd(suffix) && validInitModelName(model) {
			name = model
		}
	}
	return name, models
}

// Ignore multiline values, including apparent model assignments/table headers
// inside them. Track only quote delimiters, never their potentially secret text.
func initConfigMultiline(line, delimiter string) string {
	for i := 0; i < len(line); {
		if delimiter != "" {
			switch {
			case strings.HasPrefix(line[i:], delimiter):
				i += 3
				delimiter = ""
			case delimiter == `"""` && line[i] == '\\':
				i += 2
			default:
				i++
			}
			continue
		}
		if line[i] == '#' {
			break
		}
		if line[i] != '"' && line[i] != '\'' {
			i++
			continue
		}
		quote := line[i]
		if strings.HasPrefix(line[i:], strings.Repeat(string(quote), 3)) {
			delimiter = line[i : i+3]
			i += 3
			continue
		}
		for i++; i < len(line); i++ {
			if quote == '"' && line[i] == '\\' {
				i++
			} else if line[i] == quote {
				i++
				break
			}
		}
	}
	return delimiter
}

func initConfigEnd(s string) bool { return s == "" || strings.HasPrefix(s, "#") }

// Read only a support_efforts string array, including comments, trailing commas
// and multiline arrays. Other model/provider values are never decoded.
func initConfigEfforts(s string, scanner *bufio.Scanner) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return nil
	}
	s = s[1:]
	levels := []string{}
	needComma := false
	for {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "#") {
			if !scanner.Scan() {
				return nil
			}
			s = scanner.Text()
			continue
		}
		if strings.HasPrefix(s, "]") {
			if initConfigEnd(strings.TrimSpace(s[1:])) {
				return levels
			}
			return nil
		}
		if needComma {
			if !strings.HasPrefix(s, ",") {
				return nil
			}
			s = s[1:]
			needComma = false
			continue
		}
		value, rest, ok := initConfigString(s)
		if !ok {
			return nil
		}
		levels = append(levels, value)
		s = rest
		needComma = true
	}
}

// Only single-line TOML strings are needed for model identifiers. Ignore
// malformed/multiline values rather than letting unrelated config leak out.
func initConfigString(s string) (value, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || (s[0] != '"' && s[0] != '\'') {
		return "", "", false
	}
	quote := s[0]
	for i := 1; i < len(s); i++ {
		if quote == '"' && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == quote {
			value = s[1:i]
			if quote == '"' {
				var err error
				value, err = strconv.Unquote(s[:i+1])
				if err != nil {
					return "", "", false
				}
			}
			return value, strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

func validInitModelName(name string) bool {
	return name != "" && !strings.ContainsAny(name, " \t\r\n") && !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func initOpenCodeModels(ctx context.Context) initModelSource {
	source := initModelSource{signIn: probeHarness(ctx, "opencode")}
	if !source.signIn.installed {
		return source
	}
	help, err := cheapHarnessCommand(ctx, "opencode", "--help")
	if err != nil {
		return source
	}
	// OpenCode lists fully qualified command rows, unlike the other harnesses.
	if helpNamesHarnessCommand(string(help), "opencode", "models") {
		var rows []initOpenCodeRow
		if modelsHelp, err := cheapHarnessCommand(ctx, "opencode", "models", "--help"); err == nil && initVerboseFlag.Match(modelsHelp) {
			if out, err := cheapHarnessCommand(ctx, "opencode", "models", "--verbose"); err == nil {
				rows, _ = parseInitOpenCodeVerbose(string(out))
			}
		}
		if rows == nil {
			if out, err := cheapHarnessCommand(ctx, "opencode", "models"); err == nil {
				for line := range strings.SplitSeq(string(out), "\n") {
					rows = append(rows, initOpenCodeRow{id: strings.TrimSpace(line), active: true})
				}
			}
		}
		seen := map[string]bool{}
		for _, row := range rows {
			model := row.id
			provider, name, ok := strings.Cut(model, "/")
			if ok && provider != "" && name != "" && validInitModelName(model) && !seen[model] {
				seen[model] = true
				if !row.active {
					continue
				}
				if _, err := adapter.AgentFamily("opencode", model); err != nil {
					source.unknownCount++
					continue
				}
				source.entries = append(source.entries, initModelEntry{id: len(source.entries) + 1, name: model, value: model, note: row.note,
					effortLevels: row.effortLevels})
			}
		}
	}
	// Read-only credential list, gated by a row in root help: current CLIs name
	// "opencode providers" (alias auth), older ones "opencode auth". Never run
	// login. Output is only counted, never kept or printed.
	command := ""
	switch {
	case helpNamesHarnessCommand(string(help), "opencode", "providers"):
		command = "providers"
	case helpNamesHarnessCommand(string(help), "opencode", "auth"):
		command = "auth"
	}
	if command != "" {
		if out, err := cheapHarnessCommand(ctx, "opencode", command, "list"); err == nil {
			ran := "opencode " + command + " list"
			source.providerCount = initOpenCodeProviderCount(string(out))
			source.signIn.signedIn = source.providerCount > 0
			source.signIn.checked = true
			source.signIn.evidence = ran + " (documented by --help)"
			if source.signIn.signedIn {
				source.signIn.status = fmt.Sprintf("signed in to %d providers (%s)", source.providerCount, ran)
			} else {
				source.signIn.status = "no stored provider credentials (keys may also come from your environment)"
			}
		} else {
			source.entries = nil
			source.unknownCount = 0
		}
	}
	return source
}

var initVerboseFlag = regexp.MustCompile(`(?m)(?:^|[\s,\[])--verbose(?:[\s,\]=]|$)`)

// initOpenCodeRow is one listed model. note is empty for the plain list.
type initOpenCodeRow struct {
	id           string
	active       bool
	note         string
	effortLevels []string
}

// initOpenCodeMeta holds the only keys read from a verbose model's JSON
// object; the decoder drops every other field.
type initOpenCodeMeta struct {
	Name     string               `json:"name"`
	Status   string               `json:"status"`
	Variants initOpenCodeVariants `json:"variants"`
	Limit    struct {
		Context int64 `json:"context"`
	} `json:"limit"`
}

// A map would discard source order and retain variant settings. Keep only keys,
// consuming each value without storing any of its provider-specific contents.
type initOpenCodeVariants []string

func (v *initOpenCodeVariants) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber() // Ignored variant values must not depend on numeric ranges.
	token, err := dec.Token()
	if err != nil {
		return err
	}
	*v = nil
	if token == nil {
		return nil
	}
	if token != json.Delim('{') {
		return fmt.Errorf("opencode variants: expected an object")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		*v = append(*v, key.(string))
		depth := 0
		for {
			token, err := dec.Token()
			if err != nil {
				return err
			}
			switch token {
			case json.Delim('{'), json.Delim('['):
				depth++
			case json.Delim('}'), json.Delim(']'):
				depth--
			}
			if depth == 0 {
				break
			}
		}
	}
	_, err = dec.Token()
	return err
}

// note reads "<name>, <context> context", leaving out missing parts.
func (m initOpenCodeMeta) note() string {
	var parts []string
	if name := initCleanText(m.Name); name != "" {
		parts = append(parts, name)
	}
	if c := initContextSize(m.Limit.Context); c != "" {
		parts = append(parts, c+" context")
	}
	return strings.Join(parts, ", ")
}

// parseInitOpenCodeVerbose reads "opencode models --verbose": each
// provider/model line followed by a JSON object. Any row without a decodable
// object is an error, so the caller falls back to the plain list.
func parseInitOpenCodeVerbose(out string) ([]initOpenCodeRow, error) {
	var rows []initOpenCodeRow
	rest := out
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			break
		}
		line, after, _ := strings.Cut(rest, "\n")
		id := strings.TrimSpace(line)
		// The output is combined with stderr: a line that is no model id
		// (a shell or runtime warning) is skipped.
		if !strings.Contains(id, "/") || !validInitModelName(id) {
			rest = after
			continue
		}
		after = strings.TrimLeft(after, " \t\r\n")
		if !strings.HasPrefix(after, "{") {
			return nil, fmt.Errorf("opencode models --verbose: row without metadata")
		}
		dec := json.NewDecoder(strings.NewReader(after))
		var meta initOpenCodeMeta
		if err := dec.Decode(&meta); err != nil {
			return nil, err
		}
		rows = append(rows, initOpenCodeRow{id: id, active: meta.Status == "active", note: meta.note(), effortLevels: meta.Variants})
		rest = after[dec.InputOffset():]
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("opencode models --verbose: no rows")
	}
	return rows, nil
}

func helpNamesHarnessCommand(help, harness, command string) bool {
	return helpNamesCommand(help, command) || helpNamesCommand(help, harness+" "+command)
}

var initANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func initOpenCodeProviderCount(out string) int {
	count := 0
	for line := range strings.SplitSeq(out, "\n") {
		// auth list's provider rows use a filled circle; box borders, the
		// credential path, and the summary are not providers. Strip color only.
		line = strings.TrimSpace(initANSI.ReplaceAllString(line, ""))
		if rest, ok := strings.CutPrefix(line, "●"); ok && strings.TrimSpace(rest) != "" {
			count++
		}
	}
	return count
}
