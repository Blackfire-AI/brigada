package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, text := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const skillDoc = "---\nname: cli-flag\ndescription: add a flag\nfor: worker\n---\nbody\n"

// TestReadSkills: parse, header handling, and digests that move with every
// edit but the header.
func TestReadSkills(t *testing.T) {
	root := t.TempDir()
	const dir = ".brigada/skills"
	writeTree(t, root, map[string]string{
		dir + "/cli-flag/SKILL.md":      skillDoc,
		dir + "/cli-flag/ref/notes.txt": "notes",
		dir + "/other/SKILL.md":         "---\nname: other\ndescription: o\n---\nx",
		dir + "/README.md":              "not a skill dir",
	})
	skills, err := readSkills(root, dir)
	if err != nil || len(skills) != 2 {
		t.Fatalf("readSkills = %+v, %v", skills, err)
	}
	a := skills[0]
	if a.Name != "cli-flag" || a.Description != "add a flag" || a.For != "worker" || a.Text != skillDoc || a.Digest != textDigest(skillDoc) || a.TreeDigest == "" {
		t.Errorf("parsed skill = %+v", a)
	}
	if skills[1].For != "" {
		t.Errorf("an absent for = %q, want empty", skills[1].For)
	}

	digestOf := func(file string) (string, string) {
		t.Helper()
		writeTree(t, root, map[string]string{dir + "/cli-flag/SKILL.md": file})
		ss, err := readSkills(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		return ss[0].Digest, ss[0].TreeDigest
	}
	d0, t0 := digestOf(skillDoc)
	// The header changes nothing, wherever its fields point.
	d1, t1 := digestOf("---\nname: cli-flag\ndescription: add a flag\nfor: worker\n---\n<!-- brigada: version=2 digest=abc -->\nbody\n")
	if d1 != d0 || t1 != t0 {
		t.Errorf("a header moved the digests")
	}
	for name, edit := range map[string]string{
		"description": "---\nname: cli-flag\ndescription: add a flag.\nfor: worker\n---\nbody\n",
		"for":         "---\nname: cli-flag\ndescription: add a flag\nfor: both\n---\nbody\n",
		"body":        "---\nname: cli-flag\ndescription: add a flag\nfor: worker\n---\nbody!\n",
	} {
		d, tr := digestOf(edit)
		if d == d0 || tr == t0 {
			t.Errorf("a %s edit did not change the digest and tree digest", name)
		}
	}
	digestOf(skillDoc)
	writeTree(t, root, map[string]string{dir + "/cli-flag/ref/notes.txt": "notes 2"})
	ss, _ := readSkills(root, dir)
	if ss[0].Digest != d0 || ss[0].TreeDigest == t0 {
		t.Errorf("a supporting-file edit: digest same %t, tree changed %t; want same, changed", ss[0].Digest == d0, ss[0].TreeDigest != t0)
	}
}

// TestReadSkillsRefusals: every refusal names the file.
func TestReadSkillsRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ text, want string }{
		"name mismatch":  {"---\nname: other\ndescription: d\n---\nx", "must equal the directory name"},
		"no desc":        {"---\nname: bad\n---\nx", "no description"},
		"extra key":      {"---\nname: bad\ndescription: d\nlicense: mit\n---\nx", "license"},
		"bad for":        {"---\nname: bad\ndescription: d\nfor: all\n---\nx", "worker, reviewer or both"},
		"no frontmatter": {"just text", "frontmatter"},
		"unclosed":       {"---\nname: bad\n", "not closed"},
		"over bound":     {"---\nname: bad\ndescription: d\n---\n" + strings.Repeat("x", engine.MaxSkillBytes), "4096"},
	} {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"s/bad/SKILL.md": tc.text})
		_, err := readSkills(root, "s")
		if err == nil || !strings.Contains(err.Error(), "s/bad/SKILL.md") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want the file and %q", name, err, tc.want)
		}
	}
	root := t.TempDir()
	if ss, err := readSkills(root, "none"); err != nil || len(ss) != 0 {
		t.Errorf("a missing skills_dir = %v, %v; want none", ss, err)
	}
}

// TestRoleFileHeader: a role file's first-line header is no part of the text.
func TestRoleFileHeader(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"w.md": "<!-- brigada: version=1 digest=x -->\nrules\n", "p.md": "rules\n"})
	a, err := readRoleFile(root, "worker", "w.md")
	b, err2 := readRoleFile(root, "worker", "p.md")
	if err != nil || err2 != nil || a != "rules\n" || a != b || textDigest(a) != textDigest(b) {
		t.Errorf("header strip = %q, %q, %v, %v", a, b, err, err2)
	}
}
