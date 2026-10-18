package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
)

var initHarnesses = []string{"claude", "codex", "kimi", "opencode"}

type harnessProbe struct {
	installed bool
	signedIn  bool
	status    string
	evidence  string
	checked   bool // a documented command ran and its answer is in status
}

// Only known cheap status paths are candidates. Each command in the path
// must be documented by its parent's --help on this installation. No doctor,
// model invocation, credential file read or login command is a probe.
func probeHarness(ctx context.Context, harness string) harnessProbe {
	if _, err := exec.LookPath(harness); err != nil {
		return harnessProbe{status: "not installed"}
	}
	p := harnessProbe{installed: true, status: "sign-in can't be checked", evidence: "no verified authentication status command; user confirmation required"}
	var path []string
	switch harness {
	case "claude":
		path = []string{"auth", "status"}
	case "codex":
		path = []string{"login", "status"}
	default:
		return p
	}
	for i, word := range path {
		args := append(append([]string{}, path[:i]...), "--help")
		out, err := cheapHarnessCommand(ctx, harness, args...)
		if err != nil || !helpNamesCommand(string(out), word) {
			return p
		}
	}
	out, err := cheapHarnessCommand(ctx, harness, path...)
	p.evidence = harness + " " + strings.Join(path, " ") + " (documented by --help)"
	if err != nil {
		if ctx.Err() == nil && strings.Contains(strings.ToLower(string(out)), "not logged in") {
			p.status = "not signed in"
		}
		p.evidence += "; not authenticated or status unavailable; user confirmation required"
		return p
	}
	switch harness {
	case "claude":
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		if json.Unmarshal(out, &status) == nil {
			p.signedIn = status.LoggedIn
			p.status = "not signed in"
		}
	case "codex":
		p.signedIn = strings.Contains(strings.ToLower(string(out)), "logged in") && !strings.Contains(strings.ToLower(string(out)), "not logged in")
		if strings.Contains(strings.ToLower(string(out)), "not logged in") {
			p.status = "not signed in"
		}
	}
	if p.signedIn {
		p.status = "signed in"
		p.evidence += "; authentication confirmed"
	}
	if !p.signedIn {
		p.evidence += "; authentication not confirmed; user confirmation required"
	}
	return p
}

func helpNamesCommand(help, command string) bool {
	// Match a command row, never a mention of a command in prose/options.
	return regexp.MustCompile(`(?m)^[ \t]{2,}` + regexp.QuoteMeta(command) + `(?:\s|\[)`).MatchString(help)
}

func cheapHarnessCommand(ctx context.Context, harness string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, harness, args...)
	cmd.WaitDelay = 500 * time.Millisecond
	// Output may include account identifiers; it is interpreted in memory only,
	// never printed or persisted, including on error.
	return cmd.CombinedOutput()
}

func suggestInitDefaults(root string) (check, tools string, err error) {
	check, tools, _, err = suggestInitDefaultsWithSource(root)
	return
}

func suggestInitDefaultsWithSource(root string) (check, tools, source string, err error) {
	exists := func(name string) bool { s, e := os.Stat(filepath.Join(root, name)); return e == nil && !s.IsDir() }
	switch {
	case exists("go.mod"):
		return "go test ./...", strings.Join(claude.DefaultWorkerTools(), "+"), "go.mod", nil
	case exists("package.json"):
		data, e := os.ReadFile(filepath.Join(root, "package.json"))
		if e != nil {
			return "", "", "", e
		}
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if e = json.Unmarshal(data, &pkg); e != nil {
			return "", "", "", fmt.Errorf("read package.json: %w", e)
		}
		if strings.TrimSpace(pkg.Scripts["test"]) != "" {
			return "npm test", "Bash(npm test:*)", "package.json", nil
		}
	}
	if exists("Cargo.toml") {
		return "cargo test", "Bash(cargo test:*)", "Cargo.toml", nil
	}
	if exists("uv.lock") {
		return "uv run pytest", "Bash(uv run pytest:*)", "uv.lock", nil
	}
	if exists("poetry.lock") {
		return "poetry run pytest", "Bash(poetry run pytest:*)", "poetry.lock", nil
	}
	for _, name := range []string{"pyproject.toml", "pytest.ini"} {
		if exists(name) {
			return "python -m pytest", "Bash(python -m pytest:*)", name, nil
		}
	}
	return "", "", "", nil
}
