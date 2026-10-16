package engine

import (
	"bytes"
	"strings"
	"testing"
)

func TestProjectConfigDefaultsPreservesContent(t *testing.T) {
	old := []byte("# my settings\nschema: 1\nrepo: example.invalid/project\napproval_max_tasks: 7\ndefault_check: 'old' # why we check\nworker_tools: none\n\n# last comment\n")
	got, err := ProjectConfigDefaults(old, "example.invalid/project", "new # quoted: check", "Bash(npm test:*)")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"# my settings\nschema: 1\nrepo: example.invalid/project\napproval_max_tasks: 7\n", `default_check: "new # quoted: check" # why we check`, "\n\n# last comment\n"} {
		if !strings.Contains(string(got), part) {
			t.Errorf("lost %q in %s", part, got)
		}
	}
	again, err := ProjectConfigDefaults(got, "example.invalid/project", "new # quoted: check", "Bash(npm test:*)")
	if err != nil || !bytes.Equal(got, again) {
		t.Fatalf("not idempotent: %s %v", again, err)
	}
	if _, err := ProjectConfigDefaults(old, "example.invalid/another", "x", "none"); err == nil {
		t.Fatal("replaced another repo's identity")
	}
}
