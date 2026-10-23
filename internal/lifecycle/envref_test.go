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
