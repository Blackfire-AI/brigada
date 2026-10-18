package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

const openCodeVerboseFixture = `zai-coding-plan/glm-5.3
{
  "id": "glm-5.3",
  "name": "GLM-5.3",
  "family": "never-print-me-family",
  "status": "active",
  "provider": {"api": "never-print-me-api", "npm": "never-print-me-npm"},
  "options": {"key": "never-print-me-option"},
  "cost": {"input": 0, "output": 0, "cache_read": 0},
  "limit": {"context": 262144, "output": 32768}
}
opencode/mimo-v2-flash-free
{
  "name": "MiMo V2 Flash Free",
  "family": "mimo",
  "status": "active",
  "cost": {"input": 0, "output": 0},
  "limit": {"context": 262144}
}
opencode/longcat-flash-chat
{
  "name": "LongCat Old",
  "family": "longcat",
  "status": "deprecated",
  "cost": {"input": 0, "output": 0},
  "limit": {"context": 131072}
}
opencode/nemotron-3-super-free
{
  "name": "Nemotron 3 Super Free",
  "family": "nemotron-free",
  "status": "active",
  "cost": {"input": 0, "output": 0.5},
  "limit": {"context": 131072}
}
opencode/beam-free
{
  "name": "Beam Free",
  "family": "beam",
  "status": "active",
  "cost": {"input": 0, "output": 0},
  "limit": {"context": 131072}
}
`

const openCodeVerboseHelp = `  --verbose  use more verbose model output (includes metadata like costs)\\n`

// fakeOpenCodeVerbose installs an opencode whose verbose listing is the shell
// snippet verbose and whose plain listing is two fixture models.
func fakeOpenCodeVerbose(t *testing.T, modelsHelp, verbose string) string {
	t.Helper()
	calls := fakeInitPATH(t, "opencode")
	script := "#!/bin/sh\nprintf 'opencode %s\\n' \"$*\" >> \"$INIT_CALLS\"\ncase \"$*\" in\n" +
		" --help) printf '  opencode models [provider]\\n  opencode providers   manage AI providers   [aliases: auth]\\n' ;;\n" +
		" 'models --help') printf '" + modelsHelp + "' ;;\n" +
		" 'models --verbose') " + verbose + " ;;\n" +
		" models) printf 'fixture/gpt-5\\nfixture/glm-5.3\\n' ;;\n" +
		" 'providers list') printf '● Provider one oauth\\n' ;;\n" +
		" *) exit 89 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "opencode"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return calls
}

func openCodeVerboseFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verbose.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return "/bin/cat " + path // PATH holds only the fakes.
}

func TestInitOpenCodeVerboseModels(t *testing.T) {
	calls := fakeOpenCodeVerbose(t, openCodeVerboseHelp, openCodeVerboseFile(t, openCodeVerboseFixture))
	got := initOpenCodeModels(context.Background())
	want := []initModelEntry{
		{id: 1, name: "zai-coding-plan/glm-5.3", value: "zai-coding-plan/glm-5.3", note: "GLM-5.3, 256K context"},
		{id: 2, name: "opencode/mimo-v2-flash-free", value: "opencode/mimo-v2-flash-free", note: "MiMo V2 Flash Free, 256K context"},
		{id: 3, name: "opencode/nemotron-3-super-free", value: "opencode/nemotron-3-super-free", note: "Nemotron 3 Super Free, 128K context"},
	}
	if !reflect.DeepEqual(got.entries, want) {
		t.Fatalf("entries = %+v, want %+v", got.entries, want)
	}
	if got.unknownCount != 1 {
		t.Errorf("unknownCount = %d, want 1 (beam-free)", got.unknownCount)
	}
	if s := fmt.Sprintf("%+v", got); strings.Contains(s, "never-print-me") {
		t.Errorf("unparsed field reached the source: %s", s)
	}
	data, _ := os.ReadFile(calls)
	if string(data) != "opencode --help\nopencode models --help\nopencode models --verbose\nopencode providers list\n" {
		t.Errorf("calls = %q", data)
	}
}

func TestInitOpenCodeVerboseFallsBackToPlainList(t *testing.T) {
	const verboseCalls = "opencode --help\nopencode models --help\nopencode models --verbose\nopencode models\nopencode providers list\n"
	for _, tc := range []struct {
		name, help, verbose, wantCalls string
	}{
		{"parse error", openCodeVerboseHelp, `printf 'zai-coding-plan/glm-5.3\n{"name": "broken\n'`, verboseCalls},
		{"row without object", openCodeVerboseHelp, `printf 'zai-coding-plan/glm-5.3\nno json here\n'`, verboseCalls},
		{"verbose fails", openCodeVerboseHelp, `printf 'never-print-me'; exit 3`, verboseCalls},
		{"flag not documented", `  --provider  a provider\\n`, "exit 89", "opencode --help\nopencode models --help\nopencode models\nopencode providers list\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeOpenCodeVerbose(t, tc.help, tc.verbose)
			got := initOpenCodeModels(context.Background())
			want := []initModelEntry{
				{id: 1, name: "fixture/gpt-5", value: "fixture/gpt-5"},
				{id: 2, name: "fixture/glm-5.3", value: "fixture/glm-5.3"},
			}
			if !reflect.DeepEqual(got.entries, want) {
				t.Fatalf("entries = %+v, want the plain list %+v", got.entries, want)
			}
			data, _ := os.ReadFile(calls)
			if string(data) != tc.wantCalls {
				t.Errorf("calls = %q, want %q", data, tc.wantCalls)
			}
		})
	}
}

func TestInitOpenCodeVerboseMenu(t *testing.T) {
	wide := strings.Replace(openCodeVerboseFixture, `"GLM-5.3"`, `"GLM-5.3 with a deliberately long display name to force the note to wrap"`, 1)
	importRepo(t)
	fakeInitPATH(t, "codex", "opencode")
	fakeOpenCodeVerbose2(t, openCodeVerboseFile(t, wide))
	args := []string{"--signed-in", "codex", "--accept-limited", "--check", "true", "--tools", "none", "--no-wiring"}
	code, out, errOut := initDo(t, testsupport.ShortStateDir(t), "1\n-\nn\n", args...)
	if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
		t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
	}
	menu := strings.Split(strings.Split(out, "Which model should opencode run?\n")[1], "    Pick ")[0]
	for _, line := range strings.Split(menu, "\n") {
		if utf8.RuneCountInString(line) >= 80 {
			t.Errorf("menu line is %d columns: %q", utf8.RuneCountInString(line), line)
		}
	}
	flat := strings.Join(strings.Fields(menu), " ") // rows wrap and align
	for _, want := range []string{
		"zai-coding-plan/glm-5.3 Zhipu (GLM) GLM-5.3 with a deliberately long display name to force the note to wrap, 256K context",
		"opencode/mimo-v2-flash-free Xiaomi MiMo V2 Flash Free, 256K context",
		"opencode/nemotron-3-super-free NVIDIA Nemotron 3 Super Free, 128K context",
		"1 more from `opencode models` aren't listed",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("menu lacks %q:\n%s", want, menu)
		}
	}
	for _, bad := range []string{"longcat", "LongCat", "beam-free", "never-print-me"} {
		if strings.Contains(menu, bad) {
			t.Errorf("menu shows %q:\n%s", bad, menu)
		}
	}
	// Notes start in the same column even when family names have different widths.
	noteColumn := -1
	for _, line := range strings.Split(menu, "\n") {
		for _, name := range []string{"GLM-5.3", "MiMo V2", "Nemotron 3"} {
			if column := strings.Index(line, name); column >= 0 {
				if noteColumn >= 0 && column != noteColumn {
					t.Errorf("note column = %d, want %d: %q", column, noteColumn, line)
				}
				noteColumn = column
			}
		}
	}
	rows := regexp.MustCompile(`(?m)^\s+(\d+)  (\S+)  `).FindAllStringSubmatch(menu, -1)
	if len(rows) != 3 {
		t.Fatalf("numbered rows = %q\n%s", rows, menu)
	}
	for _, row := range rows {
		t.Run(row[2], func(t *testing.T) {
			code, out, errOut := initDo(t, testsupport.ShortStateDir(t), "1\n"+row[1]+"\nn\n", args...)
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") || strings.Contains(out, "try again") {
				t.Fatalf("numbered entry %s rejected: %d\n%s\n%s", row[1], code, out, errOut)
			}
		})
	}
}

// fakeOpenCodeVerbose2 overwrites the opencode already on PATH, keeping the
// PATH and call log that fakeInitPATH set up for the other harnesses.
func fakeOpenCodeVerbose2(t *testing.T, verbose string) {
	t.Helper()
	script := "#!/bin/sh\nprintf 'opencode %s\\n' \"$*\" >> \"$INIT_CALLS\"\ncase \"$*\" in\n" +
		" --help) printf '  opencode models [provider]\\n  opencode providers   manage AI providers   [aliases: auth]\\n' ;;\n" +
		" 'models --help') printf '" + openCodeVerboseHelp + "' ;;\n" +
		" 'models --verbose') " + verbose + " ;;\n" +
		" 'providers list') printf '● Provider one oauth\\n● Provider two api\\n' ;;\n" +
		" *) exit 89 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "opencode"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}
