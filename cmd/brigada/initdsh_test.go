package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestInitDshProbe(t *testing.T) {
	fakeInitPATH(t, "dsh")
	source := initDshModels(context.Background())
	p := source.signIn
	if !p.installed || p.signedIn || p.checked || p.status != "version 0.2.1-rc.2, sign-in can't be checked" {
		t.Errorf("dsh probe = %+v", p)
	}
	var models []string
	for _, e := range source.entries {
		models = append(models, e.value)
	}
	if strings.Join(models, ",") != "deepseek-flash,deepseek-v4-pro" {
		t.Errorf("models = %v", models)
	}
	fakeInitPATH(t)
	if p := initDshModels(context.Background()).signIn; p.installed || p.status != "not installed" {
		t.Errorf("missing dsh probe = %+v", p)
	}
}

// qwen-code's binary is qwen: the probe runs `qwen --version` only, never a
// status, model or credential command.
func TestInitQwenProbe(t *testing.T) {
	calls := fakeInitPATH(t, "qwen")
	source := initQwenModels(context.Background())
	p := source.signIn
	if !p.installed || p.signedIn || p.checked || p.status != "version 1.0.0-preview.1, sign-in can't be checked" {
		t.Errorf("qwen-code probe = %+v", p)
	}
	var models []string
	for _, e := range source.entries {
		models = append(models, e.value)
	}
	if strings.Join(models, ",") != "qwen-subscription/qwen3.8-max,qwen-subscription/qwen3.8-flash" {
		t.Errorf("models = %v", models)
	}
	if data, _ := os.ReadFile(calls); strings.TrimSpace(string(data)) != "qwen --version" {
		t.Errorf("probe ran %q; want only qwen --version", data)
	}
	fakeInitPATH(t)
	if p := initQwenModels(context.Background()).signIn; p.installed || p.status != "not installed" {
		t.Errorf("missing qwen-code probe = %+v", p)
	}
}

// qwen-code joins the foreman menu only when its sign-in is confirmed
// (--signed-in qwen-code): its probe cannot check sign-in, and its use
// question comes later, in step 2.
func TestInitQwenCodeForemanWhenSignedIn(t *testing.T) {
	importRepo(t)
	fakeInitPATH(t, "claude", "codex", "qwen")
	// Unconfirmed: the foreman menu names no qwen-code model, and step 2
	// still asks the use question.
	code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n\n\ny\n\nn\n", "--signed-in", "claude,codex", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || errOut != "" {
		t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"qwen-code (experimental): version 1.0.0-preview.1, sign-in can't be checked",
		"Use qwen-code? [y/N]:",
		"qwen-code runs with auto-approve: when it writes code it can run any command on this machine without asking. It does not review by default.",
		"    qwen-code qwen-subscription/qwen3.8-max",
		"writes code, answers questions",
		"qwen-code edit files and run any command without asking; reviews opt-in",
	} {
		if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(flat, "qwen-code login") {
		t.Errorf("qwen-code offered a login hint it cannot verify:\n%s", out)
	}
	// Only confirmed resume-capable harnesses are foreman candidates.
	if strings.Contains(flat, "qwen-code/") {
		t.Errorf("qwen-code offered as foreman before its sign-in is confirmed:\n%s", out)
	}
	// Confirmed: qwen-code's models join the foreman menu, the use question
	// goes, and it still becomes a worker; it has no effort levels, so no
	// effort question follows its pick.
	code, out, errOut = initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n\n\n\nn\n", "--signed-in", "claude,codex,qwen-code", "--check", "true", "--tools", "none", "--no-wiring")
	if code != 1 || errOut != "" {
		t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
	}
	flat = strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"Only claude, codex, opencode and qwen-code can be the foreman.",
		"qwen-code/qwen-subscription/qwen3.8-max",
		"qwen-code/qwen-subscription/qwen3.8-flash",
		"qwen-code (experimental): signed in",
		"qwen-code runs with auto-approve: when it writes code it can run any command on this machine without asking. It does not review by default.",
		"    qwen-code qwen-subscription/qwen3.8-max",
		"writes code, answers questions",
	} {
		if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(flat, "Use qwen-code?") {
		t.Errorf("asked the use question of a confirmed qwen-code:\n%s", out)
	}
}

// parseInitForeman reads --foreman qwen-code/... and names the
// subscription model whole, provider segment and all.
func TestInitParseQwenForeman(t *testing.T) {
	spec, hasEffort, ok := parseInitForeman("qwen-code/qwen-subscription/qwen3.8-max")
	if !ok || hasEffort || spec.harness != "qwen-code" || spec.model != "qwen-subscription/qwen3.8-max" || spec.effort != "" {
		t.Errorf("parseInitForeman max = %+v, hasEffort %t, ok %t", spec, hasEffort, ok)
	}
	spec, hasEffort, ok = parseInitForeman("qwen-code/qwen-subscription/qwen3.8-flash@high")
	if !ok || !hasEffort || spec.harness != "qwen-code" || spec.model != "qwen-subscription/qwen3.8-flash" || spec.effort != "high" {
		t.Errorf("parseInitForeman flash@high = %+v, hasEffort %t, ok %t", spec, hasEffort, ok)
	}
}

func TestInitDshKeyNoteAndPlan(t *testing.T) {
	const secret = "sk-test-secret-value"
	const note = "dsh needs DEEPSEEK_API_KEY exported before `brigada start`"
	for _, tc := range []struct {
		name, key string
		wantNote  bool
	}{
		{"key missing", "", true},
		{"key set", secret, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex", "dsh")
			t.Setenv("DEEPSEEK_API_KEY", tc.key)
			code, out, errOut := initFixedParallel(t, testsupport.ShortStateDir(t), "\n\n\n\ny\n\nn\n", "--signed-in", "claude,codex", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			if strings.Contains(out, secret) {
				t.Errorf("printed the key:\n%s", out)
			}
			if flat := strings.Join(strings.Fields(out), " "); strings.Contains(flat, note) != tc.wantNote {
				t.Errorf("note present = %v, want %v:\n%s", !tc.wantNote, tc.wantNote, out)
			}
			for _, want := range []string{
				"dsh (experimental): init can't check sign-in.",
				"    dsh       deepseek-flash",
				"    dsh       deepseek-v4-pro",
				"dsh is experimental and may change in any release.",
				"dsh's workspace sandbox; reviews follow the agent's permission=",
			} {
				if !strings.Contains(strings.Join(strings.Fields(out), " "), strings.Join(strings.Fields(want), " ")) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
		})
	}
}
