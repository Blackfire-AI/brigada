package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const importOrigin = "https://example.invalid/imported.git"

// importRepo makes a git repo with the origin importOrigin, chdirs into
// it and returns a writer for its .brigada/project.yaml.
func importRepo(t *testing.T) (writeFile func(body string)) {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", importOrigin}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
	t.Chdir(dir)
	return func(body string) {
		t.Helper()
		path := filepath.Join(dir, ".brigada", "project.yaml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func projectFile(body string) string {
	return "schema: 1\nrepo: example.invalid/imported\n" + body
}

func TestProjectImportCommand(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	write := importRepo(t)
	do := func(stdin string, args ...string) (int, string, string) {
		return runWithStdin(t, ctx, state, stdin, args)
	}
	statusOut := func() string {
		t.Helper()
		code, stdout, stderr := do("", "status")
		if code != 0 {
			t.Fatalf("status = %d, stderr %q", code, stderr)
		}
		return stdout
	}

	// An unknown project: status says nothing about the file.
	write(projectFile("default_check: make test\napproval_max_tasks: 2\n"))
	if out := statusOut(); strings.Contains(out, "project.yaml") {
		t.Fatalf("status flags a file for a project the engine does not know:\n%s", out)
	}

	// Import applies and show prints the provenance per key.
	code, stdout, stderr := do("", "project", "import")
	if code != 0 || !strings.Contains(stdout, "imported .brigada/project.yaml") ||
		!strings.Contains(stdout, "default_check: default -> make test") {
		t.Fatalf("import = %d %q (stderr %q)", code, stdout, stderr)
	}
	code, stdout, stderr = do("", "project", "show")
	if code != 0 || !strings.Contains(stdout, "default_check=make test (imported ") ||
		!strings.Contains(stdout, "approval_max_tasks=2 (imported ") ||
		!strings.Contains(stdout, "approval_classes=implement+review+answer (default)") {
		t.Fatalf("show = %d %q (stderr %q)", code, stdout, stderr)
	}
	if out := statusOut(); strings.Contains(out, "project.yaml") {
		t.Fatalf("status flags an imported, unchanged file:\n%s", out)
	}

	// A file change makes status flag it, and changes nothing by itself.
	write(projectFile("default_check: make check\napproval_max_tasks: 2\n"))
	if out := statusOut(); !strings.Contains(out, ".brigada/project.yaml: changed since the last import") {
		t.Fatalf("status does not flag the changed file:\n%s", out)
	}
	if _, stdout, _ = do("", "project", "show"); !strings.Contains(stdout, "default_check=make test (imported ") {
		t.Fatalf("status changed a setting: %q", stdout)
	}

	// A local override wins on re-import, with the conflict printed.
	if code, _, stderr = do("", "project", "set", "default_check=go test ./..."); code != 0 {
		t.Fatalf("set = %d %q", code, stderr)
	}
	write(projectFile("default_check: make check\napproval_max_tasks: 3\n"))
	code, stdout, stderr = do("", "project", "import")
	if code != 0 || !strings.Contains(stdout, "conflict: default_check: local override go test ./... kept, file has make check") ||
		!strings.Contains(stdout, "approval_max_tasks: 2 -> 3") ||
		!strings.Contains(stdout, "default_check=go test ./... (set)") {
		t.Fatalf("re-import = %d %q (stderr %q)", code, stdout, stderr)
	}
	if out := statusOut(); strings.Contains(out, "project.yaml") {
		t.Fatalf("status flags a file that was just imported:\n%s", out)
	}

	// A file of another repository is refused, with nothing changed.
	write("schema: 1\nrepo: example.invalid/other\ndefault_check: evil\n")
	code, _, stderr = do("", "project", "import")
	if code != 1 || !strings.Contains(stderr, "not this repository's origin") {
		t.Fatalf("mismatch = %d %q", code, stderr)
	}
	// An unknown key is refused before the engine is asked, naming it.
	write(projectFile("token: abc\n"))
	if code, _, stderr = do("", "project", "import"); code != 1 || !strings.Contains(stderr, `unknown key "token"`) {
		t.Fatalf("unknown key = %d %q", code, stderr)
	}
}

func TestProjectImportFirstOverHandSetAsks(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	write := importRepo(t)
	do := func(stdin string, args ...string) (int, string, string) {
		return runWithStdin(t, ctx, state, stdin, args)
	}
	if code, _, stderr := do("", "project", "set", "default_check=hand check"); code != 0 {
		t.Fatalf("set = %d %q", code, stderr)
	}
	write(projectFile("default_check: file check\napproval_max_tasks: 2\n"))

	// Declined: the diff is printed and nothing is stored.
	code, stdout, _ := do("n\n", "project", "import")
	if code != 1 || !strings.Contains(stdout, "default_check: hand-set hand check -> file file check") ||
		!strings.Contains(stdout, "nothing changed") {
		t.Fatalf("declined import = %d %q", code, stdout)
	}
	_, stdout, _ = do("", "project", "show")
	if !strings.Contains(stdout, "default_check=hand check (set)") || !strings.Contains(stdout, "approval_max_tasks=5 (default)") {
		t.Fatalf("a declined import changed the store: %q", stdout)
	}
	// No answer at all (EOF) is no either.
	if code, _, _ = do("", "project", "import"); code != 1 {
		t.Fatalf("an unanswered question imported anyway: exit %d", code)
	}

	// Accepted by answer.
	code, stdout, stderr := do("y\n", "project", "import")
	if code != 0 || !strings.Contains(stdout, "default_check=file check (imported ") {
		t.Fatalf("accepted import = %d %q (stderr %q)", code, stdout, stderr)
	}

	// --yes accepts without asking, on a fresh project.
	_, state2 := startSettingsEngine(t)
	state = state2
	if code, _, stderr = do("", "project", "set", "default_check=hand check"); code != 0 {
		t.Fatalf("set = %d %q", code, stderr)
	}
	code, stdout, stderr = do("", "project", "import", "--yes")
	if code != 0 || !strings.Contains(stdout, "default_check=file check (imported ") {
		t.Fatalf("--yes import = %d %q (stderr %q)", code, stdout, stderr)
	}
}

// runWithStdin runs one command against the engine of state, answering
// its questions from stdin. --state-dir goes right after the command
// words, before any key=value or flag of the call.
func runWithStdin(t *testing.T, ctx context.Context, state, stdin string, args []string) (int, string, string) {
	t.Helper()
	projectStdin = strings.NewReader(stdin)
	t.Cleanup(func() { projectStdin = os.Stdin })
	n := 1
	if args[0] == "project" {
		n = 2
	}
	full := append(append(append([]string{}, args[:n]...), "--state-dir", state), args[n:]...)
	var stdout, stderr bytes.Buffer
	code := run(ctx, full, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}
