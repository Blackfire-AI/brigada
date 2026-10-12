package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unixClient is an HTTP client that talks to a Unix socket path.
func unixClient(t *testing.T, socketPath string) *http.Client {
	t.Helper()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
}

// waitUntilHealthy polls the socket until the engine answers, so a test
// never races the server's startup.
func waitUntilHealthy(t *testing.T, socketPath string) {
	t.Helper()
	client := unixClient(t, socketPath)
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://brigada/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine never answered on %s: %v", socketPath, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestListenSocket: the socket file is created 0600, a stale socket is
// replaced, a live engine's socket is never stolen, anything else on
// the path is refused and left alone, and a clean shutdown removes the
// file (§12.1).
func TestListenSocket(t *testing.T) {
	e := newEnv(t)
	socketPath := filepath.Join(t.TempDir(), "engine.sock")

	// A file that is not a socket — a typo'd path naming real data — is
	// refused and left exactly as it was.
	if err := os.WriteFile(socketPath, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("squat on the socket path: %v", err)
	}
	if err := e.api.Listen(context.Background(), socketPath); err == nil {
		t.Fatal("a Listen over a regular file succeeded; want refusal")
	} else if !strings.Contains(err.Error(), "not a Unix socket") {
		t.Errorf("refusal = %v, want it to say the path is not a socket", err)
	}
	if data, err := os.ReadFile(socketPath); err != nil || string(data) != "not a socket" {
		t.Errorf("the file at the path was touched: %q, %v", data, err)
	}
	// A directory on the path is refused the same way.
	dirOnPath := filepath.Join(t.TempDir(), "engine.sock")
	if err := os.Mkdir(dirOnPath, 0o700); err != nil {
		t.Fatalf("make the directory: %v", err)
	}
	if err := e.api.Listen(context.Background(), dirOnPath); err == nil {
		t.Fatal("a Listen over a directory succeeded; want refusal")
	}

	// A dead engine's leftover — the file a closed listener leaves — is
	// provably stale: replaced.
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("clear the refused file: %v", err)
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("bind the dead engine's socket: %v", err)
	}
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- e.api.Listen(ctx, socketPath) }()
	waitUntilHealthy(t, socketPath)

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %04o, want 0600", perm)
	}

	// While this engine serves, a second Listen on the same socket is
	// refused: never steal a live engine's socket.
	if err := e.api.Listen(context.Background(), socketPath); err == nil {
		t.Fatal("a second Listen on a live socket succeeded; want refusal")
	} else if !strings.Contains(err.Error(), "listening") {
		t.Errorf("refusal = %v, want it to say another engine is listening", err)
	}

	// Cancel: a clean return and the file goes with it.
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("Listen after cancel = %v, want nil", err)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file after shutdown = %v, want it removed", err)
	}

	// And once more from a clean path, so the bind-and-rename itself is
	// what served the last round's health checks too.
	ctx2, cancel2 := context.WithCancel(context.Background())
	served2 := make(chan error, 1)
	go func() { served2 <- e.api.Listen(ctx2, socketPath) }()
	waitUntilHealthy(t, socketPath)
	cancel2()
	if err := <-served2; err != nil {
		t.Fatalf("Listen over a stale socket = %v, want nil", err)
	}
}

func TestBodyCap(t *testing.T) {
	e := newEnv(t)
	over := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"%s"}`,
		e.project1, e.project1, strings.Repeat("a", 2<<20))
	code, _, data := e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(over))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("submit over the cap = %d %s, want 413", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeTooLarge {
		t.Errorf("over-cap code = %q, want %q", err.Code, codeTooLarge)
	}

	// A body just under the cap is nobody's business but the caller's.
	under := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"%s"}`,
		e.project1, e.project1, strings.Repeat("a", maxBody-300))
	code, _, data = e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(under))
	if code != http.StatusOK || decode[struct {
		Answer string `json:"answer"`
	}](t, data).Answer != "created" {
		t.Errorf("submit just under the cap = %d %s, want a created job", code, data)
	}
}
