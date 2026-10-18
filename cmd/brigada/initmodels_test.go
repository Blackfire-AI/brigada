package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeInitModelFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertInitModelPrivacy(t *testing.T, source initModelSource) {
	t.Helper()
	if strings.Contains(fmt.Sprintf("%+v", source), "never-print-me") {
		t.Fatal("model source retained unrelated secret or authentication output")
	}
}

func TestInitClaudeModelSource(t *testing.T) {
	fakeInitPATH(t, "claude")
	got := initClaudeModels(context.Background())
	want := []initModelEntry{
		{id: 1, name: "sonnet", value: "sonnet", note: "balanced; the usual choice"},
		{id: 2, name: "opus", value: "opus", note: "strongest; uses your Claude plan's limits faster"},
		{id: 3, name: "haiku", value: "haiku", note: "fastest; fine for small tasks"},
	}
	if !reflect.DeepEqual(got.entries, want) || !got.signIn.installed || got.signIn.signedIn {
		t.Fatalf("source = %+v", got)
	}
}

func TestInitCodexModelSource(t *testing.T) {
	for _, location := range []string{"home", "CODEX_HOME"} {
		t.Run(location, func(t *testing.T) {
			fakeInitPATH(t, "codex")
			home := t.TempDir()
			t.Setenv("HOME", home)
			configDir := filepath.Join(home, ".codex")
			if location == "CODEX_HOME" {
				configDir = t.TempDir()
				t.Setenv("CODEX_HOME", configDir)
				writeInitModelFixture(t, filepath.Join(home, ".codex", "config.toml"), "model = \"wrong-home-model\"\n")
			}
			path := filepath.Join(configDir, "config.toml")
			writeInitModelFixture(t, path, `api_key = "never-print-me"
# model = "commented-model"
model = "runtime-codex-model" # selected default
secret = """
model = "never-print-me"
[private]
"""
[profiles.private]
model = "never-print-me"
token = "never-print-me"
`)
			got := initCodexModels(context.Background())
			want := []initModelEntry{{id: 1, name: "runtime-codex-model", value: "default", note: "your codex default (" + path + ")", isDefault: true}}
			if !reflect.DeepEqual(got.entries, want) || !got.signIn.installed {
				t.Fatalf("source = %+v", got)
			}
			assertInitModelPrivacy(t, got)
		})
	}
}

func TestInitKimiModelSource(t *testing.T) {
	fakeInitPATH(t, "kimi")
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeInitModelFixture(t, filepath.Join(home, ".kimi-code", "config.toml"), `api_key = "never-print-me"
default_model = "runtime-provider/current"
secret = '''
default_model = "never-print-me"
[models."never-print-me"]
'''
[models."runtime-provider/first"]
api_key = "never-print-me"
model = "never-print-me"
[models."runtime-provider/current"]
token = "never-print-me"
[models."runtime-provider/last"] # keep file order
[models."runtime-provider/last"]
[other]
default_model = "never-print-me"
`)
	got := initKimiModels(context.Background())
	want := []initModelEntry{
		{id: 1, name: "runtime-provider/current", value: "default", note: "your kimi default", isDefault: true},
		{id: 2, name: "runtime-provider/first", value: "runtime-provider/first"},
		{id: 3, name: "runtime-provider/last", value: "runtime-provider/last"},
	}
	if !reflect.DeepEqual(got.entries, want) || !got.signIn.installed || got.signIn.signedIn {
		t.Fatalf("source = %+v", got)
	}
	assertInitModelPrivacy(t, got)
}

func TestInitModelSourceFallbacks(t *testing.T) {
	for _, tc := range []struct {
		harness, dir, body string
		load               func(context.Context) initModelSource
	}{
		{"codex", ".codex", "", initCodexModels},
		{"kimi", ".kimi-code", "", initKimiModels},
		{"codex", ".codex", "api_key = \"never-print-me\"\n[profile]\nmodel = \"never-print-me\"\n", initCodexModels},
		{"kimi", ".kimi-code", "token = \"never-print-me\"\n[other]\ndefault_model = \"never-print-me\"\n", initKimiModels},
	} {
		t.Run(tc.harness+tc.body, func(t *testing.T) {
			fakeInitPATH(t, tc.harness)
			home := t.TempDir()
			t.Setenv("HOME", home)
			if tc.body != "" {
				writeInitModelFixture(t, filepath.Join(home, tc.dir, "config.toml"), tc.body)
			}
			got := tc.load(context.Background())
			want := []initModelEntry{{id: 1, value: "default", note: "your " + tc.harness + " default", isDefault: true}}
			if !reflect.DeepEqual(got.entries, want) {
				t.Fatalf("entries = %+v", got.entries)
			}
			assertInitModelPrivacy(t, got)
		})
	}
}

func TestInitModelConfigStrings(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`model = 'literal-model' # comment`, "literal-model"},
		{`model = "escaped\u002dmodel"`, "escaped-model"},
		{`model = "unterminated`, ""},
		{`model = """multiline`, ""},
		{`model = "line\nbreak"`, ""},
		{`model = "model" trailing`, ""},
		{`model = "\u001bsecret"`, ""},
	} {
		t.Run(tc.body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeInitModelFixture(t, path, tc.body)
			if got, _ := readInitModelConfig(path, "model", false); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInitOpenCodeModelSource(t *testing.T) {
	calls := fakeInitPATH(t, "opencode")
	got := initOpenCodeModels(context.Background())
	want := []initModelEntry{
		{id: 1, name: "fixture/gpt-5", value: "fixture/gpt-5"},
		{id: 2, name: "fixture/glm-5.3", value: "fixture/glm-5.3"},
	}
	if !reflect.DeepEqual(got.entries, want) || !got.signIn.signedIn || got.providerCount != 2 || got.signIn.status != "signed in to 2 providers (opencode providers list)" {
		t.Fatalf("source = %+v", got)
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "opencode --help\nopencode models\nopencode providers list\n" {
		t.Fatalf("unexpected calls %q: %v", data, err)
	}
}

func TestInitOpenCodeDiscoverySafety(t *testing.T) {
	for _, tc := range []struct {
		name, help, models, auth string
		wantEntries, wantCount   int
		wantCalls                string
	}{
		{"unverified prose", "Use models or auth to inspect your setup", "exit 89", "exit 89", 0, 0, "opencode --help\n"},
		{"models only", "  opencode models [provider]", "printf 'fixture/gpt-5\\nfixture/gpt-5\\ninvalid row\\n/providerless\\nprovider/\\n'", "exit 89", 1, 0, "opencode --help\nopencode models\n"},
		{"auth only", "  opencode auth Manage credentials", "exit 89", "printf '● never-print-me oauth\\n'", 0, 1, "opencode --help\nopencode auth list\n"},
		{"signed out", "  opencode models [provider]\n  opencode auth Manage credentials", "printf 'fixture/gpt-5\\n'", "printf '┌ Credentials /never-print-me\\n└ 0 credentials\\n'", 1, 0, "opencode --help\nopencode models\nopencode auth list\n"},
		{"model error", "  opencode models [provider]\n  opencode auth Manage credentials", "printf 'never-print-me'; exit 1", "printf '● never-print-me oauth\\n'", 0, 1, "opencode --help\nopencode models\nopencode auth list\n"},
		{"auth error", "  opencode models [provider]\n  opencode auth Manage credentials", "printf 'fixture/gpt-5\\n'", "printf '● never-print-me oauth\\n'; exit 1", 0, 0, "opencode --help\nopencode models\nopencode auth list\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeInitPATH(t, "opencode")
			script := "#!/bin/sh\nprintf 'opencode %s\\n' \"$*\" >> \"$INIT_CALLS\"\ncase \"$*\" in\n --help) printf '%s\\n' '" + tc.help + "' ;;\n models) " + tc.models + " ;;\n 'auth list') " + tc.auth + " ;;\n *) exit 89 ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "opencode"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			got := initOpenCodeModels(context.Background())
			if len(got.entries) != tc.wantEntries || got.providerCount != tc.wantCount || got.signIn.signedIn != (tc.wantCount > 0) {
				t.Fatalf("source = %+v", got)
			}
			assertInitModelPrivacy(t, got)
			data, err := os.ReadFile(calls)
			if err != nil || string(data) != tc.wantCalls {
				t.Fatalf("calls = %q, %v; want %q", data, err, tc.wantCalls)
			}
		})
	}
}

func TestInitOpenCodeDiscoveryTimeout(t *testing.T) {
	for _, command := range []string{"models", "auth list"} {
		t.Run(command, func(t *testing.T) {
			fakeInitPATH(t, "opencode")
			script := "#!/bin/sh\ncase \"$*\" in\n --help) printf '  opencode models [provider]\\n  opencode auth Manage credentials\\n' ;;\n '" + command + "') exec /bin/sleep 10 ;;\n models) printf 'fixture/gpt-5\\n' ;;\n 'auth list') printf '● never-print-me oauth\\n' ;;\n *) exit 89 ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "opencode"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			got := initOpenCodeModels(context.Background())
			if len(got.entries) != 0 || time.Since(start) > 6*time.Second {
				t.Fatalf("timeout did not produce a cheap typed-model fallback: %+v (%s)", got, time.Since(start))
			}
			assertInitModelPrivacy(t, got)
		})
	}
}

func TestInitHelpNamesHarnessCommand(t *testing.T) {
	for _, tc := range []struct {
		help, command string
		want          bool
	}{
		{"  opencode models [provider]", "models", true},
		{"  opencode auth Manage credentials", "auth", true},
		{"  models [provider]", "models", true},
		{"  opencode models-extra [provider]", "models", false},
		{"Use opencode models to list models", "models", false},
		{"  --models model option", "models", false},
	} {
		if got := helpNamesHarnessCommand(tc.help, "opencode", tc.command); got != tc.want {
			t.Errorf("help %q, command %q = %v", tc.help, tc.command, got)
		}
	}
	if got := initOpenCodeProviderCount("┌ Credentials /never-print-me\n│\n\x1b[32m●\x1b[0m Provider oauth\n│\n● Another api\n└ 2 credentials\n"); got != 2 {
		t.Fatalf("colored provider count = %d", got)
	}
}
