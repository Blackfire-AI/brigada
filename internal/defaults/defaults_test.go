package defaults

import (
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTexts(t *testing.T) {
	roles := map[string]bool{"foreman.md": false, "plan_review.md": false, "worker.md": false, "reviewer.md": false}
	skills := map[string]bool{"cli-flag": false, "config-setting": false, "bugfix": false, "surface-parity": false, "dependency": false, "schema-migration": false, "refactor": false, "docs-change": false}
	err := fs.WalkDir(Texts, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := Texts.ReadFile(name)
		if err != nil {
			return err
		}
		limit := 8192
		if strings.HasPrefix(name, "skills/") {
			limit = 4096
		}
		if len(data) == 0 || len(data) > limit || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
			t.Errorf("%s: invalid text or size %d (limit %d)", name, len(data), limit)
		}
		if limit == 8192 {
			if _, ok := roles[name]; !ok {
				t.Errorf("unexpected role %s", name)
			} else {
				roles[name] = true
			}
			return nil
		}
		dir := path.Base(path.Dir(name))
		if _, ok := skills[dir]; !ok || name != "skills/"+dir+"/SKILL.md" {
			t.Errorf("unexpected skill %s", name)
		} else {
			skills[dir] = true
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) < 5 || lines[0] != "---" || lines[1] != "name: "+dir || !strings.HasPrefix(lines[2], "description: ") || strings.TrimSpace(strings.TrimPrefix(lines[2], "description: ")) == "" || lines[3] != "---" {
			t.Errorf("%s: invalid frontmatter", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, found := range roles {
		if !found {
			t.Errorf("missing %s", name)
		}
	}
	for name, found := range skills {
		if !found {
			t.Errorf("missing skill %s", name)
		}
	}
}

func TestDigest(t *testing.T) {
	if got, want := Digest("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("Digest = %q, want %q", got, want)
	}
}

// TestArchiveHoldsTheCurrentVersion: the current version's archive must
// equal the live embed, file for file, so the three-way diff a later
// version runs against this one starts from the exact texts that ship.
func TestArchiveHoldsTheCurrentVersion(t *testing.T) {
	versions, err := Versions()
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) == 0 || versions[len(versions)-1] != Version {
		t.Fatalf("archive versions = %v, want the newest to be the live %s", versions, Version)
	}
	for _, role := range Roles {
		live, err := LiveRoleText(role)
		if err != nil {
			t.Fatal(err)
		}
		archived, err := RoleText(Version, role)
		if err != nil {
			t.Fatalf("role %s: %v", role, err)
		}
		if archived != live {
			t.Errorf("role %s: the %s archive differs from the live text", role, Version)
		}
	}
	for _, skill := range SkillNames {
		live, err := LiveSkillText(skill)
		if err != nil {
			t.Fatal(err)
		}
		archived, err := SkillText(Version, skill)
		if err != nil {
			t.Fatalf("skill %s: %v", skill, err)
		}
		if archived != live {
			t.Errorf("skill %s: the %s archive differs from the live text", skill, Version)
		}
	}
	// Retrieval is by version, and only what the archive holds.
	if _, err := RoleText("0.0.1", "worker"); err == nil {
		t.Error("an unarchived version's role text was found")
	}
	if _, err := SkillText(Version, "no-such-skill"); err == nil {
		t.Error("an unarchived skill was found")
	}
	if _, err := RoleText("../skills/cli-flag/SKILL.md", "worker"); err == nil {
		t.Error("a path in place of a version was accepted")
	}
}

// TestSkillsCarryProcedures: every shipped skill must carry a concrete
// numbered procedure of at least four steps, and each step set its task
// shape demands — the phrases below — so a worker handed the skill has
// a checklist, not a summary.
func TestSkillsCarryProcedures(t *testing.T) {
	phrases := map[string][]string{
		"cli-flag":         {"Register the flag", "help text", "usage text", "docs synopsis", "success test", "usage-error test", "compatibility", "sibling surfaces"},
		"config-setting":   {"Parse", "Validate", "default", "precedence", "shown", "documentation table row", "test row"},
		"bugfix":           {"before the fix", "passes after it", "Apply the fix", "documented behavior"},
		"surface-parity":   {"CLI", "API", "MCP", "UI", "why not"},
		"dependency":       {"lockfile", "license", "notices", "vulnerabilities"},
		"schema-migration": {"next migration number", "never renumber", "old data", "where state lives"},
		"refactor":         {"public API", "untouched", "renamed names"},
		"docs-change":      {"present tense", "against the code", "commands", "cross-links"},
	}
	for _, skill := range SkillNames {
		text, err := LiveSkillText(skill)
		if err != nil {
			t.Fatal(err)
		}
		steps := 0
		for i := 1; strings.Contains("\n"+text, "\n"+strconv.Itoa(i)+". "); i++ {
			steps = i
		}
		if steps < 4 {
			t.Errorf("%s: numbered procedure has %d steps, want at least 4", skill, steps)
		}
		for _, want := range phrases[skill] {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the procedure does not mention %q", skill, want)
			}
		}
	}
}
