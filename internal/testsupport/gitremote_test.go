package testsupport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureTreeToleratesVanishedFile(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"objects/pack/stay.pack":   "pack",
		"refs/heads/main":          "ref",
		"objects/maintenance.lock": "lock",
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("make %s: %v", filepath.Dir(name), err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// The seam runs before each read: make maintenance.lock vanish
	// between the walk and the read, as background maintenance would.
	captureTreeReadHook = func(path string) {
		if filepath.Base(path) == "maintenance.lock" {
			os.Remove(path)
		}
	}
	defer func() { captureTreeReadHook = nil }()
	dirs, tree, err := captureTree(root)
	if err != nil {
		t.Fatalf("captureTree: %v", err)
	}
	if _, ok := tree["objects/maintenance.lock"]; ok {
		t.Error("captured objects/maintenance.lock, want the vanished file skipped")
	}
	for _, name := range []string{"objects/pack/stay.pack", "refs/heads/main"} {
		if got := string(tree[name]); got != files[name] {
			t.Errorf("tree[%s] = %q, want %q", name, got, files[name])
		}
	}
	wantDirs := map[string]bool{".": true, "objects": true, "objects/pack": true, "refs": true, "refs/heads": true}
	if len(dirs) != len(wantDirs) {
		t.Errorf("dirs = %v, want %v", dirs, wantDirs)
	}
	for _, dir := range dirs {
		if !wantDirs[dir] {
			t.Errorf("captured unexpected directory %q", dir)
		}
	}
}
