package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestConfigShowsResolvedPermission: config shows each saved agent's
// resolved permission (its own permission=, else the machine file's, else the
// harness default, with tools= read as the deprecated alias), warns once for
// a tools= agent, and lists the permission key beside its alias.
func TestConfigShowsResolvedPermission(t *testing.T) {
	fakeInitPATH(t)
	t.Chdir(t.TempDir())
	state := testsupport.ShortStateDir(t)
	config := filepath.Join(state, "machine-config")
	if err := os.WriteFile(config, []byte("permission.claude/haiku = plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), []string{
		"harness=claude,model=opus,permission=acceptEdits",
		"harness=claude,model=sonnet,tools=none",
		"harness=claude,model=haiku",
		"harness=claude,model=fable",
	}); err != nil {
		t.Fatal(err)
	}
	out := configDo(t, context.Background(), "--state-dir", state, "--config", config)
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"permission: acceptEdits",
		"permission: none",
		"permission: plan",
		"permission: " + adapter.DefaultClaudePermission,
		"permission.claude/haiku = plan",
		"permission.<harness>/<model> = the harness default (default)",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("config lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(flat, adapter.DeprecatedKeyWarning("tools")); n != 1 {
		t.Errorf("tools= warning printed %d times, want 1:\n%s", n, out)
	}
}

// TestInitSavedAgentCarriesPermission: init suggests the permission default,
// not a tool scope: the saved claude line carries permission=, project.yaml
// carries no worker_tools, and an existing worker_tools stays and reads with
// its one warning.
func TestInitSavedAgentCarriesPermission(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	old := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = old })
	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	state := testsupport.ShortStateDir(t)
	t.Cleanup(func() {
		if code, out, errs := stopDo(t, state, "--engine"); code != 0 {
			t.Errorf("cleanup: %d\n%s\n%s", code, out, errs)
		}
	})
	base := []string{"--yes", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex", "--no-wiring"}
	if code, out, errs := initFixedParallel(t, state, "", append(base, "--check", "echo checked")...); code != 0 {
		t.Fatalf("init = %d\n%s\n%s", code, out, errs)
	}
	saved, err := os.ReadFile(filepath.Join(state, "runner", "agents"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(saved)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "permission="+adapter.DefaultClaudePermission) || strings.Contains(string(saved), "tools=") {
		t.Errorf("saved agents: %q", saved)
	}
	content, err := os.ReadFile(engine.ProjectConfigPath)
	if err != nil || strings.Contains(string(content), "worker_tools") {
		t.Fatalf("project.yaml: %s, %v", content, err)
	}
	// An existing worker_tools stays through a rerun and reads with one warning.
	content = append(content, []byte("worker_tools: Bash(echo checked:*)\n")...)
	if err := os.WriteFile(engine.ProjectConfigPath, content, 0644); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := initFixedParallel(t, state, "", append(base, "--check", "echo checked")...); code != 0 {
		t.Fatalf("rerun = %d\n%s\n%s", code, out, errs)
	}
	got, err := os.ReadFile(engine.ProjectConfigPath)
	if err != nil || !strings.Contains(string(got), "worker_tools: Bash(echo checked:*)") {
		t.Fatalf("rerun dropped worker_tools: %s, %v", got, err)
	}
	cfg, err := engine.ParseProjectConfig(got)
	if err != nil || len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "permission=") {
		t.Errorf("warnings = %q, %v", cfg.Warnings, err)
	}
}
