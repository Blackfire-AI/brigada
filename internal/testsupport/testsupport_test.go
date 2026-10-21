package testsupport

import (
	"context"
	"database/sql"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortSocketPath(t *testing.T) {
	a := ShortSocketPath(t)
	if len(a) >= 100 {
		t.Fatalf("path %s is %d bytes, want under 100", a, len(a))
	}
	if filepath.Base(a) != "s.sock" {
		t.Errorf("path = %q, want it to end in s.sock", a)
	}
	// Two calls give two sockets room: parallel tests never share.
	b := ShortSocketPath(t)
	if a == b {
		t.Fatalf("two calls returned the same path %q", a)
	}
	// The path binds — the whole point of the cap.
	l, err := net.Listen("unix", a)
	if err != nil {
		t.Fatalf("listen on %s: %v", a, err)
	}
	l.Close()
}

func TestShortSocketPathCleansUp(t *testing.T) {
	var dir string
	t.Run("inner", func(t *testing.T) {
		path := ShortSocketPath(t)
		dir = filepath.Dir(path)
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("the socket's directory: %v", err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the socket's directory %s survived the inner test: %v", dir, err)
	}
}

func TestShortStateDir(t *testing.T) {
	dir := ShortStateDir(t)
	socket := filepath.Join(dir, "brigada.sock")
	if len(socket) >= 100 {
		t.Fatalf("socket path %s is %d bytes, want under 100", socket, len(socket))
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", socket, err)
	}
	l.Close()
}

func TestBuildBrigada(t *testing.T) {
	bin := BuildBrigada(t)
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("the built binary: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable", bin)
	}
}

func TestNewGitRemote(t *testing.T) {
	remote := NewGitRemote(t)
	out, err := exec.Command("git", "ls-remote", remote, "refs/heads/main").Output()
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	if len(strings.Fields(string(out))) < 2 {
		t.Fatalf("the remote has no main branch: %q", out)
	}
	// Two calls give two remotes: parallel tests never share one.
	other := NewGitRemote(t)
	if remote == other {
		t.Fatalf("two calls returned the same remote %q", remote)
	}
	if out2, err := exec.Command("git", "ls-remote", other, "refs/heads/main").Output(); err != nil || len(strings.Fields(string(out2))) < 2 {
		t.Fatalf("the second remote has no main branch: %q (%v)", out2, err)
	}
	// The copy round-trips: a clone of it takes a commit and a push
	// back, so every part of the copied tree works — refs, objects and
	// the empty directories the copy recreates.
	clone := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=brigada test",
			"-c", "user.email=test@brigada.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s (in %s): %v: %s", args, dir, err, out)
		}
	}
	git(t.TempDir(), "clone", "-q", remote, clone)
	if err := os.WriteFile(filepath.Join(clone, "work.txt"), []byte("round trip\n"), 0o644); err != nil {
		t.Fatalf("seed the clone: %v", err)
	}
	git(clone, "checkout", "-q", "-b", "roundtrip")
	git(clone, "add", "-A")
	git(clone, "commit", "-qm", "the round trip")
	git(clone, "push", "-q", "origin", "roundtrip")
	if out, err := exec.Command("git", "ls-remote", remote, "refs/heads/roundtrip").Output(); err != nil || len(strings.Fields(string(out))) < 2 {
		t.Fatalf("the pushed branch is not on the remote: %q (%v)", out, err)
	}
}

func TestOpenStore(t *testing.T) {
	st := OpenStore(t)
	if st.Path() == ":memory:" {
		t.Errorf("the store is :memory:, want a private file")
	}
	// The template carries the whole schema: a write to a migrated
	// table works without any migration of the copy.
	if err := st.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO runners (name, created_at, last_seen_at) VALUES ('r', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		return err
	}); err != nil {
		t.Fatalf("write to the copied store: %v", err)
	}
	var n int
	if err := st.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM runners").Scan(&n)
	}); err != nil {
		t.Fatalf("read the copied store: %v", err)
	}
	if n != 1 {
		t.Errorf("runners = %d, want the one row written", n)
	}
}
