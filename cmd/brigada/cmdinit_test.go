package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
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
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// Transcript fixtures remain available after CLI tests change the process's
// working directory, including when the whole package runs in one test binary.
//
//go:embed testdata/init-interactive*.golden
var initTranscriptGoldens embed.FS

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
     printf '  opencode models [provider]\n  opencode providers   manage AI providers and credentials   [aliases: auth]\n'
   else
     echo 'Commands: no verified authentication status here'
   fi
   exit 0 ;;
 models) [ "${0##*/}" = opencode ] || exit 89; printf 'fixture/gpt-5\nfixture/glm-5.3\n' ;;
 'providers list') [ "${0##*/}" = opencode ] || exit 89; printf '┌ Credentials\n● Provider one oauth\n● Provider two api\n└ 2 credentials\n' ;;
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

// Tests for the other questions answer concurrency explicitly. Transcript
// tests and TestInitTasksAtOnce exercise the tasks-at-once prompt itself.
func initFixedParallel(t *testing.T, state, stdin string, args ...string) (int, string, string) {
	t.Helper()
	return initDo(t, state, stdin, append([]string{"--parallel", "4"}, args...)...)
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
		{"declined", []string{"--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex"}, "\n\n\nn\n", 1, "Declined"},
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
		{"several claude models", []string{"--probe", "--accept-limited", "--agent", "harness=claude,model=sonnet", "--agent", "harness=claude,model=opus,classes=none", "--signed-in", "claude"}, "", 0, "foreman only"},
		{"duplicate claude model", []string{"--probe", "--agent", "harness=claude", "--agent", "harness=claude,model=sonnet"}, "", 1, "agent claude/sonnet is offered twice: one agent per harness and model per runner"},
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
			writeInitCodexScript(t, "  debug  Diagnostic tools", "  models  Render the raw model catalog as JSON", "      --bundled  Skip refresh", initCodexCatalogCommand, initCodexCatalogCommand)
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, err := initFixedParallel(t, state, tc.stdin, tc.args...)
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
				if !strings.HasSuffix(line, " --help") && line != "opencode models" && line != "opencode models --verbose" && line != "opencode providers list" && line != "codex debug models" && line != "codex debug models --bundled" {
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
	// Enter takes the foreman's first model and effort, uses every model of
	// claude and codex (and of a confirmed kimi, review only), and leaves
	// OpenCode out. "n" leaves a CLI out; a kimi that is not confirmed by
	// --signed-in is left out by Enter.
	for _, tc := range []struct {
		name, stdin, signedIn string
		wantAgents            []string
		wantSkipped           []string
		want                  string
	}{
		{
			name: "blank uses defaults", stdin: "\n\n\n\n\n\nn\n", signedIn: "claude,codex,kimi,opencode",
			wantAgents:  []string{"claude    opus", "claude    sonnet", "claude    haiku", "codex     CLI default", "kimi      CLI default"},
			wantSkipped: []string{"opencode"},
			want:        "Declined",
		},
		{
			name: "n and Enter leave CLIs out", stdin: "4\nn\n\n\n-\nn\n", signedIn: "claude,codex,opencode",
			wantAgents: []string{"codex     CLI default"}, wantSkipped: []string{"claude", "kimi", "opencode"},
			want: "Declined",
		},
		{
			name: "all left out", stdin: "\n\nn\nn\n\n-\n", signedIn: "claude,codex,opencode",
			wantSkipped: initHarnesses, want: "zero usable agents",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, initHarnesses...)
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, err := initFixedParallel(t, state, tc.stdin, "--signed-in", tc.signedIn, "--accept-limited", "--check", "true", "--tools", "none")
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
			// Take the first foreman and effort, leave Claude out, use Codex,
			// answer Kimi's writing question, then type an OpenCode model and a
			// check. Distinct answers catch any question that consumes stdin in
			// the wrong position.
			input := "\n\nn\n\n" + tc.answer + "\nzai-coding-plan/glm-5.3\nprintf check-marker\nn\n"
			code, out, errOut := initFixedParallel(t, state, input, "--signed-in", "claude,codex,kimi,opencode", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			warning := "    kimi has no sandbox: when it writes code it can run any command on this\n    machine without asking. As a reviewer it only reads.\n    Let kimi write code too, not only review? [y/N]:\n"
			if !strings.Contains(out, "    Use codex's models for tasks (CLI default)? [Y/n]:\n\n  kimi: signed in\n"+warning+"\n  opencode:") {
				t.Fatalf("writing question is not right after Kimi sign-in:\n%s", out)
			}
			if strings.Contains(strings.Join(strings.Fields(out), " "), "Moonshot standard reviews only") == tc.write {
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
				if !strings.HasSuffix(line, " --help") && line != "opencode models" && line != "opencode providers list" {
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
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "n\n", "--agent", "harness=codex", "--agent", "harness=kimi"+tc.classes, "--signed-in", "codex,kimi", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			if strings.Contains(out, "Let kimi write") || strings.Contains(out, "kimi has no sandbox:") {
				t.Fatalf("explicit agent got writing question:\n%s", out)
			}
			want := "Moonshot standard " + tc.role
			if !strings.Contains(strings.Join(strings.Fields(out), " "), strings.Join(strings.Fields(want), " ")) {
				t.Errorf("explicit classes changed:\n%s", out)
			}
		})
	}
}

func TestInitNonTerminalPrompts(t *testing.T) {
	// The foreman comes first (model, then effort), then each CLI's use
	// question; confirmed OpenCode authentication skips its Use question and
	// Enter leaves it out. Sign-in the probes cannot check is asked up front.
	importRepo(t)
	wireGit(t, ".", "config", "core.excludesFile", os.DevNull)
	fakeInitPATH(t, initHarnesses...)
	code, out, err := initFixedParallel(t, testsupport.ShortStateDir(t), "y\ny\n\n\n\n\ny\n\n\nn\n", "--check", "true", "--tools", "none")
	if code != 1 || !strings.Contains(out, "Declined") || err != "" {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	prompts := []string{
		"    Are you signed in to claude? [y/N]",
		"    Are you signed in to codex? [y/N]",
		"    Pick 1-4, or type harness/model [1]:",
		"    Pick 1-5 or type a level [1]:",
		"    Use claude's models for tasks (opus, sonnet, haiku)? [Y/n]:",
		"    Use codex's models for tasks (CLI default)? [Y/n]:",
		"    Use kimi? [y/N]:",
		"    Let kimi write code too, not only review? [y/N]:",
		"    Pick numbers (1,3) or -, or type provider/model [-]:",
		"Write 5 files and start the runner? [y/N]",
	}
	if !strings.Contains(out, "\nSigned in: opencode (2 providers, opencode providers list).\nclaude: init can't check sign-in.\ncodex: init can't check sign-in.\nkimi: init can't check sign-in.\n\n") {
		t.Errorf("expected the signed-in line and one line per CLI that cannot be checked:\n%s", out)
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
		"Step 1 of 4: the foreman",
		"  claude: signed in",
		"Step 2 of 4: workers",
		"  kimi: init can't check sign-in. If you're not signed in, run `kimi login`\n  first, or leave kimi out.",
		"  opencode: signed in to 2 providers (opencode providers list)",
		"  Reviews: 3 model families, so every task can get a reviewer of another one.",
		"  Foreman: claude/sonnet, effort medium\n",
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
		{"use input exhausted", "y\ny\n\n\n", "--agent harness=claude", nil},
		{"foreman input exhausted", "y\ny\n", "--foreman", nil},
		{"no authentication input", "", "--signed-in claude", []string{"--agent", "harness=claude"}},
		{"no foreman input", "", "--foreman", []string{"--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex"}},
		{"no apply input", "", "--yes", []string{"--agent", "harness=claude", "--signed-in", "claude", "--accept-limited", "--foreman", "claude/sonnet@medium"}},
		{"no check input", "", "--check COMMAND", []string{"--agent", "harness=claude", "--signed-in", "claude", "--accept-limited", "--foreman", "claude/sonnet@medium", "--check="}},
		// A command that cannot be scoped gets file tools only and reaches
		// the apply prompt, without asking a separate tools question.
		{"unscopable check reaches apply", "", "--yes", []string{"--agent", "harness=claude", "--signed-in", "claude", "--accept-limited", "--foreman", "claude/sonnet@medium", "--check", "a+b", "--tools="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			args := append([]string{"--check", "true", "--tools", "none"}, tc.args...)
			code, out, err := initFixedParallel(t, testsupport.ShortStateDir(t), tc.stdin, args...)
			if code != 1 {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
			}
			for _, want := range []string{"no input available", "--agent", "--signed-in", tc.want} {
				if !strings.Contains(err, want) {
					t.Errorf("missing %q in %q", want, err)
				}
			}
			if tc.name == "unscopable check reaches apply" && (!strings.Contains(out, "\nPlan\n") || !strings.Contains(out, "claude    edit files; no commands") || strings.Contains(out, "Worker tools")) {
				t.Fatalf("unscopable command must reach apply with file tools only:\n%s", out)
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
	// Invalid typed answers retain the three-try limit, for the foreman's
	// harness/model and for OpenCode's provider/model; numbers select menu
	// entries.
	for _, tc := range []struct {
		name, harness, answers, reason, model string
		tries                                 int
	}{
		{"unknown then valid", "claude", "claude/unknown\n1\n", "cannot identify the family", "sonnet", 2},
		{"whitespace then valid", "claude", "claude/sonnet other\nclaude/sonnet\n", "model must be a nonempty model identifier", "sonnet", 2},
		{"empty model then valid", "claude", "claude/\nclaude/sonnet\n", "model must be a nonempty model identifier", "sonnet", 2},
		{"no slash then valid", "claude", "sonnet\nclaude/sonnet\n", "\"sonnet\" is neither a number from the menu nor harness/model", "sonnet", 2},
		{"kimi cannot plan", "claude", "kimi/k3\nclaude/sonnet\n", "kimi/k3 is not a model of a signed-in claude or codex", "sonnet", 2},
		{"provider then valid", "opencode", "glm\nzai-coding-plan/glm-5.3\n", "opencode needs provider/model, e.g. zai-coding-plan/glm-5.3", "zai-coding-plan/glm-5.3", 2},
		{"unknown provider model then valid", "opencode", "opencode/beam-free\n1\n", "cannot identify the family", "fixture/gpt-5", 2},
		{"retry then default", "claude", "claude/unknown\n\n", "cannot identify the family", "sonnet", 2},
		{"third answer valid", "claude", "claude/unknown\nclaude/sonnet other\nclaude/sonnet\n", "cannot identify the family", "sonnet", 3},
		{"three invalid answers", "claude", "claude/unknown\nclaude/unknown\nclaude/unknown\n", "cannot identify the family", "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			calls := fakeInitPATH(t, "claude", "codex", "opencode")
			// After the foreman's model: its effort, use claude, not codex,
			// no OpenCode model, and decline the plan.
			answers := tc.answers + "\n\nn\n\nn\n"
			prompt, hint := "    Pick 1-4, or type harness/model [1]:", "pick a numbered entry, or type harness/model"
			if tc.harness == "opencode" {
				answers = "\n\nn\nn\n" + tc.answers + "n\n"
				prompt, hint = "    Pick numbers (1,3) or -, or type provider/model [-]:", "pick a numbered entry, or type a model whose name says its family"
			}
			state := filepath.Join(testsupport.ShortStateDir(t), "absent")
			code, out, err := initFixedParallel(t, state, answers, "--signed-in", "claude,codex,opencode", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || strings.Count(out, prompt) != tc.tries {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
			}
			if !strings.Contains(out, "\n  "+tc.reason) || !strings.Contains(out, "; try again.\n") || !strings.Contains(out, hint) {
				t.Fatalf("missing indented retry reason:\n%s", out)
			}
			if tc.name == "three invalid answers" {
				if !strings.Contains(err, "--foreman HARNESS/MODEL: claude/unknown") && !strings.Contains(err, "--foreman HARNESS/MODEL: "+tc.reason) || !strings.Contains(err, "or type harness/model") || strings.Contains(out, "\nPlan\n") {
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
			}
			for _, path := range []string{state, ".brigada"} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("model retry/refusal wrote %s: %v", path, err)
				}
			}
			data, _ := os.ReadFile(calls)
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.HasSuffix(line, " --help") && line != "opencode models" && line != "opencode providers list" {
					t.Errorf("unexpected harness execution: %s", line)
				}
			}
		})
	}
}

func TestInitQuestionsBeforePlan(t *testing.T) {
	// Claude is declined before its model menu; OpenCode is confirmed by
	// its probe and skipped in its menu. The check still precedes the Plan.
	// Match the complete Plan foreman line to avoid the Step 2 notice.
	// Changed: a typed check now sets the worker tools itself, so no "Worker
	// tools" question follows it and the check is the last question asked.
	importRepo(t)
	fakeInitPATH(t, initHarnesses...)
	code, out, err := initFixedParallel(t, testsupport.ShortStateDir(t), "n\ny\n\ny\n\n\ntrue\nn\n", "--no-wiring")
	if code != 1 || err != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	if strings.Contains(out, "Use claude's models") || !strings.Contains(out, "Foreman: codex/CLI default (the only model that can plan)") {
		t.Fatalf("offered claude after sign-in was declined:\n%s", out)
	}
	if !strings.Contains(out, "Step 4 of 4: how brigada tests the work\n") || !strings.Contains(out, "\n  Found in this repo: nothing brigada recognises.\n     -  no check  work goes straight to review; add one later  default\n  Type a command, or - for no check [-]:\n") || strings.Contains(out, "Check command [") {
		t.Fatalf("check without a candidate must default to no check:\n%s", out)
	}
	if !strings.Contains(out, "  Check: true (") || strings.Contains(out, "Worker tools") {
		t.Fatalf("typed check must set its own worker tools:\n%s", out)
	}
	lastQuestion := strings.Index(out, "  Type a command, or - for no check [-]:")
	for _, line := range []string{"\nPlan\n", "    codex     CLI default", "  Reviews:", "  Foreman: codex/CLI default\n", "  Check:", "  In the background:"} {
		if pos := strings.Index(out, line); lastQuestion < 0 || pos <= lastQuestion {
			t.Errorf("plan line %q preceded the last setup question:\n%s", line, out)
		}
	}
}

func TestInitExplicitAccountQuestions(t *testing.T) {
	// Account confirmations stay in CLI order; the foreman menu then lists the
	// models of both signed-in CLIs, whatever --agent offers.
	importRepo(t)
	fakeInitPATH(t, "claude", "codex")
	code, out, err := initFixedParallel(t, testsupport.ShortStateDir(t), "y\ny\n\n\nn\n", "--agent", "harness=codex", "--agent", "harness=claude,account=work,classes=review", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || err != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, err)
	}
	claude := strings.Index(out, "Are you signed in to claude account work? [y/N]")
	codex := strings.Index(out, "Are you signed in to codex? [y/N]")
	if claude < 0 || codex <= claude || strings.Contains(out, "codex account codex") {
		t.Fatalf("incorrect sign-in question wording or order:\n%s", out)
	}
	if !strings.Contains(out, "Pick 1-4, or type harness/model [1]:") || !strings.Contains(out, "  Foreman: claude/sonnet, effort medium\n") {
		t.Fatalf("foreman menu did not default to the first claude model:\n%s", out)
	}
	if !strings.Contains(strings.Join(strings.Fields(out), " "), "Anthropic standard reviews only") || strings.Contains(out, "(implement, review, answer only)") {
		t.Fatalf("incorrect agent classes display:\n%s", out)
	}
}

func TestInitApplyPrompts(t *testing.T) {
	for _, tc := range []struct {
		name, prompt             string
		exists, running, differs bool
	}{
		{"write and start", "Write 1 file and start the runner? [y/N]", false, false, false},
		{"write", "Write 1 file? [y/N]", false, true, false},
		{"start", "Start the runner? [y/N]", true, false, false},
		{"register", "Register the project? [y/N]", true, true, false},
		{"restart", "Write 1 file and restart the runner with the new agents? [y/N]", false, true, true},
		{"restart only", "Write 0 files and restart the runner with the new agents? [y/N]", true, true, true},
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
				if err := runnerstate.PublishParallel(filepath.Join(state, "runner"), nil, 4); err != nil {
					t.Fatal(err)
				}
				if tc.differs {
					if err := runnerstate.PublishParallel(filepath.Join(state, "runner"),
						nil, 4, "harness=claude,model=opus"); err != nil {
						t.Fatal(err)
					}
				}
			}
			code, out, err := initFixedParallel(t, state, "\n\nn\n", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || err != "" || !strings.Contains(out, "\n\n"+tc.prompt+"\n") || strings.Contains(out, "\n\n\n"+tc.prompt) || !strings.Contains(out, "Declined") {
				t.Fatalf("got %d\n%s\n%s", code, out, err)
			}
			if _, err := os.Stat(filepath.Join(state, "runner", "agents")); !os.IsNotExist(err) {
				t.Fatalf("refusal saved choices: %v", err)
			}
		})
	}
}

// firstCheckCandidate is the entry init offers as 1 for a repository.
func firstCheckCandidate(t *testing.T, root string) (check, tools string) {
	t.Helper()
	candidates, err := checkCandidates(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 {
		return "", ""
	}
	return candidates[0].Command, candidates[0].Tools
}

// TestInitBuildSuggestions keeps its purpose: each build file yields the
// check and tools init suggests, and no script is ever run. Changed: the
// suggestion is now the first of checkCandidates, not suggestInitDefaults.
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
			check, tools := firstCheckCandidate(t, root)
			if check != tc.check || tools != tc.tools {
				t.Fatalf("got %q %q", check, tools)
			}
			if _, err := os.Stat(filepath.Join(root, "should-never-exist")); !os.IsNotExist(err) {
				t.Fatal("executed test script")
			}
		})
	}
}

// TestInitBuildSuggestionOrder keeps its purpose: the best file wins entry 1.
// Changed: a package.json test script is now listed ahead of go.mod's go
// test (checkCandidates puts the repo's own scripts before language
// defaults), so "go first" became "npm script before go".
func TestInitBuildSuggestionOrder(t *testing.T) {
	for _, tc := range []struct {
		name, check string
		files       []string
	}{
		{"npm script before go", "npm test", []string{"go.mod", "package.json", "Cargo.toml", "uv.lock", "poetry.lock", "pyproject.toml"}},
		{"go before cargo", "go test ./...", []string{"go.mod", "Cargo.toml", "uv.lock", "poetry.lock", "pyproject.toml"}},
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
			if check, _ := firstCheckCandidate(t, root); check != tc.check {
				t.Fatalf("got %q; want %q", check, tc.check)
			}
		})
	}
}

// TestInitSuggestedCheckQuestion keeps its purpose: the check is asked after
// the foreman menu, names where each suggestion came from, Enter accepts the
// first and a typed command replaces it. Changed: the suggestion is now menu
// entry 1 ("Found in this repo:" with a "Pick 1 or -, or type a command [1]:"
// prompt, not "Check command [x]:"), and the worker tools follow the choice:
// the candidate's own, or Bash(<typed command>:*) for a typed one.
func TestInitSuggestedCheckQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, source, body, answer, check, tools string
	}{
		{"go Enter", "go.mod", "module x", "", "go test ./...", "Bash(go build:*)+Bash(go test:*)+Bash(go vet:*)+Bash(gofmt:*)"},
		{"npm Enter", "package.json", `{"scripts":{"test":"exit 99"}}`, "", "npm test", "Bash(npm test:*)"},
		{"cargo Enter", "Cargo.toml", "[package]", "", "cargo test", "Bash(cargo test:*)"},
		{"uv Enter", "uv.lock", "", "", "uv run pytest", "Bash(uv run pytest:*)"},
		{"poetry Enter", "poetry.lock", "", "", "poetry run pytest", "Bash(poetry run pytest:*)"},
		{"pyproject Enter", "pyproject.toml", "[project]", "", "python -m pytest", "Bash(python -m pytest:*)"},
		{"pytest Enter", "pytest.ini", "[pytest]", "", "python -m pytest", "Bash(python -m pytest:*)"},
		{"pick 1", "uv.lock", "", "1", "uv run pytest", "Bash(uv run pytest:*)"},
		{"typed replacement", "uv.lock", "", "uv run pytest -q", "uv run pytest -q", "Bash(uv run pytest -q:*)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			if err := os.WriteFile(tc.source, []byte(tc.body), 0644); err != nil {
				t.Fatal(err)
			}
			candidates, err := checkCandidates(".")
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates = %+v, %v", candidates, err)
			}
			fakeInitPATH(t, "claude", "codex")
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n\n\n"+tc.answer+"\nn\n", "--signed-in", "claude,codex", "--no-wiring", "--verbose")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			prompt := "  Found in this repo:\n     1  " + candidates[0].Command + "  " + candidates[0].Note + "  default\n     -  no check" + strings.Repeat(" ", len(candidates[0].Command)-len("no check")+2) + "work goes straight to review; add one later\n  Pick 1 or -, or type a command [1]:\n"
			question := strings.Index(out, prompt)
			if question <= strings.Index(out, "  Pick 1-2 [1]:") || question >= strings.Index(out, "\nPlan\n") || strings.Count(out, prompt) != 1 {
				t.Fatalf("check question missing or out of order:\n%s", out)
			}
			for _, want := range []string{
				"Check: " + tc.check + " (init doesn't run it; change it later in .brigada/project.yaml)",
				"default_check: " + strconv.Quote(tc.check),
				"claude edit files; run only: " + initToolCommands(tc.tools),
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

// TestInitSkipsSuggestedCheckQuestion keeps its purpose: the check question
// is not asked when --check or a non-interactive run settles it. Changed: an
// existing project.yaml no longer skips the question (see
// TestInitRerunShowsCurrentCheckFirst), so the "project value" case is gone.
func TestInitSkipsSuggestedCheckQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, existing, check string
		args                  []string
	}{
		{"flag", "", "flag check", []string{"--check", "flag check"}},
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
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n\nn\n", args...)
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

const initCheckMakefile = "check: lint format-check type test rig-spec\n\ntest:\n\tuv run pytest -q\n"

// initCheckStep runs an interactive init in a repository whose Makefile
// defines check and test and which has a uv.lock, answering the foreman's
// model and effort with Enter, then the check step with answer, then
// declining the apply.
func initCheckStep(t *testing.T, answer string, existing string, args ...string) (int, string, string) {
	t.Helper()
	write := importRepo(t)
	if existing != "" {
		write(projectFile(existing))
	}
	for name, body := range map[string]string{"Makefile": initCheckMakefile, "uv.lock": ""} {
		if err := os.WriteFile(name, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	fakeInitPATH(t, "claude", "codex")
	args = append([]string{"--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--no-wiring", "--verbose"}, args...)
	return initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n"+answer+"\nn\n", args...)
}

func TestInitCheckStepTranscript(t *testing.T) {
	code, out, errOut := initCheckStep(t, "", "")
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	want := `Step 4 of 4: how brigada tests the work
  When an agent finishes a task, brigada runs one command in that task's
  own copy of the repo (never in your working copy):
    passes (exit 0)  the work goes on to review by another model family
    fails            the work goes back to the agent, with the output
  Use the command you run yourself to be sure nothing broke: tests,
  lint, type checks. Claude workers may run it too before handing in.

  Found in this repo:
     1  make check     Makefile: lint format-check type test rig-spec  default
     2  make test      Makefile: uv run pytest -q
     3  uv run pytest  pytest, from uv.lock
     -  no check       work goes straight to review; add one later
  Pick 1-3 or -, or type a command [1]:

  -> change later: edit default_check in .brigada/project.yaml, then run
     brigada project import
`
	if !strings.Contains(out, want) {
		t.Fatalf("step 3 transcript differs from the approved one:\n%s", out)
	}
	for _, want := range []string{
		"\n  -> change later: brigada project foreman --harness H --model M --effort E, or\n     run brigada init again\n\nStep 2 of 4: workers\n  Set by your --agent flags.\n\nStep 3 of 4:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Every line before the Plan stays under 80 columns, notes truncated.
	before, _, _ := strings.Cut(out, "\nPlan\n")
	for _, line := range strings.Split(before, "\n") {
		if n := utf8.RuneCountInString(line); n >= 80 {
			t.Errorf("%d columns: %q", n, line)
		}
	}
	if strings.Contains(before, "ready") {
		t.Errorf("claimed ready before the apply:\n%s", before)
	}
}

func TestInitCheckStepLongNoteStaysNarrow(t *testing.T) {
	importRepo(t)
	long := "check:" + strings.Repeat(" a-very-long-prerequisite", 6) + "\n"
	if err := os.WriteFile("Makefile", []byte(long), 0644); err != nil {
		t.Fatal(err)
	}
	fakeInitPATH(t, "claude", "codex")
	code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n\nn\n", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--no-wiring")
	if code != 1 || errOut != "" || !strings.Contains(out, "     1  make check  Makefile: a-very-long-prerequisite") || !strings.Contains(out, "…  default\n") {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	before, _, _ := strings.Cut(out, "\nPlan\n")
	for _, line := range strings.Split(before, "\n") {
		if n := utf8.RuneCountInString(line); n >= 80 {
			t.Errorf("%d columns: %q", n, line)
		}
	}
}

func TestInitCheckStepAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, answer, check, tools string
	}{
		{"Enter picks make check", "", "make check", "make check"},
		{"1 picks make check", "1", "make check", "make check"},
		{"2 picks make test", "2", "make test", "make test"},
		{"3 picks uv run pytest", "3", "uv run pytest", "uv run pytest"},
		{"typed command sets its tools", "make lint", "make lint", "make lint"},
		{"whitespace around a typed command is dropped", "  make lint  ", "make lint", "make lint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := initCheckStep(t, tc.answer, "")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			flat := strings.Join(strings.Fields(out), " ")
			for _, want := range []string{
				"Check: " + tc.check + " (init doesn't run it",
				"default_check: " + strconv.Quote(tc.check),
				"claude edit files; run only: " + tc.tools + " codex",
			} {
				if !strings.Contains(flat, want) {
					t.Errorf("missing %q in:\n%s", want, out)
				}
			}
		})
	}
}

func TestInitCheckStepAsksAgain(t *testing.T) {
	// A number outside the menu is not a command: it is asked again, and
	// Enter then takes entry 1.
	code, out, errOut := initCheckStep(t, "9\n\n", "")
	if code != 1 || errOut != "" || strings.Count(out, "  Pick 1-3 or -, or type a command [1]:\n") != 2 ||
		!strings.Contains(out, "  pick 1-3 or - or type a command; try again.\n") || !strings.Contains(out, "  Check: make check (") {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
}

func TestInitCheckStepNoCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, answer, check, permissions string
		asked                            int
		ok                               bool
	}{
		{"Enter", "", "none (work goes straight to review)", "edit files; no commands", 1, true},
		{"blank Enter", "   ", "none (work goes straight to review)", "edit files; no commands", 1, true},
		{"dash", "-", "none (work goes straight to review)", "edit files; no commands", 1, true},
		{"typed on third try", "1\n2\ntrue", "true (", "edit files; run only: true", 3, true},
		{"three invalid numbers", "1\n2\n3", "", "", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n"+tc.answer+"\nn\n", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--no-wiring", "--verbose")
			prompt := "  Type a command, or - for no check [-]:\n"
			if !strings.Contains(out, "\n  Found in this repo: nothing brigada recognises.\n     -  no check  work goes straight to review; add one later  default\n"+prompt) || strings.Count(out, prompt) != tc.asked {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			if tc.ok {
				if code != 1 || errOut != "" || !strings.Contains(out, "Declined") || !strings.Contains(out, "  Check: "+tc.check) || !strings.Contains(out, "    claude    "+tc.permissions+"\n") {
					t.Fatalf("got %d\n%s\n%s", code, out, errOut)
				}
				if strings.HasPrefix(tc.check, "none ") && strings.Contains(out, "default_check:") {
					t.Fatalf("no check must omit the key:\n%s", out)
				}
			} else if code != 1 || !strings.Contains(errOut, "--check") || strings.Contains(out, "\nPlan\n") {
				t.Fatalf("exhausted answers must refuse with --check: %d\n%s\n%s", code, out, errOut)
			}
			if strings.Contains(out, "ready") || strings.Contains(out, "Worker tools") {
				t.Fatalf("premature readiness or obsolete question:\n%s", out)
			}
			for _, line := range strings.Split(out, "\n") {
				if term.VisibleWidth(line) >= 80 {
					t.Errorf("line reaches 80 columns: %q", line)
				}
			}
			if _, err := os.Stat(".brigada"); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote configuration: %v", err)
			}
		})
	}
}

func TestInitRerunShowsCurrentCheckFirst(t *testing.T) {
	// A rerun with an existing project.yaml asks the step: the current check
	// is entry 1, the other candidates follow, Enter keeps it and its tools.
	existing := "default_check: custom check\nworker_tools: none\n"
	code, out, errOut := initCheckStep(t, "", existing)
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"     1  custom check   from .brigada/project.yaml  current\n     2  make check     Makefile: lint",
		"     4  uv run pytest  pytest, from uv.lock\n     -  no check       work goes straight to review; add one later\n  Pick 1-4 or -, or type a command [1]:\n",
		"  Check: custom check (",
		"    claude    edit files; no commands\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Picking another entry moves the tools with it.
	code, out, errOut = initCheckStep(t, "2", existing)
	flat := strings.Join(strings.Fields(out), " ")
	if code != 1 || errOut != "" || !strings.Contains(flat, "Check: make check (") || !strings.Contains(flat, "claude edit files; run only: make check codex") {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	// A current check that is also a candidate is listed once, as current.
	_, out, _ = initCheckStep(t, "", "default_check: make check\nworker_tools: Bash(make check:*)\n")
	if strings.Count(out, "make check  ") != 1 || !strings.Contains(out, "     1  make check     from .brigada/project.yaml  current\n     2  make test ") {
		t.Fatalf("current check not deduplicated:\n%s", out)
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
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "", "--probe",
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
	code, out, err := initFixedParallel(t, testsupport.ShortStateDir(t), "", args...)
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
		if !p.signedIn || !p.checked || p.status != "signed in" || !strings.Contains(p.evidence, "documented by --help") {
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
		if p := probeHarness(context.Background(), h); p.signedIn || !p.checked || p.status != "not signed in" {
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
	code, out, err := initFixedParallel(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "Detached runner started") ||
		!strings.Contains(strings.Join(strings.Fields(out), " "), "Setup ready") {
		log, _ := os.ReadFile(filepath.Join(state, "engine.log"))
		t.Fatalf("%d\n%s\n%s\nengine log: %s", code, out, err, log)
	}
	for _, want := range []string{"Done: wrote ", "files (not committed). Detached runner started", "stop it with: brigada stop", "Next:\n  1. Open Claude Code here (`claude`)", "  2. Or, without a session: brigada ask", "Stop the runner: brigada stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if utf8.RuneCountInString(line) >= 80 {
			t.Errorf("apply line must be under 80 columns: %q", line)
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
	assertInitSavedAgents(t, state, rec.Specs)
	code, out, err = initFixedParallel(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "Runner already running") || !strings.Contains(out, "agents match") {
		t.Fatalf("repeat: %d\n%s\n%s", code, out, err)
	}
	again, live, e := runnerstate.Running(filepath.Join(state, "runner"))
	if e != nil || !live || again.PID != rec.PID {
		t.Fatalf("unchanged runner replaced: %+v %v", again, e)
	}
	args[2] = "harness=claude,model=opus,account=work,effort=high"
	// Refusing a restart leaves both live and durable choices alone.
	interactive := append([]string{}, args[1:]...)
	// Changed: the rerun now asks the check step too (Enter keeps the current
	// check), so each answer list has one more Enter before the apply answer.
	// The foreman is the new --agent's opus (the saved one, sonnet, is not).
	code, out, err = initFixedParallel(t, state, "2\n\n\nn\n", interactive...)
	if code != 1 || err != "" || !strings.Contains(out,
		"Write 0 files and restart the runner with the new agents? [y/N]") {
		t.Fatalf("restart prompt: %d\n%s\n%s", code, out, err)
	}
	assertInitSavedAgents(t, state, rec.Specs)
	// Confirm the same prompt and exercise stop's real SIGTERM/lock wait.
	code, out, err = initFixedParallel(t, state, "2\n\n\ny\n", interactive...)
	if code != 0 || !strings.Contains(out, "Runner restarted with the new agents") ||
		strings.Contains(out, "Detached runner started") {
		t.Fatalf("differing agents: %d\n%s\n%s", code, out, err)
	}
	again, live, e = runnerstate.Running(filepath.Join(state, "runner"))
	if e != nil || !live || again.PID == rec.PID ||
		again.Agents[0].Model != "opus" {
		t.Fatalf("runner not replaced: %+v %v", again, e)
	}
	assertInitSavedAgents(t, state, again.Specs)
	// A rerun without --agent defaults to the saved foreman (opus, entry 2) and
	// offers every model of the CLIs it used: that differs from the one model
	// each --agent named, so declining the restart keeps the runner.
	code, out, err = initFixedParallel(t, state, "unknown\n\n\n\n\n\nn\n", "--signed-in", "claude,codex", "--no-wiring")
	if code != 1 || err != "" || strings.Count(out, "    Pick 1-4, or type harness/model [2]:") != 2 || !strings.Contains(out, "restart the runner with the new agents? [y/N]") {
		t.Fatalf("interactive retry: %d\n%s\n%s", code, out, err)
	}
	afterDeclined, live, e := runnerstate.Running(filepath.Join(state, "runner"))
	if e != nil || !live || afterDeclined.PID != again.PID {
		t.Fatalf("declining the restart replaced runner: %+v %v",
			afterDeclined, e)
	}
	assertInitSavedAgents(t, state, again.Specs)
	code, out, err = initFixedParallel(t, state, "unknown\n\n\n\n\n\ny\n", "--signed-in", "claude,codex", "--no-wiring")
	if code != 0 || err != "" || !strings.Contains(out, "Runner restarted with the new agents") {
		t.Fatalf("accepting the rerun defaults: %d\n%s\n%s", code, out, err)
	}
	afterDefaults, live, e := runnerstate.Running(filepath.Join(state, "runner"))
	if e != nil || !live || afterDefaults.PID == again.PID || len(afterDefaults.Agents) != 4 {
		t.Fatalf("rerun defaults must offer every model: %+v %v", afterDefaults, e)
	}
	assertInitSavedAgents(t, state, afterDefaults.Specs)
	again = afterDefaults
	if strings.Contains(out, "Open Claude Code here") || !strings.Contains(out, "  1. Or, without a session: brigada ask") || strings.Contains(out, "§") {
		t.Fatalf("--no-wiring Next block:\n%s", out)
	}
	args[2] = "harness=claude,model=opus,account=work,effort=low"
	code, out, err = initFixedParallel(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "Runner restarted with the new agents") {
		t.Fatalf("--yes restart: %d\n%s\n%s", code, out, err)
	}
	afterYes, live, e := runnerstate.Running(filepath.Join(state, "runner"))
	if e != nil || !live || afterYes.PID == again.PID ||
		afterYes.Agents[0].Effort != "low" {
		t.Fatalf("--yes did not apply changed effort: %+v %v", afterYes, e)
	}
	assertInitSavedAgents(t, state, afterYes.Specs)
	data, _ := os.ReadFile(calls)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasSuffix(line, " --help") {
			t.Errorf("unexpected harness execution: %s", line)
		}
	}
}

func TestInitAgentsDiffer(t *testing.T) {
	const base = "harness=claude,model=sonnet"
	var original agentSpecs
	if err := original.Set(base); err != nil {
		t.Fatal(err)
	}
	agents := []api.Agent{{Harness: "claude", Model: "sonnet"}}
	rec := runnerstate.Record{Agents: agents, MaxParallel: 4,
		Specs: runnerSpecStrings(original.specs)}
	for _, suffix := range []string{
		"", ",effort=high", ",account=work", ",classes=review",
		",tools=none", ",agent-file=/tmp/agent.yaml",
	} {
		t.Run(suffix, func(t *testing.T) {
			var chosen agentSpecs
			if err := chosen.Set(base + suffix); err != nil {
				t.Fatal(err)
			}
			if got := initAgentsDiffer(rec, agents, chosen.specs, 4); got != (suffix != "") {
				t.Fatalf("agents differ = %v", got)
			}
		})
	}
	if !initAgentsDiffer(rec, agents, original.specs, 3) {
		t.Fatal("different parallel limit matched")
	}
	legacy := runnerstate.Record{Agents: agents}
	if initAgentsDiffer(legacy, agents, original.specs, 0) {
		t.Fatal("matching legacy advertisements differ")
	}
	different := []api.Agent{{Harness: "claude", Model: "opus"}}
	if !initAgentsDiffer(legacy, different, original.specs, 0) {
		t.Fatal("different legacy advertisements match")
	}
	if initAgentsDiffer(runnerstate.Record{}, agents, original.specs, 0) {
		t.Fatal("missing metadata treated as a known difference")
	}
}

func assertInitSavedAgents(t *testing.T, state string, specs []string) {
	t.Helper()
	file := filepath.Join(state, "runner", "agents")
	data, err := os.ReadFile(file)
	if err != nil || string(data) != strings.Join(specs, "\n")+"\n" {
		t.Fatalf("saved agents: %q, %v; want %v", data, err, specs)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("saved agents mode: %v, %v", info, err)
	}
}

func TestInitSavedDefaults(t *testing.T) {
	for _, writes := range []bool{false, true} {
		t.Run(fmt.Sprintf("kimi writes %v", writes), func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, initHarnesses...)
			state := testsupport.ShortStateDir(t)
			classes := "review"
			if writes {
				classes = "implement+review+answer"
			}
			specs := []string{
				"harness=claude,model=opus,effort=high,tools=none",
				"harness=kimi,model=default,classes=" + classes,
			}
			if err := runnerstate.SaveAgents(
				filepath.Join(state, "runner"), specs); err != nil {
				t.Fatal(err)
			}
			if err := runnerstate.SaveForeman(
				filepath.Join(state, "runner"), "claude/opus@high"); err != nil {
				t.Fatal(err)
			}
			code, out, errs := initFixedParallel(t, state, strings.Repeat("\n", 6)+"n\n",
				"--signed-in", strings.Join(initHarnesses, ","),
				"--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errs != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("saved defaults: %d\n%s\n%s", code, out, errs)
			}
			for _, want := range []string{
				"current", "Pick 1-4, or type harness/model [2]:",
				"Pick 1-5 or type a level [3]:", "high    current",
				"Use claude's models for tasks (opus, sonnet, haiku)? [Y/n]:",
				"Use codex's models for tasks (CLI default)? [y/N]:",
				"Pick numbers (1,3) or -, or type provider/model [-]:",
				"Foreman: claude/opus, effort high", "opus @high",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
			question := "Let kimi write code too, not only review? [y/N]:"
			role := "reviews only"
			if writes {
				question = "Let kimi write code too, not only review? [Y/n]:"
				role = "writes code, reviews"
			}
			plan := strings.Split(out, "\nPlan\n")[1]
			kimi := strings.Split(strings.Split(plan, "    kimi ")[1], "\n")[0]
			if !strings.Contains(out, question) || !strings.Contains(kimi, role) {
				t.Fatalf("kimi default lost:\n%s", out)
			}
			assertInitEffortOutput(t, out)
			assertInitSavedAgents(t, state, specs)
			if _, err := os.Stat(engine.ProjectConfigPath); !os.IsNotExist(err) {
				t.Fatalf("refused init wrote project configuration: %v", err)
			}
		})
	}
}

func TestInitSavedDefaultsOmitKimi(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, initHarnesses...)
	state := testsupport.ShortStateDir(t)
	specs := []string{
		"harness=claude,model=sonnet,tools=none",
		"harness=codex,model=default",
	}
	if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), specs); err != nil {
		t.Fatal(err)
	}
	code, out, errs := initFixedParallel(t, state, strings.Repeat("\n", 6)+"n\n",
		"--signed-in", "claude,codex,opencode",
		"--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || errs != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("saved defaults: %d\n%s\n%s", code, out, errs)
	}
	kimi := strings.Split(strings.Split(out, "kimi: init can't check")[2], "opencode:")[0]
	if !strings.Contains(kimi, "Use kimi? [y/N]:") ||
		strings.Contains(kimi, "Let kimi write code") ||
		strings.Contains(out, "    kimi ") {
		t.Fatalf("omitted kimi was offered as a default:\n%s", out)
	}
	assertInitSavedAgents(t, state, specs)
}

// TestInitKeepsMachineToolScope keeps its purpose: a machine-level
// tools.claude/<model> entry wins over the project's suggested tools, and
// an explicit --tools wins over both. Changed: the suggested tools are now
// the first check candidate's (go.mod's go test here) rather than a
// separate suggestion; the outcome is the same.
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
	code, out, err := initFixedParallel(t, state, "", args...)
	if code != 0 || !strings.Contains(out, "  claude    edit files; no commands\n") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	args = append(args, "--tools", "Bash(go test:*)")
	code, out, err = initFixedParallel(t, state, "", args...)
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
	code, out, err := initFixedParallel(t, state, "", "--yes", "--accept-limited")
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
		{"refusal", nil, "\n\nn\n", 1},
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
			code, out, errOut := initFixedParallel(t, state, tc.stdin, args...)
			if code != tc.code {
				t.Fatalf("got %d, want %d\n%s\n%s", code, tc.code, out, errOut)
			}
			if strings.Contains(out, "ready") || !strings.Contains(strings.Join(strings.Fields(out), " "), "unchanged .claude/settings.local.json private, kept out of git:") {
				t.Errorf("existing wiring output:\n%s", out)
			}
			if wireRead(t, ".", ".claude/settings.local.json") != local {
				t.Error("probe/refusal changed private settings")
			}
		})
	}
}

// TestInitInteractiveTranscript replaces the probe layout assertions with a
// golden of all four interactive steps, the complete Plan and the refusal.
// Plain wording stays unchanged apart from tags moving to row ends, the
// merged change-later lines, and the Plan's layout: role phrases ("code +
// review", "review only"), no "(default)" after a model, file rows that start
// with their state, no final "To change later:" block, and a dim line under
// "In the background:".
func TestInitInteractiveTranscript(t *testing.T) {
	want, err := initTranscriptGoldens.ReadFile("testdata/init-interactive.golden")
	if err != nil {
		t.Fatal(err)
	}
	out := initInteractiveTranscript(t, false)
	if out != string(want) {
		t.Fatalf("interactive transcript:\n%s\nwant:\n%s", out, want)
	}
}

func TestInitStyledInteractiveTranscript(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth logo.Depth
		utf8  bool
	}{
		{"truecolor", logo.TrueColor, true},
		{"16color", logo.Color16, true},
		{"ascii", logo.Color16, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Golden fixtures use 80 columns; wide terminals are exercised
			// separately without changing their existing menu layout.
			caps := logo.Caps{TTY: true, Color: tc.depth, UTF8: tc.utf8, Width: 80}
			want, err := initTranscriptGoldens.ReadFile("testdata/init-interactive-" + tc.name + ".golden")
			if err != nil {
				t.Fatal(err)
			}
			out := initInteractiveTranscript(t, false, caps)
			assertInitMenuPointers(t, out, tc.utf8)
			if out != string(want) {
				t.Fatalf("styled transcript differs:\n%q\nwant:\n%q", out, want)
			}
			if !strings.Contains(out, "\x1b[") {
				t.Fatal("styled transcript has no styles")
			}
		})
	}
}

func TestInitStyledLongModels(t *testing.T) {
	for _, width := range []int{80, 100} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			initInteractiveTranscript(t, true, logo.Caps{TTY: true, Color: logo.TrueColor, UTF8: true, Width: width})
		})
	}
}

func TestInitPlainCapabilities(t *testing.T) {
	for _, mode := range []string{"NO_COLOR", "dumb", "pipe"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("COLORTERM", "truecolor")
			t.Setenv("NO_COLOR", "")
			if mode == "NO_COLOR" {
				t.Setenv("NO_COLOR", "1")
			}
			if mode == "dumb" {
				t.Setenv("TERM", "dumb")
			}
			want, err := initTranscriptGoldens.ReadFile("testdata/init-interactive.golden")
			if err != nil {
				t.Fatal(err)
			}
			out := initInteractiveTranscript(t, false)
			if out != string(want) {
				t.Fatalf("%s changed plain output:\n%s", mode, out)
			}
		})
	}
}

func TestInitInteractiveLongModels(t *testing.T) {
	out := initInteractiveTranscript(t, true)
	// The second of codex's levels (its own default, high, comes first) is low.
	if !strings.Contains(out, "  Foreman: codex/gpt-6.1-sol, effort low\n") {
		t.Fatalf("foreman did not use resolved default:\n%s", out)
	}
	plan := strings.Split(strings.Split(out, "foreman picks the tier per task):\n")[1], "  Reviews:")[0]
	families := []string{"Anthropic", "OpenAI", "Moonshot", "Zhipu (GLM)"}
	column := -1
	rows := 0
	for _, line := range strings.Split(plan, "\n") {
		for _, family := range families {
			if offset := strings.Index(line, family); offset >= 0 {
				if column >= 0 && offset != column {
					t.Errorf("family column = %d, want %d: %q", offset, column, line)
				}
				column = offset
				rows++
			}
		}
	}
	// Every model of claude (5), codex (3) and kimi (4), and the one OpenCode pick.
	if rows != 13 || !strings.Contains(plan, "kimi-code/kimi-for-coding") || !strings.Contains(plan, "zai-coding-plan/glm-5.3-highspeed") {
		t.Fatalf("missing agent rows:\n%s", plan)
	}
	flat := strings.Join(strings.Fields(plan), " ")
	for _, want := range []string{"Anthropic heavy writes code, reviews", "OpenAI light writes code, reviews", "Moonshot light writes code, reviews"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("missing %q:\n%s", want, plan)
		}
	}
}

func TestInitAgentsVeryLongModel(t *testing.T) {
	for _, size := range []int{35, 100} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			foreman := agentSpec{harness: "codex", model: "default", effort: "high", classes: []string{"implement", "review"}}
			agents := []agentSpec{
				foreman,
				{harness: "opencode", model: "zai-coding-plan/glm-" + strings.Repeat("x", size), classes: []string{"implement", "review"}},
			}
			sources := map[string]initModelSource{
				"codex": {entries: []initModelEntry{{value: "default", name: "gpt-6.1-sol"}}},
			}
			var out bytes.Buffer
			printInitAgents(&out, agents, 2, foreman, sources, adapter.AgentTier)
			for _, line := range strings.Split(out.String(), "\n") {
				if len([]rune(line)) >= 80 {
					t.Errorf("line must be under 80 columns: %q", line)
				}
			}
			for _, family := range []string{"OpenAI", "Zhipu (GLM)"} {
				if !strings.Contains(out.String(), "\n"+strings.Repeat(" ", 14)+family) {
					t.Errorf("family must start under the model column: %q\n%s", family, out.String())
				}
			}
			if !strings.Contains(out.String(), "  Foreman: codex/gpt-6.1-sol, effort high\n") {
				t.Errorf("foreman lost resolved default or effort:\n%s", out.String())
			}
		})
	}
}

// Both transcript variants exercise the full interactive flow and refuse apply.
func initInteractiveTranscript(t *testing.T, longModels bool, forced ...logo.Caps) string {
	t.Helper()
	oldCPUCount := initCPUCount
	initCPUCount = func() int { return 14 }
	t.Cleanup(func() { initCPUCount = oldCPUCount })
	width := 80
	if len(forced) > 0 {
		original := initDetectCaps
		initDetectCaps = func(io.Writer) logo.Caps { return forced[0] }
		t.Cleanup(func() { initDetectCaps = original })
		width = term.New(forced[0]).Width()
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
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".codex", "config.toml"), "model = \"gpt-6.1-sol\"\nmodel_reasoning_effort = \"high\"\n")
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".kimi-code", "config.toml"), `default_model = "kimi-code/k3"
[models."kimi-code/k3"]
display_name = "K3"
max_context_size = 1000000
default_effort = "high"
[models."kimi-code/k3-256k"]
display_name = "K3"
max_context_size = 262144
[models."kimi-code/kimi-for-coding"]
max_context_size = 200000
[models."kimi-code/kimi-for-coding-highspeed"]
max_context_size = 204800
`)
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".claude.json"), `{"additionalModelOptionsCache":[
 {"value":"claude-sonnet-4[1m]","label":"Sonnet","description":"Sonnet 4 · Balanced"},
 {"value":"claude-opus-4","label":"Opus","description":"Opus 4 · Most capable"}]}`)
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
	fakeInitCodexCatalog(t)
	codexPath := filepath.Join(os.Getenv("PATH"), "codex")
	codexScript, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	codexScript = bytes.ReplaceAll(codexScript, []byte(`"slug":"gpt-6.1-sol",`), []byte(`"slug":"gpt-6.1-sol","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}],"default_reasoning_level":"low",`))
	if err := os.WriteFile(codexPath, codexScript, 0755); err != nil {
		t.Fatal(err)
	}
	model := "zai-coding-plan/glm-5.3"
	if longModels {
		model += "-highspeed"
	}
	// Include different family widths and a paid plan with zero per-token cost.
	verbose := fmt.Sprintf(`%s
{"name":"GLM-5.3","variants":{"low":{},"medium":{},"high":{}},"status":"active","cost":{"input":0,"output":0},"limit":{"context":200000}}
zai-coding-plan/glm-5.2
{"name":"GLM-5.2","status":"active","limit":{"context":204800}}
opencode/deepseek-v3
{"name":"DeepSeek V3","status":"active","limit":{"context":1000000}}
`, model)
	fakeOpenCodeVerbose2(t, openCodeVerboseFile(t, verbose))
	// The golden takes the foreman's defaults, uses every claude and codex
	// model, uses kimi review-only, picks OpenCode model 1 and declines only
	// the final apply. The long-model variant types Codex as the foreman with
	// its second effort, and lets Kimi write code too.
	state := filepath.Join(os.Getenv("HOME"), ".local", "state", "brigada")
	input := "\n\n\n\ny\nN\n1\n\n\nn\n"
	if longModels {
		input = "codex/gpt-6.1-sol\n2\n\n\ny\nY\n1\n\n\nn\n"
	}
	code, out, errOut := initDo(t, state, input)
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("got %d\n%s\n%s", code, out, errOut)
	}
	tasksText, atOnceText := false, false
	for _, line := range strings.Split(out, "\n") {
		if term.VisibleWidth(line) >= width {
			t.Errorf("transcript line must be under %d columns: %q", width, line)
		}
		if strings.Contains(line, "tasks at once") {
			tasksText = true
		}
		if strings.Contains(line, "At once:") {
			atOnceText = true
		}
		if strings.Contains(line, "Check:") {
			atOnceText = false
		}
		if (tasksText || atOnceText) && term.VisibleWidth(line) >= 80 {
			t.Errorf("concurrency text must stay under 80 columns: %q", line)
		}
		if strings.Contains(line, "Tasks at once") {
			tasksText = false
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
	return out
}

func TestInitForemanQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, input, want, err string
		args                   []string
		one                    bool
	}{
		{name: "default", input: "\n\nn\n", want: "claude/sonnet, effort medium"},
		{name: "choose by number", input: "4\nn\n", want: "codex/CLI default"},
		{name: "typed harness/model", input: "claude/opus\n3\nn\n", want: "claude/opus, effort high"},
		{name: "flag asks only the effort", input: "2\nn\n", want: "claude/opus, effort low", args: []string{"--foreman", "claude/opus"}},
		{name: "flag answers both", input: "n\n", want: "claude/opus, effort max", args: []string{"--foreman", "claude/opus@max"}},
		{name: "flag names a codex model", input: "n\n", want: "codex/gpt-5", args: []string{"--foreman", "codex/gpt-5"}},
		{name: "only planner", input: "n\n", want: "codex/CLI default", one: true},
		{name: "retry", input: "9\n2\n\nn\n", want: "claude/opus, effort medium"},
		{name: "retry limit", input: "9\n0\ninvalid\n", err: "--foreman HARNESS/MODEL:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex", "opencode")
			signedIn := "claude,codex,opencode"
			if tc.one {
				// Only codex can plan, and its CLI default is its only model.
				fakeInitPATH(t, "codex", "opencode")
				signedIn = "codex,opencode"
			}
			args := append([]string{"--agent", "harness=codex,model=gpt-5", "--agent", "harness=opencode,model=zai-coding-plan/glm-5.3", "--signed-in", signedIn, "--check", "true", "--tools", "none", "--no-wiring"}, tc.args...)
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), tc.input, args...)
			if code != 1 {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			if tc.err != "" {
				if !strings.Contains(errOut, tc.err) || strings.Contains(out, "\nPlan\n") || strings.Count(out, "    Pick 1-4, or type harness/model [1]:") != 3 {
					t.Fatalf("foreman retry limit: %s\n%s", out, errOut)
				}
				return
			}
			if errOut != "" || !strings.Contains(out, "Declined") || !strings.Contains(out, "  Foreman: "+tc.want+"\n") {
				t.Fatalf("wrong foreman: %s\n%s", out, errOut)
			}
			switch {
			case tc.one:
				if !strings.Contains(out, "  Foreman: codex/CLI default (the only model that can plan)") || strings.Contains(out, "harness/model [") {
					t.Fatalf("asked for the only foreman: %s", out)
				}
			case len(tc.args) > 0 && strings.Contains(tc.args[1], "@"), tc.name == "flag names a codex model":
				if strings.Contains(out, "Pick 1-") {
					t.Fatalf("asked a question the flag answered: %s", out)
				}
			case len(tc.args) > 0:
				if strings.Contains(out, "harness/model [") || !strings.Contains(out, "Pick 1-5 or type a level [1]:") {
					t.Fatalf("flag without an effort must ask only the effort: %s", out)
				}
			}
			// A foreman outside the agents' models plans and nothing else.
			if strings.HasPrefix(tc.want, "claude/") && !strings.Contains(out, "foreman only") {
				t.Fatalf("a claude foreman with no claude agent must be foreman only:\n%s", out)
			}
		})
	}
}

func TestInitUnconfirmedUseQuestions(t *testing.T) {
	for _, tc := range []struct {
		name, input, signedIn string
		include, confirmed    bool
	}{
		{name: "Enter leaves out", input: "\n\n\n\n\n\nn\n", signedIn: "claude,codex"},
		{name: "opt in and pick numbers", input: "\n\n\n\ny\n\ny\n2\nn\n", signedIn: "claude,codex", include: true},
		{name: "flag answers Use", input: "\n\n\n\n\n2\nn\n", signedIn: "claude,codex,kimi,opencode", include: true, confirmed: true},
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
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), tc.input, "--signed-in", tc.signedIn, "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("got %d\n%s\n%s", code, out, errOut)
			}
			for _, h := range []string{"kimi", "opencode"} {
				if strings.Contains(out, "    Use "+h+"? [y/N]:") == tc.confirmed {
					t.Errorf("incorrect Use question for %s:\n%s", h, out)
				}
				asked := "Let kimi write code too"
				if h == "opencode" {
					asked = "Which models should opencode run?"
				}
				if strings.Contains(out, asked) != tc.include || strings.Contains(out, "\n    "+h+strings.Repeat(" ", 9-len(h))) != tc.include {
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
		{"mixed", "opencode/beam-free\\nopencode/claude-sonnet-4\\nopencode/big-pickle\\nopencode/beam-free\\n", "1\n", "Pick numbers (1,3) or -, or type provider/model [-]:"},
		{"all unknown", "opencode/beam-free\\nopencode/big-pickle\\n", "opencode/claude-sonnet-4\n", "Pick numbers (1,3) or -, or type provider/model [-]:"},
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
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n"+tc.answer+"n\n", "--signed-in", "codex", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			menu := strings.Split(strings.Split(out, "Which models should opencode run? Its list spans providers, so pick the\n    ones you pay for.\n")[1], "\nPlan\n")[0]
			if !strings.Contains(menu, "    2 more from `opencode models` aren't listed: brigada can't tell their model\n    family, which it needs to pick reviewers.\n") || !strings.Contains(menu, tc.prompt) {
				t.Fatalf("missing omitted count or incorrect prompt:\n%s", menu)
			}
			rows := regexp.MustCompile(`(?m)^\s+\d+  .*`).FindAllString(menu, -1)
			if tc.name == "mixed" {
				if len(rows) != 1 || strings.TrimSpace(rows[0]) != "1  opencode/claude-sonnet-4" {
					t.Fatalf("numbered unknown model or a family column: %q", rows)
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
	fakeInitCodexCatalog(t)
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".claude.json"), `{"additionalModelOptionsCache":[
 {"value":"claude-fable-5-1[1m]","label":"Fable","description":"Fable 5.1 · Most capable"},
 {"value":"opus[2m]","label":"Opus","description":"Opus · Most capable"}]}`)
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".codex", "config.toml"), "model = \"gpt-6.1-sol\"\n")
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".kimi-code", "config.toml"), `default_model = "kimi-code/k3"
[models."kimi-code/k3-256k"]
display_name = "K3 with a deliberately long display name to force wrapping of the note"
max_context_size = 262144
[models."kimi-code/kimi-for-coding"]
[models."kimi-code/kimi-for-coding-highspeed"]
display_name = "K2.7 Code Highspeed"
max_context_size = 1048576
[models."provider/gemini-2.5-pro"]
`)
	args := []string{"--signed-in", strings.Join(initHarnesses, ","), "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring"}
	// Take the first foreman and its effort, every claude and codex model,
	// review-only kimi and no OpenCode model; then decline the plan.
	// Only a claude foreman is asked an effort: the fake codex catalog has none.
	baseline := func(foreman, opencode string, effort bool) string {
		answers := foreman + "\n"
		if effort {
			answers += "\n"
		}
		return answers + "\n\n\n" + opencode + "\nn\n"
	}
	code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), baseline("1", "-", true), args...)
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("baseline init = %d\n%s\n%s", code, out, errOut)
	}
	menuRows := regexp.MustCompile(`(?m)^[ ]+(\d+)[ ]{2,}(\S+)`)
	check := func(name, menu string) [][]string {
		for _, line := range strings.Split(menu, "\n") {
			if utf8.RuneCountInString(line) >= 80 {
				t.Errorf("%s menu line is %d columns: %q", name, utf8.RuneCountInString(line), line)
			}
		}
		rows := menuRows.FindAllStringSubmatch(menu, -1)
		if len(rows) == 0 {
			t.Fatalf("no numbered entries for %s:\n%s", name, menu)
		}
		return rows
	}
	foremanMenu := strings.Split(strings.Split(out, "Which model should the foreman run?\n")[1], "    Pick ")[0]
	rows := check("foreman", foremanMenu)
	if len(rows) != 8 {
		t.Fatalf("expected claude's 5 models, codex's default and both listed catalog models:\n%s", foremanMenu)
	}
	for _, row := range rows {
		t.Run("foreman/"+row[1], func(t *testing.T) {
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), baseline(row[1], "-", strings.HasPrefix(row[2], "claude/")), args...)
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") || strings.Contains(out, "try again") {
				t.Fatalf("numbered entry %s rejected: %d\n%s\n%s", row[0], code, out, errOut)
			}
			if !strings.Contains(out, "  Foreman: "+row[2]) {
				t.Fatalf("picked foreman missing from plan:\n%s", out)
			}
		})
	}
	opencodeMenu := strings.Split(strings.Split(out, "ones you pay for.\n")[1], "    Pick ")[0]
	for _, row := range check("opencode", opencodeMenu) {
		t.Run("opencode/"+row[1], func(t *testing.T) {
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), baseline("1", row[1], true), args...)
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") || strings.Contains(out, "try again") {
				t.Fatalf("numbered entry %s rejected: %d\n%s\n%s", row[0], code, out, errOut)
			}
			plan := strings.Split(out, "\nPlan\n")[1]
			for _, line := range strings.Split(plan, "\n") {
				if strings.HasPrefix(line, "    opencode ") && strings.Contains(line, row[2]) {
					return
				}
			}
			t.Fatalf("picked entry missing model in plan:\n%s", plan)
		})
	}
}
