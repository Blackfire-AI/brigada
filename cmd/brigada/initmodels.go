package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
	id        int
	name      string
	note      string
	value     string
	isDefault bool
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
		source.entries = append(source.entries, initModelEntry{id: len(source.entries) + 1, name: name, value: name, note: note})
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
		label, description := initCleanText(m.Label), initCleanText(m.Description)
		note := label
		switch {
		case label != "" && description != "":
			note = label + ": " + description
		case label == "":
			note = description
		}
		add(value, note)
	}

	source.defaultID = 1
	for i := range source.entries {
		entry := &source.entries[i]
		if settings.Model != "" && entry.value == settings.Model {
			source.defaultID = entry.id
			entry.isDefault = true
			if entry.note != "" {
				entry.note = "your claude default; " + entry.note
			} else {
				entry.note = "your claude default"
			}
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

func initCodexModels(ctx context.Context) initModelSource {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, _ := os.UserHomeDir()
		if userHome != "" {
			home = filepath.Join(userHome, ".codex")
		}
	}
	entry := initModelEntry{id: 1, note: "your codex default", value: "default", isDefault: true}
	if home != "" {
		path := filepath.Join(home, "config.toml")
		entry.name, _ = readInitModelConfig(path, "model", false)
		if entry.name != "" {
			entry.note += " (" + path + ")"
		}
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
			if model.Description != "" {
				source.entries[0].note += "; " + model.Description
			}
			continue
		}
		if _, err := adapter.AgentFamily("codex", model.Slug); err != nil {
			source.unknownCount++
			continue
		}
		source.entries = append(source.entries, initModelEntry{id: len(source.entries) + 1, name: model.Slug, value: model.Slug, note: model.Description})
	}
	return source
}

// Decode only menu fields. Catalog metadata may contain unrelated private data.
type initCodexModel struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
	Priority    int    `json:"priority"`
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

func initKimiModels(ctx context.Context) initModelSource {
	var name string
	var models []initKimiModel
	if home, err := os.UserHomeDir(); err == nil {
		name, models = readInitModelConfig(filepath.Join(home, ".kimi-code", "config.toml"), "default_model", true)
	}
	entries := []initModelEntry{{id: 1, name: name, note: "your kimi default", value: "default", isDefault: true}}
	byID := map[string]initKimiModel{}
	for _, model := range models {
		if prev, ok := byID[model.id]; ok {
			if model.display == "" {
				model.display = prev.display
			}
			if model.context == 0 {
				model.context = prev.context
			}
		}
		byID[model.id] = model
	}
	if detail := byID[name].note(); name != "" && detail != "" {
		entries[0].note += "; " + detail
	}
	seen := map[string]bool{name: true}
	for _, model := range models {
		if !seen[model.id] {
			entries = append(entries, initModelEntry{id: len(entries) + 1, name: model.id, value: model.id, note: byID[model.id].note()})
			seen[model.id] = true
		}
	}
	return initModelSource{entries: entries, signIn: probeHarness(ctx, "kimi")}
}

// initKimiModel holds the only keys read from a [models."<id>"] table.
type initKimiModel struct {
	id      string
	display string
	context int64
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

// initContextSize writes a token count as 1M, 256K and so on, rounded to the
// nearest power-of-two K or M. Zero or negative counts give "".
func initContextSize(n int64) string {
	if n <= 0 {
		return ""
	}
	exp := int(math.Round(math.Log2(float64(n) / 1024)))
	exp = min(max(exp, 0), 30)
	if exp >= 10 {
		return strconv.Itoa(1<<(exp-10)) + "M"
	}
	return strconv.Itoa(1<<exp) + "K"
}

// Scan only the requested top-level assignment and, for Kimi, model table
// names plus their display_name and max_context_size. Never decode a whole config into a map: other fields may be secrets.
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
			// Only these two keys of a model table are read; the rest may
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
		if out, err := cheapHarnessCommand(ctx, "opencode", "models"); err == nil {
			seen := map[string]bool{}
			for line := range strings.SplitSeq(string(out), "\n") {
				model := strings.TrimSpace(line)
				provider, name, ok := strings.Cut(model, "/")
				if ok && provider != "" && name != "" && validInitModelName(model) && !seen[model] {
					seen[model] = true
					if _, err := adapter.AgentFamily("opencode", model); err != nil {
						source.unknownCount++
						continue
					}
					source.entries = append(source.entries, initModelEntry{id: len(source.entries) + 1, name: model, value: model})
				}
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
