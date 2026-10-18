package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestInitFileStateColors(t *testing.T) {
	style := term.New(logo.Caps{TTY: true, Color: logo.Color16, UTF8: true, Width: 100})
	var b bytes.Buffer
	w := initOutput{Writer: &b, style: style, styled: true, utf8: true}
	files := []wireFile{
		{rel: wiringMCPFile, next: []byte("{}\n"), state: "created"},
		{rel: wiringSettingsFile, old: []byte("a\n"), exists: true, next: []byte("b\n"), state: "added"},
		{rel: wiringSkillFile, old: []byte("mine\n"), exists: true, err: fmt.Errorf("not brigada's")},
		{rel: wiringSharedSettingsFile, old: []byte("{}\n"), exists: true, next: []byte("{}\n"), state: "unchanged"},
	}
	printInitFiles(w, files, false)
	out := b.String()
	for _, want := range []string{
		style.OK("new") + " " + wiringMCPFile,
		style.Warn("changes") + " " + wiringSettingsFile,
		style.Bad("left alone") + " " + wiringSkillFile,
		style.Dim("unchanged") + " " + wiringSharedSettingsFile,
	} {
		if !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
			t.Errorf("missing state-first row %q:\n%q", want, out)
		}
	}
}

func TestInitPlanAgentRowsFit80(t *testing.T) {
	agents := []agentSpec{
		{harness: "claude", model: "sonnet", effort: "high", classes: []string{"implement", "review", "answer"}},
		{harness: "codex", model: "gpt-6.1-sol", classes: []string{"implement", "review", "answer"}},
		{harness: "kimi", model: "kimi-code/k3", classes: []string{"review"}},
		{harness: "opencode", model: "zai-coding-plan/glm-5.3", effort: "low", classes: []string{"implement", "review", "answer"}},
	}
	var b bytes.Buffer
	printInitAgents(&b, agents, 4, agents[0], nil)
	rows := 0
	for _, line := range strings.Split(b.String(), "\n") {
		if term.VisibleWidth(line) >= 80 {
			t.Errorf("line reaches 80 columns: %q", line)
		}
		if strings.HasPrefix(line, "    ") && (strings.HasSuffix(line, "code + review") || strings.HasSuffix(line, "review only")) {
			rows++
		}
	}
	if rows != 4 || strings.Contains(b.String(), "(default)") || !strings.Contains(b.String(), "sonnet @high") {
		t.Errorf("agent rows are not one line each:\n%s", b.String())
	}
}

func TestInitProbeAndRefusalHaveNoEscapesWithNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	agents := []string{"--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex", "--check", "true", "--tools", "none"}
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		code  int
	}{
		{"probe", append([]string{"--probe"}, agents...), "", 0},
		{"refusal", agents, "\n\nn\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			caps := logo.Caps{TTY: true, Color: logo.NoColor, UTF8: true, Width: 100}
			original := initDetectCaps
			initDetectCaps = func(io.Writer) logo.Caps { return caps }
			t.Cleanup(func() { initDetectCaps = original })
			code, out, errs := initDo(t, testsupport.ShortStateDir(t), tc.stdin, tc.args...)
			if code != tc.code || strings.Contains(out, "\x1b") || strings.Contains(errs, "\x1b") {
				t.Fatalf("init = %d, or escapes:\n%q\n%q", code, out, errs)
			}
			if strings.Contains(out, "To change later:") || strings.Contains(out, "ready") {
				t.Errorf("stale or premature text:\n%s", out)
			}
		})
	}
}
