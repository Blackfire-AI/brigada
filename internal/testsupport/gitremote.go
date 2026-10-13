package testsupport

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ShortStateDir returns a fresh, short directory for one test's engine
// state, removed when the test ends. The socket inside it —
// <dir>/brigada.sock — must stay under the kernel's sun_path cap, which
// t.TempDir()'s test-named paths cannot promise; the directory is
// checked against the same 100-byte bound as ShortSocketPath.
func ShortStateDir(t testing.TB) string {
	t.Helper()
	base := os.TempDir()
	if len(filepath.Join(base, "bs0000000000", "brigada.sock")) >= 100 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "bs")
	if err != nil {
		t.Fatalf("make a short temp dir under %s: %v", base, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if got := len(filepath.Join(dir, "brigada.sock")); got >= 100 {
		t.Fatalf("socket path in %s would be %d bytes, want under 100", dir, got)
	}
	return dir
}

// NewGitRemote creates a local bare git remote holding one commit on
// main, the stand-in for a task's repo (§5.1): file-path URLs, no
// network, no credentials. The remote outlives the test's other temp
// dirs only as long as the test does.
func NewGitRemote(t testing.TB) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for the git-remote tests: %v", err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	work := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=brigada test",
			"-c", "user.email=test@brigada.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s (in %s): %v: %s", args, dir, err, out)
		}
	}
	git("", "init", "-q", "--bare", remote)
	git("", "init", "-q", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# a remote\n"), 0o644); err != nil {
		t.Fatalf("seed the remote's README: %v", err)
	}
	git(work, "add", "-A")
	git(work, "commit", "-qm", "the base commit")
	git(work, "branch", "-M", "main")
	git(work, "push", "-q", remote, "main")
	return remote
}
