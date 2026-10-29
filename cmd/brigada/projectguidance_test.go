package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRepoFile(t *testing.T, rel, text string) {
	t.Helper()
	p := filepath.Join(".", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lineWith returns the first line of out that starts with prefix.
func lineWith(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// TestProjectSetRoleInstructions: `project set instructions.<role>=` for the
// four roles, from text and from @file, the old `instructions` key still
// working, and the refusals.
func TestProjectSetRoleInstructions(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	importRepo(t)
	run1 := func(args ...string) (int, string, string) { return runWithStdin(t, ctx, state, "", args) }

	writeRepoFile(t, "rev.md", "review rules\n")
	if code, _, stderr := run1("project", "set", "instructions.foreman=plan well", "instructions.reviewer=@rev.md", "instructions.worker=", "instructions.plan_review=pr"); code != 0 {
		t.Fatalf("set roles = %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run1("project", "set", "instructions=legacy worker text"); code != 0 {
		t.Fatalf("the old instructions key = %d, stderr %q", code, stderr)
	}
	_, out, _ := run1("project", "show")
	for prefix, want := range map[string]string{
		"instructions.foreman:":     "file, file none, 9 bytes",
		"instructions.reviewer:":    "file, file none, 13 bytes",
		"instructions.worker:":      "file, file none, 18 bytes", // the old key set the worker text
		"instructions.plan_review:": "file, file none, 2 bytes",
		"instructions=":             "instructions=",
	} {
		if l := lineWith(out, prefix); !strings.Contains(l, want) {
			t.Errorf("show line %q = %q, want %q in it\n%s", prefix, l, want, out)
		}
	}
	if !strings.Contains(out, "skills: none imported") {
		t.Errorf("show has no skills line:\n%s", out)
	}

	if code, _, stderr := run1("project", "set", "instructions.boss=x"); code == 0 || !strings.Contains(stderr, "boss") {
		t.Errorf("unknown role: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run1("project", "set", "instructions.worker="+strings.Repeat("x", 8193)); code == 0 || !strings.Contains(stderr, "8192") {
		t.Errorf("oversized role text: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run1("project", "set", "instructions=a", "instructions.worker=b"); code == 0 || !strings.Contains(stderr, "same text") {
		t.Errorf("the old key with instructions.worker: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run1("project", "set", "instructions.worker=a", "instructions.worker=b"); code == 0 || !strings.Contains(stderr, "twice") {
		t.Errorf("a role given twice: code %d, stderr %q", code, stderr)
	}
}

// TestProjectGuidanceChangedSinceImport: role files and skills edited,
// added or removed on disk are flagged by show and status while the stored
// snapshot (its digest) stays until a re-import.
func TestProjectGuidanceChangedSinceImport(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	write := importRepo(t)
	run1 := func(args ...string) (int, string, string) { return runWithStdin(t, ctx, state, "", args) }

	write(projectFile("instructions:\n  worker: w.md\nskills_dir: sk\n"))
	writeRepoFile(t, "w.md", "worker rules\n")
	writeRepoFile(t, "sk/one/SKILL.md", "---\nname: one\ndescription: first\n---\nbody\n")
	writeRepoFile(t, "sk/two/SKILL.md", "---\nname: two\ndescription: second\n---\nbody\n")
	if code, out, stderr := run1("project", "import", "--yes"); code != 0 {
		t.Fatalf("import = %d %q %q", code, out, stderr)
	}
	_, base, _ := run1("project", "show")
	for _, want := range []string{"instructions.worker: file, file w.md", "changed since import false", "skill one: first", "skill two: second"} {
		if !strings.Contains(base, want) {
			t.Fatalf("show after import lacks %q:\n%s", want, base)
		}
	}
	baseWorker, baseOne := lineWith(base, "instructions.worker:"), lineWith(base, "skill one:")

	// Edit a role file and a skill, add a skill, remove a skill.
	writeRepoFile(t, "w.md", "worker rules, edited\n")
	writeRepoFile(t, "sk/one/SKILL.md", "---\nname: one\ndescription: first, reworded\n---\nbody\n")
	writeRepoFile(t, "sk/three/SKILL.md", "---\nname: three\ndescription: third\n---\nbody\n")
	if err := os.RemoveAll("sk/two"); err != nil {
		t.Fatal(err)
	}
	_, show, _ := run1("project", "show")
	for _, want := range []string{"skill one: first, sha256", "changed since import true", "skill three: added since the last import", "skill two: removed since the last import"} {
		if !strings.Contains(show, want) {
			t.Errorf("show after the edits lacks %q:\n%s", want, show)
		}
	}
	if l := lineWith(show, "instructions.worker:"); !strings.HasSuffix(l, "changed since import true") {
		t.Errorf("the edited role file is not flagged: %q", l)
	}
	// The stored snapshot is unchanged: same digest, same description.
	sha := func(l string) string {
		_, rest, _ := strings.Cut(l, "sha256 ")
		d, _, _ := strings.Cut(rest, ",")
		return d
	}
	if sha(lineWith(show, "instructions.worker:")) != sha(baseWorker) || sha(lineWith(show, "skill one:")) != sha(baseOne) {
		t.Errorf("the stored digests moved before a re-import")
	}
	_, status, _ := run1("status")
	for _, want := range []string{"instructions.worker: file w.md", "instructions.worker w.md: changed since the last import", "skill one: changed since the last import", "skill three: added since the last import", "skill two: removed since the last import"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q:\n%s", want, status)
		}
	}

	// A re-import applies the changes and clears the flags.
	if code, out, stderr := run1("project", "import", "--yes"); code != 0 {
		t.Fatalf("re-import = %d %q %q", code, out, stderr)
	}
	_, after, _ := run1("project", "show")
	if strings.Contains(after, "changed since import true") || strings.Contains(after, "added since") || strings.Contains(after, "removed since") || !strings.Contains(after, "skill three: third") || strings.Contains(after, "skill two:") {
		t.Errorf("show after the re-import:\n%s", after)
	}
}

// TestProjectImportDroppedRoleKey: a role key dropped from the file's
// instructions map makes that role not configured at the next import
// (§5.9 'Three states, no hidden fallback'), reported as an applied
// change; a local override under the dropped key keeps its text and the
// import prints the conflict, as it does for a text the file carries.
func TestProjectImportDroppedRoleKey(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	write := importRepo(t)
	run1 := func(args ...string) (int, string, string) { return runWithStdin(t, ctx, state, "", args) }

	write(projectFile("instructions:\n  foreman: f.md\n  reviewer: r.md\n"))
	writeRepoFile(t, "f.md", "foreman rules\n")
	writeRepoFile(t, "r.md", "reviewer rules\n")
	if code, _, stderr := run1("project", "import", "--yes"); code != 0 {
		t.Fatalf("import = %d %q", code, stderr)
	}

	// The foreman key is dropped from the map.
	write(projectFile("instructions:\n  reviewer: r.md\n"))
	code, out, stderr := run1("project", "import", "--yes")
	if code != 0 {
		t.Fatalf("re-import = %d %q %q", code, out, stderr)
	}
	if l := lineWith(out, "  instructions.foreman:"); !strings.Contains(l, "-> not configured") {
		t.Errorf("the dropped key was not reported as not configured: %q\n%s", l, out)
	}
	_, show, _ := run1("project", "show")
	if l := lineWith(show, "instructions.foreman:"); !strings.Contains(l, "not configured") {
		t.Errorf("show after the drop: %q\n%s", l, show)
	}
	if l := lineWith(show, "instructions.reviewer:"); !strings.Contains(l, "file") {
		t.Errorf("the reviewer, still named, lost its file state: %q", l)
	}

	// A local override under a dropped key survives it, as a conflict.
	if code, _, stderr := run1("project", "set", "instructions.reviewer=mine"); code != 0 {
		t.Fatalf("set = %d %q", code, stderr)
	}
	write(projectFile("approval_max_tasks: 3\n"))
	code, out, stderr = run1("project", "import", "--yes")
	if code != 0 {
		t.Fatalf("import without a map = %d %q %q", code, out, stderr)
	}
	if l := lineWith(out, "conflict: instructions.reviewer"); !strings.Contains(l, `"mine"`) || !strings.Contains(l, "not configured") {
		t.Errorf("the kept override was not shown against not configured: %q\n%s", l, out)
	}
	_, show, _ = run1("project", "show")
	if l := lineWith(show, "instructions.reviewer:"); !strings.Contains(l, "file") || !strings.Contains(l, "4 bytes") {
		t.Errorf("the local override did not stand: %q\n%s", l, show)
	}
}

// TestProjectRoleConflictShowsBothTexts: after the first import, a local
// override keeps its text against a different imported file, and the
// conflict prints both texts; the same file over a still-imported text
// applies. (A first import accepted with --yes replaces hand-set values,
// as it does for every setting.)
func TestProjectRoleConflictShowsBothTexts(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	write := importRepo(t)
	run1 := func(args ...string) (int, string, string) { return runWithStdin(t, ctx, state, "", args) }

	write(projectFile("instructions:\n  worker: w.md\n  reviewer: r.md\n"))
	writeRepoFile(t, "w.md", "file worker text")
	writeRepoFile(t, "r.md", "file reviewer v1")
	if code, out, stderr := run1("project", "import", "--yes"); code != 0 {
		t.Fatalf("first import = %d %q %q", code, out, stderr)
	}
	if code, _, stderr := run1("project", "set", "instructions.worker=local worker text"); code != 0 {
		t.Fatalf("set = %d %q", code, stderr)
	}
	code, out, stderr := run1("project", "import", "--yes")
	if code != 0 {
		t.Fatalf("import = %d %q %q", code, out, stderr)
	}
	conflict := lineWith(out, "conflict: instructions.worker")
	if !strings.Contains(conflict, `"local worker text"`) || !strings.Contains(conflict, `"file worker text"`) {
		t.Errorf("the conflict does not show both texts: %q", conflict)
	}
	if strings.Contains(out, "conflict: instructions.reviewer") {
		t.Errorf("an unoverridden role conflicted:\n%s", out)
	}

	// A changed file over a still-imported text applies; the override stays.
	writeRepoFile(t, "r.md", "file reviewer v2")
	if code, out, stderr = run1("project", "import", "--yes"); code != 0 {
		t.Fatalf("re-import = %d %q %q", code, out, stderr)
	}
	_, show, _ := run1("project", "show")
	if l := lineWith(show, "instructions.reviewer:"); !strings.Contains(l, "16 bytes") || !strings.Contains(l, "changed since import false") {
		t.Errorf("the changed file did not apply over the previous import: %q", l)
	}
	if l := lineWith(show, "instructions.worker:"); !strings.Contains(l, "17 bytes") {
		t.Errorf("the local override did not stand: %q", l)
	}
}
