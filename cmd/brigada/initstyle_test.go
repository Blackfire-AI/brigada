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
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
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
			for _, want := range append(tc.want, style.Bold(tc.harness)+"  ") {
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
	if !strings.Contains(b.String(), style.Accent("current")+"\n") || strings.Contains(b.String(), style.Accent("default")) {
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
				if !strings.Contains(out, style.Bold(h)+"  "+status) {
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
			note := strings.Repeat("n", 72)
			_, exhausted, err := askInitCheck(w, func(string, string) (string, error) { return "", nil },
				initCheckEntries([]checkCandidate{{Command: "check", Note: note}}, "", "", false))
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
				initCheckEntries(nil, command, "none", true))
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

func TestInitMenuPointerColumn(t *testing.T) {
	for _, utf8 := range []bool{true, false} {
		for _, indent := range []string{"   ", "    ", "      "} {
			for _, id := range []int{1, 10, 120} {
				var b bytes.Buffer
				style := term.New(logo.Caps{TTY: true, Color: logo.Color16, UTF8: utf8, Width: 100})
				w := initOutput{Writer: &b, style: style, styled: true, utf8: utf8}
				row := ansi.ReplaceAllString(initMenuPrefix(w, indent, id, "model", 5, true), "")
				plain := ansi.ReplaceAllString(initMenuPrefix(w, indent, id, "model", 5, false), "")
				number := fmt.Sprint(id)
				column := term.VisibleWidth(row[:strings.Index(row, number)])
				pointer := ansi.ReplaceAllString(style.Pointer(), "")
				pointerColumn := term.VisibleWidth(row[:strings.Index(row, pointer)])
				if pointerColumn != column-2 || column != strings.Index(plain, number) {
					t.Errorf("pointer must be two columns before aligned number: %q (plain %q)", row, plain)
				}
			}
		}
	}
}

func assertInitMenuPointers(t *testing.T, out string, utf8 bool) {
	t.Helper()
	pointer := ">"
	if utf8 {
		pointer = "▸"
	}
	menus := 0
	for _, line := range strings.Split(ansi.ReplaceAllString(out, ""), "\n") {
		index := strings.Index(line, pointer)
		if index < 0 {
			continue
		}
		rest := line[index+len(pointer):]
		choice := strings.TrimLeft(rest, " ")
		if choice == "" || !strings.ContainsRune("0123456789-", rune(choice[0])) {
			continue
		}
		if len(rest)-len(choice) != 1 {
			t.Errorf("pointer must be two columns before the choice: %q", line)
		}
		menus++
	}
	if menus != 9 {
		t.Errorf("checked %d selected rows; want all 9 model, skip, effort, foreman and check menus", menus)
	}
}

func TestInitChangeLaterWrap(t *testing.T) {
	for _, width := range []int{80, 100} {
		for _, styled := range []bool{true, false} {
			var b bytes.Buffer
			caps := logo.Caps{Width: width}
			if styled {
				caps.TTY, caps.UTF8, caps.Color = true, true, logo.Color16
			}
			style := term.New(caps)
			w := initOutput{Writer: &b, style: style, styled: styled, utf8: caps.UTF8}
			how := "brigada project foreman --harness H --model M, or run brigada init again"
			initChangeLater(w, how)
			lines := strings.Split(strings.TrimSuffix(ansi.ReplaceAllString(b.String(), ""), "\n"), "\n")[1:]
			column := term.VisibleWidth(lines[0][:strings.Index(lines[0], "change later:")])
			for i, line := range lines {
				if term.VisibleWidth(line) >= style.Width() {
					t.Errorf("line must be under Width(): %q", line)
				}
				if i > 0 && len(line)-len(strings.TrimLeft(line, " ")) != column {
					t.Errorf("continuation must align under change later: %q", line)
				}
			}
			if got := strings.Join(strings.Fields(strings.Join(lines, " ")), " "); got != style.Again()+" change later: "+how {
				t.Errorf("wrapped wording = %q", got)
			}
		}
	}
}

func TestInitModelDefaultAndCurrentTags(t *testing.T) {
	for _, styled := range []bool{false, true} {
		for _, rerun := range []bool{false, true} {
			t.Run(fmt.Sprintf("styled=%t/rerun=%t", styled, rerun), func(t *testing.T) {
				importRepo(t)
				fakeInitPATH(t, "claude", "codex")
				state := testsupport.ShortStateDir(t)
				if rerun {
					if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), []string{
						"harness=claude,model=sonnet", "harness=codex,model=default",
					}); err != nil {
						t.Fatal(err)
					}
				}
				caps := logo.Caps{Width: 100}
				if styled {
					caps.TTY, caps.UTF8, caps.Color = true, true, logo.Color16
				}
				original := initDetectCaps
				initDetectCaps = func(io.Writer) logo.Caps { return caps }
				t.Cleanup(func() { initDetectCaps = original })
				code, out, errs := initDo(t, state, strings.Repeat("\n", 10),
					"--signed-in", "claude,codex", "--check", "true", "--tools", "none", "--no-wiring")
				if code != 1 || errs != "" || !strings.Contains(out, "Declined") {
					t.Fatalf("init = %d\n%s\n%s", code, out, errs)
				}
				tag := "default"
				if rerun {
					tag = "current"
				}
				rows := 0
				for _, line := range strings.Split(out, "\n") {
					plain := ansi.ReplaceAllString(line, "")
					if strings.Contains(plain, "1  sonnet ") || strings.Contains(plain, "1  CLI default ") {
						rows++
						rowTag := tag
						if !rerun && strings.Contains(plain, "CLI default") {
							rowTag = "your codex default"
						}
						if !strings.HasSuffix(line, term.New(caps).Accent(rowTag)) {
							t.Errorf("model row must end with %s tag: %q", tag, line)
						}
						if rerun && strings.Contains(plain, " default current") {
							t.Errorf("current must replace default: %q", line)
						}
					}
					if term.VisibleWidth(line) >= term.New(caps).Width() {
						t.Errorf("line must be under Width(): %q", line)
					}
				}
				if rows != 2 {
					t.Errorf("checked %d model rows; want Claude and Codex", rows)
				}
				if strings.Contains(out, "ready") {
					t.Fatal("claimed ready before applying")
				}
			})
		}
	}
}

func TestInitConfiguredDefaultRows(t *testing.T) {
	for _, width := range []int{80, 100} {
		for _, styled := range []bool{false, true} {
			for _, rerun := range []bool{false, true} {
				t.Run(fmt.Sprintf("width=%d/styled=%t/rerun=%t", width, styled, rerun), func(t *testing.T) {
					importRepo(t)
					fakeInitPATH(t, "claude", "codex", "kimi")
					writeInitCodexScript(t, "  debug  Diagnostic tools", "  models  Render models", "",
						`printf '%s\n' '{"models":[{"slug":"gpt-6.1-sol","visibility":"list","description":"Latest workhorse model for coding and everyday work.","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}],"default_reasoning_level":"low"}]}'`, "exit 89")
					writeInitModelFixture(t, codexConfigPath(), "model = \"gpt-6.1-sol\"\nmodel_reasoning_effort = \"high\"\n")
					writeInitModelFixture(t, kimiConfigPath(), "default_model = \"kimi-code/k3\"\n[models.\"kimi-code/k3\"]\ndisplay_name = \"K3\"\nmax_context_size = 1000000\n")
					state := testsupport.ShortStateDir(t)
					if rerun {
						if err := runnerstate.SaveAgents(filepath.Join(state, "runner"), []string{
							"harness=claude,model=sonnet", "harness=codex,model=default", "harness=kimi,model=default,classes=review",
						}); err != nil {
							t.Fatal(err)
						}
					}
					caps := logo.Caps{TTY: true, Width: width}
					if styled {
						caps.UTF8, caps.Color = true, logo.Color16
					}
					original := initDetectCaps
					initDetectCaps = func(io.Writer) logo.Caps { return caps }
					t.Cleanup(func() { initDetectCaps = original })
					code, out, errs := initDo(t, state, strings.Repeat("\n", 10),
						"--signed-in", "claude,codex,kimi", "--check", "true", "--tools", "none", "--no-wiring")
					if code != 1 || errs != "" || !strings.Contains(out, "Declined") {
						t.Fatalf("init = %d\n%s\n%s", code, out, errs)
					}
					plain := ansi.ReplaceAllString(out, "")
					// Inspect the real model and effort menus, excluding the change-later line.
					menus, later, _ := strings.Cut(plain, "change later:")
					compact := strings.Join(strings.Fields(menus), " ")
					codexTag, kimiTag := "your codex default", "your kimi default"
					if rerun {
						codexTag, kimiTag = "current", "current"
					}
					for _, want := range []string{
						"1 gpt-6.1-sol OpenAI Latest workhorse model for coding and everyday work. " + codexTag,
						"3 high " + codexTag,
						"1 default claude's default",
						"1 kimi-code/k3 Moonshot K3, 1M context " + kimiTag,
					} {
						if !strings.Contains(compact, want) {
							t.Errorf("menu lacks %q:\n%s", want, menus)
						}
					}
					style := term.New(caps)
					if !strings.Contains(out, style.Accent(codexTag)) || !strings.Contains(out, style.Accent(kimiTag)) {
						t.Errorf("default/current tag must be Accent: %q", out)
					}
					if strings.Contains(menus, "config.toml") || strings.Contains(compact, "default claude's default current") {
						t.Errorf("redundant tag or config path in menu:\n%s", menus)
					}
					indent := strings.Repeat(" ", term.VisibleWidth("  "+style.Again()+" "))
					if !strings.HasPrefix(later, " run brigada init again\n"+indent+"your own defaults: ~/.codex/config.toml, ~/.kimi-code/config.toml\n") {
						t.Errorf("expected config paths on a separate change-later line:\n%s", later)
					}
					for _, path := range []string{"~/.codex/config.toml", "~/.kimi-code/config.toml"} {
						if strings.Count(plain, path) != 1 {
							t.Errorf("config path must appear once in change-later guidance: %s\n%s", path, plain)
						}
					}
					for _, line := range strings.Split(out, "\n") {
						if term.VisibleWidth(line) >= width {
							t.Errorf("line must be under Width(): %q", line)
						}
					}
					if strings.Contains(out, "ready") {
						t.Fatal("claimed ready before apply")
					}
				})
			}
		}
	}
}

func TestInitRowTagSpacing(t *testing.T) {
	for _, styled := range []bool{false, true} {
		caps := logo.Caps{Width: 100}
		if styled {
			caps.TTY, caps.Color = true, logo.Color16
		}
		style := term.New(caps)
		var b bytes.Buffer
		w := initOutput{Writer: &b, style: style, styled: styled}
		printInitRow(w, "  model  ", "    ", "A description.", "your codex default")
		if want := "  model  " + style.Dim("A description.") + "  " + style.Accent("your codex default") + "\n"; b.String() != want {
			t.Errorf("row = %q, want %q", b.String(), want)
		}
	}
}

func TestInitAgentChangeLaterConfigPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{filepath.Join(home, ".codex"), filepath.Join(home, "custom-codex"), t.TempDir()} {
		t.Setenv("CODEX_HOME", dir)
		sources := map[string]initModelSource{
			"claude": {signIn: harnessProbe{installed: true}},
			"codex":  {signIn: harnessProbe{installed: true}},
			"kimi":   {signIn: harnessProbe{installed: true}},
		}
		wantPath := filepath.Join(dir, "config.toml")
		if rel, err := filepath.Rel(home, dir); err == nil && !strings.HasPrefix(rel, "..") {
			wantPath = filepath.Join("~", rel, "config.toml")
		}
		got := initAgentChangeLater(sources)
		if want := "run brigada init again\nyour own defaults: " + wantPath + ", ~/.kimi-code/config.toml"; got != want {
			t.Errorf("change later = %q", got)
		}
	}
	if got := initAgentChangeLater(map[string]initModelSource{"claude": {signIn: harnessProbe{installed: true}}}); got != "run brigada init again" {
		t.Errorf("Claude guidance = %q", got)
	}
}

func TestInitAgentChangeLaterPresentation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		codex, kimi bool
		unknownHome bool
		defaults    string
	}{
		{name: "neither installed"},
		{name: "codex", codex: true, defaults: "your own default: ~/.codex/config.toml"},
		{name: "kimi", kimi: true, defaults: "your own default: ~/.kimi-code/config.toml"},
		{name: "both", codex: true, kimi: true, defaults: "your own defaults: ~/.codex/config.toml, ~/.kimi-code/config.toml"},
		{name: "unknown paths", codex: true, kimi: true, unknownHome: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.unknownHome {
				home = ""
			}
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", "")
			sources := map[string]initModelSource{
				"claude":   {signIn: harnessProbe{installed: true}},
				"codex":    {signIn: harnessProbe{installed: tc.codex}},
				"kimi":     {signIn: harnessProbe{installed: tc.kimi}},
				"opencode": {signIn: harnessProbe{installed: true}},
			}
			for _, styled := range []bool{false, true} {
				caps := logo.Caps{Width: 80}
				if styled {
					caps.TTY, caps.UTF8, caps.Color = true, true, logo.Color16
				}
				style := term.New(caps)
				var b bytes.Buffer
				w := initOutput{Writer: &b, style: style, styled: styled, utf8: caps.UTF8}
				initChangeLater(w, initAgentChangeLater(sources))
				prefix := "  " + style.Dim(style.Again()+" ")
				want := "\n" + prefix + style.Dim("change later: run brigada init again") + "\n"
				if tc.defaults != "" {
					want += strings.Repeat(" ", term.VisibleWidth(prefix)) + style.Dim(tc.defaults) + "\n"
				}
				if b.String() != want {
					t.Errorf("styled=%t: change later = %q, want %q", styled, b.String(), want)
				}
				for _, line := range strings.Split(b.String(), "\n") {
					if term.VisibleWidth(line) >= 80 {
						t.Errorf("line must be under 80 columns: %q", line)
					}
				}
			}
		})
	}
}

func TestInitNoCheckMenuPresentation(t *testing.T) {
	for _, width := range []int{80, 100} {
		for _, depth := range []logo.Depth{logo.NoColor, logo.Color16, logo.TrueColor} {
			for _, hasProject := range []bool{false, true} {
				t.Run(fmt.Sprintf("width=%d/color=%d/current=%t", width, depth, hasProject), func(t *testing.T) {
					var b bytes.Buffer
					style := term.New(logo.Caps{TTY: true, Color: depth, UTF8: true, Width: width})
					w := initOutput{Writer: &b, style: style, styled: depth != logo.NoColor, utf8: true}
					var candidates []checkCandidate
					if hasProject {
						candidates = []checkCandidate{{Command: "make check", Note: "Makefile: test lint"}}
					}
					entries := initCheckEntries(candidates, "", "", hasProject)
					command, exhausted, err := askInitCheck(w, func(prompt, flags string) (string, error) {
						fmt.Fprintln(w, initPrompt(w, strings.TrimSpace(prompt)))
						return "", nil
					}, entries)
					if err != nil || exhausted || command != "" {
						t.Fatalf("Enter = %q, exhausted=%v, err=%v", command, exhausted, err)
					}
					plain := ansi.ReplaceAllString(b.String(), "")
					tag := "default"
					if hasProject {
						tag = "current"
					}
					if !strings.Contains(plain, "work goes straight to review; add one later  "+tag+"\n") || !strings.Contains(plain, "[-]:") {
						t.Fatalf("wrong no-check default:\n%s", plain)
					}
					if depth != logo.NoColor {
						want := "   " + style.Pointer() + style.Accent(" -") + "  " + style.Bold("no check")
						if !strings.Contains(b.String(), want) {
							t.Errorf("no-check row lacks the selected pointer: %q", b.String())
						}
					}
					for _, line := range strings.Split(b.String(), "\n") {
						if term.VisibleWidth(line) >= width {
							t.Errorf("line reaches %d columns: %q", width, line)
						}
					}
				})
			}
		}
	}
}
