package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestInitNoCheckChoices(t *testing.T) {
	for _, tc := range []struct {
		name, existing, answer string
		args                   []string
		goRepo, candidates     bool
		current                bool
	}{
		{name: "dash with candidates", answer: "-", candidates: true},
		{name: "dash with Go", answer: "-", candidates: true, goRepo: true},
		{name: "flag none", args: []string{"--check", "none"}, candidates: true},
		{name: "flag dash", args: []string{"--check", "-"}, candidates: true},
		{name: "flag none with Go", args: []string{"--check", "none"}, goRepo: true},
		{name: "probe empty", args: []string{"--probe"}},
		{name: "rerun no check", existing: "worker_tools: Bash(old command:*)\n", candidates: true, current: true},
		{name: "rerun no check Go", existing: "worker_tools: none\n", goRepo: true, current: true},
		{name: "probe rerun no check", existing: "worker_tools: none\n", args: []string{"--probe"}, candidates: true},
		{name: "remove current check", existing: "# keep policy\napproval_max_tasks: 9\ndefault_check: old command # old check\nworker_tools: Bash(old command:*)\n", answer: "-", candidates: true},
		{name: "flag removes check", existing: "default_check: old command\nworker_tools: Bash(old command:*)\n", args: []string{"--check", "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write := importRepo(t)
			original := ""
			if tc.existing != "" {
				original = projectFile(tc.existing)
				write(original)
			}
			if tc.candidates {
				if err := os.WriteFile("Makefile", []byte(initCheckMakefile), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.goRepo {
				if err := os.WriteFile("go.mod", []byte("module example.invalid/test\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			fakeInitPATH(t, "claude", "codex")
			args := append([]string{"--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--no-wiring", "--verbose"}, tc.args...)
			// The foreman's model and effort come first.
			stdin := "\n\n" + tc.answer + "\nn\n"
			if len(tc.args) > 0 { // --check skips the check question.
				stdin = "\n\nn\n"
			}
			code, out, errs := initFixedParallel(t, testsupport.ShortStateDir(t), stdin, args...)
			wantCode := 1
			if strings.Contains(strings.Join(tc.args, " "), "--probe") {
				wantCode = 0
			}
			if code != wantCode || errs != "" || !strings.Contains(out, "  Check: none (work goes straight to review)\n") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errs)
			}
			permissions := "edit files; no commands"
			if tc.goRepo {
				permissions = "edit files; run only: go build, go test, go vet, gofmt"
			}
			if !strings.Contains(out, "    claude    "+permissions+"\n") {
				t.Fatalf("wrong no-check permissions:\n%s", out)
			}
			if tc.current {
				flat := strings.Join(strings.Fields(out), " ")
				if !strings.Contains(flat, "no check from .brigada/project.yaml current") || !strings.Contains(flat, "Check command [-]:") || strings.Contains(flat, "make check Makefile: lint format-check type test rig-spec default") {
					t.Fatalf("no-check must be the current default:\n%s", out)
				}
			}
			for _, line := range strings.Split(out, "\n") {
				if term.VisibleWidth(line) >= 80 {
					t.Errorf("line reaches 80 columns: %q", line)
				}
				if strings.Contains(line, "default_check:") && !strings.HasPrefix(strings.TrimSpace(line), "-default_check:") {
					t.Errorf("no-check proposal sets default_check: %q", line)
				}
			}
			if strings.Contains(out, "ready") || strings.Contains(out, "Worker tools") {
				t.Errorf("premature readiness or obsolete question:\n%s", out)
			}
			got, err := os.ReadFile(engine.ProjectConfigPath)
			if original == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("probe/refusal wrote project config: %s, %v", got, err)
				}
			} else if err != nil || string(got) != original {
				t.Fatalf("probe/refusal changed project config: %s, %v", got, err)
			}
		})
	}
}

func TestInitNoCheckApply(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	old := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = old })
	importRepo(t) // No build files: --yes must pick no check.
	fakeInitPATH(t, "claude", "codex")
	state := testsupport.ShortStateDir(t)
	t.Cleanup(func() {
		if code, out, errs := stopDo(t, state, "--engine"); code != 0 {
			t.Errorf("cleanup: %d\n%s\n%s", code, out, errs)
		}
	})
	base := []string{"--yes", "--agent", "harness=claude", "--agent", "harness=codex", "--signed-in", "claude,codex", "--no-wiring"}
	for _, step := range []struct {
		name, check, tools string
		args               []string
	}{
		{"empty", "", "none", nil},
		{"add check", "echo checked", "Bash(echo checked:*)", []string{"--check", "echo checked"}},
		{"remove check", "", "none", []string{"--check", "none"}},
		{"keep no check", "", "none", nil},
	} {
		t.Run(step.name, func(t *testing.T) {
			code, out, errs := initFixedParallel(t, state, "", append(append([]string{}, base...), step.args...)...)
			if code != 0 || errs != "" || !strings.Contains(strings.Join(strings.Fields(out), " "), "Setup ready") {
				log, _ := os.ReadFile(filepath.Join(state, "engine.log"))
				t.Fatalf("init = %d\n%s\n%s\nengine log: %s", code, out, errs, log)
			}
			content, err := os.ReadFile(engine.ProjectConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := engine.ParseProjectConfig(content)
			if err != nil {
				t.Fatal(err)
			}
			value, hasCheck := cfg.Values[engine.SettingDefaultCheck]
			var check, tools string
			_ = json.Unmarshal(value, &check)
			_ = json.Unmarshal(cfg.Values[engine.SettingWorkerTools], &tools)
			if check != step.check || hasCheck != (step.check != "") || tools != step.tools {
				t.Fatalf("wrong project defaults: %s", content)
			}
			_, token, err := lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, "client")
			if err != nil {
				t.Fatal(err)
			}
			settings, err := api.NewClient(lifecycle.PathsOf(state).Socket, token).ProjectSettings(context.Background(), importOrigin)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, setting := range settings.Settings {
				if setting.Key == engine.SettingDefaultCheck {
					found = true
					if step.check == "" {
						if !setting.Default || setting.Value != "none" {
							t.Errorf("no check must unset the project setting: %+v", setting)
						}
					} else if setting.Value != step.check || setting.Default {
						t.Errorf("wrong project check: %+v", setting)
					}
				}
			}
			if !found {
				t.Error("default_check missing from settings")
			}
			rec, live, err := runnerstate.Running(filepath.Join(state, "runner"))
			if err != nil || !live || len(rec.Agents) != 2 || rec.Agents[0].Tools != step.tools {
				t.Fatalf("wrong worker tools: %+v live=%v err=%v", rec, live, err)
			}
		})
	}
}
