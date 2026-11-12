package claude

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// The read-scope hook is on by default and left out of the settings when
// the agent's read-scope switch is off; the status line and the pinned
// disableAllHooks stay either way.
func TestForemanSettingsReadScopeSwitch(t *testing.T) {
	root := t.TempDir()
	on, err := foremanSettingsJSON(root, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on, ReadScopeSubcommand) {
		t.Errorf("settings with the switch on lack the hook: %s", on)
	}
	off, err := foremanSettingsJSON(root, "brigada status --line", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, ReadScopeSubcommand) || strings.Contains(off, "PreToolUse") {
		t.Errorf("settings with the switch off carry the hook: %s", off)
	}
	if !strings.Contains(off, `"disableAllHooks":false`) || !strings.Contains(off, "statusLine") {
		t.Errorf("settings with the switch off lost the rest: %s", off)
	}
}

// The zero adapter keeps the hook on.
func TestAdapterReadScopeDefaultsOn(t *testing.T) {
	if (&Adapter{}).NoReadScope {
		t.Error("the zero Adapter has the read scope off")
	}
}

// The hook reaches the argv a foreman turn and an attach run under: on by
// default, gone when the agent says read-scope=false, with the settings
// isolation untouched either way.
func TestForemanCommandsReadScopeSwitch(t *testing.T) {
	stubCLI(t)
	job := adapter.Job{MCPConfig: "/runner/mcp/config.json", Prompt: "p"}
	settingsOf := func(args []string) string {
		i := slices.Index(args, "--settings")
		if i < 0 {
			t.Fatalf("no --settings in %v", args)
		}
		return args[i+1]
	}
	for _, tc := range []struct {
		name   string
		ad     Adapter
		hooked bool
	}{
		{"default", Adapter{}, true},
		{"off", Adapter{NoReadScope: true}, false},
	} {
		headless, err := tc.ad.Command(job, t.TempDir(), "")
		if err != nil {
			t.Fatal(err)
		}
		attached, err := tc.ad.AttachCommand(job, t.TempDir(), "", "sess-new")
		if err != nil {
			t.Fatal(err)
		}
		for kind, cmd := range map[string]*exec.Cmd{"headless": headless, "attached": attached} {
			if got := strings.Contains(settingsOf(cmd.Args), ReadScopeSubcommand); got != tc.hooked {
				t.Errorf("%s/%s: hook present = %v, want %v", tc.name, kind, got, tc.hooked)
			}
		}
		for _, flag := range []string{"--setting-sources", "--strict-mcp-config"} {
			if !slices.Contains(headless.Args, flag) {
				t.Errorf("%s: headless turn lost %s: %v", tc.name, flag, headless.Args)
			}
		}
	}
}
