package testsupport

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// HostileToolScript writes a CLI script that plays a notification tool
// which never returns: it traps SIGTERM and parks a grandchild that
// holds the stdout pipe open, so the exec's context kill takes only the
// script itself while the pipe stays open past it. It returns the
// script's path; the cleanup kills the grandchild once its pid lands in
// the script directory's grandchild.pid, so no process outlives the
// test.
func HostileToolScript(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := filepath.Join(dir, "hostile-tool")
	body := "#!/bin/sh\n" +
		"trap '' TERM\n" +
		"sleep 60 &\n" +
		"echo $! > " + shQuote(pidFile) + "\n" +
		"wait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write the hostile tool script: %v", err)
	}
	t.Cleanup(func() { killPidFile(pidFile) })
	return script
}

// killPidFile kills the pid a script wrote and waits for it to leave
// the process table, waiting briefly for the file itself: the writer
// may still be starting when the test ends. A pid that never appears
// or never goes is left to its own short sleep, never the test.
func killPidFile(pidFile string) {
	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				pid = n
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid <= 0 {
		return
	}
	syscall.Kill(pid, syscall.SIGKILL)
	for time.Now().Before(deadline) {
		if ProcGone(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shQuote quotes s as one single-quoted shell word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
