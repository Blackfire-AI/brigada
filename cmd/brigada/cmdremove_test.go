package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

func removalSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// Git's internal entries are unrelated, except init's exclude edit.
		if rel == "." || (strings.HasPrefix(rel, ".git/") && rel != ".git/info/exclude") {
			return nil
		}
		if entry.IsDir() {
			files[rel+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil {
			files[rel] = string(data)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRemovalReversesInitWiringBytes(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "user entries"}[own], func(t *testing.T) {
			root := wireRepo(t)
			if own {
				wireWrite(t, root, wiringMCPFile, "{\n\t\"mcpServers\" : {\n\t  \"mine\": {\"command\":\"my-mcp\"}\n\t},\n\t\"other\": [1,  2]\n}\n")
				wireWrite(t, root, wiringSettingsFile, "{\n  \"model\": \"opus\",\n  \"hooks\": {\n    \"Stop\": [{\"matcher\":\"mine\",\"hooks\":[{\"command\":\"mine\"}]}]\n  }\n}\n")
				wireWrite(t, root, wiringSharedSettingsFile, "{ \"permissions\": {\"allow\": []} }\n")
			}
			before := removalSnapshot(t, root)
			wireApply(t, root)
			if err := applyRemoval(planRemoval(root, false)); err != nil {
				t.Fatal(err)
			}
			after := removalSnapshot(t, root)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("round trip:\nbefore %q\nafter %q", before, after)
			}
			for _, rel := range []string{wiringRulesFile, wiringAgentsFile, wiringClaudeFile} {
				if fileExists(filepath.Join(root, rel)) {
					t.Errorf("%s remains", rel)
				}
			}
		})
	}
}

func TestRemovalMinimalJSON(t *testing.T) {
	for _, tc := range []struct{ name, rel, old, want string }{
		{"mcp siblings", wiringMCPFile, `{"mine": [1,  2], "mcpServers":{"brigada":{}, "other": {"command":"mine"}}}`, `{"mine": [1,  2], "mcpServers":{"other": {"command":"mine"}}}`},
		{"mcp parent", wiringMCPFile, `{"mine": [1,  2], "mcpServers":{"brigada":{}}}`, `{"mine": [1,  2]}`},
		{"mcp only", wiringMCPFile, `{"mcpServers":{"brigada":{}}}`, ""},
		{"mcp duplicates", wiringMCPFile, `{"mcpServers":{"brigada":{},"brigada":{},"mine":{}}}`, `{"mcpServers":{"mine":{}}}`},
		{"hook siblings", wiringSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}, {"command":"mine"}]}]},"a": 2}`, `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}]},"a": 2}`},
		{"hook group", wiringSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}, {"hooks":[{"command":"brigada hold yield"}]}]}}`, `{"hooks":{"Stop":[{"hooks":[{"command":"mine"}]}]}}`},
		{"hook event", wiringSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}]}], "PreToolUse":[]}}`, `{"hooks":{"PreToolUse":[]}}`},
		{"hook parent", wiringSettingsFile, `{"mine": 1, "hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}]}]}}`, `{"mine": 1}`},
		{"hook only", wiringSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}]}]}}`, ""},
		{"legacy only", wiringSharedSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield"}]}],"SessionStart":[{"hooks":[{"command":"brigada hold confirm"}]}]}}`, ""},
		{"user flags", wiringSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield --mine"}]}]}}`, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold yield --mine"}]}]}}`},
		{"legacy flags", wiringSharedSettingsFile, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold confirm --mine"}]}]}}`, `{"hooks":{"Stop":[{"hooks":[{"command":"brigada hold confirm --mine"}]}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, tc.rel, tc.old)
			if err := applyRemoval(planRemoval(root, false)); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if fileExists(filepath.Join(root, tc.rel)) {
					t.Fatal("empty file remains")
				}
			} else if got := wireRead(t, root, tc.rel); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRemovalLeavesDamagedFilesAndUnmarkedSkill(t *testing.T) {
	for _, tc := range []struct{ rel, body string }{
		{wiringMCPFile, `{"mcpServers":`},
		{wiringMCPFile, `{"mcpServers":[]}`},
		{wiringCodexFile, "[mcp_servers.brigada]\ncommand = \"brigada\"\n\n[mcp_servers.brigada]\ncommand = \"brigada\"\n"},
		{wiringCodexFile, "mcp_servers = { brigada = {} }\n"},
		{wiringSettingsFile, `{"hooks":{"Stop":{}}}`},
		{wiringSettingsFile, ""},
		{wiringSharedSettingsFile, `{"hooks":[]}`},
		{wiringSkillFile, "my instructions\n"},
		{wiringRulesFile, "my rules\n"},
		{wiringAgentsFile, "<!-- brigada-managed: router v1 -->\nno end marker\n"},
		{wiringClaudeFile, "<!-- /brigada-managed: import -->\n"},
	} {
		t.Run(tc.rel+tc.body, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, tc.rel, tc.body)
			files := planRemoval(root, false)
			var out bytes.Buffer
			printRemovalPlan(&out, "example.invalid/repo", files, 0, false, 0, false)
			if !strings.Contains(out.String(), "left alone:") {
				t.Fatal(out.String())
			}
			if err := applyRemoval(files); err != nil {
				t.Fatal(err)
			}
			if got := wireRead(t, root, tc.rel); got != tc.body {
				t.Fatalf("rewrote %q", got)
			}
		})
	}
}

func TestRemovalGuardChecksAllFilesBeforeWriting(t *testing.T) {
	for _, rel := range []string{wiringMCPFile, wiringSettingsFile, wiringRulesFile, wiringAgentsFile, wiringClaudeFile, ".git/info/exclude", engine.ProjectConfigPath} {
		t.Run(rel, func(t *testing.T) {
			root := wireRepo(t)
			wireApply(t, root)
			wireWrite(t, root, engine.ProjectConfigPath, "my config\n")
			files := planRemoval(root, true)
			wireWrite(t, root, rel, "edited after plan\n")
			before := removalSnapshot(t, root)
			if err := applyRemoval(files); err == nil || !strings.Contains(err.Error(), "changed after the plan") {
				t.Fatalf("guard: %v", err)
			}
			if after := removalSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed a file")
			}
		})
	}
}

func removeTestClient(t *testing.T, ctx context.Context, state string) *api.Client {
	t.Helper()
	_, token, err := lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return api.NewClient(lifecycle.PathsOf(state).Socket, token)
}

// The socket fixture remains the default. The in-process suite below also
// runs these same assertions against the real API handler without a listener.
var startRemovalTestEngine = startSettingsEngine
var newRemovalTestClient = removeTestClient

func removeTestRepo(t *testing.T) (context.Context, string, *api.Client, string) {
	t.Helper()
	ctx, state := startRemovalTestEngine(t)
	importRepo(t)
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	wireGit(t, remote, "init", "--bare", "-q")
	wireGit(t, root, "remote", "set-url", "origin", remote)
	wireGit(t, root, "config", "core.excludesFile", os.DevNull)
	client := newRemovalTestClient(t, ctx, state)
	if _, err := client.SetProjectForeman(ctx, remote, "claude", "sonnet", ""); err != nil {
		t.Fatal(err)
	}
	wireApply(t, root)
	wireWrite(t, root, engine.ProjectConfigPath, "my config\n")
	return ctx, state, client, remote
}

func removeDo(t *testing.T, ctx context.Context, state, input string, args ...string) (int, string, string) {
	t.Helper()
	old := removeStdin
	removeStdin = strings.NewReader(input)
	defer func() { removeStdin = old }()
	var out, errs bytes.Buffer
	code := run(ctx, append([]string{"remove", "--state-dir", state}, args...), &out, &errs)
	return code, out.String(), errs.String()
}

func TestRemoveCommandAndDeleteConfig(t *testing.T) {
	for _, deleteConfig := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep", true: "delete"}[deleteConfig], func(t *testing.T) {
			ctx, state, client, remote := removeTestRepo(t)
			foreman, err := client.ProjectForeman(ctx, remote)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"--yes"}
			if deleteConfig {
				args = append(args, "--delete-config")
			}
			code, out, errs := removeDo(t, ctx, state, "", args...)
			if code != 0 || errs != "" {
				t.Fatalf("code=%d\n%s\n%s", code, out, errs)
			}
			for _, want := range []string{"brigada remove:", "In this repository", "In brigada", "history --all", "On the remote"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
			if fileExists(engine.ProjectConfigPath) == deleteConfig {
				t.Error("wrong config retention")
			}
			for _, rel := range []string{wiringMCPFile, wiringSettingsFile, wiringRulesFile, wiringAgentsFile, wiringClaudeFile} {
				if fileExists(rel) {
					t.Errorf("wiring remains: %s", rel)
				}
			}
			projects, err := client.Projects(ctx)
			if err != nil || len(projects) != 0 {
				t.Fatalf("projects=%v err=%v", projects, err)
			}
			st, err := client.ProjectReader(foreman.ProjectID).Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Plans) != 0 || len(st.Running) != 0 {
				t.Fatalf("archived project still has visible work: %+v", st)
			}
			for state, count := range st.Jobs {
				if count != 0 {
					t.Errorf("archived project's %s count = %d, want zero", state, count)
				}
			}
			for _, line := range strings.Split(out, "\n") {
				if utf8.RuneCountInString(line) >= 80 {
					t.Errorf("long line: %q", line)
				}
			}
			// Removal can be repeated after archival.
			if code, _, errs := removeDo(t, ctx, state, "", "--yes"); code != 0 {
				t.Fatal(errs)
			}
		})
	}
}

func TestRemoveLiveWorkRefusesThenCancels(t *testing.T) {
	ctx, state, client, remote := removeTestRepo(t)
	if _, err := client.SubmitTask(ctx, api.TaskSubmit{RepoURL: remote, BaseRef: "main", Spec: "do work"}); err != nil {
		t.Fatal(err)
	}
	before := removalSnapshot(t, ".")
	code, _, errs := removeDo(t, ctx, state, "", "--yes")
	if code != 1 || !strings.Contains(errs, "wait") || !strings.Contains(errs, "--cancel") {
		t.Fatalf("code=%d err=%s", code, errs)
	}
	if after := removalSnapshot(t, "."); !reflect.DeepEqual(before, after) {
		t.Fatal("busy refusal changed files")
	}
	projects, err := client.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects=%v err=%v", projects, err)
	}
	if code, _, errs := removeDo(t, ctx, state, "", "--yes", "--cancel"); code != 0 {
		t.Fatal(errs)
	}
}

func TestRemoveConfirmationAndRunner(t *testing.T) {
	for _, answer := range []string{"decline", "stop", "keep", "yes", "other project"} {
		t.Run(answer, func(t *testing.T) {
			ctx, state, client, _ := removeTestRepo(t)
			if answer == "other project" {
				// Creating a project needs a fetchable transport URL, not
				// a canonical host/path identity. Use a real local remote
				// so plan mirroring also stays independent of the network.
				otherRemote := t.TempDir()
				wireGit(t, otherRemote, "init", "--bare", "-q")
				if _, err := client.SubmitTask(ctx, api.TaskSubmit{RepoURL: otherRemote, BaseRef: "main", Spec: "other project's live work"}); err != nil {
					t.Fatal(err)
				}
			}
			startStopHelper(t, "runner", state)
			input := "y\n\n"
			if answer == "decline" {
				input = "n\n"
			}
			if answer == "keep" {
				input = "y\nn\n"
			}
			var args []string
			if answer == "yes" {
				args = []string{"--yes"}
			}
			before := removalSnapshot(t, ".")
			code, out, errs := removeDo(t, ctx, state, input, args...)
			if code != 0 {
				t.Fatalf("code=%d\n%s\n%s", code, out, errs)
			}
			_, running, err := runnerstate.Running(filepath.Join(state, "runner"))
			wantRunning := answer == "decline" || answer == "keep" || answer == "other project"
			if err != nil || running != wantRunning {
				t.Fatalf("running=%v want=%v err=%v", running, wantRunning, err)
			}
			if answer == "decline" && !reflect.DeepEqual(before, removalSnapshot(t, ".")) {
				t.Fatal("decline changed files")
			}
			if answer == "stop" && !strings.Contains(out, "stop the runner? [Y/n]") {
				t.Fatal(out)
			}
			if answer == "other project" && !strings.Contains(out, "Other projects use the runner") {
				t.Fatal(out)
			}
			if answer != "yes" {
				plan := strings.Index(out, "On the remote")
				prompt := strings.Index(out, "Remove brigada from this repository?")
				if plan < 0 || prompt < plan {
					t.Fatal("confirmation preceded complete plan")
				}
			}
		})
	}
}

func TestRemoveOutsideRepository(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, errs bytes.Buffer
	if code := run(context.Background(), []string{"remove", "--yes"}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "run brigada remove inside the repository") {
		t.Fatalf("code=%d err=%s", code, &errs)
	}
}

func TestRemoveRemoteOnlyPrintsDeletes(t *testing.T) {
	ctx, state, _, remote := removeTestRepo(t)
	wireGit(t, ".", "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "test")
	wireGit(t, ".", "push", "-q", "origin", "HEAD:refs/heads/brigada/task-1", "HEAD:refs/heads/brigada/task-2", "HEAD:refs/heads/brigada/plans", "HEAD:refs/heads/brigada/other")
	before := wireGit(t, remote, "show-ref")
	fakeInitPATH(t, "codex")
	code, out, errs := removeDo(t, ctx, state, "", "--yes")
	if code != 0 {
		t.Fatal(errs)
	}
	for _, want := range []string{"2 brigada/task-* branches, brigada/plans", "git push origin --delete", "codex mcp remove brigada-foreman"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if after := wireGit(t, remote, "show-ref"); before != after {
		t.Fatal("remote changed")
	}
	// A failing query is reported without failing removal itself.
	wireGit(t, ".", "remote", "set-url", "origin", filepath.Join(t.TempDir(), "absent"))
	var failed bytes.Buffer
	printRemovalRemote(ctx, &failed, ".")
	if !strings.Contains(failed.String(), "could not be checked") {
		t.Fatal(failed.String())
	}
}

// A reader can mutate a file at the confirmation boundary, like an editor.
type removalEditReader struct {
	edit func()
	io.Reader
}

func (r *removalEditReader) Read(p []byte) (int, error) {
	if r.edit != nil {
		r.edit()
		r.edit = nil
	}
	return r.Reader.Read(p)
}

func TestRemoveCommandGuardPrecedesArchive(t *testing.T) {
	ctx, state, client, _ := removeTestRepo(t)
	old := removeStdin
	defer func() { removeStdin = old }()
	removeStdin = &removalEditReader{edit: func() { wireWrite(t, ".", wiringSettingsFile, "edited\n") }, Reader: strings.NewReader("y\n")}
	var out, errs bytes.Buffer
	code := run(ctx, []string{"remove", "--state-dir", state}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "changed after the plan") {
		t.Fatalf("code=%d err=%s", code, &errs)
	}
	projects, err := client.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects=%v err=%v", projects, err)
	}
	if !fileExists(wiringMCPFile) || wireRead(t, ".", wiringSettingsFile) != "edited\n" {
		t.Fatal("files changed")
	}
}

func TestInitThenRemoveRepositoryRoundTrip(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "user files"}[own], func(t *testing.T) {
			ctx, state := startSettingsEngine(t)
			importRepo(t)
			root, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			remote := t.TempDir()
			wireGit(t, remote, "init", "--bare", "-q")
			wireGit(t, root, "remote", "set-url", "origin", remote)
			wireGit(t, root, "config", "core.excludesFile", os.DevNull)
			if own {
				wireWrite(t, root, wiringMCPFile, "{ \"mcpServers\": { \"mine\": {\"command\": \"mine\"} }, \"extra\": 2 }\n")
				wireWrite(t, root, wiringSettingsFile, "{\n  \"permissions\": {\"allow\": []},\n  \"hooks\": {\"Stop\": [{\"hooks\": [{\"command\": \"mine\"}]}]}\n}\n")
				wireWrite(t, root, ".claude/skills/mine/SKILL.md", "my skill\n")
				wireWrite(t, root, ".claude/skills/brigada-foreman/notes/mine.txt", "my notes\n")
			}
			before := removalSnapshot(t, root)
			startStopHelper(t, "runner", state)
			fakeInitPATH(t, "claude")
			// Keep the existing helper runner's one-task limit. Changing it
			// would restart the helper as a real detached runner instead.
			code, out, errs := initDo(t, state, "", "--yes", "--parallel", "1", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude", "--check", "true", "--tools", "none")
			if code != 0 {
				t.Fatalf("init code=%d\n%s\n%s", code, out, errs)
			}
			// Default removal keeps only the project config and its directory.
			before[engine.ProjectConfigPath] = wireRead(t, root, engine.ProjectConfigPath)
			before[filepath.ToSlash(filepath.Dir(engine.ProjectConfigPath))+"/"] = ""
			code, out, errs = removeDo(t, ctx, state, "", "--yes")
			if code != 0 {
				t.Fatalf("remove code=%d\n%s\n%s", code, out, errs)
			}
			if after := removalSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("round trip:\nbefore=%q\nafter=%q", before, after)
			}
			// Explicit config deletion restores the entire original tree too.
			code, out, errs = removeDo(t, ctx, state, "", "--yes", "--delete-config")
			if code != 0 {
				t.Fatalf("delete config code=%d\n%s\n%s", code, out, errs)
			}
			delete(before, engine.ProjectConfigPath)
			delete(before, filepath.ToSlash(filepath.Dir(engine.ProjectConfigPath))+"/")
			if after := removalSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("round trip with config deleted:\nbefore=%q\nafter=%q", before, after)
			}
		})
	}
}

func TestRemovalKeepsEmptyRepositoryRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	wireWrite(t, root, wiringRulesFile, string(oldRulesContent()))
	files := planSteps(root, []wireStep{{wiringRulesFile, func(d []byte, exists bool) ([]byte, string, error) {
		return nil, "removed", nil
	}}})
	if err := applyRemoval(files); err != nil {
		t.Fatal(err)
	}
	if got := removalSnapshot(t, root); len(got) != 0 {
		t.Fatalf("empty directories remain: %q", got)
	}
}

func TestRemovalParentCleanupRejectsOutsidePath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	// A shared prefix is insufficient to establish repository containment.
	outside := filepath.Join(parent, "repo-other")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	removeEmptyWiringParents(root, filepath.Join(outside, "deleted.txt"))
	if info, err := os.Stat(outside); err != nil || !info.IsDir() {
		t.Fatalf("cleanup removed an outside directory: %v", err)
	}
}

func TestRemoveWorkArrivingSincePlanChangesNoFiles(t *testing.T) {
	ctx, state, client, remote := removeTestRepo(t)
	before := removalSnapshot(t, ".")
	old := removeStdin
	defer func() { removeStdin = old }()
	removeStdin = &removalEditReader{edit: func() {
		if _, err := client.SubmitTask(ctx, api.TaskSubmit{RepoURL: remote, BaseRef: "main", Spec: "new work"}); err != nil {
			t.Fatal(err)
		}
	}, Reader: strings.NewReader("y\n")}
	var out, errs bytes.Buffer
	code := run(ctx, []string{"remove", "--state-dir", state}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "--cancel") {
		t.Fatalf("code=%d err=%s", code, &errs)
	}
	if after := removalSnapshot(t, "."); !reflect.DeepEqual(before, after) {
		t.Fatal("busy archive refusal changed files")
	}
}

func TestRemovalLinkedWorktreeExcludeAndSkillDirectory(t *testing.T) {
	root := wireRepo(t)
	wireGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	wireGit(t, root, "worktree", "add", "--detach", "-q", linked)
	exclude := wireGit(t, linked, "rev-parse", "--git-path", "info/exclude")
	before, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	wireApply(t, linked)
	wireWrite(t, linked, ".claude/skills/brigada-foreman/mine.txt", "my file\n")
	if err := applyRemoval(planRemoval(linked, false)); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(exclude); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("exclude=%q err=%v", got, err)
	}
	if got := wireRead(t, linked, ".claude/skills/brigada-foreman/mine.txt"); got != "my file\n" {
		t.Fatal("removed user's skill directory content")
	}
}

func TestRemovalRefusesSymlinksIncludingAfterPlan(t *testing.T) {
	for _, afterPlan := range []bool{false, true} {
		t.Run(map[bool]string{false: "before plan", true: "after plan"}[afterPlan], func(t *testing.T) {
			root := wireRepo(t)
			wireApply(t, root)
			files := planRemoval(root, false)
			moved := filepath.Join(t.TempDir(), "claude")
			if err := os.Rename(filepath.Join(root, ".claude"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, filepath.Join(root, ".claude")); err != nil {
				t.Fatal(err)
			}
			if afterPlan {
				if err := applyRemoval(files); err == nil || !strings.Contains(err.Error(), "symlink") {
					t.Fatalf("guard: %v", err)
				}
			} else {
				files = planRemoval(root, false)
				for _, f := range files {
					if strings.HasPrefix(f.rel, ".claude/") && f.err == nil {
						t.Errorf("symlink not reported for %s", f.rel)
					}
				}
				if err := applyRemoval(files); err != nil {
					t.Fatal(err)
				}
			}
			if !fileExists(filepath.Join(moved, "settings.local.json")) {
				t.Fatal("modified a symlink target")
			}
		})
	}
}

func TestRemovalPlanWrapsLongPathsAndErrors(t *testing.T) {
	root := wireRepo(t)
	wireWrite(t, root, wiringMCPFile, strings.Repeat("x", 120))
	files := planRemoval(root, false)
	files = append(files, wireFile{rel: strings.Repeat("path/", 30), err: os.ErrPermission})
	var out bytes.Buffer
	printRemovalPlan(&out, strings.Repeat("long-repository-name", 12), files, 0, false, 1, true)
	for _, line := range strings.Split(out.String(), "\n") {
		if utf8.RuneCountInString(line) >= 80 {
			t.Errorf("long line: %q", line)
		}
	}
}

func TestRemovalDeletesEmptyConfigOnRequest(t *testing.T) {
	root := wireRepo(t)
	wireWrite(t, root, engine.ProjectConfigPath, "")
	if err := applyRemoval(planRemoval(root, true)); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(root, engine.ProjectConfigPath)) {
		t.Fatal("empty config remains")
	}
}

func TestRemovalRemotePlanLeavesRefs(t *testing.T) {
	root := wireRepo(t)
	remote := t.TempDir()
	wireGit(t, remote, "init", "--bare", "-q")
	wireGit(t, root, "remote", "add", "origin", remote)
	wireGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "test")
	wireGit(t, root, "push", "-q", "origin", "HEAD:refs/heads/brigada/task-1", "HEAD:refs/heads/brigada/task-2", "HEAD:refs/heads/brigada/plans", "HEAD:refs/heads/brigada/other")
	before := wireGit(t, remote, "show-ref")
	var out bytes.Buffer
	printRemovalRemote(context.Background(), &out, root)
	for _, want := range []string{"2 brigada/task-* branches, brigada/plans", "git push origin --delete 'brigada/plans'", "git push origin --delete 'brigada/task-1'", "git push origin --delete 'brigada/task-2'"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "brigada/other") {
		t.Fatal("unrelated remote branch included")
	}
	if after := wireGit(t, remote, "show-ref"); before != after {
		t.Fatal("remote refs changed")
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if utf8.RuneCountInString(line) >= 80 {
			t.Errorf("long line: %q", line)
		}
	}
	wireGit(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "absent"))
	out.Reset()
	printRemovalRemote(context.Background(), &out, root)
	if !strings.Contains(out.String(), "could not be checked") {
		t.Fatal(out.String())
	}
}

func TestRemovalExcludeSeparatorPolicy(t *testing.T) {
	for _, tc := range []struct{ name, old, want string }{
		{"terminated", "# my rules\n/cache/\n", "# my rules\n/cache/\n"},
		{"unterminated", "# my rules\n/cache/", "# my rules\n/cache/\n"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wireRepo(t)
			wireWrite(t, root, ".git/info/exclude", tc.old)
			wireApply(t, root)
			// Init adds a separator newline to an unterminated exclude
			// file without recording whether that newline was already
			// there. Remove only the exact ignore line, retaining the
			// separator: guessing could delete a user's original byte.
			if err := applyRemoval(planRemoval(root, false)); err != nil {
				t.Fatal(err)
			}
			if got := wireRead(t, root, ".git/info/exclude"); got != tc.want {
				t.Fatalf("exclude = %q, want %q", got, tc.want)
			}
		})
	}
}
