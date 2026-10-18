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

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

const initCodexCatalogFixture = `{"models":[
 {"slug":"gpt-6.1-sol","description":"General-purpose coding model.","visibility":"list","priority":30,"display_name":"never-print-me","private":{"token":"never-print-me"}},
 {"slug":"gpt-5.4-mini","description":"A smaller coding model for quick tasks, with a long description that must wrap within the menu without losing any of its words.","visibility":"list","priority":10},
 {"slug":"gpt-hidden","description":"never-print-me","visibility":"hide","priority":0},
 {"slug":"unrecognized-model","description":"never-print-me","visibility":"list","priority":-1},
 {"slug":"gpt invalid","description":"never-print-me","visibility":"list","priority":0},
 {"slug":"gpt-escape\u001b","description":"never-print-me","visibility":"list","priority":0},
 {"slug":"","description":"never-print-me","visibility":"list","priority":0},
 {"slug":"gpt-5","description":"Earlier coding model.","visibility":"list","priority":10},
 {"slug":"gpt-5","description":"Duplicate entry.","visibility":"list","priority":10}
]}`

const initCodexCatalogCommand = "printf '%s\\n' '" + initCodexCatalogFixture + "'"

func writeInitCodexScript(t *testing.T, rootHelp, debugHelp, modelsHelp, refresh, bundled string) {
	t.Helper()
	script := `#!/bin/sh
printf 'codex %s\n' "$*" >> "$INIT_CALLS"
case "$*" in
 --help) printf '%s\n' '` + rootHelp + `' ;;
 'debug --help') printf '%s\n' '` + debugHelp + `' ;;
 'debug models --help') printf '%s\n' '` + modelsHelp + `' ;;
 'debug models') ` + refresh + ` ;;
 'debug models --bundled') ` + bundled + ` ;;
 'login --help') printf '  status  Authentication status\n' ;;
 'login status') printf 'Logged in\n' ;;
 *) exit 89 ;;
esac
`
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "codex"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

func fakeInitCodexCatalog(t *testing.T) {
	t.Helper()
	writeInitCodexScript(t, "  debug  Diagnostic tools\n  login  Authentication", "  models  Render the raw model catalog as JSON", "      --bundled  Skip refresh", initCodexCatalogCommand, "exit 89")
}

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
		{id: 2, name: "opus", value: "opus", note: "strong; uses your plan's limits faster"},
		{id: 3, name: "haiku", value: "haiku", note: "fastest; fine for small tasks"},
	}
	if !reflect.DeepEqual(got.entries, want) || !got.signIn.installed || got.signIn.signedIn {
		t.Fatalf("source = %+v", got)
	}
}

func fakeInitClaudeHelp(t *testing.T) {
	t.Helper()
	fakeInitPATH(t, "claude")
	script := `#!/bin/sh
printf 'claude %s\n' "$*" >> "$INIT_CALLS"
case "$*" in
 --help) printf '%s\n' "  --model <model>  Model for the current session. Provide an alias for the latest" "                   model (e.g. 'fable', 'opus', or 'sonnet') or a model's full name." "  --other  something" ;;
 *) exit 89 ;;
esac
`
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestInitClaudeModelsFromCLIAndAccount(t *testing.T) {
	fakeInitClaudeHelp(t)
	home := os.Getenv("HOME")
	writeInitModelFixture(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"token":"never-print-me"},"additionalModelOptionsCache":[
 {"value":"claude-fable-5-1[1m]","label":"Fable","description":"Fable 5.1 · Most capable"},
 {"value":"opus","label":"Opus","description":"already an alias"}]}`)
	got := initClaudeModels(context.Background())
	want := []initModelEntry{
		{id: 1, name: "fable", value: "fable", note: "Fable 5.1 · Most capable"},
		{id: 2, name: "opus", value: "opus", note: "already an alias"},
		{id: 3, name: "sonnet", value: "sonnet", note: "balanced; the usual choice"},
		{id: 4, name: "haiku", value: "haiku", note: "fastest; fine for small tasks"},
		{id: 5, name: "claude-fable-5-1[1m]", value: "claude-fable-5-1[1m]", note: "Fable: Fable 5.1 · Most capable"},
	}
	if !reflect.DeepEqual(got.entries, want) || got.defaultID != 3 {
		t.Fatalf("source = %+v", got)
	}
	assertInitModelPrivacy(t, got)
	if family, err := initModelFamily("claude", got.entries[4].value); err != nil || family != "anthropic" {
		t.Fatalf("family = %q, %v", family, err)
	}
}

func TestInitClaudeSettingsDefault(t *testing.T) {
	fakeInitClaudeHelp(t)
	writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"), `{"model":"opus","env":{"K":"never-print-me"}}`)
	got := initClaudeModels(context.Background())
	if got.defaultID != 2 || !got.entries[1].isDefault || got.entries[1].note != "your claude default; strong; uses your plan's limits faster" {
		t.Fatalf("source = %+v", got)
	}
	assertInitModelPrivacy(t, got)
}

func TestInitClaudeAliasesFallback(t *testing.T) {
	if got := initClaudeAliases("no model row here"); !reflect.DeepEqual(got, []string{"sonnet", "opus", "haiku"}) {
		t.Fatalf("aliases = %v", got)
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

func TestInitCodexCatalog(t *testing.T) {
	// Runner tests can leave the process in a removed state directory; a shell
	// warning on startup would otherwise contaminate the fake CLI's JSON.
	t.Chdir(t.TempDir())
	for _, model := range []string{"gpt-6.1-sol", "gpt-config-only", ""} {
		t.Run("default="+model, func(t *testing.T) {
			fakeInitPATH(t, "codex")
			fakeInitCodexCatalog(t)
			path := filepath.Join(os.Getenv("HOME"), ".codex", "config.toml")
			writeInitModelFixture(t, path, "model = \""+model+"\"\n")
			got := initCodexModels(context.Background())
			note := "your codex default"
			if model != "" {
				note += " (" + path + ")"
			}
			if model == "gpt-6.1-sol" {
				note += "; General-purpose coding model."
			}
			want := []initModelEntry{
				{id: 1, name: model, value: "default", isDefault: true, note: note},
				{id: 2, name: "gpt-5", value: "gpt-5", note: "Earlier coding model."},
				{id: 3, name: "gpt-5.4-mini", value: "gpt-5.4-mini", note: "A smaller coding model for quick tasks, with a long description that must wrap within the menu without losing any of its words."},
			}
			if model != "gpt-6.1-sol" {
				want = append(want, initModelEntry{id: 4, name: "gpt-6.1-sol", value: "gpt-6.1-sol", note: "General-purpose coding model."})
			}
			if !reflect.DeepEqual(got.entries, want) || got.unknownCount != 1 {
				t.Fatalf("source = %+v; want entries %+v and 1 unknown", got, want)
			}
			assertInitModelPrivacy(t, got)
		})
	}
}

func TestInitCodexDiscoverySafety(t *testing.T) {
	t.Chdir(t.TempDir())
	const root = "  debug  Diagnostic tools"
	const debug = "  models  Render the raw model catalog as JSON"
	const bundledHelp = "      --bundled  Skip refresh"
	const catalog = `printf '%s\n' '{"models":[{"slug":"gpt-5","visibility":"list","priority":1,"description":"Coding model.","display_name":"never-print-me"}]}'`
	const probeCalls = "codex --help\ncodex --help\n"
	const discoveryCalls = probeCalls + "codex debug --help\ncodex debug models\n"
	for _, tc := range []struct {
		name, root, debug, help, refresh, bundled string
		wantCatalog                               bool
		wantCalls                                 string
	}{
		{"no debug row", "Commands: no verified commands here", debug, bundledHelp, "exit 89", "exit 89", false, probeCalls},
		{"debug prose", "Use debug for diagnostics", debug, bundledHelp, "exit 89", "exit 89", false, probeCalls},
		{"debug help failure", root, "", bundledHelp, "exit 89", "exit 89", false, probeCalls + "codex debug --help\n"},
		{"models prose", root, "Use models to see the catalog", bundledHelp, "exit 89", "exit 89", false, probeCalls + "codex debug --help\n"},
		{"success", root, debug, bundledHelp, catalog, "exit 89", true, discoveryCalls},
		{"bundled after error", root, debug, bundledHelp, "printf never-print-me; exit 1", catalog, true, discoveryCalls + "codex debug models --help\ncodex debug models --bundled\n"},
		{"bundled after malformed JSON", root, debug, bundledHelp, "printf never-print-me", catalog, true, discoveryCalls + "codex debug models --help\ncodex debug models --bundled\n"},
		{"both fail", root, debug, bundledHelp, "exit 1", "exit 1", false, discoveryCalls + "codex debug models --help\ncodex debug models --bundled\n"},
		{"bundled malformed JSON", root, debug, bundledHelp, "exit 1", "printf never-print-me", false, discoveryCalls + "codex debug models --help\ncodex debug models --bundled\n"},
		{"no bundled option", root, debug, "Options: other options", "exit 1", "exit 89", false, discoveryCalls + "codex debug models --help\n"},
		{"bundled prose", root, debug, "Use --bundled to skip refresh", "exit 1", "exit 89", false, discoveryCalls + "codex debug models --help\n"},
		{"bundled after timeout", root, debug, bundledHelp, "exec /bin/sleep 10", catalog, true, discoveryCalls + "codex debug models --help\ncodex debug models --bundled\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeInitPATH(t, "codex")
			writeInitCodexScript(t, tc.root, tc.debug, tc.help, tc.refresh, tc.bundled)
			start := time.Now()
			got := initCodexModels(context.Background())
			want := []initModelEntry{{id: 1, value: "default", note: "your codex default", isDefault: true}}
			if tc.wantCatalog {
				want = append(want, initModelEntry{id: 2, name: "gpt-5", value: "gpt-5", note: "Coding model."})
			}
			if !reflect.DeepEqual(got.entries, want) || time.Since(start) > 6*time.Second {
				t.Fatalf("source = %+v after %s; want %+v", got, time.Since(start), want)
			}
			assertInitModelPrivacy(t, got)
			data, err := os.ReadFile(calls)
			if err != nil || string(data) != tc.wantCalls {
				t.Fatalf("calls = %q, %v; want %q", data, err, tc.wantCalls)
			}
		})
	}
}

func TestInitCodexCatalogPicks(t *testing.T) {
	for _, pick := range []struct{ answer, model string }{{"1", "default"}, {"3", "gpt-5.4-mini"}} {
		t.Run(pick.answer, func(t *testing.T) {
			importRepo(t)
			fakeInitPATH(t, "claude", "codex")
			fakeInitCodexCatalog(t)
			writeInitModelFixture(t, filepath.Join(os.Getenv("HOME"), ".codex", "config.toml"), "model = \"gpt-6.1-sol\"\n")
			state := testsupport.ShortStateDir(t)
			// --foreman must exactly match the stored model, so this also verifies
			// entry 1 keeps "default" while other picks store their catalog slug.
			code, out, errOut := initDo(t, state, "1\n"+pick.answer+"\nn\n", "--foreman", "codex/"+pick.model, "--signed-in", "claude", "--check", "true", "--tools", "none", "--no-wiring")
			if code != 1 || errOut != "" || !strings.Contains(out, "Declined") {
				t.Fatalf("init = %d\n%s\n%s", code, out, errOut)
			}
			menu := strings.Split(strings.Split(out, "Which model should codex run?\n")[1], "\nStep 2")[0]
			if strings.Contains(out, "never-print-me") || !strings.Contains(menu, "your codex default (~/.codex/config.toml);") || !strings.Contains(menu, "1 more from `codex debug models` aren't listed:") || !strings.Contains(menu, "Pick 1-3 or -, or type a model id [1]:") {
				t.Fatalf("unexpected menu:\n%s", menu)
			}
			for _, line := range strings.Split(out, "\n") {
				if len([]rune(line)) >= 80 {
					t.Errorf("menu line must be under 80 columns: %q", line)
				}
			}
			beforeApply := strings.Split(out, "Write ")[0]
			if strings.Contains(beforeApply, "ready") {
				t.Fatalf("claimed ready before apply:\n%s", beforeApply)
			}
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

func TestInitKimiModelNotes(t *testing.T) {
	fakeInitPATH(t, "kimi")
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeInitModelFixture(t, filepath.Join(home, ".kimi-code", "config.toml"), `default_model = "kimi-code/k3"
[providers."kimi-code"]
api_key = "never-print-me"
display_name = "never-print-me"
max_context_size = 7
[models."kimi-code/k3"]
provider = "kimi-code"
display_name = "K3"
max_context_size = 1048576
[models."kimi-code/k2.8-preview"]
display_name = "K2.8 Preview" # shown
max_context_size = 262144
oauth = "never-print-me"
[models."kimi-code/no-name"]
max_context_size = 200000
[models."kimi-code/bare"]
provider = "never-print-me"
[models."kimi-code/no-size"]
display_name = "Only Name"
`)
	got := initKimiModels(context.Background())
	want := []initModelEntry{
		{id: 1, name: "kimi-code/k3", value: "default", note: "your kimi default; K3, 1M context", isDefault: true},
		{id: 2, name: "kimi-code/k2.8-preview", value: "kimi-code/k2.8-preview", note: "K2.8 Preview, 256K context"},
		{id: 3, name: "kimi-code/no-name", value: "kimi-code/no-name", note: "256K context"},
		{id: 4, name: "kimi-code/bare", value: "kimi-code/bare"},
		{id: 5, name: "kimi-code/no-size", value: "kimi-code/no-size", note: "Only Name"},
	}
	if !reflect.DeepEqual(got.entries, want) {
		t.Fatalf("entries = %+v", got.entries)
	}
	assertInitModelPrivacy(t, got)
}

func TestInitContextSize(t *testing.T) {
	for n, want := range map[int64]string{
		0: "", -5: "", 1048576: "1M", 262144: "256K", 131072: "128K", 200000: "256K",
		32768: "32K", 2097152: "2M", 1000: "1K", 10: "1K",
	} {
		if got := initContextSize(n); got != want {
			t.Errorf("initContextSize(%d) = %q, want %q", n, got, want)
		}
	}
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
