package testsupport

import (
	"net"
	"os"
	"path/filepath"
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
