package runner

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// TestSaveRecordIsOwnerOnly: the run record on disk holds what the worker
// printed, so it is written readable by its owner alone, in a directory
// only the owner enters — and a record or directory an older runner left
// open is narrowed when a record is rewritten.
func TestSaveRecordIsOwnerOnly(t *testing.T) {
	state := t.TempDir()
	client := api.NewClientWithHTTPClient(&http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"answer":"ok"}`)), Header: make(http.Header)}, nil
	})}, "test-token")
	r := &jobRun{cfg: &Config{StateDir: state}, client: client, grant: &api.JobGrant{JobID: 1, LeaseID: "lease"}}

	dir := filepath.Join(state, "records")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"new.jsonl", "old.jsonl"} {
		if err := r.saveRecord(context.Background(), "transcript", name, []byte("a line\n")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s has mode %04o, want 0600", name, mode)
		}
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("records directory = %v, %v; want mode 0700", info, err)
	}
}
