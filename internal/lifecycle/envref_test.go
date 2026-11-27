package lifecycle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeValue = "value-from-file"

func writeRefFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ref")
	if err := os.WriteFile(path, []byte(fakeValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveHarnessEnvFile(t *testing.T) {
	path := writeRefFile(t, 0o600)
	refs := map[string]map[string]string{"dsh": {"SOME_VAR": "file:" + path}}
	got, err := ResolveHarnessEnv(refs, []string{"dsh"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["dsh"]) != 1 || got["dsh"][0] != "SOME_VAR="+fakeValue {
		t.Errorf("resolved = %v", got)
	}
}

func TestResolveHarnessEnvFileModeRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o400, 0o666} {
		path := writeRefFile(t, mode)
		refs := map[string]map[string]string{"dsh": {"SOME_VAR": "file:" + path}}
		_, err := ResolveHarnessEnv(refs, []string{"dsh"}, nil)
		if err == nil {
			t.Fatalf("mode %04o accepted", mode)
		}
		if !strings.Contains(err.Error(), "0600") || strings.Contains(err.Error(), fakeValue) {
			t.Errorf("mode %04o error = %q", mode, err)
		}
	}
}

func TestResolveHarnessEnvMissingFile(t *testing.T) {
	refs := map[string]map[string]string{"dsh": {"SOME_VAR": "file:/no/such/file"}}
	if _, err := ResolveHarnessEnv(refs, []string{"dsh"}, nil); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestResolveHarnessEnvKeychain(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte("value-from-keychain\n"), nil
	}
	refs := map[string]map[string]string{"dsh": {"SOME_VAR": "keychain:brigada-item"}}
	got, err := ResolveHarnessEnv(refs, []string{"dsh"}, run)
	if err != nil {
		t.Fatal(err)
	}
	if got["dsh"][0] != "SOME_VAR=value-from-keychain" {
		t.Errorf("resolved = %v", got)
	}
	if gotName != "security" || !strings.Contains(strings.Join(gotArgs, " "), "-s brigada-item") {
		t.Errorf("ran %s %v", gotName, gotArgs)
	}
}

func TestResolveHarnessEnvKeychainFailureHasNoValue(t *testing.T) {
	run := func(string, ...string) ([]byte, error) {
		return []byte("value-from-keychain"), errors.New("exit 44 value-from-keychain")
	}
	refs := map[string]map[string]string{"dsh": {"SOME_VAR": "keychain:brigada-item"}}
	_, err := ResolveHarnessEnv(refs, []string{"dsh"}, run)
	if err == nil {
		t.Fatal("failure swallowed")
	}
	if strings.Contains(err.Error(), "value-from-keychain") {
		t.Errorf("error carries the value: %q", err)
	}
	if !strings.Contains(err.Error(), "brigada-item") {
		t.Errorf("error does not name the item: %q", err)
	}
}

func TestResolveHarnessEnvScopedToHarness(t *testing.T) {
	path := writeRefFile(t, 0o600)
	refs := map[string]map[string]string{
		"dsh":   {"SOME_VAR": "file:" + path},
		"codex": {"OTHER_VAR": "file:/no/such/file"},
	}
	got, err := ResolveHarnessEnv(refs, []string{"dsh"}, nil)
	if err != nil {
		t.Fatalf("a harness not offered was resolved: %v", err)
	}
	if _, ok := got["codex"]; ok || len(got["dsh"]) != 1 {
		t.Errorf("resolved = %v", got)
	}
}

func TestConfigEnvKeys(t *testing.T) {
	dir := t.TempDir()
	write := func(line string) string {
		p := filepath.Join(dir, "config")
		if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := LoadConfigFile(write("env.dsh.SOME_VAR = keychain:brigada-item"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env["dsh"]["SOME_VAR"] != "keychain:brigada-item" {
		t.Errorf("env = %v", cfg.Env)
	}
	cfg, err = LoadConfigFile(write("env.qwen-code.SOME_VAR = keychain:brigada-item"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env["qwen-code"]["SOME_VAR"] != "keychain:brigada-item" {
		t.Errorf("env = %v", cfg.Env)
	}
	// A value in place of a reference, an unknown harness or a bad name is
	// refused, and the refusal never echoes the value.
	for _, line := range []string{
		"env.dsh.SOME_VAR = literal-not-a-reference",
		"env.nobody.SOME_VAR = keychain:x",
		"env.dsh.bad-name = keychain:x",
		"env.dsh.SOME_VAR = file:",
		"env.dsh.PWD = keychain:x",
		"env.dsh.DSH_PERMISSION_MODE = keychain:x",
		"env.dsh.BRIGADA_TOKEN = keychain:x",
		"env.dsh.DSH_TELEMETRY_MODE = keychain:x",
		"env.opencode.OPENCODE_PERMISSION = keychain:x",
		"env.opencode.OPENCODE_DISABLE_PROJECT_CONFIG = keychain:x",
	} {
		_, err := LoadConfigFile(write(line))
		if err == nil {
			t.Errorf("%q accepted", line)
			continue
		}
		if strings.Contains(err.Error(), "literal-not-a-reference") {
			t.Errorf("%q: error echoes the value: %v", line, err)
		}
	}
}

// TestConfigTrackerTokenKeys covers tracker.<host>.token: the reference
// forms are kept as written under the lowercased host[:port], and a
// value, an empty reference or a host that is not one is refused without
// echoing the value.
func TestConfigTrackerTokenKeys(t *testing.T) {
	dir := t.TempDir()
	write := func(lines string) string {
		p := filepath.Join(dir, "config")
		if err := os.WriteFile(p, []byte(lines+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := LoadConfigFile(write("tracker.Git.Example.invalid:3000.token = keychain:brigada-forge\n" +
		"tracker.codeberg.example.token = file:/etc/brigada/forge-token\n" +
		"tracker.[::1]:3000.token = keychain:local"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"git.example.invalid:3000": "keychain:brigada-forge",
		"codeberg.example":         "file:/etc/brigada/forge-token",
		"[::1]:3000":               "keychain:local",
	}
	if len(cfg.TrackerTokens) != len(want) {
		t.Errorf("tracker tokens = %v, want %v", cfg.TrackerTokens, want)
	}
	for host, ref := range want {
		if cfg.TrackerTokens[host] != ref {
			t.Errorf("tracker.%s.token = %q, want %q", host, cfg.TrackerTokens[host], ref)
		}
	}
	if cfg, err := LoadConfigFile(write("notify = none")); err != nil || cfg.TrackerTokens != nil {
		t.Errorf("unset: tracker tokens = %v, %v; want none", cfg.TrackerTokens, err)
	}
	for _, line := range []string{
		"tracker.git.example.invalid.token = literal-not-a-reference",
		"tracker.git.example.invalid.token = keychain:",
		"tracker.git.example.invalid.token = file:",
		"tracker.git.example.invalid.token = env:FORGE_TOKEN",
		"tracker.git.example.invalid = keychain:x",
		"tracker..token = keychain:x",
		"tracker.token = keychain:x",
		"tracker.https://git.example.invalid.token = keychain:x",
		"tracker.user@git.example.invalid.token = keychain:x",
		"tracker.git.example.invalid/forge.token = keychain:x",
		"tracker.git.example.invalid:port.token = keychain:x",
	} {
		_, err := LoadConfigFile(write(line))
		if err == nil {
			t.Errorf("%q accepted", line)
			continue
		}
		if strings.Contains(err.Error(), "literal-not-a-reference") {
			t.Errorf("%q: error echoes the value: %v", line, err)
		}
	}
}

// TestResolveTrackerToken covers the resolver the engine calls at fetch
// time: a host with no key is no token, a file and a keychain reference
// resolve, and a failure names the key and the reference, never a value.
func TestResolveTrackerToken(t *testing.T) {
	path := writeRefFile(t, 0o600)
	refs := map[string]string{
		"git.example.invalid:3000": "file:" + path,
		"keys.example.invalid":     "keychain:brigada-forge",
		"gone.example.invalid":     "file:" + filepath.Join(t.TempDir(), "missing"),
	}
	if tok, ok, err := ResolveTrackerToken(refs, "other.example.invalid", nil); tok != "" || ok || err != nil {
		t.Errorf("unconfigured host: %q, %v, %v", tok, ok, err)
	}
	if tok, ok, err := ResolveTrackerToken(nil, "git.example.invalid:3000", nil); tok != "" || ok || err != nil {
		t.Errorf("no keys: %q, %v, %v", tok, ok, err)
	}
	fileTok, ok, err := ResolveTrackerToken(refs, "Git.Example.invalid:3000", nil)
	if err != nil || !ok || fileTok == "" || strings.ContainsAny(fileTok, "\r\n") {
		t.Fatalf("file reference: %q, %v, %v", fileTok, ok, err)
	}
	run := func(name string, args ...string) ([]byte, error) {
		if name != "security" || args[len(args)-1] != "brigada-forge" {
			t.Errorf("ran %s %v", name, args)
		}
		return []byte("token-from-keychain\n"), nil
	}
	if tok, ok, err := ResolveTrackerToken(refs, "keys.example.invalid", run); tok != "token-from-keychain" || !ok || err != nil {
		t.Errorf("keychain reference: %q, %v, %v", tok, ok, err)
	}
	failing := func(string, ...string) ([]byte, error) {
		return []byte("token-from-keychain"), errors.New("exit 44 token-from-keychain")
	}
	_, _, err = ResolveTrackerToken(refs, "keys.example.invalid", failing)
	if err == nil || !strings.Contains(err.Error(), "tracker.keys.example.invalid.token = keychain:brigada-forge") {
		t.Errorf("keychain failure: err = %v, want it to name the key and the reference", err)
	} else if strings.Contains(err.Error(), "token-from-keychain") {
		t.Errorf("keychain failure echoes a value: %v", err)
	}
	_, _, err = ResolveTrackerToken(refs, "gone.example.invalid", nil)
	if err == nil || !strings.Contains(err.Error(), "tracker.gone.example.invalid.token = file:") {
		t.Errorf("missing file: err = %v, want it to name the key and the reference", err)
	}
}
