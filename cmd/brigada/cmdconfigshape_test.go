package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

func TestConfigStateDirSource(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, flag, file, want string
	}{
		{"default", "", "", "(default)"},
		{"config file", "", "from-file", "(config file)"},
		{"flag over file", "/elsewhere/state", "from-file", "(--state-dir)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &runtime{stateDir: "/in/use/state", configPath: dir + "/none", file: lifecycle.FileConfig{StateDir: tc.file}}
			var out, errs bytes.Buffer
			if code := printConfigMachine(rt, tc.flag, &out, &errs); code != 0 || errs.Len() != 0 {
				t.Fatalf("code %d: %s", code, errs.String())
			}
			assertConfigWidth(t, out.String())
			want := "  state_dir = /in/use/state " + tc.want + "\n"
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %q in:\n%s", want, out.String())
			}
			if strings.Count(out.String(), "state_dir") != 1 {
				t.Errorf("state_dir listed more than once:\n%s", out.String())
			}
		})
	}
}

// A tools value wraps after a "+", never inside a "Bash(go vet:*)" pattern.
func TestConfigToolsWrapAtPlus(t *testing.T) {
	var machine bytes.Buffer
	rt := &runtime{stateDir: "/s", configPath: t.TempDir() + "/none"}
	if code := printConfigMachine(rt, "", &machine, &machine); code != 0 {
		t.Fatalf("code %d: %s", code, machine.String())
	}
	var project bytes.Buffer
	tools := "Bash(go build:*)+Bash(go test:*)+Bash(go vet:*)+Bash(gofmt:*)+Bash(git status:*)"
	printConfigProjectSettings(&project, engine.ProjectSettings{
		RepoURL:  "github.com/example/app",
		Settings: []engine.SettingValue{{Key: engine.SettingWorkerTools, Value: tools, Source: engine.SourceLocal}},
	}, engine.ForemanSetting{})
	for name, out := range map[string]string{"machine": machine.String(), "project": project.String()} {
		assertConfigWidth(t, out)
		wrapped := false
		lines := strings.Split(out, "\n")
		for i, line := range lines {
			if strings.HasSuffix(line, "+") {
				wrapped = true
				if i+1 < len(lines) && !strings.HasPrefix(lines[i+1], "    Bash(") {
					t.Errorf("%s: continuation after + is %q", name, lines[i+1])
				}
			}
			if strings.Contains(line, "Bash(") && strings.Count(line, "(") != strings.Count(line, ")") {
				t.Errorf("%s: pattern split across lines: %q", name, line)
			}
		}
		if !wrapped {
			t.Errorf("%s: tools value did not wrap:\n%s", name, out)
		}
	}
	if !strings.Contains(project.String(), "  worker_tools: Bash(go build:*)+Bash(go test:*)+Bash(go vet:*)+Bash(gofmt:*)+\n    Bash(git status:*) set by hand\n") {
		t.Errorf("project tools:\n%s", project.String())
	}
}
