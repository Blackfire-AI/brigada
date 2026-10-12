// Package testsupport holds the helpers the repository's tests share.
// Nothing outside tests imports it; nothing in it is product code.
package testsupport

import (
	"os"
	"path/filepath"
	"testing"
)

// ShortSocketPath returns a fresh, short path for one test's Unix
// socket, with its directory removed when the test ends. A kernel's
// sockaddr_un takes at most 104 bytes of path (macOS; 108 on Linux),
// and the obvious spot — t.TempDir(), whose name carries the test's —
// runs past that where temporary directories are long, so the bind
// fails with a bare "invalid argument". The socket gets its own short
// directory instead: under os.TempDir() when that leaves room, under
// /tmp otherwise. The result is asserted under 100 bytes — inside
// every kernel's cap with room to spare.
func ShortSocketPath(t testing.TB) string {
	t.Helper()
	base := os.TempDir()
	if len(longestPathUnder(base)) >= 100 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "bs")
	if err != nil {
		t.Fatalf("make a short temp dir under %s: %v", base, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	if len(path) >= 100 {
		t.Fatalf("socket path %s is %d bytes, want under 100", path, len(path))
	}
	return path
}

// longestPathUnder estimates the longest path ShortSocketPath could
// build under base: MkdirTemp's random suffix runs about ten bytes.
func longestPathUnder(base string) string {
	return filepath.Join(base, "bs0000000000", "s.sock")
}
