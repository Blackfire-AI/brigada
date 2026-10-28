package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestReadRoleFile: the three states and the refusals, each naming the path.
func TestReadRoleFile(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "out.md")
	write := func(rel, text string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("w.md", "rules")
	write("empty.md", "")
	write("big.md", strings.Repeat("x", engine.MaxRoleTextBytes+1))
	write("at.md", strings.Repeat("x", engine.MaxRoleTextBytes))
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got, err := readRoleFile(root, "worker", "w.md"); err != nil || got != "rules" {
		t.Errorf("regular file = %q, %v", got, err)
	}
	if got, err := readRoleFile(root, "worker", "empty.md"); err != nil || got != "" {
		t.Errorf("empty file = %q, %v; want empty by choice", got, err)
	}
	if _, err := readRoleFile(root, "worker", "at.md"); err != nil {
		t.Errorf("file at the bound refused: %v", err)
	}
	for _, rel := range []string{"big.md", "link.md", "dir.md", "missing.md"} {
		_, err := readRoleFile(root, "worker", rel)
		if err == nil || !strings.Contains(err.Error(), rel) {
			t.Errorf("%s: err = %v, want a refusal naming the path", rel, err)
		}
	}

	roles, err := readRoleFiles(root, engine.ProjectConfig{RoleFiles: map[string]string{"worker": "w.md", "reviewer": "empty.md"}})
	if err != nil || len(roles) != 2 || roles["worker"] != "rules" {
		t.Errorf("readRoleFiles = %v, %v", roles, err)
	}
	if _, ok := roles["foreman"]; ok {
		t.Errorf("an absent key was sent: not configured must not be sent")
	}
}
