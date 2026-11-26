package opencode

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// opencode carries the provider-specific reasoning effort as a #variant
// suffix on -m (`-m provider/model#variant`); variant names depend on the
// provider and model.
func TestEffortMapsToVariant(t *testing.T) {
	stubCLI(t)
	for _, level := range []string{"minimal", "low", "medium", "high", "xhigh", "max", "turbo", "custom-2"} {
		for name, resume := range map[string]string{"fresh": "", "resume": "ses_1"} {
			cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Model: "anthropic/claude", Effort: level}, t.TempDir(), resume)
			if err != nil {
				t.Fatalf("%s %s: %v", name, level, err)
			}
			want := "anthropic/claude#" + level
			if !slices.Contains(cmd.Args, "-m") || !slices.Contains(cmd.Args, want) {
				t.Errorf("%s %s: args %q lack -m %s", name, level, cmd.Args, want)
			}
			if slices.Contains(cmd.Args, "--auto") || slices.Contains(cmd.Args, "--variant") {
				t.Errorf("%s %s: a forbidden flag rode in: %q", name, level, cmd.Args)
			}
		}
	}
}

// A model string carrying its own #variant keeps it when no task effort
// overrides it, and loses it to the task effort when one is set.
func TestEffortOverridesModelVariant(t *testing.T) {
	stubCLI(t)
	withEffort, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Model: "anthropic/claude#minimal", Effort: "high"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(withEffort.Args, "-m") || !slices.Contains(withEffort.Args, "anthropic/claude#high") {
		t.Errorf("a task effort did not replace the model's own variant: %q", withEffort.Args)
	}
	noEffort, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Model: "anthropic/claude#minimal"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(noEffort.Args, "-m") || !slices.Contains(noEffort.Args, "anthropic/claude#minimal") {
		t.Errorf("a model's own variant was lost with no task effort: %q", noEffort.Args)
	}
}

func TestNoEffortAddsNoVariant(t *testing.T) {
	stubCLI(t)
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Model: "anthropic/claude"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	// -m passes the model unchanged, with no #suffix.
	if !slices.Contains(cmd.Args, "-m") || !slices.Contains(cmd.Args, "anthropic/claude") {
		t.Errorf("the model did not ride -m: %q", cmd.Args)
	}
	for _, arg := range cmd.Args {
		if strings.Contains(arg, "#") {
			t.Errorf("an unset effort added a variant suffix: %q", cmd.Args)
		}
	}
}

// writeConfigFile writes content into dir/name, making parent directories
// as needed.
func writeConfigFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// clearConfigEnv points the config search away from any real user config:
// no OPENCODE_CONFIG_DIR, an empty XDG_CONFIG_HOME, and a fresh HOME.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func TestEffortResolvesDefaultModel(t *testing.T) {
	stubCLI(t)
	tests := []struct {
		name    string
		prepare func(t *testing.T) // writes fixture configs and sets env
		wantM   string             // the -m argument; "" means no -m
	}{
		{
			name: "OPENCODE_CONFIG_DIR opencode.json names the default",
			prepare: func(t *testing.T) {
				clearConfigEnv(t)
				dir := t.TempDir()
				writeConfigFile(t, dir, "opencode.json", `{"model":"anthropic/claude"}`)
				t.Setenv("OPENCODE_CONFIG_DIR", dir)
			},
			wantM: "anthropic/claude#high",
		},
		{
			name: "XDG_CONFIG_HOME opencode.jsonc with comments and a URL",
			prepare: func(t *testing.T) {
				clearConfigEnv(t)
				dir := t.TempDir()
				writeConfigFile(t, dir, filepath.Join("opencode", "opencode.jsonc"),
					"{\n  // the CLI's default model\n  \"model\": \"anthropic/claude\",\n  \"endpoint\": \"https://example.invalid/v1//chat\" /* URL survives */\n}\n")
				t.Setenv("XDG_CONFIG_HOME", dir)
			},
			wantM: "anthropic/claude#high",
		},
		{
			name: "a config model carrying a variant is replaced",
			prepare: func(t *testing.T) {
				clearConfigEnv(t)
				dir := t.TempDir()
				writeConfigFile(t, dir, "opencode.json", `{"model":"anthropic/claude#minimal"}`)
				t.Setenv("OPENCODE_CONFIG_DIR", dir)
			},
			wantM: "anthropic/claude#high",
		},
		{
			name: "HOME .config fallback",
			prepare: func(t *testing.T) {
				t.Setenv("OPENCODE_CONFIG_DIR", "")
				home := t.TempDir()
				writeConfigFile(t, home, filepath.Join(".config", "opencode", "opencode.json"), `{"model":"anthropic/claude"}`)
				t.Setenv("XDG_CONFIG_HOME", "")
				t.Setenv("HOME", home)
			},
			wantM: "anthropic/claude#high",
		},
		{
			name: "no config anywhere drops the variant",
			prepare: func(t *testing.T) {
				clearConfigEnv(t)
			},
			wantM: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.prepare(t)
			cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Effort: "high"}, t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantM == "" {
				for _, arg := range cmd.Args {
					if arg == "-m" || strings.Contains(arg, "#") {
						t.Errorf("no model is nameable, yet args carry one: %q", cmd.Args)
					}
				}
				return
			}
			if !slices.Contains(cmd.Args, "-m") || !slices.Contains(cmd.Args, tc.wantM) {
				t.Errorf("args = %q, want -m %s", cmd.Args, tc.wantM)
			}
		})
	}
}

func TestUnsupportedEffortIsRefused(t *testing.T) {
	stubCLI(t)
	for _, level := range []string{"bad level", "UPPER", "bad_variant", "12345678901234567"} {
		_, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Effort: level}, t.TempDir(), "")
		if err == nil || !strings.Contains(err.Error(), level) {
			t.Errorf("level %q: error = %v, want a refusal naming it", level, err)
		}
	}
}
