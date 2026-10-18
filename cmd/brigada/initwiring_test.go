package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func wireGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func wireRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	wireGit(t, root, "init", "-q")
	wireGit(t, root, "config", "core.excludesFile", os.DevNull)
	return root
}

func wireWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wireRead(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func wireApply(t *testing.T, root string) []wireFile {
	t.Helper()
	files := planWiring(root)
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	return files
}

func wireJSON(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}
	return v
}

// keepsBytes reports whether every byte of old appears in order in next: an
// edit that only inserts leaves the user's bytes untouched.
func keepsBytes(old, next string) bool {
	i := 0
	for j := 0; j < len(next) && i < len(old); j++ {
		if next[j] == old[i] {
			i++
		}
	}
	return i == len(old)
}

func TestWiringFreshFilesHoldExactlyBrigadasEntries(t *testing.T) {
	root := wireRepo(t)
	wireApply(t, root)
	wantMCP := map[string]any{"mcpServers": map[string]any{"brigada": map[string]any{"command": "brigada", "args": []any{"mcp", "--foreman"}}}}
	if got := wireJSON(t, wireRead(t, root, ".mcp.json")); !reflect.DeepEqual(got, wantMCP) {
		t.Errorf(".mcp.json = %v", got)
	}
	hook := func(cmd string) []any {
		return []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}}}
	}
	wantSettings := map[string]any{"hooks": map[string]any{"Stop": hook("brigada hold yield")}}
	if got := wireJSON(t, wireRead(t, root, ".claude/settings.local.json")); !reflect.DeepEqual(got, wantSettings) {
		t.Errorf("settings.local.json = %v", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude/settings.json")); !os.IsNotExist(err) {
		t.Errorf("fresh wiring created shared settings: %v", err)
	}
	skill := wireRead(t, root, ".claude/skills/brigada-foreman/SKILL.md")
	if !strings.HasPrefix(skill, "---\nname: brigada-foreman\n") || !strings.Contains(skill, wiringSkillMarker) || !strings.Contains(skill, "hold_acquire") {
		t.Errorf("skill = %.200s", skill)
	}
	for _, rel := range []string{".mcp.json", ".claude/settings.local.json"} {
		for _, secret := range []string{"token", "secret", root} {
			if strings.Contains(wireRead(t, root, rel), secret) {
				t.Errorf("%s mentions %q", rel, secret)
			}
		}
	}
}

func TestWiringKeepsOthersAndIsIdempotent(t *testing.T) {
	root := wireRepo(t)
	mcp := "{\n\t\"mcpServers\": {\n\t\t\"other\": { \"command\": \"x\", \"args\": [] }\n\t},\n\t\"extra\": [1,  2]\n}\n"
	settings := "{\n    \"model\": \"opus\",\n    \"hooks\": {\n        \"Stop\": [\n            {\"hooks\": [{\"type\": \"command\", \"command\": \"mine\"}]}\n        ],\n        \"PreToolUse\": []\n    }\n}\n"
	claudeMD := "# mine\n"
	wireWrite(t, root, ".mcp.json", mcp)
	wireWrite(t, root, ".claude/settings.local.json", settings)
	wireWrite(t, root, ".claude/settings.json", settings)
	wireWrite(t, root, "CLAUDE.md", claudeMD)
	wireApply(t, root)

	gotMCP, gotSettings := wireRead(t, root, ".mcp.json"), wireRead(t, root, ".claude/settings.local.json")
	if !keepsBytes(mcp, gotMCP) || !keepsBytes(settings, gotSettings) {
		t.Errorf("user bytes changed:\n%s\n%s", gotMCP, gotSettings)
	}
	if !strings.Contains(gotMCP, "\t\t\"brigada\": {") {
		t.Errorf("entry does not follow the file's tab layout:\n%s", gotMCP)
	}
	if got := wireRead(t, root, ".claude/settings.json"); got != settings {
		t.Errorf("shared settings changed: %s", got)
	}
	if wireRead(t, root, "CLAUDE.md") != claudeMD {
		t.Error("CLAUDE.md touched")
	}
	m := wireJSON(t, gotMCP).(map[string]any)
	servers := m["mcpServers"].(map[string]any)
	if len(servers) != 2 || servers["other"] == nil || servers["brigada"] == nil || m["extra"] == nil {
		t.Errorf("servers = %v", m)
	}
	s := wireJSON(t, gotSettings).(map[string]any)
	hooks := s["hooks"].(map[string]any)
	if s["model"] != "opus" || len(hooks["Stop"].([]any)) != 2 || hooks["SessionStart"] != nil || hooks["PreToolUse"] == nil {
		t.Errorf("settings = %v", s)
	}
	if !strings.HasPrefix(strings.SplitN(gotSettings, "\"Stop\"", 2)[1], ": [\n            {\"hooks\": [{\"type\": \"command\", \"command\": \"mine\"}]}") {
		t.Errorf("the user's hook moved:\n%s", gotSettings)
	}

	again := planWiring(root)
	for _, f := range again {
		if f.err != nil || f.changed() || f.state != "unchanged" {
			t.Errorf("re-run on %s: state %s err %v", f.rel, f.state, f.err)
		}
	}
	if err := applyWiring(again); err != nil {
		t.Fatal(err)
	}
	if wireRead(t, root, ".mcp.json") != gotMCP || wireRead(t, root, ".claude/settings.local.json") != gotSettings {
		t.Error("re-run rewrote a file")
	}
}

func TestWiringReplacesBrigadasEntriesInPlace(t *testing.T) {
	root := wireRepo(t)
	mcp := "{\"mcpServers\":{\"a\":{\"command\":\"a\"},\"brigada\":{\"command\":\"/old/brigada\"},\"z\":{\"command\":\"z\"}}}"
	settings := "{\"hooks\":{\"Stop\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"before\"}]},{\"hooks\":[{\"type\":\"bogus\",\"command\":\"brigada hold yield\"}]},{\"hooks\":[{\"type\":\"command\",\"command\":\"after\"}]}]}}"
	wireWrite(t, root, ".mcp.json", mcp)
	wireWrite(t, root, ".claude/settings.local.json", settings)
	wireApply(t, root)
	wantMCP := strings.Replace(mcp, "{\"command\":\"/old/brigada\"}", "{\"command\":\"brigada\",\"args\":[\"mcp\",\"--foreman\"]}", 1)
	if got := wireRead(t, root, ".mcp.json"); got != wantMCP {
		t.Errorf("mcp:\n%s\nwant\n%s", got, wantMCP)
	}
	wantSettings := strings.Replace(settings, "\"type\":\"bogus\"", "\"type\":\"command\"", 1)
	if got := wireRead(t, root, ".claude/settings.local.json"); got != wantSettings {
		t.Errorf("settings:\n%s\nwant\n%s", got, wantSettings)
	}
}

func TestWiringDamagedFilesAreReportedAndLeftAlone(t *testing.T) {
	for _, tc := range []struct{ name, rel, body string }{
		{"truncated mcp", ".mcp.json", "{\"mcpServers\": "},
		{"mcp servers not an object", ".mcp.json", "{\"mcpServers\": []}"},
		{"mcp top level array", ".mcp.json", "[]"},
		{"empty settings", ".claude/settings.local.json", ""},
		{"hooks not an object", ".claude/settings.local.json", "{\"hooks\": []}"},
		{"stop not an array", ".claude/settings.local.json", "{\"hooks\": {\"Stop\": {}}}"},
		{"settings with comments", ".claude/settings.local.json", "{\n// no\n}"},
		{"damaged shared settings", ".claude/settings.json", "{\"hooks\": "},
		{"foreign skill", ".claude/skills/brigada-foreman/SKILL.md", "my own skill\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, tc.rel, tc.body)
			files := planWiring(root)
			var hit bool
			for _, f := range files {
				if f.rel == tc.rel {
					hit = true
					if f.err == nil || f.changed() {
						t.Errorf("%s not reported: state %s err %v", f.rel, f.state, f.err)
					}
				}
			}
			if !hit {
				t.Fatal("file not planned")
			}
			var plan bytes.Buffer
			printInitFiles(&plan, files, false)
			if !strings.Contains(strings.Join(strings.Fields(plan.String()), " "), "left alone "+tc.rel+" ") {
				t.Errorf("missing damaged-file reason: %s", plan.String())
			}
			if err := applyWiring(files); err != nil {
				t.Fatal(err)
			}
			if got := wireRead(t, root, tc.rel); got != tc.body {
				t.Errorf("damaged file rewritten: %q", got)
			}
		})
	}
}

func TestWiringSkillRefreshesOnlyItsOwnFile(t *testing.T) {
	root := wireRepo(t)
	rel := ".claude/skills/brigada-foreman/SKILL.md"
	wireWrite(t, root, rel, "---\nname: brigada-foreman\n---\n"+wiringSkillMarker+" v0 -->\nold text\n")
	wireApply(t, root)
	if got := wireRead(t, root, rel); strings.Contains(got, "old text") || !strings.Contains(got, "hold_acquire") {
		t.Errorf("skill not refreshed: %.200s", got)
	}
}

func TestInitProbeListsWiringWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		extra      []string
		wantWiring bool
	}{
		{"wiring", nil, true},
		{"no wiring", []string{"--no-wiring"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			if err := os.WriteFile("go.mod", []byte("module example.invalid/test\n"), 0644); err != nil {
				t.Fatal(err)
			}
			calls := fakeInitPATH(t, "claude", "codex")
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			args := append([]string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}, tc.extra...)
			code, out, errOut := initDo(t, state, "", args...)
			if code != 0 {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			for _, want := range []string{"new .mcp.json lets Claude Code here use brigada", "new .claude/settings.local.json private, kept out of git:", "new .claude/skills/brigada-foreman/SKILL.md rules for your Claude session", "codex mcp add brigada-foreman -- brigada mcp --foreman"} {
				if strings.Contains(strings.Join(strings.Fields(out), " "), want) != tc.wantWiring {
					t.Errorf("%q present = %v, want %v\n%s", want, !tc.wantWiring, tc.wantWiring, out)
				}
			}
			if !strings.Contains(out, "Files in this repository (brigada never commits them):") || !strings.Contains(strings.Join(strings.Fields(out), " "), "new .brigada/project.yaml the settings above") {
				t.Errorf("missing repository files block:\n%s", out)
			}
			if strings.Contains(out, "auto_review") || strings.Contains(out, "already") || strings.Contains(out, "ready") {
				t.Errorf("unexpected setup/approval claim:\n%s", out)
			}
			for _, rel := range []string{".mcp.json", ".claude"} {
				if _, err := os.Stat(rel); !os.IsNotExist(err) {
					t.Errorf("probe wrote %s: %v", rel, err)
				}
			}
			data, _ := os.ReadFile(calls)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, " --help") {
					t.Errorf("harness invoked beyond --help (codex command must be printed, not run): %q", line)
				}
			}
		})
	}
}

func TestHoldConfirmAlwaysExitsZero(t *testing.T) {
	for _, args := range [][]string{{"hold", "confirm"}, {"hold", "confirm", "--session", "s1"}, {"hold", "confirm", "--bogus"}} {
		var out, errOut strings.Builder
		if code := run(context.Background(), args, &out, &errOut); code != 0 {
			t.Errorf("%v exited %d: %s", args, code, errOut.String())
		}
	}
}

func TestInitProbeShowsDiffOfExistingWiring(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, "claude")
	wireWrite(t, ".", ".mcp.json", "{\n  \"mcpServers\": {\n    \"other\": {\"command\": \"x\"}\n  }\n}\n")
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, errOut := initDo(t, state, "", "--probe", "--check", "true", "--tools", "none", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude")
	if code != 0 {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "+++ b/.mcp.json") || !strings.Contains(out, "+        \"brigada\": {") && !strings.Contains(out, "+    \"brigada\": {") {
		t.Errorf("no diff for .mcp.json:\n%s", out)
	}
	if got := wireRead(t, ".", ".mcp.json"); !strings.Contains(got, "\"other\"") || strings.Contains(got, "brigada") {
		t.Errorf("probe changed .mcp.json: %s", got)
	}
}

func TestInitNewFileContentsRequireVerbose(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		t.Run(fmt.Sprint(verbose), func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			args := []string{"--probe", "--check", "true", "--tools", "none", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}
			if verbose {
				args = append(args, "--verbose")
			}
			code, out, errOut := initDo(t, filepath.Join(testsupport.ShortStateDir(t), "absent"), "", args...)
			if code != 0 {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			if !verbose {
				for _, content := range []string{"default_check:", `"mcpServers":`, `"hooks":`, "name: brigada-foreman"} {
					if strings.Contains(out, content) {
						t.Errorf("new file contents shown without --verbose (%q):\n%s", content, out)
					}
				}
			}
			project, err := engine.ProjectConfigDefaults(nil, "example.invalid/imported", "true", "none")
			if err != nil {
				t.Fatal(err)
			}
			files := append([]wireFile{{rel: engine.ProjectConfigPath, next: project}}, planWiring(".")...)
			previous := -1
			for _, f := range files {
				if f.exists || f.err != nil || f.state == "unchanged" {
					continue
				}
				full := "      " + strings.Join(splitDiffLines(f.next), "\n      ") + "\n"
				if strings.Contains(out, full) != verbose {
					t.Errorf("%s full contents present = %v, want %v\n%s", f.rel, !verbose, verbose, out)
				}
				if !verbose && strings.Contains(out, "+++ b/"+f.rel) {
					t.Errorf("new file %s printed as a diff\n%s", f.rel, out)
				}
				position := strings.Index(out, "    "+f.rel)
				if position <= previous {
					t.Errorf("%s missing or out of order\n%s", f.rel, out)
				}
				previous = position
			}
		})
	}
}

func TestInitApplyPromptFileCounts(t *testing.T) {
	for _, tc := range []struct {
		files   int
		running bool
		want    string
	}{
		{1, false, "Write 1 file and start the runner?"},
		{1, true, "Write 1 file?"},
		{5, false, "Write 5 files and start the runner?"},
		{5, true, "Write 5 files?"},
	} {
		if got := initApplyPrompt(tc.files, tc.running); got != tc.want {
			t.Errorf("initApplyPrompt(%d, %v) = %q, want %q", tc.files, tc.running, got, tc.want)
		}
	}
}

func TestInitWorkerOnlyWiringNotes(t *testing.T) {
	for _, workers := range [][]string{nil, {"kimi"}, {"opencode"}, {"kimi", "opencode"}} {
		t.Run(strings.Join(workers, ","), func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, append([]string{"claude"}, workers...)...)
			code, out, errOut := initDo(t, filepath.Join(testsupport.ShortStateDir(t), "absent"), "", "--probe", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude", "--check", "true", "--tools", "none")
			if code != 0 {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			if strings.Contains(out, "work as workers only") {
				t.Errorf("redundant installed-worker note:\n%s", out)
			}
			if !strings.Contains(out, "Files in this repository (brigada never commits them):") {
				t.Errorf("missing wiring plan:\n%s", out)
			}
		})
	}
}

func TestWiringMigratesOldInstallAndStaysUnchanged(t *testing.T) {
	root := wireRepo(t)
	shared := `{
  "model": "opus",
  "hooks": {
    "Stop": [{"matcher":"mine","hooks":[{"type":"command","command":"before"},{"type":"command","command":"brigada hold yield"},{"type":"command","command":"after"}]}],
    "SessionStart": [{"hooks":[{"type":"command","command":"brigada hold confirm"}]}],
    "PreToolUse": [{"hooks":[{"type":"command","command":"brigada hold yield --mine"},{"type":"command","command":"brigada hold confirm --mine"}]}]
  },
  "extra": [1,  2]
}
`
	wireWrite(t, root, wiringSharedSettingsFile, shared)
	files := planWiring(root)
	var plan bytes.Buffer
	printInitFiles(&plan, files, false)
	if !strings.Contains(plan.String(), "--- a/"+wiringSharedSettingsFile) || !strings.Contains(plan.String(), "+++ b/"+wiringSharedSettingsFile) {
		t.Fatalf("missing migration diff:\n%s", plan.String())
	}
	if got := wireRead(t, root, wiringSharedSettingsFile); got != shared {
		t.Fatal("planning changed shared settings")
	}
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(shared, `{"type":"command","command":"brigada hold yield"},`, "", 1)
	want = strings.Replace(want, `"SessionStart": [{"hooks":[{"type":"command","command":"brigada hold confirm"}]}],`+"\n    ", "", 1)
	if got := wireRead(t, root, wiringSharedSettingsFile); got != want {
		t.Fatalf("migration:\n%s\nwant:\n%s", got, want)
	}
	local := wireRead(t, root, wiringSettingsFile)
	if strings.Contains(local, hookConfirmCommand) || !strings.Contains(local, hookYieldCommand) {
		t.Fatalf("private settings: %s", local)
	}
	for _, f := range wireApply(t, root) {
		if f.err != nil || f.changed() || f.state != "unchanged" {
			t.Errorf("rerun %s: state %s, err %v", f.rel, f.state, f.err)
		}
	}
	if wireRead(t, root, wiringSharedSettingsFile) != want || wireRead(t, root, wiringSettingsFile) != local {
		t.Fatal("rerun rewrote settings")
	}
}

func TestWiringMigrationDropsOnlyOwnedHooks(t *testing.T) {
	for _, tc := range []struct{ name, old, want string }{
		{"only event", `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}]}]}}`, `{"hooks":{}}`},
		{"first inner hook", `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}, {"command":"mine"}]}]}}`, `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}]}}`},
		{"last inner hook", `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}, {"command":"brigada hold yield"}]}]}}`, `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}]}}`},
		{"first group", `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}]}, {"hooks":[{"command":"mine"}]}]}}`, `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}]}}`},
		{"last group", `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}, {"hooks":[{"command":"brigada hold yield"}]}]}}`, `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}]}}`},
		{"last event", `{"hooks":{"PreToolUse":[], "SessionStart":[{"hooks":[{"command":"brigada hold confirm"}]}]}}`, `{"hooks":{"PreToolUse":[]}}`},
		{"both commands in one group", `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"},{"command":"brigada hold confirm"}]}],"PreToolUse":[]}}`, `{"hooks":{"PreToolUse":[]}}`},
		{"user variants", `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield --mine"},{"command":"brigada hold confirm --mine"}]}]}}`, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield --mine"},{"command":"brigada hold confirm --mine"}]}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := removeSharedHooks([]byte(tc.old), true)
			if err != nil || string(got) != tc.want {
				t.Fatalf("migration = %s, %v; want %s", got, err, tc.want)
			}
			wireJSON(t, string(got))
		})
	}
}

func TestWiringPrivateSettingsExcludeAddedOnce(t *testing.T) {
	root := wireRepo(t)
	wireWrite(t, root, ".gitignore", "# user's rules\n")
	wireWrite(t, root, ".git/info/exclude", "# user's exclude rules")
	files := planWiring(root)
	var plan bytes.Buffer
	printInitFiles(&plan, files, false)
	if !strings.Contains(strings.Join(strings.Fields(plan.String()), " "), "changes .git/info/exclude keeps private Claude Code settings out of git") || !strings.Contains(plan.String(), "+/.claude/settings.local.json") {
		t.Fatalf("exclude missing from plan:\n%s", plan.String())
	}
	if got := wireRead(t, root, ".git/info/exclude"); got != "# user's exclude rules" {
		t.Fatal("plan wrote exclude")
	}
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	want := "# user's exclude rules\n/.claude/settings.local.json\n"
	if got := wireRead(t, root, ".git/info/exclude"); got != want {
		t.Fatalf("exclude = %q, want %q", got, want)
	}
	wireGit(t, root, "check-ignore", "-q", "--", wiringSettingsFile)
	// Re-create the private file as well as rerunning unchanged: neither should
	// append another entry once git reports it ignored.
	wireApply(t, root)
	if err := os.Remove(filepath.Join(root, wiringSettingsFile)); err != nil {
		t.Fatal(err)
	}
	wireApply(t, root)
	if got := wireRead(t, root, ".git/info/exclude"); got != want {
		t.Fatalf("exclude duplicated: %q", got)
	}
	if got := wireRead(t, root, ".gitignore"); got != "# user's rules\n" {
		t.Fatalf(".gitignore changed: %q", got)
	}
}

func TestWiringPrivateSettingsIgnoredByUser(t *testing.T) {
	root := wireRepo(t)
	wireWrite(t, root, ".gitignore", ".claude/settings.local.json\n")
	exclude := wireRead(t, root, ".git/info/exclude")
	files := wireApply(t, root)
	for _, f := range files {
		if strings.Contains(f.rel, "info/exclude") {
			t.Errorf("ignored file planned exclude edit: %+v", f)
		}
	}
	if got := wireRead(t, root, ".git/info/exclude"); got != exclude {
		t.Fatalf("exclude changed for ignored file: %q", got)
	}
}

func TestWiringPrivateSettingsExcludeInLinkedWorktree(t *testing.T) {
	root := wireRepo(t)
	wireGit(t, root, "-c", "user.name=brigada test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	wireGit(t, root, "worktree", "add", "--detach", "-q", linked)
	exclude := wireGit(t, linked, "rev-parse", "--git-path", "info/exclude")
	old, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	files := wireApply(t, linked)
	var found bool
	for _, f := range files {
		if f.path == exclude {
			found = true
		}
	}
	if !found {
		t.Fatalf("worktree exclude %q not planned", exclude)
	}
	got, err := os.ReadFile(exclude)
	if err != nil || string(got) != string(old)+"/.claude/settings.local.json\n" {
		t.Fatalf("worktree exclude = %q, %v", got, err)
	}
	wireGit(t, linked, "check-ignore", "-q", "--", wiringSettingsFile)
}

func TestWiringMigrationAndExcludeRefuseEditsSincePlan(t *testing.T) {
	for _, rel := range []string{wiringSharedSettingsFile, ".git/info/exclude"} {
		t.Run(rel, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, wiringSharedSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}]}]}}`)
			files := planWiring(root)
			wireWrite(t, root, rel, "edited after plan\n")
			if err := applyWiring(files); err == nil || !strings.Contains(err.Error(), "changed after the plan") {
				t.Fatalf("concurrent edit not refused: %v", err)
			}
			if got := wireRead(t, root, rel); got != "edited after plan\n" {
				t.Fatalf("concurrent edit overwritten: %q", got)
			}
		})
	}
}
