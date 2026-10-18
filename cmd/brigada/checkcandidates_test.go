package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
)

func candidateRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func candidateCommands(cs []checkCandidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Command)
	}
	return out
}

func TestCheckCandidatesOwnersRepo(t *testing.T) {
	root := candidateRepo(t, map[string]string{
		"Makefile":       ".PHONY: check test\nCC := gcc\ncheck: lint format-check type test rig-spec\n\t@echo hi\ntest:\n\t@uv run pytest -q\n",
		"uv.lock":        "",
		"pyproject.toml": "",
	})
	cs, err := checkCandidates(root)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(candidateCommands(cs), "|")
	if want := "make check|make test|uv run pytest|python -m pytest"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if cs[0].Note != "Makefile: lint format-check type test rig-spec" {
		t.Errorf("note = %q", cs[0].Note)
	}
	if cs[0].Tools != "Bash(make check:*)" {
		t.Errorf("tools = %q", cs[0].Tools)
	}
	if cs[1].Note != "Makefile: uv run pytest -q" {
		t.Errorf("recipe note = %q", cs[1].Note)
	}
	if cs[2].Note != "pytest, from uv.lock" || cs[3].Note != "pytest, from pyproject.toml" {
		t.Errorf("notes = %q, %q", cs[2].Note, cs[3].Note)
	}
}

func TestCheckCandidatesPackageJSON(t *testing.T) {
	root := candidateRepo(t, map[string]string{
		"package.json": `{"name":"x","scripts":{"test":"vitest run","check":"tsc && eslint .","lint":"eslint ."},"other":{"scripts":{"ci":"no"}}}`,
	})
	cs, err := checkCandidates(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(candidateCommands(cs), "|"), "npm run check|npm test|npm run lint"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if cs[0].Note != "package.json: tsc && eslint ." || cs[1].Tools != "Bash(npm test:*)" {
		t.Errorf("got %+v", cs)
	}
}

func TestCheckCandidatesJustfile(t *testing.T) {
	root := candidateRepo(t, map[string]string{
		"justfile": "set shell := [\"sh\", \"-c\"]\ntest: build\n    cargo test\nlint:\n    cargo clippy\nbuild:\n    cargo build\n",
	})
	cs, err := checkCandidates(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(candidateCommands(cs), "|"), "just test|just lint"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if cs[0].Note != "justfile: build" || cs[1].Note != "justfile: cargo clippy" {
		t.Errorf("notes = %q, %q", cs[0].Note, cs[1].Note)
	}
}

func TestCheckCandidatesGoWithMakefile(t *testing.T) {
	root := candidateRepo(t, map[string]string{
		"go.mod":   "module x\n",
		"Makefile": "ci: vet test\n",
	})
	cs, err := checkCandidates(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(candidateCommands(cs), "|"), "make ci|go test ./..."; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	goTools := strings.Join(claude.DefaultWorkerTools(), "+")
	if cs[0].Tools != goTools+"+Bash(make ci:*)" {
		t.Errorf("make tools = %q", cs[0].Tools)
	}
	if cs[1].Tools != goTools {
		t.Errorf("go tools = %q", cs[1].Tools)
	}
}

func TestCheckCandidatesNothing(t *testing.T) {
	cs, err := checkCandidates(t.TempDir())
	if err != nil || len(cs) != 0 {
		t.Fatalf("got %v, %v", cs, err)
	}
}

func TestCheckCandidatesMakeNoPrereqs(t *testing.T) {
	root := candidateRepo(t, map[string]string{
		"GNUmakefile": "check:\n\t@go vet ./... && go test ./...\n",
	})
	cs, err := checkCandidates(root)
	if err != nil || len(cs) != 1 {
		t.Fatalf("got %v, %v", cs, err)
	}
	if cs[0].Note != "Makefile: go vet ./... && go test ./..." {
		t.Errorf("note = %q", cs[0].Note)
	}
}

func TestCheckCandidatesDedupAndBadJSON(t *testing.T) {
	root := candidateRepo(t, map[string]string{"Makefile": "test: a\ntest: b\n"})
	cs, _ := checkCandidates(root)
	if len(cs) != 1 || cs[0].Note != "Makefile: a" {
		t.Errorf("got %+v", cs)
	}
	bad := candidateRepo(t, map[string]string{"package.json": "{"})
	if _, err := checkCandidates(bad); err == nil {
		t.Error("want error for bad package.json")
	}
}

func TestTruncateNote(t *testing.T) {
	long := "Makefile: " + strings.Repeat("é", 200)
	for _, w := range []int{0, 1, 10, 80} {
		if got := []rune(truncateNote(long, w)); len(got) > w {
			t.Errorf("width %d: got %d runes", w, len(got))
		}
	}
	if got := truncateNote("short", 80); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := truncateNote("abcdef", 4); got != "abc…" {
		t.Errorf("got %q", got)
	}
}
