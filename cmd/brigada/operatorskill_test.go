package main

import (
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"
)

// operatorSkillPath locates the canonical operator skill from this test
// file's own directory, never the working directory: other tests change
// it.
func operatorSkillPath(t *testing.T) string {
	t.Helper()
	_, here, _, ok := stdruntime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	return filepath.Join(filepath.Dir(here), "..", "..", "skills", "brigada-operator", "SKILL.md")
}

func TestOperatorSkillFile(t *testing.T) {
	b, err := os.ReadFile(operatorSkillPath(t))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("skill must open its frontmatter with ---:\n%s", text)
	}
	rest := text[len("---\n"):]
	i := strings.Index(rest, "\n---\n")
	if i < 0 {
		t.Fatal("skill frontmatter never closes with ---")
	}
	fm, body := rest[:i], rest[i+len("\n---\n"):]

	var name, desc string
	for _, line := range strings.Split(fm, "\n") {
		switch {
		case strings.HasPrefix(line, "name: "):
			name = strings.TrimPrefix(line, "name: ")
		case strings.HasPrefix(line, "description: "):
			desc = strings.TrimPrefix(line, "description: ")
		default:
			t.Errorf("unexpected frontmatter line %q", line)
		}
	}
	if name != "brigada-operator" || len(name) > 64 {
		t.Errorf("frontmatter name = %q, want brigada-operator within 64 characters", name)
	}
	if desc == "" {
		t.Fatal("frontmatter has no description")
	}
	if len(desc) > 1024 {
		t.Errorf("description is %d characters, over the 1024 limit", len(desc))
	}
	for _, w := range []string{"brigada", "build", "fix", "review", "verify", "land"} {
		if !strings.Contains(desc, w) {
			t.Errorf("description does not name the %q request", w)
		}
	}

	stages := []string{
		"## 1. Specify",
		"## 2. Delegate implementation",
		"## 3. Watch",
		"## 4. Verify independently",
		"## 5. Close follow-ups",
		"## 6. Land and report",
	}
	at := -1
	for _, s := range stages {
		j := strings.Index(body, s)
		if j < 0 {
			t.Errorf("stage %q missing", s)
			continue
		}
		if j <= at {
			t.Errorf("stage %q is out of order", s)
		}
		at = j
	}

	for _, want := range []string{
		// Verification is its own stage after implementation, and only
		// evidence for every acceptance criterion completes the work.
		"Verification is its own stage after implementation",
		"every acceptance criterion has evidence",
		// What stays the user's alone.
		"brigada plan approve", "brigada land approve",
		"brigada push approve", "--and-stop-asking",
		"Tags and releases", "Rule changes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("skill body lacks %q", want)
		}
	}
}
