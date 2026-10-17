package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		{"GLM second family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=zai-coding-plan/glm-5.3", "--signed-in", "claude,opencode"}, "", 0, "Zhipu (GLM)"},
		{"runner defaults", []string{"--probe", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex"}, "", 0, "    codex     CLI default"},
		{"codex default second family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=default", "--signed-in", "claude,codex"}, "", 0, "Review: 2 model families"},
		{"opencode default unknown family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=default", "--signed-in", "claude,opencode"}, "", 1, "--accept-limited"},
		{"opencode default limited acknowledged", []string{"--probe", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=default", "--signed-in", "claude,opencode"}, "", 0, "family: unknown (give provider/model to count it for review)"},
		{"opencode needs provider", []string{"--probe", "--agent", "harness=opencode,model=glm"}, "", 1, "opencode needs provider/model, e.g. zai-coding-plan/glm-5.3"},
		{"model whitespace", []string{"--probe", "--agent", "harness=claude,model=sonnet other"}, "", 1, "model must be a nonempty model identifier"},
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
			if (slices.Contains(tc.args, "--yes") || slices.Contains(tc.args, "--probe")) && strings.Contains(out, "brigada runs coding tasks") {
				t.Errorf("noninteractive run printed the introduction: %s", out)
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
			wantAgents: []string{"claude    sonnet", "codex     CLI default", "kimi      CLI default", "opencode  CLI default"},
			want:       "Declined",
		},
		{
			name: "dash skips accounts", stdin: "-\n\n-\n-\nn",
			wantAgents: []string{"codex     CLI default"}, wantSkipped: []string{"claude", "kimi", "opencode"},
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
				if !strings.Contains(out, "    "+agent) {
					t.Errorf("missing default agent %s in %s", agent, out)
				}
			}
			for _, harness := range tc.wantSkipped {
				if strings.Contains(out, "    "+harness+" ") {
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
	code, out, err := initDo(t, testsupport.ShortStateDir(t), "y\n\ny\n\ny\n\ny\n\nn\n", "--check", "true", "--tools", "none")
	if code != 1 || !strings.Contains(out, "Declined") || err != "" {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	prompts := []string{
		"Are you signed in to claude? [y/N]",
		"claude model [sonnet] (Enter = default, - = don't use claude):",
		"Are you signed in to codex? [y/N]",
		"codex model [CLI default] (Enter = default, - = don't use codex):",
		"Are you signed in to kimi? [y/N]",
		"kimi model [CLI default] (Enter = default, - = don't use kimi):",
		"Are you signed in to opencode? [y/N]",
		"opencode model as provider/model, e.g. zai-coding-plan/glm-5.3 [CLI default] (Enter = default, - = don't use opencode):",
		"Write 4 files and start the runner? [y/N]",
	}
	previous := -1
	for _, prompt := range prompts {
		position := strings.Index(out, "\n"+prompt+"\n")
		if position < 0 || position <= previous {
			t.Errorf("prompt is missing, out of order or not on its own line: %q\n%s", prompt, out)
		}
		previous = position
	}
	plan := strings.Index(out, "\nPlan\n")
	if last := strings.Index(out, prompts[len(prompts)-2]); plan <= last || plan >= previous {
		t.Errorf("plan must follow setup questions and precede confirmation:\n%s", out)
	}
	for _, want := range []string{
		"brigada init: set up example.invalid/imported",
		"changes nothing until you confirm.",
		"Agent CLIs found:",
		"  claude    sign-in can't be checked",
		"  Review: 3 model families; a task is always reviewed by a family other than its author's.",
		"  Foreman: claude/sonnet (the first agent that can resume a session headless; change it with --foreman HARNESS/MODEL)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	for _, internal := range []string{"documented by --help", "agent harness=", "classes=implement+review+answer"} {
		if strings.Contains(out, internal) {
			t.Errorf("printed internal detail %q", internal)
		}
	}
}

func TestInitNoInput(t *testing.T) {
	for _, tc := range []struct {
		name, stdin, want string
		args              []string
	}{
		{"no model input", "", "--signed-in claude", nil},
		{"model input exhausted", "y\n", "--agent harness=claude", nil},
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
			if strings.Contains(out, "\nPlan\n") && tc.name == "no model input" {
				t.Fatal("treated EOF as a default model answer")
			}
			if _, err := os.Stat(".brigada"); !os.IsNotExist(err) {
				t.Fatalf("EOF wrote configuration: %v", err)
			}
		})
	}
}

func TestInitModelRetries(t *testing.T) {
	for _, tc := range []struct {
		name, harness, answers, reason, model string
		tries                                 int
	}{
		{"unknown then valid", "claude", "unknown\nsonnet\n", "cannot identify the family", "sonnet", 2},
		{"whitespace then valid", "claude", "sonnet other\nsonnet\n", "model must be a nonempty model identifier", "sonnet", 2},
		{"spaces then valid", "claude", "   \nsonnet\n", "model must be a nonempty model identifier", "sonnet", 2},
		{"provider then valid", "opencode", "glm\nzai-coding-plan/glm-5.3\n", "opencode needs provider/model, e.g. zai-coding-plan/glm-5.3", "zai-coding-plan/glm-5.3", 2},
		{"retry then default", "claude", "unknown\n\n", "cannot identify the family", "sonnet", 2},
		{"retry then skip", "claude", "unknown\n-\n", "cannot identify the family", "", 2},
		{"third answer valid", "claude", "unknown\nsonnet other\nsonnet\n", "cannot identify the family", "sonnet", 3},
		{"three invalid answers", "claude", "unknown\nunknown\nunknown\n", "cannot identify the family", "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			calls := fakeInitPATH(t, "claude", "codex", "opencode")
			answers := tc.answers + "\n-\nn\n"
			if tc.harness == "opencode" {
				answers = "\n\n" + tc.answers + "n\n"
			}
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, err := initDo(t, state, answers, "--signed-in", "claude,codex,opencode", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || strings.Count(out, tc.harness+" model ") != tc.tries {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
			}
			if !strings.Contains(out, "\n  "+tc.reason) || !strings.Contains(out, "; try again.\n") {
				t.Fatalf("missing indented retry reason:\n%s", out)
			}
			if tc.name == "three invalid answers" {
				if !strings.Contains(err, "--agent claude: "+tc.reason) || strings.Contains(out, "\nPlan\n") {
					t.Fatalf("did not stop with the model error after three attempts:\n%s\n%s", out, err)
				}
			} else {
				if err != "" || !strings.Contains(out, "Declined") {
					t.Fatalf("valid answer did not reach the plan:\n%s\n%s", out, err)
				}
				plan := out[strings.Index(out, "\nPlan\n"):]
				if tc.model != "" && !strings.Contains(plan, tc.model) {
					t.Fatalf("accepted model missing from plan:\n%s", plan)
				}
				if tc.name == "retry then skip" && strings.Contains(plan, "    claude ") {
					t.Fatalf("skipped model in plan:\n%s", plan)
				}
			}
			for _, path := range []string{state, ".brigada"} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("model retry/refusal wrote %s: %v", path, err)
				}
			}
			data, _ := os.ReadFile(calls)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, " --help") {
					t.Errorf("unexpected harness execution: %s", line)
				}
			}
		})
	}
}

func TestInitQuestionsBeforePlan(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, initHarnesses...)
	code, out, err := initDo(t, testsupport.ShortStateDir(t), "n\ny\n\ny\n\nn\ntrue\nnone\nn\n", "--no-wiring")
	if code != 1 || err != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	if strings.Contains(out, "claude model [") || strings.Contains(out, "opencode model [") {
		t.Fatalf("asked for a model after sign-in was declined:\n%s", out)
	}
	lastQuestion := strings.Index(out, "Worker tools (required;")
	for _, line := range []string{"\nPlan\n", "    codex     CLI default", "  Review:", "  Foreman:", "Default check:", "Register project and foreman"} {
		if pos := strings.Index(out, line); lastQuestion < 0 || pos <= lastQuestion {
			t.Errorf("plan line %q preceded the last setup question:\n%s", line, out)
		}
	}
}

func TestInitExplicitAccountQuestions(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	code, out, err := initDo(t, testsupport.ShortStateDir(t), "y\ny\nn\n", "--agent", "harness=codex", "--agent", "harness=claude,account=work,classes=review", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || err != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	claude := strings.Index(out, "Are you signed in to claude account work? [y/N]")
	codex := strings.Index(out, "Are you signed in to codex? [y/N]")
	if claude < 0 || codex <= claude || strings.Contains(out, "codex account codex") {
		t.Fatalf("incorrect sign-in question wording or order:\n%s", out)
	}
	if !strings.Contains(out, "family: Anthropic (review only)") || strings.Contains(out, "(implement, review, answer only)") {
		t.Fatalf("incorrect agent classes display:\n%s", out)
	}
}

func TestInitApplyPrompts(t *testing.T) {
	for _, tc := range []struct {
		name, prompt    string
		exists, running bool
	}{
		{"write and start", "Write 1 files and start the runner? [y/N]", false, false},
		{"write", "Write 1 files? [y/N]", false, true},
		{"start", "Start the runner? [y/N]", true, false},
		{"register", "Register the project? [y/N]", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write := importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			state := testsupport.ShortStateDir(t)
			if tc.exists {
				config, err := engine.ProjectConfigDefaults(nil, "example.invalid/imported", "true", "none")
				if err != nil {
					t.Fatal(err)
				}
				write(string(config))
			}
			if tc.running {
				release, err := runnerstate.Acquire(filepath.Join(state, "runner"))
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			code, out, err := initDo(t, state, "n\n", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || err != "" || !strings.Contains(out, "\n"+tc.prompt+"\n") || !strings.Contains(out, "Declined") {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
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
		if !p.signedIn || p.status != "signed in" || !strings.Contains(p.evidence, "documented by --help") {
			t.Fatalf("%s: %+v", h, p)
		}
		if strings.Contains(p.evidence, "never-print-me") {
			t.Fatal("leaked authentication output")
		}
	}
	if p := probeHarness(context.Background(), "kimi"); p.signedIn || p.status != "sign-in can't be checked" {
		t.Fatalf("unexpected kimi status: %+v", p)
	}
	if p := probeHarness(context.Background(), "opencode"); p.installed || p.status != "not installed" {
		t.Fatalf("unexpected missing CLI status: %+v", p)
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

func TestInitSignedOutStatus(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	for _, h := range []string{"claude", "codex"} {
		parent, status := "auth", `{"loggedIn":false}`
		if h == "codex" {
			parent, status = "login", "Not logged in"
		}
		script := "#!/bin/sh\ncase \"$*\" in\n --help) echo '  " + parent + "  Authentication' ;;\n '" + parent + " --help') echo '  status  Authentication status' ;;\n '" + parent + " status') echo '" + status + "' ;;\n *) exit 89 ;;\nesac\n"
		if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), h), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
		if p := probeHarness(context.Background(), h); p.signedIn || p.status != "not signed in" {
			t.Fatalf("%s: %+v", h, p)
		}
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
	code, out, err = initDo(t, state, "unknown\nsonnet\ngpt-5\ny\n", "--signed-in", "claude,codex", "--no-wiring")
	if code != 0 || strings.Count(out, "claude model [") != 2 || !strings.Contains(out, "Runner already running") {
		t.Fatalf("interactive retry: %d\n%s\n%s", code, out, err)
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
