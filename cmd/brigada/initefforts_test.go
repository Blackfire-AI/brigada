package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func assertInitEfforts(t *testing.T, entry initModelEntry, levels []string, defaultEffort string) {
	t.Helper()
	if !reflect.DeepEqual(entry.effortLevels, levels) || entry.defaultEffort != defaultEffort {
		t.Errorf("model %q: efforts = %v, default = %q; want %v, %q", entry.name, entry.effortLevels, entry.defaultEffort, levels, defaultEffort)
	}
}

func TestInitCodexEfforts(t *testing.T) {
	t.Chdir(t.TempDir())
	const catalog = `{"models":[
	 {"slug":"gpt-5","visibility":"list","priority":1,
	  "supported_reasoning_levels":[{"effort":"low","description":"never-print-me"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra","private":{"effort":"never-print-me"}}],
	  "default_reasoning_level":"medium","other":{"effort":"never-print-me"}},
	 {"slug":"gpt-6.1-sol","visibility":"list","priority":2,
	  "supported_reasoning_levels":[{"effort":"high"},{"effort":"low"}],"default_reasoning_level":"high"},
	 {"slug":"gpt-5.4-mini","visibility":"list","priority":3}
	]}`
	for _, defaultModel := range []string{"gpt-5", "gpt-config-only", ""} {
		t.Run("default="+defaultModel, func(t *testing.T) {
			fakeInitPATH(t, "codex")
			writeInitCodexScript(t, "  debug  Diagnostic tools", "  models  Render models", "", "printf '%s\\n' '"+catalog+"'", "exit 89")
			writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".codex", "config.toml"), fmt.Sprintf("model = %q\n", defaultModel))
			got := initCodexModels(context.Background())
			wantCount := 4
			if defaultModel == "gpt-5" {
				wantCount = 3
			}
			if len(got.entries) != wantCount {
				t.Fatalf("entries = %+v", got.entries)
			}
			for _, entry := range got.entries {
				switch entry.name {
				case "gpt-5":
					assertInitEfforts(t, entry, []string{"low", "medium", "high", "xhigh", "max", "ultra"}, "medium")
				case "gpt-6.1-sol":
					assertInitEfforts(t, entry, []string{"high", "low"}, "high")
				default:
					assertInitEfforts(t, entry, nil, "")
				}
			}
			assertInitModelPrivacy(t, got)
		})
	}
}

func TestInitOpenCodeEfforts(t *testing.T) {
	const listing = `fixture/gpt-5
{"status":"active","variants":{"max":{"reasoningEffort":"never-print-me","nested":[{"token":"never-print-me"}]},"low":{},"medium":{"private":"never-print-me"},"high":{},"xhigh":{}},"default_effort":"never-print-me","private":{"variants":{"never-print-me":{}}}}
fixture/glm-5.3
{"status":"active","variants":{}}
fixture/gpt-6.1-sol
{"status":"active"}
`
	fakeOpenCodeVerbose(t, openCodeVerboseHelp, openCodeVerboseFile(t, listing))
	got := initOpenCodeModels(context.Background())
	if len(got.entries) != 3 {
		t.Fatalf("entries = %+v", got.entries)
	}
	assertInitEfforts(t, got.entries[0], []string{"max", "low", "medium", "high", "xhigh"}, "")
	assertInitEfforts(t, got.entries[1], nil, "")
	assertInitEfforts(t, got.entries[2], nil, "")
	assertInitModelPrivacy(t, got)
}

func TestInitKimiEfforts(t *testing.T) {
	fakeInitPATH(t, "kimi")
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".kimi-code", "config.toml"), `default_model = "kimi-code/current"
support_efforts = ["never-print-me"]
default_effort = "never-print-me"
secret = '''
[models."never-print-me"]
support_efforts = ["never-print-me"]
default_effort = "never-print-me"
'''
[providers."kimi-code"]
support_efforts = ["never-print-me"]
default_effort = "never-print-me"
[models."kimi-code/current"]
support_efforts = ["low", 'medium', "high"] # keep array order
default_effort = "medium" # current default
private = "never-print-me"
[models."kimi-code/other"]
support_efforts = [
  "high", # keep a different order
  # skipped comment
  'low',
]
default_effort = 'high'
[models."kimi-code/current"] # metadata survives repeated tables
[models."kimi-code/bare"]
provider = "never-print-me"
[models."kimi-code/empty"]
support_efforts = []
`)
	got := initKimiModels(context.Background())
	if len(got.entries) != 4 {
		t.Fatalf("entries = %+v", got.entries)
	}
	assertInitEfforts(t, got.entries[0], []string{"low", "medium", "high"}, "medium")
	assertInitEfforts(t, got.entries[1], []string{"high", "low"}, "high")
	assertInitEfforts(t, got.entries[2], nil, "")
	assertInitEfforts(t, got.entries[3], []string{}, "")
	assertInitModelPrivacy(t, got)
}

// Exercise the real model-to-effort prompt sequence and the resulting agent
// and foreman specs without applying the plan or starting a model.
func TestInitInteractiveEfforts(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "opencode"} {
		for _, answer := range []string{"", "high", "number"} {
			t.Run(harness+"/"+answer, func(t *testing.T) {
				importRepo(t)
				fakeInitPATH(t, harness)
				model, pick, prompt := "sonnet", "1", "Pick 1-6 [1]:"
				number := "4" // Claude's unknown default precedes its five levels.
				if harness == "codex" {
					model, number, prompt = "gpt-6.1-sol", "3", "Pick 1-3 [1]:"
					catalog := `{"models":[{"slug":"gpt-6.1-sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}],"default_reasoning_level":"low"}]}`
					writeInitCodexScript(t, "  debug  Diagnostic tools", "  models  Render models", "", "printf '%s\\n' '"+catalog+"'", "exit 89")
					pick = "2" // Entry 1 is the unresolved CLI model default.
				}
				if harness == "opencode" {
					model, number, prompt = "fixture/gpt-5", "3", "Pick 1-3 [1]:"
					fakeInitPATH(t, "claude", "opencode")
					fakeOpenCodeVerbose2(t, openCodeVerboseFile(t, "fixture/gpt-5\n{\"status\":\"active\",\"variants\":{\"low\":{},\"high\":{}}}\n"))
					pick = "1\n\n1" // Claude model, default effort, OpenCode model.
				}
				input := answer
				if answer == "number" {
					input = number
				}
				code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), pick+"\n"+input+"\nn\n", "--signed-in", "claude,codex,opencode", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring")
				if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
					t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
				}
				if !strings.Contains(out, "How hard should "+model+" think?") || !strings.Contains(out, prompt) {
					t.Fatalf("missing effort question:\n%s", out)
				}
				suffix, effort := " @high", "high"
				if answer == "" {
					suffix, effort = "", ""
				}
				plan := strings.Split(out, "\nPlan\n")[1]
				if !strings.Contains(plan, model+suffix) || (effort == "" && strings.Contains(plan, model+" @")) {
					t.Fatalf("wrong agent effort:\n%s", plan)
				}
				if harness != "opencode" && !strings.Contains(plan, "Foreman: "+agentName(harness, model, effort)+"\n") {
					t.Fatalf("foreman lost effort:\n%s", plan)
				}
				assertInitEffortOutput(t, out)
			})
		}
	}
}

func assertInitEffortOutput(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) >= 80 {
			t.Errorf("line must be under 80 columns: %q", line)
		}
	}
	if strings.Contains(out, "ready") {
		t.Errorf("claimed ready before apply:\n%s", out)
	}
}

func TestInitEffortDefaultsAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name, defaultEffort, input, want string
		wantErr                          bool
	}{
		{name: "known default Enter", defaultEffort: "medium", input: "\n"},
		{name: "known default number", defaultEffort: "medium", input: "2\n"},
		{name: "known default typed", defaultEffort: "medium", input: "medium\n"},
		{name: "source order", defaultEffort: "medium", input: "1\n", want: "low"},
		{name: "unknown default", input: "1\n"},
		{name: "typed high", input: "high\n", want: "high"},
		{name: "retry", input: "bogus\n8\n4\n", want: "high"},
		{name: "retry limit", input: "bogus\n8\nLOW\n", wantErr: true},
		{name: "EOF", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			reader := bufio.NewReader(strings.NewReader(tc.input))
			attempts := 0
			effort, err := initPickEffort(&out, "codex", initModelEntry{name: "gpt-5", effortLevels: []string{"low", "medium", "high"}, defaultEffort: tc.defaultEffort}, func(prompt, flags string) (string, error) {
				attempts++
				fmt.Fprint(&out, prompt)
				line, err := reader.ReadString('\n')
				return strings.TrimSuffix(line, "\n"), err
			})
			if effort != tc.want || (err != nil) != tc.wantErr || attempts > 3 {
				t.Fatalf("effort = %q, err = %v, attempts = %d", effort, err, attempts)
			}
			if tc.defaultEffort != "" && (!strings.Contains(out.String(), "Pick 1-3 [2]:") || !strings.Contains(out.String(), "medium  default")) {
				t.Fatalf("known default not marked in source order:\n%s", out.String())
			}
		})
	}
}

func TestInitEffortInformationOnly(t *testing.T) {
	for _, tc := range []struct {
		harness, defaultEffort string
		levels                 []string
	}{
		{"kimi", "high", []string{"low", "high"}},
		{"kimi", "", []string{"low", "high"}},
		{"opencode", "", nil},
		{"opencode", "", []string{"high"}},
	} {
		t.Run(tc.harness+tc.defaultEffort+fmt.Sprint(len(tc.levels)), func(t *testing.T) {
			var out bytes.Buffer
			effort, err := initPickEffort(&out, tc.harness, initModelEntry{effortLevels: tc.levels, defaultEffort: tc.defaultEffort}, func(string, string) (string, error) {
				t.Fatal("unexpected effort question")
				return "", nil
			})
			if effort != "" || err != nil {
				t.Fatalf("effort = %q, err = %v", effort, err)
			}
			want := ""
			if tc.harness == "kimi" && tc.defaultEffort != "" {
				want = "    Effort: high, kimi's own setting for this model (brigada can't set it per\n    run).\n"
			}
			if out.String() != want {
				t.Fatalf("output = %q, want %q", out.String(), want)
			}
			assertInitEffortOutput(t, out.String())
		})
	}
}

func TestInitSavedEffortDefault(t *testing.T) {
	for _, tc := range []struct {
		name, saved, answer, want, prompt string
	}{
		{"override", "high", "", "high", "Pick 1-3 [3]:"},
		{"explicit CLI default", "low", "", "low", "Pick 1-3 [1]:"},
		{"CLI default", "", "", "", "Pick 1-3 [1]:"},
		{"change", "high", "2", "medium", "Pick 1-3 [3]:"},
		{"reset", "high", "1", "", "Pick 1-3 [3]:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			entry := initModelEntry{
				name: "gpt-5", defaultEffort: "low",
				effortLevels: []string{"low", "medium", "high"},
			}
			got, err := initPickEffort(&out, "codex", entry,
				func(prompt, flags string) (string, error) {
					if !strings.Contains(prompt, tc.prompt) {
						t.Errorf("prompt = %q, want %q", prompt, tc.prompt)
					}
					return tc.answer, nil
				}, tc.saved)
			if err != nil || got != tc.want {
				t.Fatalf("effort = %q, %v; want %q", got, err, tc.want)
			}
			if !strings.Contains(out.String(), "current") {
				t.Fatalf("saved effort not marked current:\n%s", out.String())
			}
		})
	}
}

func TestInitExplicitEffortsSkipQuestions(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, "claude", "codex", "opencode")
	code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\nn\n",
		"--agent", "harness=claude,model=sonnet,effort=high",
		"--agent", "harness=codex,model=gpt-5,effort=high",
		"--agent", "harness=opencode,model=fixture/glm-5.3,effort=high",
		"--signed-in", "claude,codex,opencode", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out, "How hard should") || strings.Contains(out, "Which model should") {
		t.Fatalf("explicit agents got model or effort questions:\n%s", out)
	}
	for _, model := range []string{"sonnet", "gpt-5", "fixture/glm-5.3"} {
		if !strings.Contains(out, model+" @high") {
			t.Errorf("explicit effort missing for %s:\n%s", model, out)
		}
	}
	assertInitEffortOutput(t, out)
}
