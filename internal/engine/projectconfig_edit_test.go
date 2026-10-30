package engine

import (
	"strings"
	"testing"
)

// TestProjectConfigGuidanceGeneration: init writes the instructions map
// and skills_dir a first-generation file lacks — the whole map, or the
// three other roles when the file keeps the 1.0 instructions_file, the
// worker staying on its alias (§5.9). Once the map exists a missing key
// is the user's choice: nothing is added back, so dropping a key reaches
// not configured, and only skills_dir can still be added.
func TestProjectConfigGuidanceGeneration(t *testing.T) {
	full := func(body string) string { return string(cfgFile(body)) }

	// First generation: the whole map, and skills_dir.
	got, err := ProjectConfigGuidance(cfgFile("default_check: make test\n"))
	if err != nil {
		t.Fatalf("first generation: %v", err)
	}
	cfg, err := ParseProjectConfig(got)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, got)
	}
	if len(cfg.RoleFiles) != len(RoleKeys) || cfg.SkillsDir != DefaultSkillsDir {
		t.Fatalf("first generation RoleFiles = %v, skills_dir %q\n%s", cfg.RoleFiles, cfg.SkillsDir, got)
	}
	for _, role := range RoleKeys {
		if cfg.RoleFiles[role] != DefaultRoleFile(role) {
			t.Errorf("role %s = %q, want the default path", role, cfg.RoleFiles[role])
		}
	}

	// The 1.0 alias: the three other roles join the map, the worker
	// keeps instructions_file, and the two keys never name one text.
	got, err = ProjectConfigGuidance(cfgFile("instructions_file: .brigada/instructions.md\n"))
	if err != nil {
		t.Fatalf("alias generation: %v", err)
	}
	cfg, err = ParseProjectConfig(got)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, got)
	}
	if cfg.InstructionsFile != ".brigada/instructions.md" {
		t.Errorf("instructions_file = %q, want the alias kept", cfg.InstructionsFile)
	}
	if _, both := cfg.RoleFiles[RoleWorker]; both {
		t.Errorf("the worker gained a map key beside its alias:\n%s", got)
	}
	for _, role := range []string{RoleForeman, RolePlanReview, RoleReviewer} {
		if cfg.RoleFiles[role] != DefaultRoleFile(role) {
			t.Errorf("role %s = %q, want the default path", role, cfg.RoleFiles[role])
		}
	}

	// An existing map, partial or whole, gains nothing: its missing keys
	// were left out on purpose (§5.9 'Three states').
	partial := full("instructions:\n  worker: docs/worker-rules.md  # mine\nskills_dir: .brigada/skills\n")
	if got, err = ProjectConfigGuidance([]byte(partial)); err != nil {
		t.Fatalf("partial map: %v", err)
	}
	if string(got) != partial {
		t.Errorf("a partial map changed:\n%s", got)
	}
	// The alias with a partial map: the same rule, the worker untouched.
	aliasPartial := full("instructions_file: .brigada/instructions.md\ninstructions:\n  plan_review: pr.md\nskills_dir: .brigada/skills\n")
	if got, err = ProjectConfigGuidance([]byte(aliasPartial)); err != nil {
		t.Fatalf("alias with a partial map: %v", err)
	}
	if string(got) != aliasPartial {
		t.Errorf("the alias with a partial map changed:\n%s", got)
	}

	// Only skills_dir can still be added to an existing map.
	missingSkills := full("instructions:\n  foreman: f.md\n")
	if got, err = ProjectConfigGuidance([]byte(missingSkills)); err != nil {
		t.Fatalf("missing skills_dir: %v", err)
	}
	if string(got) != missingSkills+"skills_dir: "+DefaultSkillsDir+"\n" {
		t.Errorf("skills_dir addition:\n%s", got)
	}
}

// TestProjectConfigRoleKey: the restore path of a role deliberately left
// out — `brigada init --take-default <role>` (§5.9) — adds the role's
// key at its default path, creates the map when the file has none, keeps
// a key it already has and the worker's instructions_file alias, and
// refuses an unknown role.
func TestProjectConfigRoleKey(t *testing.T) {
	partial := string(cfgFile("instructions:\n  worker: docs/worker-rules.md  # mine\nland_target: main\n"))
	got, err := ProjectConfigRoleKey([]byte(partial), RoleReviewer)
	if err != nil {
		t.Fatalf("add reviewer: %v", err)
	}
	cfg, err := ParseProjectConfig(got)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, got)
	}
	if cfg.RoleFiles[RoleReviewer] != DefaultRoleFile(RoleReviewer) || cfg.RoleFiles[RoleWorker] != "docs/worker-rules.md" {
		t.Fatalf("RoleFiles after the restore = %v\n%s", cfg.RoleFiles, got)
	}
	if !strings.Contains(string(got), "# mine") || !strings.Contains(string(got), "land_target: main") {
		t.Errorf("the restore disturbed other lines:\n%s", got)
	}

	again, err := ProjectConfigRoleKey(got, RoleReviewer)
	if err != nil || string(again) != string(got) {
		t.Errorf("a key already present changed: %v\n%s", err, again)
	}

	// No map at all: the map is created around the one key.
	fresh := string(cfgFile("default_check: make test\n"))
	got, err = ProjectConfigRoleKey([]byte(fresh), RoleForeman)
	if err != nil {
		t.Fatalf("create the map: %v", err)
	}
	if cfg, err = ParseProjectConfig(got); err != nil || len(cfg.RoleFiles) != 1 || cfg.RoleFiles[RoleForeman] != DefaultRoleFile(RoleForeman) {
		t.Fatalf("created map = %v, %v\n%s", cfg.RoleFiles, err, got)
	}

	// The worker beside its alias: the alias is the key; nothing is
	// added, for the file refuses naming the text twice.
	alias := string(cfgFile("instructions_file: .brigada/instructions.md\n"))
	if got, err = ProjectConfigRoleKey([]byte(alias), RoleWorker); err != nil || string(got) != alias {
		t.Errorf("worker beside the alias = %v\n%s", err, got)
	}

	if _, err := ProjectConfigRoleKey([]byte(partial), "boss"); err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Errorf("unknown role: err = %v, want a refusal", err)
	}
}
