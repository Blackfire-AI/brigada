package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
	"github.com/Blackfire-AI/brigada/internal/prompt"
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

// oldRouterBody rebuilds the router block an older init wrote between its
// markers, so removal is tested against the real thing.
func oldRouterBody() string {
	return fmt.Sprintf(`If your prompt opens with %q or
%q, you are a worker or reviewer run by brigada:
follow that prompt; this block and the foreman rules do not apply to you.

Otherwise: in this repository, code changes go through brigada, and you are
its foreman. Read `+"`"+wiringRulesFile+"`"+` first and follow it, unless your
prompt already gives you brigada's foreman rules. Plan work with brigada's
tools; do not edit code yourself unless the user explicitly asks you to.
If brigada's tools are not available in this session, say so and tell the
user how to enable them (restart the session in this repository and approve
the `+"`brigada`"+` MCP server) instead of doing the work by hand.`, prompt.WorkerOpener, prompt.ReviewerOpener)
}

// oldRulesContent rebuilds the rules file an older init wrote.
func oldRulesContent() []byte {
	return []byte(fmt.Sprintf(`%s v%d -->

These are brigada's foreman rules for your own session (mode A). Brigada owns
this file and replaces it on "brigada init"; put your own instructions elsewhere.

%s`, wiringSkillMarker, foreman.InstructionsVersion, foreman.Instructions()))
}

// wireSection appends a marked section to a Markdown file the way an older
// init did: what the record in its begin marker names is what this adds. A
// missing file is created holding only the section; an existing empty one
// gets no separators, as init added none.
func wireSection(d []byte, exists bool, kind, body string) []byte {
	render := func(record string) []byte {
		return []byte("<!-- brigada-managed: " + kind + " v1" + record + " -->\n" + body + "\n<!-- /brigada-managed: " + kind + " -->\n")
	}
	if !exists {
		return render("; init added the file")
	}
	record := ""
	next := append([]byte(nil), d...)
	if len(next) > 0 {
		record = fmt.Sprintf("; init added a blank line after byte %d", len(d))
		if next[len(next)-1] != '\n' {
			record = fmt.Sprintf("; init added a newline, a blank line after byte %d", len(d))
			next = append(next, '\n')
		}
		next = append(next, '\n')
	}
	return append(next, render(record)...)
}

// wireApply writes the mode A wiring an older init left behind — the files,
// their markers and the records exactly as init wrote them — including the
// git exclude line for the private settings when git does not ignore it.
func wireApply(t *testing.T, root string) {
	t.Helper()
	// .mcp.json: the "brigada" server, merged into any user servers with
	// init's own byte-preserving edit.
	mcpData, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(wiringMCPFile)))
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	mcpNext, _, err := mergeMCPEntry(mcpData, exists, "brigada", []string{"mcp", "--foreman"})
	if err != nil {
		t.Fatal(err)
	}
	wireWrite(t, root, wiringMCPFile, string(mcpNext))
	// The private settings: brigada's Stop hook, appended as a group of the
	// Stop list or into a fresh file, other bytes untouched.
	group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookYieldCommand}}}
	settingsPath := filepath.Join(root, filepath.FromSlash(wiringSettingsFile))
	settings, err := os.ReadFile(settingsPath)
	exists = err == nil
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var settingsNext []byte
	if !exists {
		settingsNext, err = json.MarshalIndent(map[string]any{"hooks": map[string]any{"Stop": []any{group}}}, "", "  ")
		settingsNext = append(settingsNext, '\n')
	} else {
		doc, derr := jsonRoot(settings)
		if derr != nil {
			t.Fatal(derr)
		}
		hi := doc.lookup("hooks")
		if hi < 0 {
			settingsNext, err = jsonInsert(settings, doc, "hooks", map[string]any{"Stop": []any{group}})
		} else {
			hooks, derr := jsonSub(settings, doc, hi, '{', "hooks")
			if derr != nil {
				t.Fatal(derr)
			}
			ei := hooks.lookup("Stop")
			if ei < 0 {
				settingsNext, err = jsonInsert(settings, hooks, "Stop", []any{group})
			} else {
				list, derr := jsonSub(settings, hooks, ei, '[', "hooks.Stop")
				if derr != nil {
					t.Fatal(derr)
				}
				settingsNext, err = jsonInsert(settings, list, "", group)
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	wireWrite(t, root, wiringSettingsFile, string(settingsNext))
	wireWrite(t, root, wiringRulesFile, string(oldRulesContent()))
	wireWrite(t, root, wiringCodexFile, "[mcp_servers.brigada]\ncommand = \"brigada\"\nargs = [\"mcp\", \"--foreman\"]\n")
	router, imprt := wiringAgentsFile, wiringClaudeFile
	switch {
	case linksTo(root, wiringAgentsFile, wiringClaudeFile) && !linksTo(root, wiringClaudeFile, wiringAgentsFile):
		router, imprt = wiringClaudeFile, ""
	case linksTo(root, wiringClaudeFile, wiringAgentsFile) && !linksTo(root, wiringAgentsFile, wiringClaudeFile):
		router, imprt = wiringAgentsFile, ""
	}
	sections := map[string][2]string{router: {"router", oldRouterBody()}}
	if imprt != "" {
		sections[imprt] = [2]string{"import", "@AGENTS.md"}
	}
	for rel, s := range sections {
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		exists := err == nil
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, wireSection(data, exists, s[0], s[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The exclude line for the private settings, added only when git does
	// not ignore the file, to a linked worktree's shared exclude file too.
	if exec.Command("git", "-C", root, "check-ignore", "-q", "--", wiringSettingsFile).Run() == nil {
		return
	}
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	data, _ := os.ReadFile(path)
	next := string(data)
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	if err := os.WriteFile(path, []byte(next+"/"+wiringSettingsFile+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
		{"foreign rules", wiringRulesFile, "my own rules\n"},
		{"router begin without end", wiringAgentsFile, "# mine\n<!-- brigada-managed: router v1 -->\ntext\n"},
		{"router end without begin", wiringAgentsFile, "<!-- /brigada-managed: router -->\n"},
		{"router end before begin", wiringAgentsFile, "<!-- /brigada-managed: router -->\n<!-- brigada-managed: router v1 -->\n"},
		{"two router blocks", wiringAgentsFile, strings.Repeat("<!-- brigada-managed: router v1 -->\n<!-- /brigada-managed: router -->\n", 2)},
		{"two import blocks", wiringClaudeFile, strings.Repeat("<!-- brigada-managed: import v1 -->\n@AGENTS.md\n<!-- /brigada-managed: import -->\n", 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, tc.rel, tc.body)
			files := planLegacyWiring(root)
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

// A rerun of init strips everything an older init wired, exactly what
// brigada remove removes, and leaves the user's own bytes: the shared
// settings migration included, and the exclude line from .git/info/exclude.
func TestInitRerunStripsOlderWiring(t *testing.T) {
	root := wireRepo(t)
	shared := `{
  "model": "opus",
  "hooks": {
    "Stop": [{"hooks":[{"type":"command","command":"mine"},{"type":"command","command":"brigada hold yield"}]}],
    "SessionStart": [{"hooks":[{"type":"command","command":"brigada hold confirm"}]}]
  }
}
`
	wireWrite(t, root, wiringSharedSettingsFile, shared)
	wireWrite(t, root, ".gitignore", "# user's rules\n")
	wireWrite(t, root, ".git/info/exclude", "# user's exclude rules\n")
	wireApply(t, root)
	files := planLegacyWiring(root)
	var plan bytes.Buffer
	printInitFiles(&plan, files, false)
	flat := strings.Join(strings.Fields(plan.String()), " ")
	for _, want := range []string{
		"removed .mcp.json removes the brigada server an older init added",
		"removed .codex/config.toml removes the brigada table",
		"removed .claude/settings.local.json removes the Stop hook",
		"changes .claude/settings.json removes the legacy brigada hooks",
		"removed .brigada/foreman.md deletes the foreman rules",
		"removed AGENTS.md removes the marked router block",
		"removed CLAUDE.md removes the marked import",
		"changes .git/info/exclude removes the exclude line",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan.String())
		}
	}
	if err := applyWiring(files); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{wiringMCPFile, wiringCodexFile, wiringSettingsFile, wiringRulesFile, wiringAgentsFile, wiringClaudeFile, ".codex"} {
		if fileExists(filepath.Join(root, rel)) {
			t.Errorf("%s remains", rel)
		}
	}
	wantShared := strings.Replace(shared, `{"type":"command","command":"mine"},{"type":"command","command":"brigada hold yield"}`, `{"type":"command","command":"mine"}`, 1)
	wantShared = strings.Replace(wantShared, `,
    "SessionStart": [{"hooks":[{"type":"command","command":"brigada hold confirm"}]}]`, "", 1)
	if got := wireRead(t, root, wiringSharedSettingsFile); got != wantShared {
		t.Fatalf("shared settings:\n%s\nwant\n%s", got, wantShared)
	}
	if got := wireRead(t, root, ".git/info/exclude"); got != "# user's exclude rules\n" {
		t.Fatalf("exclude = %q", got)
	}
	if got := wireRead(t, root, ".gitignore"); got != "# user's rules\n" {
		t.Fatalf(".gitignore changed: %q", got)
	}
	// Nothing is left to strip: a second rerun changes nothing.
	for _, f := range planLegacyWiring(root) {
		if f.err != nil || f.changed() {
			t.Errorf("second rerun plans %s: state %s err %v", f.rel, f.state, f.err)
		}
	}
}

// A repository init with --no-wiring touches no harness file at all, old
// wiring included.
func TestInitRerunNoWiringKeepsOldWiring(t *testing.T) {
	// The remote is made before the engine: the plan-file mirror pushes
	// into it, and the engine's cleanup waits that push out, so the temp
	// dir must be removed after the engine is stopped.
	remote := t.TempDir()
	wireGit(t, remote, "init", "--bare", "-q")
	_, state := startSettingsEngine(t)
	importRepo(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wireGit(t, root, "remote", "set-url", "origin", remote)
	wireGit(t, root, "config", "core.excludesFile", os.DevNull)
	wireApply(t, root)
	before := removalSnapshot(t, root)
	startStopHelper(t, "runner", state)
	fakeInitPATH(t, "claude")
	code, out, errs := initDo(t, state, "", "--yes", "--parallel", "1", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 0 {
		t.Fatalf("init code=%d\n%s\n%s", code, out, errs)
	}
	after := removalSnapshot(t, root)
	want := map[string]string{}
	for k, v := range before {
		want[k] = v
	}
	want[engine.ProjectConfigPath] = after[engine.ProjectConfigPath]
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("init --no-wiring changed harness files:\nbefore %q\nafter %q", before, after)
	}
	if !strings.Contains(out, "no longer wires or unwires") {
		t.Errorf("missing --no-wiring note:\n%s", out)
	}
}

func TestInitProbeListsRemovalsWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extra       []string
		wantRemoval bool
	}{
		{"lists removals", nil, true},
		{"no wiring", []string{"--no-wiring"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			wireGit(t, ".", "config", "core.excludesFile", os.DevNull)
			if err := os.WriteFile("go.mod", []byte("module example.invalid/test\n"), 0644); err != nil {
				t.Fatal(err)
			}
			wireApply(t, ".")
			mcp := wireRead(t, ".", wiringMCPFile)
			calls := fakeInitPATH(t, "claude", "codex")
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			args := append([]string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}, tc.extra...)
			code, out, errOut := initDo(t, state, "", args...)
			if code != 0 {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			flat := strings.Join(strings.Fields(out), " ")
			for _, want := range []string{
				"removed .mcp.json removes the brigada server an older init added",
				"removed .brigada/foreman.md deletes the foreman rules",
				"changes .git/info/exclude removes the exclude line",
			} {
				if strings.Contains(flat, want) != tc.wantRemoval {
					t.Errorf("%q present = %v, want %v\n%s", want, !tc.wantRemoval, tc.wantRemoval, out)
				}
			}
			if !strings.Contains(out, "Files in this repository (brigada never commits them):") || !strings.Contains(flat, "new .brigada/project.yaml the settings above") {
				t.Errorf("missing repository files block:\n%s", out)
			}
			if strings.Contains(out, "auto_review") || strings.Contains(out, "already") {
				t.Errorf("unexpected setup/approval claim:\n%s", out)
			}
			if got := wireRead(t, ".", wiringMCPFile); got != mcp {
				t.Errorf("probe changed .mcp.json: %s", got)
			}
			data, _ := os.ReadFile(calls)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, " --help") {
					t.Errorf("harness invoked beyond --help: %q", line)
				}
			}
		})
	}
}

func TestInitProbeShowsDiffOfExistingWiring(t *testing.T) {
	importRepo(t)
	wireGit(t, ".", "config", "core.excludesFile", os.DevNull)
	wireWrite(t, ".", wiringMCPFile, "{\n  \"mcpServers\": {\n    \"other\": {\"command\": \"x\"}\n  }\n}\n")
	wireApply(t, ".")
	fakeInitPATH(t, "claude")
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, errOut := initDo(t, state, "", "--probe", "--check", "true", "--tools", "none", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude")
	if code != 0 {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "--- a/.mcp.json") || !strings.Contains(out, "-    \"brigada\"") {
		t.Errorf("no removal diff for .mcp.json:\n%s", out)
	}
	if got := wireRead(t, ".", wiringMCPFile); !strings.Contains(got, "brigada") || !strings.Contains(got, "other") {
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
			if !verbose && strings.Contains(out, "default_check:") {
				t.Errorf("new file contents shown without --verbose:\n%s", out)
			}
			project, err := engine.ProjectConfigDefaults(nil, "example.invalid/imported", "true", "none")
			if err != nil {
				t.Fatal(err)
			}
			files := append([]wireFile{{rel: engine.ProjectConfigPath, next: project}}, planLegacyWiring(".")...)
			previous := -1
			for _, f := range files {
				if f.exists || f.err != nil || f.state == "unchanged" {
					continue
				}
				full := "      " + strings.Join(splitDiffLines(f.next), "\n      ") + "\n"
				if strings.Contains(out, full) != verbose {
					t.Errorf("%s full contents present = %v, want %v\n%s", f.rel, !verbose, verbose, out)
				}
				// The row: the state column, two spaces, the path, then padding.
				position := strings.Index(out, "  "+f.rel+"  ")
				if position <= previous {
					t.Errorf("%s missing or out of order\n%s", f.rel, out)
				}
				previous = position
			}
		})
	}
}

func TestWiringMigrationAndExcludeRefuseEditsSincePlan(t *testing.T) {
	for _, rel := range []string{wiringSharedSettingsFile, ".git/info/exclude"} {
		t.Run(rel, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, wiringSharedSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}]}]}}`)
			wireApply(t, root)
			files := planLegacyWiring(root)
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
