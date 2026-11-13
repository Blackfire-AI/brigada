package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// initDefaultPermissions is the "What workers may do" row of a claude agent
// that sets no permission= and no deprecated tools= scope.
const initDefaultPermissions = "edit files and run commands under permission bypassPermissions"

// TestInitSuggestsPermissionDefault: without --tools, init proposes the
// harness's permission default per agent (carried as permission= in the
// saved agent line), not a worker scope derived from the check, and writes no
// worker_tools to project.yaml.
func TestInitSuggestsPermissionDefault(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	state := testsupport.ShortStateDir(t)
	code, out, errOut := initFixedParallel(t, state, "", "--probe", "--check", "make check",
		"--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5",
		"--signed-in", "claude,codex", "--no-wiring", "--verbose")
	if code != 0 || errOut != "" {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "    claude    "+initDefaultPermissions+"\n") {
		t.Errorf("permission default missing:\n%s", out)
	}
	if strings.Contains(out, "worker_tools") || strings.Contains(out, "run only") {
		t.Errorf("init suggested a tool scope:\n%s", out)
	}
}

// TestInitSavedAgentCarriesPermission: the agent line init saves carries
// permission= and no tools=; an explicit --tools still writes its deprecated
// spelling, and the agent line reads back with the deprecation warning.
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
	args := []string{"--yes", "--check", "none", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex", "--no-wiring"}
	if code, out, errs := initFixedParallel(t, state, "", args...); code != 0 {
		t.Fatalf("init = %d\n%s\n%s", code, out, errs)
	}
	lines, err := runnerstate.LoadAgents(filepath.Join(state, "runner"))
	if err != nil {
		t.Fatal(err)
	}
	var claudeLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "harness=claude") {
			claudeLine = l
		}
	}
	if !strings.Contains(claudeLine, "permission=bypassPermissions") || strings.Contains(claudeLine, "tools=") {
		t.Fatalf("saved claude line = %q, want permission= and no tools=", claudeLine)
	}
	content, err := os.ReadFile(engine.ProjectConfigPath)
	if err != nil || strings.Contains(string(content), "worker_tools") {
		t.Fatalf("project.yaml = %q, %v; want no worker_tools", content, err)
	}
	var specs agentSpecs
	if err := specs.Set(claudeLine); err != nil || len(specs.specs[0].warnings()) != 0 {
		t.Fatalf("permission line read back: %v %v", err, specs.specs)
	}

	// An existing tools= line still reads, with the deprecation warning.
	var legacy agentSpecs
	if err := legacy.Set("harness=claude,model=sonnet,tools=none"); err != nil {
		t.Fatal(err)
	}
	if w := legacy.specs[0].warnings(); len(w) != 1 || !strings.Contains(w[0], "permission=") {
		t.Fatalf("tools= warnings = %v", w)
	}
}

// TestProjectConfigDefaultsKeepsWorkerTools: an empty scope proposes no
// worker_tools and leaves the one the file has.
func TestProjectConfigDefaultsKeepsWorkerTools(t *testing.T) {
	fresh, err := engine.ProjectConfigDefaults(nil, "example.com/org/repo", "make check", "")
	if err != nil || strings.Contains(string(fresh), "worker_tools") {
		t.Fatalf("fresh = %q, %v", fresh, err)
	}
	kept, err := engine.ProjectConfigDefaults([]byte("schema: 1\nrepo: example.com/org/repo\nworker_tools: none\n"), "example.com/org/repo", "make check", "")
	if err != nil || !strings.Contains(string(kept), "worker_tools: none") {
		t.Fatalf("kept = %q, %v", kept, err)
	}
	cfg, err := engine.ParseProjectConfig(kept)
	if err != nil || len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "worker_tools") {
		t.Fatalf("worker_tools reads without its warning: %+v %v", cfg.Warnings, err)
	}
}
