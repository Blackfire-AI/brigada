package claude

import (
	"strings"
	"testing"
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
