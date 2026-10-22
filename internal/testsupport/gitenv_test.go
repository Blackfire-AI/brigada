package testsupport

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestDisableGitAutoMaintenance: the environment TestMain sets reaches
// the git commands a test runs, so a fresh `git init` repository sees
// gc.auto=0 and maintenance.auto=false without help from any config
// file. These two settings keep git's background writes out of .git
// while a TempDir cleanup removes it.
func TestDisableGitAutoMaintenance(t *testing.T) {
	if got := os.Getenv("GIT_CONFIG_COUNT"); got != "2" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 2 (TestMain must call DisableGitAutoMaintenance)", got)
	}

	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "--", ".")
	if got := run("config", "--get", "gc.auto"); got != "0" {
		t.Errorf("git config --get gc.auto = %q, want 0", got)
	}
	if got := run("config", "--get", "maintenance.auto"); got != "false" {
		t.Errorf("git config --get maintenance.auto = %q, want false", got)
	}
}
