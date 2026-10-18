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

func TestProjectConfigDefaultsNoCheck(t *testing.T) {
	const repo = "example.invalid/project"
	for _, old := range []string{
		"",
		"# policy\nschema: 1\nrepo: example.invalid/project\napproval_max_tasks: 7\ndefault_check: old # check comment\nworker_tools: none\n\n# last comment\n",
		"schema: 1\nrepo: example.invalid/project\nworker_tools: none\n",
	} {
		got, err := ProjectConfigDefaults([]byte(old), repo, "", "none")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ParseProjectConfig(got)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cfg.Values[SettingDefaultCheck]; ok || strings.Contains(string(got), "default_check:") {
			t.Fatalf("no-check must omit the key: %s", got)
		}
		if strings.Contains(old, "default_check:") {
			want := strings.ReplaceAll(old, "default_check: old # check comment\n", "")
			if string(got) != want {
				t.Fatalf("must remove only the check line: %s, want %s", got, want)
			}
		}
		again, err := ProjectConfigDefaults(got, repo, "", "none")
		if err != nil || !bytes.Equal(got, again) {
			t.Fatalf("not idempotent: %s %v", again, err)
		}
	}
}
