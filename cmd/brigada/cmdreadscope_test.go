package main

// The read-scope hook command (§4.1): run dispatches it outside the
// command table, --root names the one directory the session's file
// tools may read, the payload arrives on stdin, and the exit code is
// the verdict — 0 lets the call through, 2 blocks it with the reason
// on stderr for the model to see. The verdicts themselves are
// TestReadScopeDeny's, in the claude adapter's tests.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readScopePayload renders one PreToolUse payload for the command's
// stdin, the way the CLI feeds the hook.
func readScopePayload(tool, path string) string {
	return fmt.Sprintf(`{"tool_name":%q,"cwd":"/nowhere","tool_input":{"file_path":%q}}`, tool, path)
}

func TestReadScopeCommand(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("read me"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The files the scope must refuse: the user's own settings, another
	// directory under the state tree, a neighbour of the checkout.
	home := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home, []byte(`{"permissions":{"allow":["Read"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	neighbour := t.TempDir()
	cases := []struct {
		name     string
		payload  string
		wantCode int
		wantErr  string
	}{
		{"a Read inside the checkout", readScopePayload("Read", filepath.Join(checkout, "README.md")), 0, ""},
		{"a Read outside, the user's own settings", readScopePayload("Read", home), 2, "read scope"},
		{"a Grep outside the checkout", fmt.Sprintf(`{"tool_name":"Grep","cwd":"/nowhere","tool_input":{"pattern":"token","path":%q}}`, neighbour), 2, "read scope"},
		{"not a hook payload", "garbage", 0, ""},
	}
	for _, tc := range cases {
		var out, errb bytes.Buffer
		code := runReadScope(context.Background(), []string{"--root", checkout},
			strings.NewReader(tc.payload), &out, &errb)
		if code != tc.wantCode {
			t.Errorf("%s: exit = %d, want %d (stderr %q)", tc.name, code, tc.wantCode, errb.String())
		}
		if tc.wantErr != "" && !strings.Contains(errb.String(), tc.wantErr) {
			t.Errorf("%s: stderr = %q, want it to name the %q", tc.name, errb.String(), tc.wantErr)
		}
		if code == 0 && errb.Len() != 0 {
			t.Errorf("%s: an allowed call spoke on stderr: %q", tc.name, errb.String())
		}
	}
}

// TestReadScopeCommandRefusals: no root names nothing to guard, and the
// command is dispatched from run's argument path, so it must parse
// without touching stdin.
func TestReadScopeCommandRefusals(t *testing.T) {
	var out, errb bytes.Buffer
	code := runReadScope(context.Background(), nil, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "--root") {
		t.Errorf("no root: exit = %d, stderr = %q, want a usage error naming --root", code, errb.String())
	}
	code = runReadScope(context.Background(), []string{"--root", t.TempDir(), "stray"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "positional") {
		t.Errorf("stray argument: exit = %d, stderr = %q, want a usage error", code, errb.String())
	}
	// The dispatch itself: a bare readscope reaches the command (and
	// stops at the usage error) rather than the unknown-command path.
	errb.Reset()
	if code := run(context.Background(), []string{"readscope"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "--root") {
		t.Errorf("run(readscope): exit = %d, stderr = %q, want the readscope usage error", code, errb.String())
	}
}
