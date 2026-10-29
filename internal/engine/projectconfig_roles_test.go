package engine

import (
	"strings"
	"testing"
)

// TestParseProjectConfigRoles: the nested instructions map, the
// skills_dir key, the alias, and old files still parsing.
func TestParseProjectConfigRoles(t *testing.T) {
	cfg, err := ParseProjectConfig(cfgFile("instructions:\n  worker: docs/w.md\n  reviewer:\nskills_dir:\n"))
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if cfg.RoleFiles[RoleWorker] != "docs/w.md" || cfg.RoleFiles[RoleReviewer] != DefaultRoleFile(RoleReviewer) || len(cfg.RoleFiles) != 2 {
		t.Errorf("RoleFiles = %v", cfg.RoleFiles)
	}
	if cfg.SkillsDir != DefaultSkillsDir {
		t.Errorf("SkillsDir = %q, want the default", cfg.SkillsDir)
	}
	if cfg, err = ParseProjectConfig(cfgFile("instructions_file: a.md\nskills_dir: s\n")); err != nil || cfg.InstructionsFile != "a.md" || cfg.SkillsDir != "s" || cfg.RoleFiles != nil {
		t.Errorf("alias/old file = %+v, %v", cfg, err)
	}
	if cfg, err = ParseProjectConfig(cfgFile("")); err != nil || cfg.RoleFiles != nil || cfg.SkillsDir != "" {
		t.Errorf("minimal old file = %+v, %v", cfg, err)
	}

	_, err = ParseProjectConfig(cfgFile("instructions_file: a.md\ninstructions:\n  worker: b.md\n"))
	if err == nil || !strings.Contains(err.Error(), "instructions_file") || !strings.Contains(err.Error(), "instructions.worker") {
		t.Errorf("both keys: err = %v, want both named", err)
	}
	for _, bad := range []string{
		"instructions:\n  boss: a.md\n", "instructions:\n  worker: /etc/passwd\n", "instructions:\n  worker: ../x\n",
		"instructions:\n  worker: a\n  worker: b\n", "instructions: text\n", "instructions:\n  - a\n",
		"instructions:\n  worker: a\n  - b\n", "skills_dir: ../x\n", "skills_dir: [a]\n", "repo2:\n  a: b\n",
	} {
		if _, err := ParseProjectConfig(cfgFile(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
