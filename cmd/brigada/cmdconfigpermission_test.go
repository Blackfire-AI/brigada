package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestConfigShowsResolvedPermission: a saved permission= round-trips through
// the saved agents file to the config view, a file permission.<h>/<m> key
// fills an agent that names none, and neither warns.
func TestConfigShowsResolvedPermission(t *testing.T) {
	fakeInitPATH(t)
	t.Chdir(t.TempDir())
	state := testsupport.ShortStateDir(t)
	config := filepath.Join(state, "machine-config")
	if err := os.WriteFile(config, []byte("permission.claude/opus = plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), []string{
		"harness=claude,model=sonnet,permission=acceptEdits",
		"harness=claude,model=opus",
		"harness=claude,model=haiku,classes=answer",
	}); err != nil {
		t.Fatal(err)
	}
	out := configDo(t, context.Background(), "--state-dir", state, "--config", config)
	if strings.Contains(out, "warning:") {
		t.Errorf("permission= warned:\n%s", out)
	}
	for _, want := range []string{"permission: acceptEdits", "permission: plan", "permission: bypassPermissions", "permission.claude/opus = plan"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
