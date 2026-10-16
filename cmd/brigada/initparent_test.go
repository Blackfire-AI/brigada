package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/foreman"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// parentRepo makes a repository named name under parent; an empty origin
// leaves it without a remote.
func parentRepo(t *testing.T, parent, name, origin string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q")
	if origin != "" {
		gitIn(t, dir, "remote", "add", "origin", origin)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// parentFolder builds the folder the scan tests share: two good
// repositories, a duplicate of the first, one without origin, one with
// a token in its origin URL, a hidden repository, a symlink out of the
// folder, a plain folder holding a repository (nested), and a plain file.
func parentFolder(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	parentRepo(t, parent, "alpha", "https://example.invalid/org/alpha.git")
	parentRepo(t, parent, "alpha2", "git@example.invalid:org/alpha")
	parentRepo(t, parent, "beta", "git@example.invalid:org/beta.git")
	parentRepo(t, parent, "gamma", "https://user:tok3n-secret@example.invalid/org/gamma.git")
	parentRepo(t, parent, "noorigin", "")
	parentRepo(t, parent, ".hidden", "https://example.invalid/org/hidden.git")
	outside := parentRepo(t, t.TempDir(), "outside", "https://example.invalid/org/outside.git")
	if err := os.Symlink(outside, filepath.Join(parent, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	parentRepo(t, filepath.Join(parent, "plain"), "inner", "https://example.invalid/org/inner.git")
	if err := os.WriteFile(filepath.Join(parent, "notes.txt"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return parent
}

// snapshotTree maps every path under root, relative, to its contents (a
// directory to "<dir>", a symlink to its target), .git included.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			out[rel] = "-> " + target
		case d.IsDir():
			out[rel] = "<dir>"
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScanParent(t *testing.T) {
	parent := parentFolder(t)
	got, err := scanParent(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ canon, skip string }
	have := map[string]row{}
	for _, c := range got {
		have[c.name] = row{c.canon, c.skip}
	}
	want := map[string]row{
		"alpha":    {"example.invalid/org/alpha", ""},
		"alpha2":   {"example.invalid/org/alpha", "duplicate of alpha (one project)"},
		"beta":     {"example.invalid/org/beta", ""},
		"gamma":    {"example.invalid/org/gamma", ""},
		"noorigin": {"", "no origin remote"},
		"linked":   {"", "symlink, not followed"},
		"plain":    {"", "not a git repository (not descended into)"},
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("scan = %v\nwant %v", have, want)
	}
	for _, c := range got {
		if strings.HasPrefix(c.name, ".") || c.name == "notes.txt" || c.name == "inner" {
			t.Errorf("the scan listed %q: hidden entries, plain files and nested repositories are not children to list", c.name)
		}
	}
	names := make([]string, len(got))
	for i, c := range got {
		names[i] = c.name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("children are not in name order: %v", names)
	}
}

// TestScanParentNotMistakenForRepo: a folder with a bogus .git is not a
// repository of its own, and a plain folder inside an enclosing
// repository is not taken for that repository.
func TestScanParentNotMistakenForRepo(t *testing.T) {
	parent := t.TempDir()
	if err := os.MkdirAll(filepath.Join(parent, "bogus", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(parent, "gitlink")); err != nil {
		t.Fatal(err)
	}
	realish := parentRepo(t, parent, "realish", "https://example.invalid/org/real.git")
	if err := os.RemoveAll(filepath.Join(realish, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parentRepo(t, t.TempDir(), "elsewhere", "https://example.invalid/org/else.git"), ".git"), filepath.Join(realish, ".git")); err != nil {
		t.Fatal(err)
	}
	got, err := scanParent(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	skips := map[string]string{}
	for _, c := range got {
		skips[c.name] = c.skip
	}
	if skips["bogus"] != "not a git repository (not descended into)" {
		t.Errorf("bogus = %q", skips["bogus"])
	}
	if skips["realish"] != ".git is a symlink, not followed" {
		t.Errorf("realish = %q", skips["realish"])
	}
	if skips["gitlink"] != "symlink, not followed" {
		t.Errorf("gitlink = %q", skips["gitlink"])
	}
}

func TestParseSelection(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
		bad  bool
	}{
		{"", nil, false},
		{"none", nil, false},
		{"all", []int{1, 2, 3}, false},
		{"2", []int{2}, false},
		{"1, 3 3", []int{1, 3}, false},
		{"0", nil, true},
		{"4", nil, true},
		{"x", nil, true},
	} {
		got, err := parseSelection(tc.in, 3)
		if (err != nil) != tc.bad || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseSelection(%q) = %v, %v; want %v (bad %v)", tc.in, got, err, tc.want, tc.bad)
		}
	}
}

// TestInitParentProbe: the list shows each child's path, canonical URL,
// state and skip reason, never a token from an origin URL, and a probe
// writes nothing and starts nothing.
func TestInitParentProbe(t *testing.T) {
	parent := parentFolder(t)
	before := snapshotTree(t, parent)
	fakeInitPATH(t)
	t.Chdir(parent)
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, errout := initDo(t, state, "", "--probe", "--all", "--foreman", "claude/sonnet")
	if code != 0 {
		t.Fatalf("probe = %d\n%s\n%s", code, out, errout)
	}
	for _, want := range []string{
		"(not a repository)",
		// Paths are matched from their last element: a temp dir may be
		// spelled through a symlinked prefix.
		"/alpha  example.invalid/org/alpha  unknown (engine not running)",
		"/beta  example.invalid/org/beta  unknown",
		"/gamma  example.invalid/org/gamma  unknown",
		"/alpha2: skipped: duplicate of alpha (one project)",
		"/noorigin: skipped: no origin remote",
		"/linked: skipped: symlink, not followed",
		"/plain: skipped: not a git repository",
		"[1] ", "[2] ", "[3] ",
		"Register 3 project(s) with foreman claude/sonnet",
		"Nothing is written into the child repositories.",
		".mcp.json: created",
		"Probe only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("probe output lacks %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{"tok3n-secret", ".hidden", "org/hidden", "inner", "notes.txt", "outside"} {
		if strings.Contains(out, absent) {
			t.Errorf("probe output mentions %q:\n%s", absent, out)
		}
	}
	if after := snapshotTree(t, parent); !reflect.DeepEqual(before, after) {
		t.Errorf("probe changed the folder:\nbefore %v\nafter  %v", before, after)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("probe created the state directory: %v", err)
	}
}

// TestInitParentRefusals: a foreman is needed to register, flags that set
// up one repository are refused in a folder, and a refusal writes nothing.
func TestInitParentRefusals(t *testing.T) {
	parent := parentFolder(t)
	before := snapshotTree(t, parent)
	fakeInitPATH(t)
	t.Chdir(parent)
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no foreman", []string{"--yes", "--all"}, "no foreman to give new projects"},
		{"bad foreman", []string{"--yes", "--all", "--foreman", "claude"}, "harness/model"},
		{"foreman cannot resume", []string{"--yes", "--all", "--foreman", "kimi/kimi-k2"}, "resumes headless"},
		{"repository flags", []string{"--yes", "--check", "go test ./..."}, "set up one repository"},
	} {
		code, out, errout := initDo(t, state, "", tc.args...)
		if code != 1 || !strings.Contains(out+errout, tc.want) {
			t.Errorf("%s: got %d\n%s\n%s", tc.name, code, out, errout)
		}
	}
	if after := snapshotTree(t, parent); !reflect.DeepEqual(before, after) {
		t.Errorf("a refusal changed the folder")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("a refusal created the state directory: %v", err)
	}
}

// TestInitParentNothingSelected: without --all and with no prompt answer
// (--yes), nothing is registered, the engine is not started, and the
// coordinator is still wired.
func TestInitParentNothingSelected(t *testing.T) {
	parent := parentFolder(t)
	fakeInitPATH(t)
	t.Chdir(parent)
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, errout := initDo(t, state, "", "--yes")
	if code != 0 || !strings.Contains(out, "No project selected") {
		t.Fatalf("got %d\n%s\n%s", code, out, errout)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("init started the engine with nothing to register: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, ".mcp.json")); err != nil {
		t.Errorf("the coordinator was not wired: %v", err)
	}
}

func registeredRepos(t *testing.T, ctx context.Context, state string) []string {
	t.Helper()
	paths := lifecycle.PathsOf(state)
	_, secret, err := lifecycle.TokenForRole(paths.Tokens, "client")
	if err != nil {
		t.Fatal(err)
	}
	projects, err := api.NewClient(paths.Socket, secret).Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, p := range projects {
		urls = append(urls, p.RepoURL)
	}
	sort.Strings(urls)
	return urls
}

// TestInitParentApplyAll: --all registers each new repository once, wires
// the coordinator in the folder's own scope (the bridge in .mcp.json, the
// instructions as a brigada-owned skill, no hooks, no settings), writes
// nothing into the children, and a second run changes nothing.
func TestInitParentApplyAll(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	parent := parentFolder(t)
	childrenBefore := snapshotTree(t, parent)
	fakeInitPATH(t)
	t.Chdir(parent)

	code, out, errout := runWithStdin(t, ctx, state, "", []string{"init", "--yes", "--all", "--foreman", "claude/sonnet"})
	if code != 0 {
		t.Fatalf("init = %d\n%s\n%s", code, out, errout)
	}
	want := []string{"example.invalid/org/alpha", "example.invalid/org/beta", "example.invalid/org/gamma"}
	if got := registeredRepos(t, ctx, state); !reflect.DeepEqual(got, want) {
		t.Errorf("registered %v, want %v", got, want)
	}
	if strings.Contains(out+errout, "tok3n-secret") {
		t.Errorf("a token from an origin URL reached the output:\n%s\n%s", out, errout)
	}

	// Only the folder's two coordinator files are new; every child is as it was.
	after := snapshotTree(t, parent)
	for rel, content := range after {
		if _, existed := childrenBefore[rel]; existed {
			if childrenBefore[rel] != content {
				t.Errorf("%s changed", rel)
			}
			continue
		}
		switch filepath.ToSlash(rel) {
		case ".mcp.json", ".claude", ".claude/skills", ".claude/skills/brigada-coordinator", ".claude/skills/brigada-coordinator/SKILL.md":
		default:
			t.Errorf("init wrote %s", rel)
		}
	}
	for rel := range childrenBefore {
		if _, ok := after[rel]; !ok {
			t.Errorf("init removed %s", rel)
		}
	}

	mcp := wireRead(t, parent, ".mcp.json")
	wantMCP := map[string]any{"mcpServers": map[string]any{"brigada-coordinator": map[string]any{"command": "brigada", "args": []any{"mcp", "--coordinator"}}}}
	if got := wireJSON(t, mcp); !reflect.DeepEqual(got, wantMCP) {
		t.Errorf(".mcp.json = %v", got)
	}
	skill := wireRead(t, parent, coordinatorSkillFile)
	if !strings.HasPrefix(skill, "---\nname: brigada-coordinator\n") || !strings.Contains(skill, coordinatorSkillMarker) || !strings.HasSuffix(skill, foreman.CoordinatorInstructions()) {
		t.Errorf("coordinator skill is not brigada's marked file with the instructions:\n%s", skill)
	}
	if !strings.Contains(skill, "explicit yes") || strings.Contains(skill, "`plan_create`") {
		t.Errorf("coordinator skill must tell the coordinator to wait for the user's yes and name no plan_create:\n%s", skill)
	}

	// Again: all known now, nothing changes.
	snapshot := snapshotTree(t, parent)
	code, out, errout = runWithStdin(t, ctx, state, "", []string{"init", "--yes", "--all", "--foreman", "claude/sonnet"})
	if code != 0 || !strings.Contains(out, "known") || !strings.Contains(out, "No project selected") || !strings.Contains(out, "already wired; unchanged") {
		t.Fatalf("second init = %d\n%s\n%s", code, out, errout)
	}
	if again := snapshotTree(t, parent); !reflect.DeepEqual(snapshot, again) {
		t.Errorf("the second init changed the folder")
	}
	if got := registeredRepos(t, ctx, state); !reflect.DeepEqual(got, want) {
		t.Errorf("registered %v after the second run, want %v", got, want)
	}
}

// TestInitParentSelection: the prompt registers only what is picked.
func TestInitParentSelection(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	parent := parentFolder(t)
	fakeInitPATH(t)
	t.Chdir(parent)
	code, out, errout := runWithStdin(t, ctx, state, "2\ny\n", []string{"init", "--foreman", "claude/sonnet"})
	if code != 0 {
		t.Fatalf("init = %d\n%s\n%s", code, out, errout)
	}
	if got := registeredRepos(t, ctx, state); !reflect.DeepEqual(got, []string{"example.invalid/org/beta"}) {
		t.Errorf("registered %v, want beta alone", got)
	}
	// A declined plan writes nothing.
	other := parentFolder(t)
	t.Chdir(other)
	before := snapshotTree(t, other)
	code, out, errout = runWithStdin(t, ctx, state, "all\nn\n", []string{"init", "--foreman", "claude/sonnet"})
	if code != 1 || !strings.Contains(out, "Declined") {
		t.Fatalf("declined init = %d\n%s\n%s", code, out, errout)
	}
	if after := snapshotTree(t, other); !reflect.DeepEqual(before, after) {
		t.Errorf("a declined init changed the folder")
	}
	if got := registeredRepos(t, ctx, state); !reflect.DeepEqual(got, []string{"example.invalid/org/beta"}) {
		t.Errorf("a declined init registered %v", got)
	}
}

func coordApply(t *testing.T, dir string) []wireFile {
	t.Helper()
	files := planCoordinatorWiring(dir)
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	return files
}

// TestCoordinatorWiringAdditive: the folder's .mcp.json takes the same
// additive, idempotent, damaged-file rules as the repository's.
func TestCoordinatorWiringAdditive(t *testing.T) {
	t.Run("adds beside the user's servers and keys, keeping their bytes", func(t *testing.T) {
		dir := t.TempDir()
		old := "{\n  \"mcpServers\": {\n    \"other\": {\"command\": \"x\", \"args\": [\"a\"]}\n  },\n  \"extra\": 1\n}\n"
		wireWrite(t, dir, ".mcp.json", old)
		coordApply(t, dir)
		got := wireRead(t, dir, ".mcp.json")
		if !keepsBytes(old, got) || !strings.Contains(got, `"brigada-coordinator"`) || !strings.Contains(got, `"--coordinator"`) {
			t.Errorf("the user's bytes were not kept, or the entry is missing:\n%s", got)
		}
		v := wireJSON(t, got).(map[string]any)
		servers := v["mcpServers"].(map[string]any)
		if _, ok := servers["other"]; !ok || v["extra"] != float64(1) {
			t.Errorf("the user's entries are gone: %v", v)
		}
		again := coordApply(t, dir)
		for _, f := range again {
			if f.changed() {
				t.Errorf("a second run changes %s (%s)", f.rel, f.state)
			}
		}
		if wireRead(t, dir, ".mcp.json") != got {
			t.Errorf("a second run rewrote .mcp.json")
		}
	})

	t.Run("adds the key to a file without mcpServers", func(t *testing.T) {
		dir := t.TempDir()
		wireWrite(t, dir, ".mcp.json", "{\n  \"other\": true\n}\n")
		coordApply(t, dir)
		v := wireJSON(t, wireRead(t, dir, ".mcp.json")).(map[string]any)
		if v["other"] != true || v["mcpServers"] == nil {
			t.Errorf(".mcp.json = %v", v)
		}
	})

	t.Run("replaces a stale entry of its own and no other", func(t *testing.T) {
		dir := t.TempDir()
		wireWrite(t, dir, ".mcp.json", "{\"mcpServers\":{\"brigada\":{\"command\":\"mine\"},\"brigada-coordinator\":{\"command\":\"old\"}}}")
		coordApply(t, dir)
		servers := wireJSON(t, wireRead(t, dir, ".mcp.json")).(map[string]any)["mcpServers"].(map[string]any)
		if !reflect.DeepEqual(servers["brigada"], map[string]any{"command": "mine"}) {
			t.Errorf("another server was touched: %v", servers["brigada"])
		}
		if !reflect.DeepEqual(servers["brigada-coordinator"], map[string]any{"command": "brigada", "args": []any{"mcp", "--coordinator"}}) {
			t.Errorf("the coordinator entry = %v", servers["brigada-coordinator"])
		}
	})

	t.Run("a damaged file is left alone", func(t *testing.T) {
		dir := t.TempDir()
		for _, body := range []string{"{not json", "[]", "{\"mcpServers\": []}"} {
			wireWrite(t, dir, ".mcp.json", body)
			files := coordApply(t, dir)
			if files[0].err == nil {
				t.Errorf("%q: no error for a damaged file", body)
			}
			if got := wireRead(t, dir, ".mcp.json"); got != body {
				t.Errorf("%q: the damaged file was changed to %q", body, got)
			}
		}
		// The skill is still written beside it: files are judged one by one.
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(coordinatorSkillFile))); err != nil {
			t.Errorf("the skill was not written beside a damaged .mcp.json: %v", err)
		}
	})

	t.Run("a skill that is not brigada's is left alone", func(t *testing.T) {
		dir := t.TempDir()
		wireWrite(t, dir, coordinatorSkillFile, "my own coordinator rules\n")
		files := coordApply(t, dir)
		if files[1].err == nil || wireRead(t, dir, coordinatorSkillFile) != "my own coordinator rules\n" {
			t.Errorf("the user's skill was touched: %v", files[1].err)
		}
	})

	t.Run("a symlinked path is refused", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".claude")); err != nil {
			t.Fatal(err)
		}
		files := planCoordinatorWiring(dir)
		if files[1].err == nil {
			t.Errorf("a skill path through a symlink was planned")
		}
	})
}
