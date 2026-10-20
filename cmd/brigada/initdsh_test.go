package main

import (
	"context"
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
				"dsh's workspace sandbox; reviews run read-only",
			} {
				if !strings.Contains(strings.Join(strings.Fields(out), " "), strings.Join(strings.Fields(want), " ")) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
		})
	}
}
