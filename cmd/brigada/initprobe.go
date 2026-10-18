package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var initHarnesses = []string{"claude", "codex", "kimi", "opencode"}

type harnessProbe struct {
	installed bool
	signedIn  bool
	status    string
	evidence  string
	checked   bool   // a documented command ran and its answer is in status
	rootHelp  string // claude only: root --help, kept in memory for its model row
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
		if i == 0 && err == nil && harness == "claude" {
			p.rootHelp = string(out)
		}
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
