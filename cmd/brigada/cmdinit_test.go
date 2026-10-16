package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func fakeInitPATH(t *testing.T, names ...string) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("INIT_CALLS", calls)
	for _, name := range names {
		script := `#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >> "$INIT_CALLS"
case "$*" in
 --help) echo 'Commands: no verified authentication status here'; exit 0 ;;
 *) echo 'unexpected harness invocation' >&2; exit 89 ;;
esac
`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", filepath.Join(t.TempDir(), "unused-home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "unused-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "unused-state"))
	return calls
}

func initDo(t *testing.T, state, stdin string, args ...string) (int, string, string) {
	t.Helper()
	old := projectStdin
	projectStdin = strings.NewReader(stdin)
	defer func() { projectStdin = old }()
	var out, err bytes.Buffer
	code := run(context.Background(), append([]string{"init", "--state-dir", state}, args...), &out, &err)
	return code, out.String(), err.String()
}

func TestInitProbeAndRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		stdin    string
		wantCode int
		want     string
	}{
		{"probe", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}, "", 0, "Probe only"},
		{"declined", []string{"--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}, "n\n", 1, "Declined"},
		{"one family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=anthropic/claude-opus-4", "--signed-in", "claude,opencode"}, "", 1, "--accept-limited"},
		{"limited acknowledged", []string{"--probe", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude"}, "", 0, "LIMITED SETUP"},
		{"zero", []string{"--yes", "--agent", "harness=claude,model=sonnet"}, "", 1, "zero usable agents"},
		{"no explicit models", []string{"--probe"}, "", 1, "zero usable agents"},
		{"bad model", []string{"--probe", "--agent", "harness=claude,model=unknown"}, "", 1, "cannot identify the family"},
		{"bad classes", []string{"--probe", "--agent", "harness=claude,model=sonnet,classes=bad class"}, "", 1, "agent classes"},
		{"non claude tools", []string{"--probe", "--agent", "harness=codex,model=gpt-5,tools=none"}, "", 1, "tools="},
		{"bad effort", []string{"--probe", "--agent", "harness=codex,model=gpt-5,effort=bogus"}, "", 1, "effort"},
		{"incapable foreman", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=kimi,model=kimi-k2", "--signed-in", "claude,kimi", "--foreman", "kimi/kimi-k2"}, "", 1, "resumes headless"},
		{"GLM second family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=zai-coding-plan/glm-5.3", "--signed-in", "claude,opencode"}, "", 0, "family=zhipu"},
		{"runner defaults", []string{"--probe", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex"}, "", 0, "harness=codex,model=default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			if err := os.WriteFile("go.mod", []byte("module example.invalid/test\n"), 0644); err != nil {
				t.Fatal(err)
			}
			calls := fakeInitPATH(t, "claude", "codex", "kimi", "opencode")
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, err := initDo(t, state, tc.stdin, tc.args...)
			if code != tc.wantCode || !strings.Contains(out+err, tc.want) {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
			}
			for _, path := range []string{state, ".brigada", os.Getenv("HOME"), os.Getenv("XDG_STATE_HOME"), os.Getenv("XDG_CONFIG_HOME")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("probe/refusal wrote %s: %v", path, err)
				}
			}
			data, _ := os.ReadFile(calls)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, " --help") {
					t.Errorf("guessed/paid invocation: %q", line)
				}
			}
			if strings.Contains(out, "ready") {
				t.Errorf("probe/limited/refusal claimed ready: %s", out)
			}
		})
	}
}

func TestInitPromptDefaultsAndSkip(t *testing.T) {
	for _, tc := range []struct {
		name, stdin string
		wantAgents  []string
		wantSkipped []string
		want        string
	}{
		{
			name: "blank uses every default", stdin: "\n\n\n\nn\n",
			wantAgents: []string{"claude,model=sonnet", "codex,model=default", "kimi,model=default", "opencode,model=default"},
			want:       "Declined",
		},
		{
			name: "dash skips accounts", stdin: "-\n\n-\n-\nn",
			wantAgents: []string{"codex,model=default"}, wantSkipped: []string{"claude", "kimi", "opencode"},
			want: "Declined",
		},
		{
			name: "all skipped", stdin: "-\n-\n-\n-\n",
			wantSkipped: initHarnesses, want: "zero usable agents",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, initHarnesses...)
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, err := initDo(t, state, tc.stdin, "--signed-in", "claude,codex,kimi,opencode", "--accept-limited", "--check", "true", "--tools", "none")
			if code != 1 || !strings.Contains(out+err, tc.want) {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
			}
			for _, agent := range tc.wantAgents {
				if !strings.Contains(out, "  agent harness="+agent+",") {
					t.Errorf("missing default agent %s in %s", agent, out)
				}
			}
			for _, harness := range tc.wantSkipped {
				if strings.Contains(out, "  agent harness="+harness+",") {
					t.Errorf("registered skipped harness %s", harness)
				}
			}
			for _, path := range []string{state, ".brigada"} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("refusal wrote %s: %v", path, err)
				}
			}
		})
	}
}

func TestInitNonTerminalPrompts(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, initHarnesses...)
	code, out, err := initDo(t, testsupport.ShortStateDir(t), "\n\n\n\ny\ny\ny\ny\nn\n", "--check", "true", "--tools", "none")
	if code != 1 || !strings.Contains(out, "Declined") || err != "" {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	for _, prompt := range []string{
		"Model served by claude account [sonnet] (- to skip):",
		"Model served by codex account [default] (- to skip):",
		"Model served by kimi account [default] (- to skip):",
		"Model served by opencode account [default] (- to skip):",
		"Confirm claude account claude is signed in and serves sonnet? [y/N]",
		"Confirm codex account codex is signed in and serves default? [y/N]",
		"Confirm kimi account kimi is signed in and serves default? [y/N]",
		"Confirm opencode account opencode is signed in and serves default? [y/N]",
		"Apply this plan (including the file diff above)? [y/N]",
	} {
		if !strings.Contains(out, "\n"+prompt+"\n") {
			t.Errorf("prompt is missing or not on its own line: %q\n%s", prompt, out)
		}
	}
}

func TestInitNoInput(t *testing.T) {
	for _, tc := range []struct {
		name, stdin, want string
		args              []string
	}{
		{"no model input", "", "--agent harness=claude", nil},
		{"model input exhausted", "\n", "--agent harness=codex", nil},
		{"no authentication input", "", "--signed-in claude", []string{"--agent", "harness=claude"}},
		{"no apply input", "", "--yes", []string{"--agent", "harness=claude", "--signed-in", "claude", "--accept-limited"}},
		{"no check input", "", "--check COMMAND", []string{"--agent", "harness=claude", "--signed-in", "claude", "--accept-limited", "--check="}},
		{"no tools input", "", "--tools", []string{"--agent", "harness=claude", "--signed-in", "claude", "--accept-limited", "--tools="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			args := append([]string{"--check", "true", "--tools", "none"}, tc.args...)
			code, out, err := initDo(t, testsupport.ShortStateDir(t), tc.stdin, args...)
			if code != 1 {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
			}
			for _, want := range []string{"no input available", "--agent", "--signed-in", tc.want} {
				if !strings.Contains(err, want) {
					t.Errorf("missing %q in %q", want, err)
				}
			}
			if strings.Contains(out, "  agent harness=") && tc.name == "no model input" {
				t.Fatal("treated EOF as a default model answer")
			}
			if _, err := os.Stat(".brigada"); !os.IsNotExist(err) {
				t.Fatalf("EOF wrote configuration: %v", err)
			}
		})
	}
}

func TestInitBuildSuggestions(t *testing.T) {
	for _, tc := range []struct{ name, body, check, tools string }{
		{"go.mod", "module x", "go test ./...", "Bash(go build:*)+Bash(go test:*)+Bash(go vet:*)+Bash(gofmt:*)"},
		{"package.json", `{"scripts":{"test":"touch should-never-exist"}}`, "npm test", "Bash(npm test:*)"},
		{"Cargo.toml", "[package]", "cargo test", "Bash(cargo test:*)"},
		{"pyproject.toml", "[project]", "python -m pytest", "Bash(python -m pytest:*)"},
		{"pytest.ini", "[pytest]", "python -m pytest", "Bash(python -m pytest:*)"},
		{"package.json", `{"scripts":{"build":"x"}}`, "", ""},
		{"unmatched", "", "", ""},
	} {
		t.Run(tc.name+tc.check, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, tc.name), []byte(tc.body), 0644); err != nil {
				t.Fatal(err)
			}
			check, tools, err := suggestInitDefaults(root)
			if err != nil || check != tc.check || tools != tc.tools {
				t.Fatalf("got %q %q %v", check, tools, err)
			}
			if _, err := os.Stat(filepath.Join(root, "should-never-exist")); !os.IsNotExist(err) {
				t.Fatal("executed test script")
			}
		})
	}
}

func TestInitExistingProjectDiffAndOrigin(t *testing.T) {
	write := importRepo(t)
	original := projectFile("# keep my policy\napproval_max_tasks: 9\ndefault_check: 'old check' # retain this comment\nworker_tools: none\n")
	write(original)
	fakeInitPATH(t, "claude", "codex")
	args := []string{"--probe", "--agent", "harness=claude,model=sonnet,account=work,effort=high,classes=review+answer,tools=none", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex", "--check", "new check"}
	code, out, err := initDo(t, testsupport.ShortStateDir(t), "", args...)
	if code != 0 || !strings.Contains(out, "--- existing") || !strings.Contains(out, `+default_check: "new check" # retain this comment`) {
		t.Fatalf("%d %s %s", code, out, err)
	}
	got, _ := os.ReadFile(engine.ProjectConfigPath)
	if string(got) != original {
		t.Fatal("probe modified existing file")
	}
	proposed, err2 := engine.ProjectConfigDefaults(got, "example.invalid/imported", "new check", "none")
	if err2 != nil {
		t.Fatal(err2)
	}
	if !strings.Contains(string(proposed), "# keep my policy\napproval_max_tasks: 9") || !strings.Contains(string(proposed), "# retain this comment") {
		t.Fatal("lost user content")
	}
	// Supported file origins are identities, including a local bare remote.
	remote := filepath.Join(t.TempDir(), "remote.git")
	if output, e := exec.Command("git", "init", "--bare", remote).CombinedOutput(); e != nil {
		t.Fatalf("%v %s", e, output)
	}
	if output, e := exec.Command("git", "remote", "set-url", "origin", "file://"+remote).CombinedOutput(); e != nil {
		t.Fatalf("%v %s", e, output)
	}
	_, canon, e := initOrigin(context.Background(), ".")
	if e != nil || canon != "file:"+remote {
		t.Fatalf("file origin = %q %v", canon, e)
	}
	if output, e := exec.Command("git", "remote", "remove", "origin").CombinedOutput(); e != nil {
		t.Fatalf("%v %s", e, output)
	}
	if _, _, e := initOrigin(context.Background(), "."); e == nil {
		t.Fatal("accepted no origin")
	}
}

func TestInitDocumentedStatusOnly(t *testing.T) {
	importRepo(t)
	calls := fakeInitPATH(t, "claude", "codex", "kimi")
	for _, h := range []string{"claude", "codex"} {
		parent := "auth"
		status := `{"loggedIn":true,"email":"never-print-me"}`
		if h == "codex" {
			parent = "login"
			status = "Logged in using never-print-me"
		}
		script := `#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >> "$INIT_CALLS"
case "$*" in
 --help) echo '  ` + parent + `  Manage authentication' ;;
 '` + parent + ` --help') echo '  status  Show authentication status' ;;
 '` + parent + ` status') echo '` + status + `' ;;
 *) exit 89 ;;
esac
`
		if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), h), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"claude", "codex"} {
		p := probeHarness(context.Background(), h)
		if !p.signedIn || !strings.Contains(p.evidence, "documented by --help") {
			t.Fatalf("%s: %+v", h, p)
		}
		if strings.Contains(p.evidence, "never-print-me") {
			t.Fatal("leaked authentication output")
		}
	}
	if p := probeHarness(context.Background(), "kimi"); p.signedIn {
		t.Fatal("assumed kimi authenticated")
	}
	data, _ := os.ReadFile(calls)
	if strings.Contains(string(data), "kimi ") {
		t.Fatal("guessed kimi status command")
	}
	// A status word in prose is not a documented command.
	if helpNamesCommand("Use status to check your job", "status") {
		t.Fatal("matched prose")
	}
}

func TestInitApplyDetachedAndRepeat(t *testing.T) {
	// Build before replacing PATH, so the only harnesses visible to init are fakes.
	binary := testsupport.BuildBrigada(t)
	old := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = old })
	importRepo(t)
	if err := os.WriteFile("package.json", []byte(`{"scripts":{"test":"exit 99"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	calls := fakeInitPATH(t, "claude", "codex")
	state := testsupport.ShortStateDir(t)
	t.Cleanup(func() {
		if rec, running, _ := runnerstate.Running(filepath.Join(state, "runner")); running {
			_ = syscall.Kill(rec.PID, syscall.SIGTERM)
		}
		data, _ := os.ReadFile(lifecycle.PathsOf(state).Lock)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			_, live, _ := runnerstate.Running(filepath.Join(state, "runner"))
			if !live && !lifecycle.EngineRunning(context.Background(), lifecycle.PathsOf(state).Socket) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("detached children did not stop")
	})
	args := []string{"--yes", "--agent", "harness=claude,model=sonnet,account=work,effort=high", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}
	code, out, err := initDo(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "Detached runner started") || !strings.Contains(out, "Setup ready") {
		log, _ := os.ReadFile(filepath.Join(state, "engine.log"))
		t.Fatalf("%d\n%s\n%s\nengine log: %s", code, out, err, log)
	}
	if !lifecycle.EngineRunning(context.Background(), lifecycle.PathsOf(state).Socket) {
		t.Fatal("engine not started")
	}
	content, e := os.ReadFile(engine.ProjectConfigPath)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(content), "account") || strings.Contains(string(content), "work\n") {
		t.Fatal("local metadata in project file")
	}
	cfg, e := engine.ParseProjectConfig(content)
	if e != nil {
		t.Fatal(e)
	}
	var check string
	_ = json.Unmarshal(cfg.Values[engine.SettingDefaultCheck], &check)
	if check != "npm test" {
		t.Fatalf("check=%s", check)
	}
	_, token, e := lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, "client")
	if e != nil {
		t.Fatal(e)
	}
	client := api.NewClient(lifecycle.PathsOf(state).Socket, token)
	settings, e := client.ProjectSettings(context.Background(), importOrigin)
	if e != nil || settings.ImportHash != engine.ConfigHash(content) {
		t.Fatalf("settings=%+v %v", settings, e)
	}
	fm, e := client.ProjectForeman(context.Background(), importOrigin)
	if e != nil || fm.Harness != "claude" || fm.Effort != "high" {
		t.Fatalf("foreman=%+v %v", fm, e)
	}
	rec, live, e := runnerstate.Running(filepath.Join(state, "runner"))
	if e != nil || !live || len(rec.Agents) != 2 {
		t.Fatalf("runner=%+v %v %v", rec, live, e)
	}
	if rec.Agents[0].Tools != "Bash(npm test:*)" || rec.Agents[1].Tools != "" || rec.Agents[0].Family != "anthropic" || rec.Agents[1].Family != "openai" {
		t.Fatalf("wrong agents: %+v", rec.Agents)
	}
	code, out, err = initDo(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "Runner already running") || !strings.Contains(out, "agents match") {
		t.Fatalf("repeat: %d\n%s\n%s", code, out, err)
	}
	args[2] = "harness=claude,model=opus,account=work,effort=high"
	code, out, err = initDo(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "agents differ") {
		t.Fatalf("differing agents: %d\n%s\n%s", code, out, err)
	}
	again, live, e := runnerstate.Running(filepath.Join(state, "runner"))
	if e != nil || !live || again.PID != rec.PID {
		t.Fatalf("runner replaced: %+v %v", again, e)
	}
	data, _ := os.ReadFile(calls)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasSuffix(line, " --help") {
			t.Errorf("unexpected harness execution: %s", line)
		}
	}
}

func TestInitKeepsMachineToolScope(t *testing.T) {
	importRepo(t)
	if err := os.WriteFile("go.mod", []byte("module x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(config, []byte("tools.claude/sonnet = none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeInitPATH(t, "claude", "codex")
	args := []string{"--probe", "--config", config, "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}
	state := testsupport.ShortStateDir(t)
	code, out, err := initDo(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "claude/sonnet effective worker tools: none") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	args = append(args, "--tools", "Bash(go test:*)")
	code, out, err = initDo(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "claude/sonnet effective worker tools: Bash(go test:*)") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	data, e := os.ReadFile(config)
	if e != nil || string(data) != "tools.claude/sonnet = none\n" {
		t.Fatal("changed local config")
	}
}

func TestInitNoInstalledAgents(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t)
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, err := initDo(t, state, "", "--yes", "--accept-limited")
	if code != 1 || !strings.Contains(err, "zero usable agents") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	if _, e := os.Stat(state); !os.IsNotExist(e) {
		t.Fatal("created state")
	}
	if _, e := os.Stat(".brigada"); !os.IsNotExist(e) {
		t.Fatal("created project config")
	}
}
