package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestProjectConfigPermissionAlias(t *testing.T) {
	const head = "schema: 1\nrepo: github.com/acme/widgets\n"

	// A file written before permission= existed still reads as schema 1,
	// with no permission and no warning.
	old, err := ParseProjectConfig([]byte(head + "default_check: go test ./...\n"))
	if err != nil {
		t.Fatalf("pre-permission file: %v", err)
	}
	if old.Permission != "" || len(old.Warnings) != 0 {
		t.Fatalf("pre-permission file: Permission %q Warnings %v, want none", old.Permission, old.Warnings)
	}

	for _, tc := range []struct{ name, value, want string }{
		{"patterns", "Bash(go test:*)+Bash(gofmt:*)", "Bash(go test:*)+Bash(gofmt:*)"},
		{"none", "none", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseProjectConfig([]byte(head + "worker_tools: " + `"` + tc.value + `"` + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Permission != tc.want {
				t.Fatalf("Permission = %q, want %q", cfg.Permission, tc.want)
			}
			if got := string(cfg.Values[SettingWorkerTools]); got != `"`+tc.want+`"` {
				t.Fatalf("stored worker_tools = %s, want it unchanged", got)
			}
			if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "worker_tools") || !strings.Contains(cfg.Warnings[0], "permission=") {
				t.Fatalf("Warnings = %v, want one naming worker_tools and permission=", cfg.Warnings)
			}
		})
	}
}

func TestProjectConfigPermission(t *testing.T) {
	const head = "schema: 1\nrepo: github.com/acme/widgets\n"

	// The permission= key reads with no warning: it is not the alias.
	cfg, err := ParseProjectConfig([]byte(head + "permission: bypassPermissions\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Permission != "bypassPermissions" {
		t.Fatalf("Permission = %q, want bypassPermissions", cfg.Permission)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none", cfg.Warnings)
	}
	if got := string(cfg.Values[SettingPermission]); got != `"bypassPermissions"` {
		t.Fatalf("stored permission = %s, want it", got)
	}

	// The alias beside its key is fine only when the two agree.
	for _, both := range []string{
		head + "permission: bypassPermissions\nworker_tools: bypassPermissions\n",
		head + "worker_tools: bypassPermissions\npermission: bypassPermissions\n",
	} {
		if cfg, err := ParseProjectConfig([]byte(both)); err != nil || cfg.Permission != "bypassPermissions" {
			t.Fatalf("agreeing keys = %+v, %v", cfg, err)
		}
	}
	disagree := head + "permission: plan\nworker_tools: none\n"
	if _, err := ParseProjectConfig([]byte(disagree)); err == nil ||
		!strings.Contains(err.Error(), "disagree") || !strings.Contains(err.Error(), "worker_tools") {
		t.Fatalf("disagreeing keys = %v, want a refusal naming the alias", err)
	}

	// A value that is not one permission mode is refused.
	for _, bad := range []string{"", "a b", "one,two"} {
		line := head + "permission: " + bad + "\n"
		if bad == "" {
			line = head + "permission: \"\"\n"
		}
		if _, err := ParseProjectConfig([]byte(line)); err == nil {
			t.Fatalf("permission %q accepted", bad)
		}
	}
}

func TestProjectSettingsPermissionRoundTrip(t *testing.T) {
	e := newEnv(t)

	// Set stores the mode; get reads it back with its provenance.
	got := e.set(map[string]string{SettingPermission: "acceptEdits"})
	if s := settingOf(t, got, SettingPermission); s.Value != "acceptEdits" || s.Default || s.Source != SourceLocal {
		t.Fatalf("permission = %+v, want acceptEdits set by hand", s)
	}

	// An empty value resets to the default.
	got = e.set(map[string]string{SettingPermission: ""})
	if s := settingOf(t, got, SettingPermission); s.Value != "agent default" || !s.Default {
		t.Fatalf("reset permission = %+v, want the default", s)
	}

	// A value that is not one permission mode is refused.
	if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingPermission: "two modes"}); !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("set a spaced permission value: err = %v, want ErrInvalidSetting", err)
	}
}
