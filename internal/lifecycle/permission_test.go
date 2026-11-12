package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadConfigText(t *testing.T, text string) (FileConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfigFile(path)
}

func TestConfigPermissionKey(t *testing.T) {
	cfg, err := loadConfigText(t, "permission.claude/sonnet = acceptEdits\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AgentPermission("claude", "sonnet"); got != "acceptEdits" {
		t.Fatalf("AgentPermission = %q, want acceptEdits", got)
	}
	if got := cfg.AgentPermission("claude", "opus"); got != "" {
		t.Fatalf("unnamed pair = %q, want empty", got)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none", cfg.Warnings)
	}
	for _, bad := range []string{"permission.claude = x\n", "permission.claude/sonnet =\n", "permission.claude/sonnet = a b\n"} {
		if _, err := loadConfigText(t, bad); err == nil {
			t.Errorf("%q: want a refusal", bad)
		}
	}
}

func TestConfigToolsAliasesPermission(t *testing.T) {
	cfg, err := loadConfigText(t, "tools.claude/sonnet = Bash(go test:*)+Bash(gofmt:*)\ntools.claude/opus = none\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AgentPermission("claude", "sonnet"); got != "Bash(go test:*)+Bash(gofmt:*)" {
		t.Fatalf("sonnet permission = %q", got)
	}
	if got := cfg.AgentPermission("claude", "opus"); got != "none" {
		t.Fatalf("opus permission = %q, want none", got)
	}
	if len(cfg.Tools) != 2 {
		t.Fatalf("Tools = %v, the deprecated key must keep reading", cfg.Tools)
	}
	if len(cfg.Warnings) != 2 || !strings.Contains(cfg.Warnings[0], "tools.claude/opus") || !strings.Contains(cfg.Warnings[0], "permission=") {
		t.Fatalf("Warnings = %v, want one per tools key naming it and permission=", cfg.Warnings)
	}

	// The same value under both spellings is fine; different values are not,
	// in either order.
	if _, err := loadConfigText(t, "tools.claude/sonnet = none\npermission.claude/sonnet = none\n"); err != nil {
		t.Fatalf("equal values: %v", err)
	}
	for _, both := range []string{"tools.claude/sonnet = none\npermission.claude/sonnet = plan\n", "permission.claude/sonnet = plan\ntools.claude/sonnet = none\n"} {
		if _, err := loadConfigText(t, both); err == nil || !strings.Contains(err.Error(), "disagree") {
			t.Errorf("%q: err = %v, want a disagreement", both, err)
		}
	}
}

func TestConfigReadScope(t *testing.T) {
	cfg, err := loadConfigText(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AgentReadScope("claude", "sonnet") {
		t.Fatal("read scope must default to on")
	}
	cfg, err = loadConfigText(t, "read_scope.claude/sonnet = false\nread_scope.claude/opus = true\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentReadScope("claude", "sonnet") {
		t.Fatal("read_scope.claude/sonnet = false must turn it off")
	}
	if !cfg.AgentReadScope("claude", "opus") || !cfg.AgentReadScope("claude", "haiku") {
		t.Fatal("an agent not turned off stays on")
	}
	if _, err := loadConfigText(t, "read_scope.claude/sonnet = maybe\n"); err == nil {
		t.Fatal("want a refusal of a non-boolean")
	}
}

func TestResolvePermission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		harness    string
		permission string
		tools      []string
		file       string
		want       string
	}{
		{"claude default", "claude", "", nil, "", "bypassPermissions"},
		{"other harness has no default", "codex", "", nil, "", ""},
		{"file", "claude", "", nil, "plan", "plan"},
		{"tools alias over file", "claude", "", []string{"Bash(go build:*)"}, "plan", "Bash(go build:*)"},
		{"tools none alias", "claude", "", []string{}, "", "none"},
		{"permission over all", "claude", "acceptEdits", []string{"x"}, "plan", "acceptEdits"},
	} {
		if got := ResolvePermission(tc.harness, tc.permission, tc.tools, tc.file); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The config file's set and get round-trip a permission key: what
// SetConfigValues writes is what LoadConfigFile reads back, with no
// deprecation notice (permission= is not the alias).
func TestConfigPermissionSetGetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	changed, err := SetConfigValues(path, map[string]string{
		"permission.claude/sonnet": "acceptEdits",
		"permission.claude/opus":   "plan",
	})
	if err != nil || !changed {
		t.Fatalf("set: changed=%v err=%v", changed, err)
	}
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AgentPermission("claude", "sonnet"); got != "acceptEdits" {
		t.Errorf("sonnet = %q, want acceptEdits", got)
	}
	if got := cfg.AgentPermission("claude", "opus"); got != "plan" {
		t.Errorf("opus = %q, want plan", got)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", cfg.Warnings)
	}
	// A second set of the same values changes nothing.
	changed, err = SetConfigValues(path, map[string]string{
		"permission.claude/sonnet": "acceptEdits",
		"permission.claude/opus":   "plan",
	})
	if err != nil || changed {
		t.Errorf("idempotent set: changed=%v err=%v", changed, err)
	}
}
