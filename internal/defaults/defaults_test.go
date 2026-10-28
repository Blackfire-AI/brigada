package defaults

import (
	"io/fs"
	"path"
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
