package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestResolveRuntimeRequiresExplicitConfig(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	missing := filepath.Join(t.TempDir(), "missing-config")
	common := addCommon(newFlagSet("test", "brigada test", "Test command."))
	common.config, common.stateDir = missing, state
	var stderr bytes.Buffer
	if rt, code := resolveRuntime(common, &stderr); code == 0 || rt != nil || !strings.Contains(stderr.String(), "read config "+missing) {
		t.Fatalf("missing explicit config: runtime=%v code=%d stderr=%q", rt, code, stderr.String())
	}
	// The unconfigured default remains optional.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	common.config = ""
	stderr.Reset()
	if rt, code := resolveRuntime(common, &stderr); code != 0 || rt == nil || rt.stateDir != state {
		t.Fatalf("missing default config: runtime=%v code=%d stderr=%q", rt, code, stderr.String())
	}
	// An explicit file is parsed normally once it exists.
	if err := os.WriteFile(missing, []byte("notify = none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common.config = missing
	if rt, code := resolveRuntime(common, &stderr); code != 0 || rt == nil || rt.file.Notify != "none" {
		t.Fatalf("valid explicit config: runtime=%v code=%d stderr=%q", rt, code, stderr.String())
	}
}
