package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func TestInitCLIStyles(t *testing.T) {
	style := term.New(logo.Caps{TTY: true, Color: logo.Color16, UTF8: true, Width: 100})
	for _, tc := range []struct {
		name, harness string
		probe         harnessProbe
		confirmed     bool
		want          []string
	}{
		{"signed in", "claude", harnessProbe{}, true, []string{style.OK(style.Check() + " signed in")}},
		{"signed out", "claude", harnessProbe{checked: true, status: "not signed in"}, false, []string{style.Bad("not signed in")}},
		{"kimi unknown", "kimi", harnessProbe{}, false, []string{style.Warn("? sign-in can't be checked"), style.Dim(" · sign in with "), style.Accent("kimi login")}},
		{"opencode unknown", "opencode", harnessProbe{}, false, []string{style.Warn("? sign-in can't be checked"), style.Accent("opencode auth login")}},
		{"opencode no keys", "opencode", harnessProbe{checked: true, status: "no stored provider credentials"}, false, []string{style.Warn("no stored provider credentials")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			w := initOutput{Writer: &b, style: style, styled: true, utf8: true}
			initCLIStatus(w, tc.harness, tc.probe, tc.confirmed)
			for _, want := range append(tc.want, style.Bold(tc.harness)) {
				if !strings.Contains(b.String(), want) {
					t.Errorf("status lacks %q: %q", want, b.String())
				}
			}
		})
	}
}

func TestInitStyledCurrentEffortTagAtEnd(t *testing.T) {
	var b bytes.Buffer
	style := term.New(logo.Caps{TTY: true, Color: logo.Color16, UTF8: true, Width: 100})
	w := initOutput{Writer: &b, style: style, styled: true, utf8: true}
	entry := initModelEntry{name: "model", effortLevels: []string{"low", "high"}, defaultEffort: "low"}
	_, err := initPickEffort(w, "codex", entry, func(string, string) (string, error) { return "", nil }, "low")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), style.Accent("current")+"\n") {
		t.Fatalf("current must end the row: %q", b.String())
	}
	if !strings.Contains(b.String(), style.Pointer()) || !strings.Contains(b.String(), style.Bold("low")) {
		t.Fatalf("missing selected effort: %q", b.String())
	}
}

func TestInitWrapStyledIdentifiers(t *testing.T) {
	for _, width := range []int{80, 100} {
		var b bytes.Buffer
		style := term.New(logo.Caps{TTY: true, Color: logo.TrueColor, UTF8: true, Width: width})
		w := initOutput{Writer: &b, style: style, styled: true, utf8: true}
		identifier := strings.Repeat("模型", 90)
		printInitWrapped(w, "    ", "    ", style.Accent(identifier))
		var restored strings.Builder
		for _, line := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
			if term.VisibleWidth(line) >= width {
				t.Errorf("line exceeds %d: %q", width, line)
			}
			restored.WriteString(strings.TrimSpace(ansi.ReplaceAllString(line, "")))
		}
		if restored.String() != identifier {
			t.Fatalf("wrapping lost identifier text: %q", restored.String())
		}
		if strings.Contains(ansi.ReplaceAllString(b.String(), ""), "\x1b") {
			t.Fatal("wrapping split an escape sequence")
		}
	}
}

// Exercise the real documented-command probes rather than inventing checked
// flags for the renderer: signed-out responses must remain distinct from an
// unavailable or unrecognized status command.
func TestInitStyledProbedAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		claudeStatus, codexStatus string
		exit                      int
		checked                   bool
	}{
		{"signed out", `{"loggedIn":false}`, "Not logged in", 0, true},
		{"signed out with nonzero exit", "Not logged in", "Not logged in", 1, true},
		{"unrecognized response", "unrecognized status", "unrecognized status", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			for _, h := range []string{"claude", "codex"} {
				parent, status := "auth", tc.claudeStatus
				if h == "codex" {
					parent, status = "login", tc.codexStatus
				}
				script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n --help) echo '  %s  Authentication' ;;\n '%s --help') echo '  status  Authentication status' ;;\n '%s status') echo '%s'; exit %d ;;\n *) exit 89 ;;\nesac\n", parent, parent, parent, status, tc.exit)
				if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), h), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
				p := probeHarness(context.Background(), h)
				if p.checked != tc.checked || p.signedIn {
					t.Fatalf("%s probe = %+v", h, p)
				}
			}
			caps := logo.Caps{TTY: true, Color: logo.TrueColor, UTF8: true, Width: 100}
			original := initDetectCaps
			initDetectCaps = func(io.Writer) logo.Caps { return caps }
			t.Cleanup(func() { initDetectCaps = original })
			code, out, errs := initDo(t, testsupport.ShortStateDir(t), "n\nn\n",
				"--agent", "harness=claude", "--agent", "harness=codex",
				"--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || !strings.Contains(errs, "zero usable agents") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errs)
			}
			style := term.New(caps)
			for _, h := range []string{"claude", "codex"} {
				status := style.Warn(style.Question() + " sign-in can't be checked")
				if tc.checked {
					status = style.Bad("not signed in")
				}
				if !strings.Contains(out, style.Bold(h)+": "+status) {
					t.Errorf("incorrect %s status: %q", h, out)
				}
			}
			if strings.Contains(out, "ready") {
				t.Fatal("claimed ready before applying")
			}
		})
	}
}

func TestInitPlainTerminalLayout(t *testing.T) {
	// Other CLI tests can leave the process in an engine's state directory.
	// Loading transcript fixtures must not depend on the working directory.
	t.Chdir(t.TempDir())
	for _, utf8 := range []bool{true, false} {
		t.Run(fmt.Sprintf("UTF8=%t", utf8), func(t *testing.T) {
			want, err := initTranscriptGoldens.ReadFile("testdata/init-interactive.golden")
			if err != nil {
				t.Fatal(err)
			}
			// Model NO_COLOR and TERM=dumb on a wide TTY through the caps
			// seam; a bytes buffer alone would always detect a nonterminal.
			caps := logo.Caps{TTY: true, Color: logo.NoColor, UTF8: utf8, Width: 100}
			out := initInteractiveTranscript(t, false, caps)
			if strings.Contains(out, "\x1b") {
				t.Fatal("plain terminal received escapes")
			}
			if strings.Join(strings.Fields(out), " ") != strings.Join(strings.Fields(string(want)), " ") {
				t.Fatalf("plain terminal changed transcript words:\n%s", out)
			}
			wideModelRow := false
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "gpt-5.4-mini") && term.VisibleWidth(line) > 80 {
					wideModelRow = true
				}
			}
			if !wideModelRow {
				t.Fatalf("plain TTY still wraps the model menu at 80:\n%s", out)
			}
		})
	}
}

func TestInitPlainCheckMenuWidth(t *testing.T) {
	for _, width := range []int{80, 100} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			var b bytes.Buffer
			w := initOutput{Writer: &b, style: term.New(logo.Caps{TTY: true, Width: width})}
			note := strings.Repeat("n", 75)
			_, exhausted, err := askInitCheck(w, func(string, string) (string, error) { return "", nil },
				[]checkCandidate{{Command: "check", Note: note}})
			if err != nil || exhausted {
				t.Fatalf("check question: exhausted=%v, err=%v", exhausted, err)
			}
			if strings.Contains(b.String(), note) != (width == 100) {
				t.Fatalf("note did not use %d columns:\n%s", width, b.String())
			}
			for _, line := range strings.Split(b.String(), "\n") {
				if term.VisibleWidth(line) >= width {
					t.Errorf("line must be under %d columns: %q", width, line)
				}
			}
		})
	}
}

func TestInitPlainCurrentCheckWidth(t *testing.T) {
	for _, tc := range []struct{ width, commandLength int }{
		{80, 40}, {100, 60}, // Truncated notes must leave room for " current".
		{80, 63}, {100, 83}, // The command leaves no room even for the tag alone.
	} {
		t.Run(fmt.Sprintf("width=%d/command=%d", tc.width, tc.commandLength), func(t *testing.T) {
			var b bytes.Buffer
			w := initOutput{Writer: &b, style: term.New(logo.Caps{TTY: true, Width: tc.width})}
			command := strings.Repeat("c", tc.commandLength)
			got, exhausted, err := askInitCheck(w, func(string, string) (string, error) { return "", nil },
				initCheckEntries(nil, command, "none"))
			if err != nil || exhausted || got != command {
				t.Fatalf("check question = %q, exhausted=%v, err=%v", got, exhausted, err)
			}
			if !strings.Contains(b.String(), "current\n") {
				t.Fatalf("current tag must end a row:\n%s", b.String())
			}
			for _, line := range strings.Split(b.String(), "\n") {
				if term.VisibleWidth(line) >= tc.width {
					t.Errorf("line must be under %d columns: %q", tc.width, line)
				}
			}
		})
	}
}
