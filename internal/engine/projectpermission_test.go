package engine

import (
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
