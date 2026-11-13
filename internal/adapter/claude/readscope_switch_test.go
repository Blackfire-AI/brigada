package claude

import (
	"strings"
	"testing"
)

// TestForemanSettingsReadScopeSwitch: the per-agent read-scope switch
// reaches the settings that install the hook; the default is on.
func TestForemanSettingsReadScopeSwitch(t *testing.T) {
	root := t.TempDir()
	on, err := foremanSettingsWith(root, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on, ReadScopeSubcommand) {
		t.Errorf("switch on: settings %s lack the hook", on)
	}
	off, err := foremanSettingsWith(root, "status", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, ReadScopeSubcommand) || !strings.Contains(off, `"disableAllHooks":false`) || !strings.Contains(off, "status") {
		t.Errorf("switch off: settings %s", off)
	}
	if def, _ := foremanSettingsJSON(root, ""); def != on {
		t.Errorf("default settings differ from the switch on:\n%s\n%s", def, on)
	}
	if !(&Adapter{}).readScopeOn() || (&Adapter{NoReadScope: true}).readScopeOn() {
		t.Errorf("the zero Adapter must keep the hook on")
	}
}
