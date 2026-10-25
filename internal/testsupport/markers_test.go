package testsupport

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// awaitScript writes a script that ends on AwaitRelease and returns its
// path and a marker directory for it.
func awaitScript(t *testing.T) (script, markers string) {
	t.Helper()
	dir := t.TempDir()
	markers = filepath.Join(dir, "markers")
	if err := os.Mkdir(markers, 0o755); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(dir, "await.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+AwaitRelease+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, markers
}

// startAwait starts the script directly, as the fake adapter does.
func startAwait(t *testing.T, script, markers string) (*exec.Cmd, chan error) {
	t.Helper()
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "BRIGADA_TEST_MARKERS="+markers)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-done
	})
	return cmd, done
}

func waitExit(t *testing.T, done chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		done <- err // for the cleanup's receive
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("the script did not end after %s", what)
		return nil
	}
}

func TestAwaitReleaseEndsOnTheMarker(t *testing.T) {
	script, markers := awaitScript(t)
	_, done := startAwait(t, script, markers)
	if err := os.WriteFile(filepath.Join(markers, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, done, "the release"); err != nil {
		t.Fatalf("released script: %v, want exit 0", err)
	}
}

func TestAwaitReleaseEndsWhenTheMarkerDirGoes(t *testing.T) {
	script, markers := awaitScript(t)
	_, done := startAwait(t, script, markers)
	time.Sleep(200 * time.Millisecond) // into the loop
	if err := os.RemoveAll(markers); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, done, "its marker directory was removed"); err == nil {
		t.Fatal("script exited 0 without a release, want exit 1")
	}
}

// TestAwaitReleaseEndsWhenItsParentDies starts the script from a shell
// that exits, as a timed-out test binary does: the script, reparented,
// must end by itself although its marker directory is still there.
func TestAwaitReleaseEndsWhenItsParentDies(t *testing.T) {
	script, markers := awaitScript(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	// The short sleep lets the script start, and record this shell as
	// its parent, before the shell exits.
	parent := exec.Command("/bin/sh", "-c", `"$0" >/dev/null 2>&1 & echo $! > "$1"; sleep 0.2`, script, pidFile)
	parent.Env = append(os.Environ(), "BRIGADA_TEST_MARKERS="+markers)
	if err := parent.Run(); err != nil {
		t.Fatalf("parent shell: %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid file %q: %v", data, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !ProcGone(pid) {
		if time.Now().After(deadline) {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
			t.Fatalf("script (pid %d) still runs after its parent died", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
