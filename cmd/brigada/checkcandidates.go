package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
)

// checkCandidate is one suggestion for the init check step.
type checkCandidate struct {
	Command string // what runs as the check
	Note    string // where it came from, full length; see truncateNote
	Tools   string // worker tool scope that lets the command run, "+"-joined
	File    string // the build file that supplied this candidate
}

// checkTargetRank orders target names: aggregates first, then test, then
// lint and typecheck. The names are also the only ones looked for.
var checkTargetRank = map[string]int{"check": 0, "ci": 0, "test": 1, "lint": 2, "typecheck": 2}

// checkTargetOrder is the preference order within a rank.
var checkTargetOrder = []string{"check", "ci", "test", "lint", "typecheck"}

var targetLineRE = regexp.MustCompile(`^([A-Za-z_][\w.-]*)(\s[^:=#]*)?:(?:[^=]|$)`)

// checkCandidates lists the checks the repository itself seems to define,
// best first. It only reads files; nothing is ever run.
func checkCandidates(root string) ([]checkCandidate, error) {
	exists := func(name string) bool { s, e := os.Stat(filepath.Join(root, name)); return e == nil && !s.IsDir() }
	files, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	// On a case-insensitive filesystem, Stat("justfile") also finds Justfile.
	// Use the directory's spelling so the suggestion names its actual source.
	fileNames := map[string]bool{}
	for _, file := range files {
		fileNames[file.Name()] = true
	}
	first := func(names ...string) string {
		for _, n := range names {
			if fileNames[n] && exists(n) {
				return n
			}
		}
		return ""
	}
	goTools := ""
	if exists("go.mod") {
		goTools = strings.Join(claude.DefaultWorkerTools(), "+")
	}
	tools := func(command string) string {
		pattern := "Bash(" + command + ":*)"
		if goTools != "" {
			return goTools + "+" + pattern
		}
		return pattern
	}

	type ranked struct {
		rank int
		pos  int
		c    checkCandidate
	}
	var found []ranked
	add := func(name, command, note, file string) {
		found = append(found, ranked{checkTargetRank[name], len(found), checkCandidate{command, note, tools(command), file}})
	}

	if f := first("Makefile", "makefile", "GNUmakefile"); f != "" {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			return nil, err
		}
		targets := parseRecipeTargets(string(data), false)
		for _, name := range checkTargetOrder {
			if note, ok := targets[name]; ok {
				add(name, "make "+name, "Makefile: "+note, f)
			}
		}
	}
	if f := first("justfile", "Justfile", ".justfile"); f != "" {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			return nil, err
		}
		targets := parseRecipeTargets(string(data), true)
		for _, name := range checkTargetOrder {
			if note, ok := targets[name]; ok {
				add(name, "just "+name, "justfile: "+note, f)
			}
		}
	}
	if exists("package.json") {
		data, err := os.ReadFile(filepath.Join(root, "package.json"))
		if err != nil {
			return nil, err
		}
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if err = json.Unmarshal(data, &pkg); err != nil {
			return nil, fmt.Errorf("read package.json: %w", err)
		}
		for _, name := range []string{"check", "test", "lint", "typecheck"} {
			script := strings.TrimSpace(pkg.Scripts[name])
			if script == "" {
				continue
			}
			command := "npm run " + name
			if name == "test" {
				command = "npm test"
			}
			add(name, command, "package.json: "+script, "package.json")
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].rank < found[j].rank })

	var out []checkCandidate
	seen := map[string]bool{}
	push := func(c checkCandidate) {
		if !seen[c.Command] {
			seen[c.Command] = true
			out = append(out, c)
		}
	}
	for _, r := range found {
		push(r.c)
	}

	pytest := func(command, file string) {
		push(checkCandidate{command, "pytest, from " + file, "Bash(" + command + ":*)", file})
	}
	if exists("go.mod") {
		push(checkCandidate{"go test ./...", "go test, from go.mod", goTools, "go.mod"})
	}
	if exists("Cargo.toml") {
		push(checkCandidate{"cargo test", "cargo test, from Cargo.toml", "Bash(cargo test:*)", "Cargo.toml"})
	}
	if exists("uv.lock") {
		pytest("uv run pytest", "uv.lock")
	}
	if exists("poetry.lock") {
		pytest("poetry run pytest", "poetry.lock")
	}
	if f := first("pyproject.toml", "pytest.ini"); f != "" {
		pytest("python -m pytest", f)
	}
	return out, nil
}

// parseRecipeTargets maps each wanted target name defined at column 0 to its
// note: its prerequisites, or its first recipe line when it has none. Recipe
// lines start with a tab (make) or any blank (just).
func parseRecipeTargets(text string, anyIndent bool) map[string]string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	isRecipe := func(l string) bool {
		if anyIndent {
			return strings.HasPrefix(l, "\t") || strings.HasPrefix(l, " ")
		}
		return strings.HasPrefix(l, "\t")
	}
	out := map[string]string{}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" || isRecipe(line) {
			continue
		}
		m := targetLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if _, wanted := checkTargetRank[name]; !wanted {
			continue
		}
		if _, dup := out[name]; dup {
			continue
		}
		rest := line[strings.Index(line, ":")+1:]
		inline := ""
		if k := strings.Index(rest, ";"); k >= 0 && !anyIndent {
			inline = strings.TrimSpace(rest[k+1:])
			rest = rest[:k]
		}
		if k := strings.Index(rest, "#"); k >= 0 {
			rest = rest[:k]
		}
		if k := strings.Index(rest, "|"); k >= 0 {
			rest = rest[:k]
		}
		var prereqs []string
		for _, f := range strings.Fields(rest) {
			if f != "&&" {
				prereqs = append(prereqs, f)
			}
		}
		note := strings.Join(prereqs, " ")
		if note == "" {
			note = strings.TrimLeft(inline, "@")
		}
		for j := i + 1; note == "" && j < len(lines); j++ {
			l := lines[j]
			if strings.TrimSpace(l) == "" {
				continue
			}
			if !isRecipe(l) {
				break
			}
			note = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "@"))
			if strings.HasPrefix(note, "#") {
				note = ""
			}
		}
		if note == "" {
			note = "target " + name
		}
		out[name] = note
	}
	return out
}

// truncateNote shortens note to at most width columns, ending in "…" when cut.
func truncateNote(note string, width int) string {
	r := []rune(note)
	if len(r) <= width {
		return note
	}
	if width <= 0 {
		return ""
	}
	return string(r[:width-1]) + "…"
}
