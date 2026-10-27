// Package testsupport holds the helpers the repository's tests share.
// Nothing outside tests imports it; nothing in it is product code.
package testsupport

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// WriteScript writes one executable #!/bin/sh script and returns its
// path: the scripted CLI a test drives in place of a real tool. A
// script that backgrounds children leaves their pids in files and the
// test's cleanup kills them with ReadPid and KillPids, so no orphan
// process outlives the test.
func WriteScript(t testing.TB, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script %s: %v", path, err)
	}
	return path
}

// ReadPid reads the pid a script wrote to path; 0 when the file is
// missing, unreadable or holds no pid.
func ReadPid(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// KillPids kills each pid that is still live, ignoring the ones already
// gone, so a scripted CLI's leftover children never outlive the test.
func KillPids(t testing.TB, pids ...int) {
	t.Helper()
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("kill pid %d: %v", pid, err)
		}
	}
}
