package defaults

import (
	"io/fs"
	"path"
	"regexp"
	"slices"
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

// TestVersionedArchiveMatchesLive: the current version's archive equals
// the live embed, role texts and skill texts alike, so a three-way diff
// can rebuild the exact text a file was generated from; a name or
// version the archive does not hold is refused.
func TestVersionedArchiveMatchesLive(t *testing.T) {
	vers, err := Versions()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(vers, Version) {
		t.Fatalf("Versions() = %v, want the current %s", vers, Version)
	}
	for _, role := range Roles {
		live, err := RoleText(role)
		if err != nil {
			t.Fatal(err)
		}
		archived, err := ArchivedRoleText(Version, role)
		if err != nil || archived != live {
			t.Errorf("archive %s/%s.md = %d bytes, %v; want the live %d bytes", Version, role, len(archived), err, len(live))
		}
	}
	for _, skill := range Skills {
		live, err := SkillText(skill)
		if err != nil {
			t.Fatal(err)
		}
		archived, err := ArchivedSkillText(Version, skill)
		if err != nil || archived != live {
			t.Errorf("archive %s/skills/%s/SKILL.md = %d bytes, %v; want the live %d bytes", Version, skill, len(archived), err, len(live))
		}
	}
	for _, missing := range []struct{ version, name string }{
		{"0.0.1", "worker.md"},
		{Version, "nope.md"},
		{Version, "../foreman.md"},
	} {
		if _, err := ArchivedText(missing.version, missing.name); err == nil {
			t.Errorf("ArchivedText(%q, %q) was accepted", missing.version, missing.name)
		}
	}
}

// skillStep is one numbered step line of a SKILL.md procedure.
var skillStep = regexp.MustCompile(`(?m)^(\d+)\. `)

// TestSkillProcedures: each shipped skill carries a concrete numbered
// procedure of at least four steps covering its required items.
func TestSkillProcedures(t *testing.T) {
	phrases := map[string][]string{
		"cli-flag":         {"Register the flag", "help text", "usage text", "success test", "usage-error test", "compatibility statement", "sibling surfaces"},
		"config-setting":   {"Parse", "Validate", "Default", "Precedence", "shown", "docs table row", "test row"},
		"bugfix":           {"fail before the fix", "passes after the fix", "the fix", "documented behavior"},
		"surface-parity":   {"siblings", "CLI", "API", "MCP", "UI", "why not"},
		"dependency":       {"lockfile", "license", "notices", "vulnerability"},
		"schema-migration": {"next migration number", "never renumber", "Test on old data", "state lives"},
		"refactor":         {"no public API change", "untouched", "renamed names"},
		"docs-change":      {"present tense", "commands", "cross-links"},
	}
	for _, skill := range Skills {
		text, err := SkillText(skill)
		if err != nil {
			t.Fatalf("%s: %v", skill, err)
		}
		want, ok := phrases[skill]
		if !ok {
			t.Fatalf("no phrases asserted for skill %s", skill)
		}
		steps := 0
		for _, m := range skillStep.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err != nil || n != steps+1 {
				t.Errorf("%s: procedure steps are not numbered 1, 2, 3, ... at %q", skill, m[0])
			} else {
				steps = n
			}
		}
		if steps < 4 {
			t.Errorf("%s: procedure has %d steps, want at least 4", skill, steps)
		}
		for _, phrase := range want {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s: procedure does not cover %q", skill, phrase)
			}
		}
	}
}
