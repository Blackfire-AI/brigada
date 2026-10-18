package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	unknownCount  int // OpenCode models omitted because their family is unknown.
}

func initClaudeModels(ctx context.Context) initModelSource {
	// These are explicit aliases, not Claude's potentially configured CLI default.
	return initModelSource{signIn: probeHarness(ctx, "claude"), entries: []initModelEntry{
		{id: 1, name: "sonnet", value: "sonnet", note: "balanced; the usual choice"},
		{id: 2, name: "opus", value: "opus", note: "strongest; uses your Claude plan's limits faster"},
		{id: 3, name: "haiku", value: "haiku", note: "fastest; fine for small tasks"},
	}}
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
	return initModelSource{entries: []initModelEntry{entry}, signIn: probeHarness(ctx, "codex")}
}

func initKimiModels(ctx context.Context) initModelSource {
	var name string
	var models []string
	if home, err := os.UserHomeDir(); err == nil {
		name, models = readInitModelConfig(filepath.Join(home, ".kimi-code", "config.toml"), "default_model", true)
	}
	entries := []initModelEntry{{id: 1, name: name, note: "your kimi default", value: "default", isDefault: true}}
	seen := map[string]bool{name: true}
	for _, model := range models {
		if !seen[model] {
			entries = append(entries, initModelEntry{id: len(entries) + 1, name: model, value: model})
			seen[model] = true
		}
	}
	return initModelSource{entries: entries, signIn: probeHarness(ctx, "kimi")}
}

// Scan only the requested top-level assignment and, for Kimi, model table
// names. Never decode a whole config into a map: other fields may be secrets.
// Unreadable or unrecognized config is a best-effort unresolved CLI default.
func readInitModelConfig(path, key string, tables bool) (name string, models []string) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	topLevel := true
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
			if tables {
				if rest, ok := strings.CutPrefix(line, "[models."); ok {
					if model, suffix, ok := initConfigString(rest); ok && strings.HasPrefix(suffix, "]") && initConfigEnd(strings.TrimSpace(suffix[1:])) && validInitModelName(model) {
						models = append(models, model)
					}
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
