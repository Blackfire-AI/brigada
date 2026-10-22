package testsupport

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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

// The template remote: its files by path and its directories by path,
// captured once per process. Writing a copy is plain file work; git
// runs only while the template is built.
var (
	gitRemoteOnce sync.Once
	gitRemoteDirs []string
	gitRemoteTree map[string][]byte
	gitRemoteErr  error
)

// NewGitRemote creates a local bare git remote holding one commit on
// main, the stand-in for a task's repo (§5.1): file-path URLs, no
// network, no credentials. The template is built by the process's
// first call; every call then writes its own copy — files and
// directories, empty ones included — under the test's temp dir,
// removed when the test ends.
func NewGitRemote(t testing.TB) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for the git-remote tests: %v", err)
	}
	gitRemoteOnce.Do(buildGitRemoteTemplate)
	if gitRemoteErr != nil {
		t.Fatalf("build the git-remote template: %v", gitRemoteErr)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	for _, dir := range gitRemoteDirs {
		if err := os.MkdirAll(filepath.Join(remote, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatalf("make the remote's %s: %v", dir, err)
		}
	}
	for name, data := range gitRemoteTree {
		path := filepath.Join(remote, filepath.FromSlash(name))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write the remote's %s: %v", name, err)
		}
	}
	return remote
}

// buildGitRemoteTemplate builds the bare remote — init, one commit,
// push — and keeps its tree: every directory and file, so a copy is the
// whole repository, empty directories (refs/tags, objects/pack) and
// all.
func buildGitRemoteTemplate() {
	dir, err := os.MkdirTemp("", "bs-git-remote-template-")
	if err != nil {
		gitRemoteErr = err
		return
	}
	defer os.RemoveAll(dir)
	remote := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=brigada test",
			"-c", "user.email=test@brigada.invalid", "-c", "commit.gpgsign=false",
			"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			gitRemoteErr = &gitError{args: args, dir: dir, err: err, out: out}
			return
		}
	}
	git("", "init", "-q", "--bare", remote)
	if gitRemoteErr != nil {
		return
	}
	// Background maintenance and auto-gc transiently create and remove
	// files (objects/maintenance.lock) inside the remote while its tree
	// is captured; turn both off in the remote so later commands
	// inherit the quiet config.
	git(remote, "config", "gc.auto", "0")
	if gitRemoteErr != nil {
		return
	}
	git(remote, "config", "maintenance.auto", "false")
	if gitRemoteErr != nil {
		return
	}
	git("", "init", "-q", work)
	if gitRemoteErr != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# a remote\n"), 0o644); err != nil {
		gitRemoteErr = err
		return
	}
	git(work, "add", "-A")
	if gitRemoteErr != nil {
		return
	}
	git(work, "commit", "-qm", "the base commit")
	if gitRemoteErr != nil {
		return
	}
	git(work, "branch", "-M", "main")
	if gitRemoteErr != nil {
		return
	}
	git(work, "push", "-q", remote, "main")
	if gitRemoteErr != nil {
		return
	}
	gitRemoteDirs, gitRemoteTree, gitRemoteErr = captureTree(remote)
}

// captureTree walks root and captures every directory by slash path and
// every file by slash path to its contents. A file that vanishes between
// the walk and the read is skipped: git's background maintenance can
// transiently create and remove objects/maintenance.lock inside a live
// repository mid-walk. Other errors are returned.
func captureTree(root string) (dirs []string, tree map[string][]byte, err error) {
	dirs, tree = nil, map[string][]byte{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, filepath.ToSlash(rel))
			return nil
		}
		if captureTreeReadHook != nil {
			captureTreeReadHook(path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		tree[filepath.ToSlash(rel)] = data
		return nil
	})
	return dirs, tree, err
}

// captureTreeReadHook, nil outside tests, runs before each file read so
// a test can make a file vanish between the walk and the read.
var captureTreeReadHook func(path string)

// gitError carries the failing git run out of the template builder.
type gitError struct {
	args []string
	dir  string
	err  error
	out  []byte
}

func (e *gitError) Error() string {
	return "git " + e.args[0] + " (in " + e.dir + "): " + e.err.Error() + ": " + string(e.out)
}
