package claude

// The read scope's decisions (§4.1): what the hook the foreman
// session's settings carry decides for one PreToolUse payload. The
// settings themselves — the hook riding them, the root they name — are
// TestForemanSettings's, in claude_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hookJSON renders one PreToolUse payload the way the CLI feeds the
// hook: the tool's name, the directory the session runs in, and the
// call's own arguments as raw JSON.
func hookJSON(tool, cwd, input string) []byte {
	return []byte(`{"tool_name":"` + tool + `","cwd":"` + cwd + `","tool_input":` + input + `}`)
}

// readInput is a Read call's arguments.
func readInput(path string) string {
	return `{"file_path":"` + path + `"}`
}

// TestReadScopeDeny covers the hook's verdicts: a file tool call is
// refused unless every path it names stays inside the allowed root —
// the reading checkout — however the path is spelled, and every other
// shape of payload stands. The allowed-root rule is what the settings
// carry, so an allow rule in the user's own settings changes nothing
// here: the hook never sees one.
func TestReadScopeDeny(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("read me"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory beside the checkout: another project's data, the
	// state directory's files, the user's own — all outside.
	outside := t.TempDir()
	home := filepath.Join(outside, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(outside, "tokens")
	if err := os.WriteFile(tokens, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the root that points outside it: reading through
	// the link reads the target.
	linked := filepath.Join(root, "linked-secret")
	if err := os.Symlink(home, linked); err != nil {
		t.Skipf("symlinks are not ours to make here: %v", err)
	}
	// The root spelled through a symlink of its own: macOS /var against
	// /private/var is the shape.
	alias := filepath.Join(outside, "checkout-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks are not ours to make here: %v", err)
	}

	cases := []struct {
		name     string
		root     string
		payload  []byte
		wantDeny bool
	}{
		{"read inside the root, absolute", root, hookJSON("Read", root, readInput(filepath.Join(root, "README.md"))), false},
		{"read inside the root, relative", root, hookJSON("Read", root, readInput("docs/deep/notes.md")), false},
		{"read the root itself", root, hookJSON("Read", root, readInput(root)), false},
		{"read a neighbour of the root", root, hookJSON("Read", root, readInput(home)), true},
		{"read the state directory's tokens", root, hookJSON("Read", root, readInput(tokens)), true},
		{"read through .. escapes", root, hookJSON("Read", root, readInput("../../etc/passwd")), true},
		{"read through a symlink inside the root", root, hookJSON("Read", root, readInput("linked-secret")), true},
		{"read inside, the root spelled through a symlink", alias, hookJSON("Read", alias, readInput(filepath.Join(alias, "README.md"))), false},
		{"read outside, the root spelled through a symlink", alias, hookJSON("Read", alias, readInput(home)), true},
		{"read a notebook inside the root", root, hookJSON("Read", root, `{"notebook_path":"docs/notes.ipynb"}`), false},
		{"read a notebook outside the root", root, hookJSON("Read", root, `{"notebook_path":"`+home+`"}`), true},
		{"grep inside the root", root, hookJSON("Grep", root, `{"pattern":"TODO","path":"docs"}`), false},
		{"grep outside the root", root, hookJSON("Grep", root, `{"pattern":"token","path":"`+tokens+`"}`), true},
		{"grep with no path stays in the working directory", root, hookJSON("Grep", root, `{"pattern":"TODO"}`), false},
		{"glob inside the root", root, hookJSON("Glob", root, `{"pattern":"docs/**/*.go"}`), false},
		{"glob through .. escapes", root, hookJSON("Glob", root, `{"pattern":"../../`+filepath.Base(outside)+`/*.md"}`), true},
		{"glob outside the root, absolute", root, hookJSON("Glob", root, `{"pattern":"`+outside+`/*.json"}`), true},
		{"glob with a path outside the root", root, hookJSON("Glob", root, `{"pattern":"*.json","path":"`+outside+`"}`), true},
		{"a relative path against a cwd outside the root", root, hookJSON("Read", outside, readInput("settings.json")), true},
		{"a tool that is not a file tool", root, hookJSON("Task", root, `{"prompt":"read everything"}`), false},
		{"a payload that is not JSON", root, []byte("not json"), false},
		{"a payload that names no tool", root, []byte(`{"tool_input":{"file_path":"/etc/passwd"}}`), false},
		{"a payload with no path argument", root, hookJSON("Read", root, `{}`), false},
	}
	for _, tc := range cases {
		reason, deny := ReadScopeDeny(tc.root, tc.payload)
		if deny != tc.wantDeny {
			t.Errorf("%s: deny = %v (reason %q), want %v", tc.name, deny, reason, tc.wantDeny)
		}
		if deny && !strings.Contains(reason, "read scope") {
			t.Errorf("%s: the refusal names nothing to act on: %q", tc.name, reason)
		}
	}
}
