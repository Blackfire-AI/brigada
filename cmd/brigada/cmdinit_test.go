package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
 --help)
   if [ "${0##*/}" = opencode ]; then
     printf '  opencode models [provider]\n  opencode auth Manage credentials\n'
   else
     echo 'Commands: no verified authentication status here'
   fi
   exit 0 ;;
 models) [ "${0##*/}" = opencode ] || exit 89; printf 'fixture/gpt-5\nfixture/glm-5.3\n' ;;
 'auth list') [ "${0##*/}" = opencode ] || exit 89; printf '┌ Credentials\n● Provider one oauth\n● Provider two api\n└ 2 credentials\n' ;;
 *) echo 'unexpected harness invocation' >&2; exit 89 ;;
esac
`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", filepath.Join(t.TempDir(), "unused-home"))
	t.Setenv("CODEX_HOME", "")
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
		{"declined", []string{"--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}, "\n\nn\n", 1, "Declined"},
		{"one family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=anthropic/claude-opus-4", "--signed-in", "claude,opencode"}, "", 1, "--accept-limited"},
		{"limited acknowledged", []string{"--probe", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--signed-in", "claude"}, "", 0, "LIMITED SETUP"},
		{"zero", []string{"--yes", "--agent", "harness=claude,model=sonnet"}, "", 1, "zero usable agents"},
		{"no explicit models", []string{"--probe"}, "", 1, "zero usable agents"},
		{"bad model", []string{"--probe", "--agent", "harness=claude,model=unknown"}, "", 1, "cannot identify the family"},
		{"bad classes", []string{"--probe", "--agent", "harness=claude,model=sonnet,classes=bad class"}, "", 1, "agent classes"},
		{"non claude tools", []string{"--probe", "--agent", "harness=codex,model=gpt-5,tools=none"}, "", 1, "tools="},
		{"bad effort", []string{"--probe", "--agent", "harness=codex,model=gpt-5,effort=bogus"}, "", 1, "effort"},
		{"incapable foreman", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=kimi,model=kimi-k2", "--signed-in", "claude,kimi", "--foreman", "kimi/kimi-k2"}, "", 1, "resumes headless"},
		{"GLM second family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=zai-coding-plan/glm-5.3", "--signed-in", "claude,opencode"}, "", 0, "Zhipu"},
		{"runner defaults", []string{"--probe", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex"}, "", 0, "    codex     CLI default"},
		{"codex default second family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=default", "--signed-in", "claude,codex"}, "", 0, "Reviews: 2 model families"},
		{"opencode default unknown family", []string{"--probe", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=default", "--signed-in", "claude,opencode"}, "", 1, "--accept-limited"},
		{"opencode default limited acknowledged", []string{"--probe", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--agent", "harness=opencode,model=default", "--signed-in", "claude,opencode"}, "", 0, "unknown family"},
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
				if !strings.HasSuffix(line, " --help") && line != "opencode models" && line != "opencode auth list" {
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
	// Enter now picks menu entry 1, except OpenCode defaults to leaving it out;
	// two planning agents also require a foreman answer before confirmation.
	for _, tc := range []struct {
		name, stdin string
		wantAgents  []string
		wantSkipped []string
		want        string
	}{
		{
			name: "blank uses menu defaults", stdin: "\n\n\n\n\n\nn\n",
			wantAgents:  []string{"claude    sonnet", "codex     CLI default", "kimi      CLI default"},
			wantSkipped: []string{"opencode"},
			want:        "Declined",
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

func TestInitKimiWriteOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		write        bool
	}{
		{name: "Enter reviews only"},
		{name: "no reviews only", answer: "n"},
		{name: "yes allows writing", answer: "y", write: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			calls := fakeInitPATH(t, initHarnesses...)
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			// Skip Claude, pick Codex, pick Kimi, answer its writing question,
			// then type an OpenCode model and a check. Distinct answers catch
			// any question that consumes stdin in the wrong position.
			input := "-\n\n\n" + tc.answer + "\nzai-coding-plan/glm-5.3\nprintf check-marker\nn\n"
			code, out, errOut := initDo(t, state, input, "--signed-in", "claude,codex,kimi,opencode", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			warning := "    kimi has no sandbox: when it writes code it can run any command on this\n    machine without asking. As a reviewer it only reads.\n    Let kimi write code too, not only review? [y/N]:\n"
			if !strings.Contains(out, "    Pick 1, or type a model id [1]:\n"+warning+"\n  opencode:") {
				t.Fatalf("writing question is not right after Kimi model:\n%s", out)
			}
			if strings.Contains(strings.Join(strings.Fields(out), " "), "Moonshot reviews only") == tc.write {
				t.Fatalf("incorrect Kimi classes after %q:\n%s", tc.answer, out)
			}
			for _, want := range []string{"opencode  zai-coding-plan/glm-5.3", "Check: printf check-marker"} {
				if !strings.Contains(out, want) {
					t.Errorf("stdin shifted; missing %q:\n%s", want, out)
				}
			}
			_, workers, _ := strings.Cut(out, "  What workers may do:\n")
			if !tc.write && strings.Contains(workers, "    kimi ") {
				t.Errorf("review-only Kimi included in worker permissions:\n%s", out)
			}
			if strings.Contains(out, "ready") {
				t.Errorf("claimed ready before apply:\n%s", out)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("declined init wrote state: %v", err)
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, " --help") && line != "opencode models" && line != "opencode auth list" {
					t.Errorf("init ran a model or login: %q", line)
				}
			}
		})
	}
}

func TestInitExplicitKimiClassesStayAsGiven(t *testing.T) {
	for _, tc := range []struct{ classes, role string }{
		{"", "writes code, reviews"},
		{",classes=review", "reviews only"},
		{",classes=implement+answer", "writes code, answers questions"},
		{",classes=review+answer", "reviews, answers questions"},
		{",classes=answer", "answers questions only"},
		{",classes=implement", "writes code only"},
		{",classes=implement+review", "writes code, reviews"},
		{",classes=custom", "handles custom tasks only"},
	} {
		t.Run(tc.classes, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "codex", "kimi")
			code, out, errOut := initDo(t, testsupport.ShortStateDir(t), "n\n", "--agent", "harness=codex", "--agent", "harness=kimi"+tc.classes, "--signed-in", "codex,kimi", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			if strings.Contains(out, "Let kimi write") || strings.Contains(out, "kimi has no sandbox:") {
				t.Fatalf("explicit agent got writing question:\n%s", out)
			}
			want := "Moonshot " + tc.role
			if !strings.Contains(strings.Join(strings.Fields(out), " "), strings.Join(strings.Fields(want), " ")) {
				t.Errorf("explicit classes changed:\n%s", out)
			}
		})
	}
}

func TestInitNonTerminalPrompts(t *testing.T) {
	// Menus and the foreman choice replace free-form model questions; confirmed
	// OpenCode authentication skips its Use question and Enter leaves it out.
	importRepo(t)
	wireGit(t, ".", "config", "core.excludesFile", os.DevNull)
	fakeInitPATH(t, initHarnesses...)
	code, out, err := initDo(t, testsupport.ShortStateDir(t), "y\n\ny\n\ny\n\n\n\n\nn\n", "--check", "true", "--tools", "none")
	if code != 1 || !strings.Contains(out, "Declined") || err != "" {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	prompts := []string{
		"    Are you signed in to claude? [y/N]",
		"    Pick 1-3 or -, or type a model id [1]:",
		"    Are you signed in to codex? [y/N]",
		"    Pick 1 or -, or type a model id [1]:",
		"    Use kimi? [y/N]:",
		"    Pick 1, or type a model id [1]:",
		"    Let kimi write code too, not only review? [y/N]:",
		"    Pick 1-2 or -, or type provider/model [-]:",
		"  Pick 1-2 [1]:",
		"Write 5 files and start the runner? [y/N]",
	}
	if !strings.Contains(out, "\n\n  claude: sign-in can't be checked\n"+prompts[0]+"\n") || strings.Contains(out, "\n\n\n  claude:") {
		t.Errorf("expected one blank line before the CLI block:\n%s", out)
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
		"Nothing is written or started until you confirm at the end.",
		"Step 1 of 3: agents",
		"  claude: sign-in can't be checked",
		"  kimi: init can't check sign-in. If you're not signed in, run `kimi login`\n  first, or leave kimi out.",
		"  opencode: signed in to 2 providers (opencode auth list)",
		"Step 2 of 3: the foreman",
		"  Reviews: 3 model families, so every task can get a reviewer of another one.",
		"  Foreman: claude/sonnet\n",
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
	// The numbered model and foreman questions still report their answering
	// flag on EOF, rather than treating missing input as a menu default.
	for _, tc := range []struct {
		name, stdin, want string
		args              []string
	}{
		{"no model input", "", "--signed-in claude", nil},
		{"model input exhausted", "y\n", "--agent harness=claude", nil},
		{"no authentication input", "", "--signed-in claude", []string{"--agent", "harness=claude"}},
		{"no foreman input", "", "--foreman", []string{"--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex"}},
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
	// Invalid typed IDs retain the three-try limit; numeric answers now select
	// menu entries, and surviving planning agents require a foreman answer.
	for _, tc := range []struct {
		name, harness, answers, reason, model string
		tries                                 int
	}{
		{"unknown then valid", "claude", "unknown\n1\n", "cannot identify the family", "sonnet", 2},
		{"whitespace then valid", "claude", "sonnet other\nsonnet\n", "model must be a nonempty model identifier", "sonnet", 2},
		{"spaces then valid", "claude", "   \nsonnet\n", "model must be a nonempty model identifier", "sonnet", 2},
		{"provider then valid", "opencode", "glm\nzai-coding-plan/glm-5.3\n", "opencode needs provider/model, e.g. zai-coding-plan/glm-5.3", "zai-coding-plan/glm-5.3", 2},
		{"unknown provider model then valid", "opencode", "opencode/beam-free\n1\n", "cannot identify the family", "fixture/gpt-5", 2},
		{"retry then default", "claude", "unknown\n\n", "cannot identify the family", "sonnet", 2},
		{"retry then skip", "claude", "unknown\n-\n", "cannot identify the family", "", 2},
		{"third answer valid", "claude", "unknown\nsonnet other\nsonnet\n", "cannot identify the family", "sonnet", 3},
		{"three invalid answers", "claude", "unknown\nunknown\nunknown\n", "cannot identify the family", "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			calls := fakeInitPATH(t, "claude", "codex", "opencode")
			answers := tc.answers + "\n-\n\nn\n"
			if tc.name == "retry then skip" {
				answers = tc.answers + "\n-\nn\n"
			}
			if tc.harness == "opencode" {
				answers = "\n\n" + tc.answers + "\nn\n"
			}
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, err := initDo(t, state, answers, "--signed-in", "claude,codex,opencode", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || strings.Count(out, map[string]string{"claude": "    Pick 1-3 or -, or type a model id [1]:", "opencode": "    Pick 1-2 or -, or type provider/model [-]:"}[tc.harness]) != tc.tries {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
			}
			if !strings.Contains(out, "\n  "+tc.reason) || !strings.Contains(out, "; try again.\n") || !strings.Contains(out, "pick a numbered entry, or type a model whose name says its family") {
				t.Fatalf("missing indented retry reason:\n%s", out)
			}
			if tc.name == "three invalid answers" {
				if !strings.Contains(err, "--agent claude: "+tc.reason) || !strings.Contains(err, "pick a numbered entry, or type a model whose name says its family") || strings.Contains(out, "\nPlan\n") {
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
				if !strings.HasSuffix(line, " --help") && line != "opencode models" && line != "opencode auth list" {
					t.Errorf("unexpected harness execution: %s", line)
				}
			}
		})
	}
}

func TestInitQuestionsBeforePlan(t *testing.T) {
	// Claude is declined before its model menu; OpenCode is confirmed by
	// its probe and skipped in its menu. Check/tools still precede the Plan.
	// The single-foreman notice is now part of Step 2, so match the Plan
	// foreman line by its existing explanation rather than its shared label.
	importRepo(t)
	fakeInitPATH(t, initHarnesses...)
	code, out, err := initDo(t, testsupport.ShortStateDir(t), "n\ny\n\ny\n\n\n-\ntrue\nnone\nn\n", "--no-wiring")
	if code != 1 || err != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	if strings.Contains(out, "Which model should claude run?") {
		t.Fatalf("asked for a model after sign-in was declined:\n%s", out)
	}
	if !strings.Contains(out, "Step 3 of 3: the check\n  A command that must succeed (exit 0) before a task's work is accepted.\n  Check command:\n") || strings.Contains(out, "  Found ") {
		t.Fatalf("check without a build-file suggestion must have no default:\n%s", out)
	}
	lastQuestion := strings.Index(out, "Worker tools (required;")
	for _, line := range []string{"\nPlan\n", "    codex     CLI default", "  Reviews:", "  Foreman: codex/default", "  Check:", "  In the background:"} {
		if pos := strings.Index(out, line); lastQuestion < 0 || pos <= lastQuestion {
			t.Errorf("plan line %q preceded the last setup question:\n%s", line, out)
		}
	}
}

func TestInitExplicitAccountQuestions(t *testing.T) {
	// Account confirmations stay in CLI order; the new foreman menu retains
	// the first resume-capable --agent (codex here) as its default.
	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	code, out, err := initDo(t, testsupport.ShortStateDir(t), "y\ny\n\nn\n", "--agent", "harness=codex", "--agent", "harness=claude,account=work,classes=review", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || err != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	claude := strings.Index(out, "Are you signed in to claude account work? [y/N]")
	codex := strings.Index(out, "Are you signed in to codex? [y/N]")
	if claude < 0 || codex <= claude || strings.Contains(out, "codex account codex") {
		t.Fatalf("incorrect sign-in question wording or order:\n%s", out)
	}
	if !strings.Contains(out, "  Pick 1-2 [2]:") || !strings.Contains(out, "  Foreman: codex/default\n") {
		t.Fatalf("did not preserve flag-order foreman default:\n%s", out)
	}
	if !strings.Contains(strings.Join(strings.Fields(out), " "), "Anthropic reviews only") || strings.Contains(out, "(implement, review, answer only)") {
		t.Fatalf("incorrect agent classes display:\n%s", out)
	}
}

func TestInitApplyPrompts(t *testing.T) {
	for _, tc := range []struct {
		name, prompt    string
		exists, running bool
	}{
		{"write and start", "Write 1 file and start the runner? [y/N]", false, false},
		{"write", "Write 1 file? [y/N]", false, true},
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
			code, out, err := initDo(t, state, "\nn\n", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || err != "" || !strings.Contains(out, "\n\n"+tc.prompt+"\n") || strings.Contains(out, "\n\n\n"+tc.prompt) || !strings.Contains(out, "Declined") {
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
		{"uv.lock", "", "uv run pytest", "Bash(uv run pytest:*)"},
		{"poetry.lock", "", "poetry run pytest", "Bash(poetry run pytest:*)"},
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

func TestInitBuildSuggestionOrder(t *testing.T) {
	for _, tc := range []struct {
		name, check string
		files       []string
	}{
		{"go first", "go test ./...", []string{"go.mod", "package.json", "Cargo.toml", "uv.lock", "poetry.lock", "pyproject.toml"}},
		{"npm before cargo", "npm test", []string{"package.json", "Cargo.toml", "uv.lock", "poetry.lock"}},
		{"cargo before python", "cargo test", []string{"Cargo.toml", "uv.lock", "poetry.lock"}},
		{"uv before poetry and plain pytest", "uv run pytest", []string{"uv.lock", "poetry.lock", "pyproject.toml", "pytest.ini"}},
		{"poetry before plain pytest", "poetry run pytest", []string{"poetry.lock", "pyproject.toml", "pytest.ini"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range tc.files {
				body := ""
				if name == "package.json" {
					body = `{"scripts":{"test":"exit 99"}}`
				}
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			check, _, err := suggestInitDefaults(root)
			if err != nil || check != tc.check {
				t.Fatalf("got %q, %v; want %q", check, err, tc.check)
			}
		})
	}
}

func TestInitSuggestedCheckQuestion(t *testing.T) {
	// The check now follows a foreman menu and names its build-file source on
	// a separate line; Enter still accepts it and typed commands replace it.
	for _, tc := range []struct {
		name, source, body, answer, check string
	}{
		{"go Enter", "go.mod", "module x", "", "go test ./..."},
		{"npm Enter", "package.json", `{"scripts":{"test":"exit 99"}}`, "", "npm test"},
		{"cargo Enter", "Cargo.toml", "[package]", "", "cargo test"},
		{"uv Enter", "uv.lock", "", "", "uv run pytest"},
		{"poetry Enter", "poetry.lock", "", "", "poetry run pytest"},
		{"pyproject Enter", "pyproject.toml", "[project]", "", "python -m pytest"},
		{"pytest Enter", "pytest.ini", "[pytest]", "", "python -m pytest"},
		{"typed replacement", "uv.lock", "", "uv run pytest -q", "uv run pytest -q"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			if err := os.WriteFile(tc.source, []byte(tc.body), 0644); err != nil {
				t.Fatal(err)
			}
			suggestion, _, err := suggestInitDefaults(".")
			if err != nil {
				t.Fatal(err)
			}
			fakeInitPATH(t, "claude", "codex")
			code, out, errOut := initDo(t, testsupport.ShortStateDir(t), "\n\n\n"+tc.answer+"\nn\n", "--signed-in", "claude,codex", "--no-wiring", "--verbose")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			prompt := "  Found " + tc.source + ", so the suggestion is:\n  Check command [" + suggestion + "]:\n"
			question := strings.Index(out, prompt)
			if question <= strings.Index(out, "  Pick 1-2 [1]:") || question >= strings.Index(out, "\nPlan\n") || strings.Count(out, prompt) != 1 {
				t.Fatalf("check question missing or out of order:\n%s", out)
			}
			for _, want := range []string{
				"Check: " + tc.check + " (init doesn't run it; change it later in .brigada/project.yaml)",
				"default_check: " + strconv.Quote(tc.check),
			} {
				if !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
			if _, err := os.Stat(".brigada"); !os.IsNotExist(err) {
				t.Fatalf("declined init wrote configuration: %v", err)
			}
		})
	}
}

func TestInitSkipsSuggestedCheckQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, existing, check string
		args                  []string
	}{
		{"flag", "", "flag check", []string{"--check", "flag check"}},
		{"project value", "default_check: project check\n", "project check", nil},
		{"flag overrides project", "default_check: project check\n", "flag check", []string{"--check", "flag check"}},
		{"probe", "", "uv run pytest", []string{"--probe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write := importRepo(t)
			if tc.existing != "" {
				write(projectFile(tc.existing))
			}
			if err := os.WriteFile("uv.lock", nil, 0644); err != nil {
				t.Fatal(err)
			}
			fakeInitPATH(t, "claude", "codex")
			args := append([]string{"--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--no-wiring"}, tc.args...)
			code, out, errOut := initDo(t, testsupport.ShortStateDir(t), "\nn\n", args...)
			wantCode := 1
			if tc.name == "probe" {
				wantCode = 0
			}
			if code != wantCode || errOut != "" || strings.Contains(out, "Check command") || !strings.Contains(out, "Check: "+tc.check+" (") {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
		})
	}
}

func TestInitWorkerPermissions(t *testing.T) {
	for _, tc := range []struct{ tools, permissions string }{
		{"", "edit files; run only: uv run pytest"},
		{",tools=none", "edit files; no commands"},
		{",tools=Bash(custom check:*)", "edit files; run only: custom check"},
	} {
		t.Run(tc.permissions, func(t *testing.T) {
			importRepo(t)
			if err := os.WriteFile("uv.lock", nil, 0644); err != nil {
				t.Fatal(err)
			}
			fakeInitPATH(t, initHarnesses...)
			code, out, errOut := initDo(t, testsupport.ShortStateDir(t), "", "--probe",
				"--agent", "harness=claude"+tc.tools, "--agent", "harness=codex", "--agent", "harness=kimi", "--agent", "harness=opencode",
				"--signed-in", "claude,codex,kimi,opencode", "--no-wiring")
			if code != 0 || errOut != "" {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			want := "  What workers may do:\n" +
				"    claude    " + tc.permissions + "\n" +
				"    codex     edit files and run commands inside codex's sandbox\n" +
				"    kimi      edit files and run any command without asking\n" +
				"    opencode  edit files; no commands\n"
			if !strings.Contains(out, want) {
				t.Errorf("permissions table missing:\n%s", out)
			}
			for _, old := range []string{"Default check:", "Worker tools:", "effective worker tools:", "ready"} {
				if strings.Contains(out, old) {
					t.Errorf("unexpected %q in:\n%s", old, out)
				}
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
	if code != 0 || !strings.Contains(out, "\n      --- existing\n") || !strings.Contains(out, "\n      "+`+default_check: "new check" # retain this comment`) {
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
	for _, want := range []string{"Done: wrote ", "files (not committed). Detached runner started (pid ", "stop it with: brigada stop", "Next:\n  1. Open Claude Code here (`claude`)", "  2. Or, without a session: brigada ask", "Stop the runner: brigada stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "§") {
		t.Errorf("output names a design section:\n%s", out)
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
	code, out, err = initDo(t, state, "unknown\nsonnet\ngpt-5\n\ny\n", "--signed-in", "claude,codex", "--no-wiring")
	if code != 0 || strings.Count(out, "    Pick 1-3 or -, or type a model id [1]:") != 2 || !strings.Contains(out, "Runner already running") {
		t.Fatalf("interactive retry: %d\n%s\n%s", code, out, err)
	}
	if strings.Contains(out, "Open Claude Code here") || !strings.Contains(out, "  1. Or, without a session: brigada ask") || strings.Contains(out, "§") {
		t.Fatalf("--no-wiring Next block:\n%s", out)
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
	if code != 0 || !strings.Contains(out, "  claude    edit files; no commands\n") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	args = append(args, "--tools", "Bash(go test:*)")
	code, out, err = initDo(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "    claude    edit files; run only: go test\n") {
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

func TestInitProbeAndRefusalsWithExistingWiring(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		stdin string
		code  int
	}{
		{"probe", []string{"--probe"}, "", 0},
		{"refusal", nil, "\nn\n", 1},
		{"limited", []string{"--probe", "--accept-limited"}, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			wireApply(t, ".")
			local := wireRead(t, ".", ".claude/settings.local.json")
			fakeInitPATH(t, "claude", "codex")
			args := []string{"--agent", "harness=claude,model=sonnet", "--signed-in", "claude,codex", "--check", "true", "--tools", "none"}
			if tc.name != "limited" {
				args = append(args, "--agent", "harness=codex,model=gpt-5")
			}
			args = append(args, tc.extra...)
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, errOut := initDo(t, state, tc.stdin, args...)
			if code != tc.code {
				t.Fatalf("got %d, want %d\n%s\n%s", code, tc.code, out, errOut)
			}
			if strings.Contains(out, "ready") || !strings.Contains(strings.Join(strings.Fields(out), " "), ".claude/settings.local.json unchanged private, kept out of git:") {
				t.Errorf("existing wiring output:\n%s", out)
			}
			if wireRead(t, ".", ".claude/settings.local.json") != local {
				t.Error("probe/refusal changed private settings")
			}
		})
	}
}

// TestInitInteractiveTranscript replaces the probe layout assertions with a
// golden of all three interactive steps, the complete Plan and the refusal.
func TestInitInteractiveTranscript(t *testing.T) {
	want, err := os.ReadFile("testdata/init-interactive.golden")
	if err != nil {
		t.Fatal(err)
	}
	importRepo(t)
	wireGit(t, ".", "config", "core.excludesFile", os.DevNull)
	wireWrite(t, ".", ".gitignore", "/.claude/settings.local.json\n")
	if err := os.WriteFile("uv.lock", nil, 0644); err != nil {
		t.Fatal(err)
	}
	fakeInitPATH(t, initHarnesses...)
	// Keep the HOME-relative state socket below the platform path limit.
	t.Setenv("HOME", testsupport.ShortStateDir(t))
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".codex", "config.toml"), "model = \"gpt-6.1-sol\"\n")
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".kimi-code", "config.toml"), `default_model = "kimi-code/k3"
[models."kimi-code/k3-256k"]
[models."kimi-code/kimi-for-coding"]
[models."kimi-code/kimi-for-coding-highspeed"]
`)
	for _, h := range []string{"claude", "codex"} {
		parent, status := "auth", `{"loggedIn":true}`
		if h == "codex" {
			parent, status = "login", "Logged in"
		}
		script := "#!/bin/sh\ncase \"$*\" in\n --help) echo '  " + parent + "  Authentication' ;;\n '" + parent + " --help') echo '  status  Authentication status' ;;\n '" + parent + " status') echo '" + status + "' ;;\n *) exit 89 ;;\nesac\n"
		if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), h), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	opencode := filepath.Join(os.Getenv("PATH"), "opencode")
	script, err := os.ReadFile(opencode)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.ReplaceAll(script, []byte("fixture/gpt-5"), []byte("zai-coding-plan/glm-5.3"))
	script = bytes.ReplaceAll(script, []byte("fixture/glm-5.3"), []byte("zai-coding-plan/glm-5.2"))
	if err := os.WriteFile(opencode, script, 0755); err != nil {
		t.Fatal(err)
	}
	// Accept defaults, opt in to Kimi but leave it review-only, and pick
	// OpenCode model 1. Decline only the final apply to keep the run read-only.
	state := filepath.Join(os.Getenv("HOME"), ".local", "state", "brigada")
	code, out, errOut := initDo(t, state, "\n\ny\n\nN\n1\n\n\nn\n")
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	if out != string(want) {
		t.Fatalf("interactive transcript:\n%s\nwant:\n%s", out, want)
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) >= 80 {
			t.Errorf("transcript line must be under 80 columns: %q", line)
		}
	}
	if strings.Contains(out, "ready") {
		t.Fatalf("refusal claimed ready:\n%s", out)
	}
	for _, path := range []string{".brigada", ".claude", ".mcp.json", state} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("declined init wrote %s: %v", path, err)
		}
	}
}

func TestInitForemanQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, input, want, err string
		args                   []string
		one                    bool
	}{
		{name: "default", input: "\nn\n", want: "claude/sonnet"},
		{name: "choose codex", input: "2\nn\n", want: "codex/gpt-5"},
		{name: "flag", input: "n\n", want: "codex/gpt-5", args: []string{"--foreman", "codex/gpt-5"}},
		{name: "only planner", input: "n\n", want: "claude/sonnet", one: true},
		{name: "retry", input: "3\n2\nn\n", want: "codex/gpt-5"},
		{name: "retry limit", input: "3\n0\ninvalid\n", err: "--foreman: pick 1-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex", "opencode")
			second := "harness=codex,model=gpt-5"
			if tc.one {
				second = "harness=opencode,model=zai-coding-plan/glm-5.3"
			}
			args := append([]string{"--agent", "harness=claude,model=sonnet", "--agent", second, "--signed-in", "claude,codex,opencode", "--check", "true", "--tools", "none", "--no-wiring"}, tc.args...)
			code, out, errOut := initDo(t, testsupport.ShortStateDir(t), tc.input, args...)
			if code != 1 {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			if tc.err != "" {
				if !strings.Contains(errOut, tc.err) || strings.Contains(out, "\nPlan\n") || strings.Count(out, "  Pick 1-2 [1]:") != 3 {
					t.Fatalf("foreman retry limit: %s\n%s", out, errOut)
				}
				return
			}
			if errOut != "" || !strings.Contains(out, "Declined") || !strings.Contains(out, "  Foreman: "+tc.want+"\n") {
				t.Fatalf("wrong foreman: %s\n%s", out, errOut)
			}
			if tc.one {
				if !strings.Contains(out, "  Foreman: claude/sonnet (the only agent that can plan)") || strings.Contains(out, "  Pick 1-2") {
					t.Fatalf("asked for the only foreman: %s", out)
				}
			} else if tc.name == "flag" && strings.Contains(out, "  Pick 1-2") {
				t.Fatalf("asked for a foreman answered by the flag: %s", out)
			}
		})
	}
}

func TestInitUnconfirmedUseQuestions(t *testing.T) {
	for _, tc := range []struct {
		name, input, signedIn string
		include, confirmed    bool
	}{
		{name: "Enter leaves out", input: "\n\n\n\n\nn\n", signedIn: "claude,codex"},
		{name: "opt in and pick numbers", input: "\n\ny\n1\n\ny\n2\n\nn\n", signedIn: "claude,codex", include: true},
		{name: "flag answers Use", input: "\n\n1\n\n2\n\nn\n", signedIn: "claude,codex,kimi,opencode", include: true, confirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, initHarnesses...)
			// A CLI with models but no documented authentication status remains
			// unconfirmed, so init must ask rather than assume it is signed in.
			script := "#!/bin/sh\ncase \"$*\" in\n --help) echo '  opencode models [provider]' ;;\n models) printf 'zai-coding-plan/glm-5.3\\nzai-coding-plan/glm-5.2\\n' ;;\n *) exit 89 ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "opencode"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := initDo(t, testsupport.ShortStateDir(t), tc.input, "--signed-in", tc.signedIn, "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			for _, h := range []string{"kimi", "opencode"} {
				if strings.Contains(out, "    Use "+h+"? [y/N]:") == tc.confirmed {
					t.Errorf("incorrect Use question for %s:\n%s", h, out)
				}
				if strings.Contains(out, "Which model should "+h+" run?") != tc.include || strings.Contains(out, "    "+h+strings.Repeat(" ", 9-len(h))) != tc.include {
					t.Errorf("incorrect inclusion of %s:\n%s", h, out)
				}
			}
			if !tc.confirmed && !strings.Contains(out, "  opencode: init can't check sign-in. If you're not signed in, run\n  `opencode auth login` first, or leave opencode out.") {
				t.Errorf("missing OpenCode sign-in guidance:\n%s", out)
			}
			if tc.include && !strings.Contains(out, "opencode  zai-coding-plan/glm-5.2") {
				t.Errorf("number did not pick the second OpenCode model:\n%s", out)
			}
		})
	}
}

func TestInitOpenCodeMenuUnknownFamilies(t *testing.T) {
	for _, tc := range []struct {
		name, models, answer, prompt string
	}{
		{"mixed", "opencode/beam-free\\nopencode/claude-sonnet-4\\nopencode/big-pickle\\nopencode/beam-free\\n", "1\n", "Pick 1 or -, or type provider/model [-]:"},
		{"all unknown", "opencode/beam-free\\nopencode/big-pickle\\n", "opencode/claude-sonnet-4\n", "Pick -, or type provider/model [-]:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "codex", "opencode")
			path := filepath.Join(os.Getenv("PATH"), "opencode")
			script, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			script = bytes.ReplaceAll(script, []byte("fixture/gpt-5\\nfixture/glm-5.3\\n"), []byte(tc.models))
			if err := os.WriteFile(path, script, 0755); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := initDo(t, testsupport.ShortStateDir(t), "1\n"+tc.answer+"n\n", "--signed-in", "codex", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			menu := strings.Split(strings.Split(out, "Which model should opencode run?\n")[1], "\nPlan\n")[0]
			if !strings.Contains(menu, "2 more from `opencode models` aren't listed: brigada can't tell their model family, which it needs to pick reviewers.\n") || !strings.Contains(menu, tc.prompt) {
				t.Fatalf("missing omitted count or incorrect prompt:\n%s", menu)
			}
			rows := regexp.MustCompile(`(?m)^\s+\d+  .*`).FindAllString(menu, -1)
			if tc.name == "mixed" {
				if len(rows) != 1 || strings.TrimSpace(rows[0]) != "1  opencode/claude-sonnet-4  Anthropic" {
					t.Fatalf("numbered unknown model or missing family: %q", rows)
				}
			} else if len(rows) != 0 {
				t.Fatalf("numbered unknown models: %q", rows)
			}
			if strings.Contains(menu, "beam-free") || strings.Contains(menu, "big-pickle") {
				t.Fatalf("listed unknown model:\n%s", menu)
			}
		})
	}
}

func TestInitEveryNumberedModelCanBePicked(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, initHarnesses...)
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".codex", "config.toml"), "model = \"gpt-6.1-sol\"\n")
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".kimi-code", "config.toml"), `default_model = "kimi-code/k3"
[models."kimi-code/k3-256k"]
[models."kimi-code/kimi-for-coding"]
[models."kimi-code/kimi-for-coding-highspeed"]
[models."provider/gemini-2.5-pro"]
`)
	args := []string{"--signed-in", strings.Join(initHarnesses, ","), "--foreman", "codex/default", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring"}
	code, out, errOut := initDo(t, testsupport.ShortStateDir(t), "1\n1\n-\n-\nn\n", args...)
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("baseline init = %d\n%s\n%s", code, out, errOut)
	}
	rowsPattern := regexp.MustCompile(`(?m)^\s+(\d+)  (\S+)  (\S+(?: \(GLM\))?)`)
	for _, harness := range initHarnesses {
		menu := strings.Split(strings.Split(out, "Which model should "+harness+" run?\n")[1], "    Pick ")[0]
		rows := rowsPattern.FindAllStringSubmatch(menu, -1)
		if len(rows) == 0 {
			t.Fatalf("no numbered entries for %s:\n%s", harness, menu)
		}
		for _, row := range rows {
			t.Run(harness+"/"+row[1], func(t *testing.T) {
				answers := map[string]string{"claude": "1\n", "codex": "1\n", "kimi": "-\n", "opencode": "-\n"}
				answers[harness] = row[1] + "\n"
				if harness == "kimi" {
					answers[harness] += "n\n" // Keep Kimi review-only.
				}
				var input strings.Builder
				for _, h := range initHarnesses {
					input.WriteString(answers[h])
				}
				input.WriteString("n\n")
				code, out, errOut := initDo(t, testsupport.ShortStateDir(t), input.String(), args...)
				if code != 1 || errOut != "" || !strings.Contains(out, "Declined") || strings.Contains(out, "try again") {
					t.Fatalf("numbered entry %s rejected: %d\n%s\n%s", row[0], code, out, errOut)
				}
				plan := strings.Split(out, "\nPlan\n")[1]
				for _, line := range strings.Split(plan, "\n") {
					if strings.HasPrefix(line, "    "+harness+" ") && strings.Contains(line, row[2]) && strings.Contains(line, row[3]) {
						return
					}
				}
				t.Fatalf("picked entry missing model/family in plan:\n%s", plan)
			})
		}
	}
}
