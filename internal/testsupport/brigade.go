package testsupport

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// brigadaBin is the once-built binary all tests share: one build per
// test binary, in a directory of its own under the system temp dir, so
// no test's cleanup takes another's binary away.
var brigadaBin = sync.OnceValues(func() (string, error) {
	root := repoRoot()
	out, err := os.MkdirTemp("", "brigada-test-bin-")
	if err != nil {
		return "", fmt.Errorf("make the test bin dir: %w", err)
	}
	bin := filepath.Join(out, "brigada")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/brigada")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build ./cmd/brigada: %v: %s", err, out)
	}
	return bin, nil
})

// BuildBrigada builds the brigada binary once per test binary and returns
// its path, failing the test when the build does. The lifecycle and CLI
// tests run it as a real process: engines detached by clients, runners
// polling, the whole §13 story.
func BuildBrigada(t testing.TB) string {
	t.Helper()
	bin, err := brigadaBin()
	if err != nil {
		t.Fatalf("build the brigada binary: %v", err)
	}
	return bin
}

// repoRoot finds the repository's root: the testsupport package's own
// place, walked up to the first go.mod. Tests run from their package
// directories, so the source's directory is the anchor that does not
// move.
func repoRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}
