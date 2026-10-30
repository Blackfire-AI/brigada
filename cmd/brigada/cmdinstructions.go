package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/defaults"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

var instructionsCommands = map[string]command{
	"diff": {name: "diff", description: "Compare a role or skill file with the defaults it came from and the new ones.", run: runInstructionsDiff},
}

// runInstructions is the instructions command's dispatch (§5.9).
func runInstructions(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "instructions", "usage: brigada instructions diff <role|skill>", "diff", instructionsCommands)
}

// guidanceNames lists the names `instructions diff` and `init --take-default`
// take, for a usage error.
func guidanceNames() string {
	return "roles " + strings.Join(engine.RoleKeys, ", ") + "; skills " + strings.Join(defaults.Skills, ", ")
}

// resolveGuidance finds the generated file a role or skill name stands for
// in the repository at root: its path under project.yaml (the default path
// when the file does not name one) and its current default text.
func resolveGuidance(root, name string) (guidanceTarget, error) {
	var cfg engine.ProjectConfig
	if content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(engine.ProjectConfigPath))); err == nil {
		if cfg, err = engine.ParseProjectConfig(content); err != nil {
			return guidanceTarget{}, err
		}
	}
	for _, role := range engine.RoleKeys {
		if role != name {
			continue
		}
		text, err := defaults.LiveRoleText(role)
		if err != nil {
			return guidanceTarget{}, err
		}
		rel, ok := cfg.RoleFiles[role]
		switch {
		case ok:
		case role == engine.RoleWorker && cfg.InstructionsFile != "":
			rel = cfg.InstructionsFile
		default:
			rel = engine.DefaultRoleFile(role)
		}
		return guidanceTarget{rel: rel, text: text, what: "role " + role, name: role}, nil
	}
	for _, skill := range defaults.Skills {
		if skill != name {
			continue
		}
		text, err := defaults.LiveSkillText(skill)
		if err != nil {
			return guidanceTarget{}, err
		}
		dir := cfg.SkillsDir
		if dir == "" {
			dir = engine.DefaultSkillsDir
		}
		return guidanceTarget{rel: dir + "/" + skill + "/SKILL.md", text: text, skill: true, what: "skill " + skill, name: skill}, nil
	}
	return guidanceTarget{}, fmt.Errorf("unknown role or skill %q; want one of: %s", name, guidanceNames())
}

// archivedDefault is the exact text a file was generated from, by the
// version its header names; the error says why it cannot be had.
func archivedDefault(t guidanceTarget, name, version, digest string) (string, error) {
	var text string
	var err error
	if t.skill {
		text, err = defaults.ArchiveSkillText(version, name)
	} else {
		text, err = defaults.ArchiveRoleText(version, name)
	}
	if err != nil {
		return "", fmt.Errorf("the header names version %s, which this brigada does not ship", version)
	}
	if defaults.Digest(text) != digest {
		return "", fmt.Errorf("the header's digest does not match version %s's shipped text", version)
	}
	return text, nil
}

// runInstructionsDiff is `brigada instructions diff <role|skill>` (§5.9):
// the project's file against the default it was generated from, found in
// the archive by the version in its header, and against the new default. It
// shows what you changed and what brigada changed, side by side. A header
// whose version is not shipped, or no header at all, falls back to a
// two-way diff of the file against the new default and says why.
func runInstructionsDiff(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("instructions diff",
		"brigada instructions diff <role|skill>",
		"Compare a role or skill file with the default it was generated from and with the new default. Roles: "+strings.Join(engine.RoleKeys, ", ")+". Skills: "+strings.Join(defaults.Skills, ", ")+".")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 1 {
		return usagef(stderr, "usage: brigada instructions diff <role|skill>; %s", guidanceNames())
	}
	name := fs.Args()[0]
	cwd, err := os.Getwd()
	if err != nil {
		return fail(stderr, err)
	}
	root, err := gitRoot(cwd)
	if err != nil {
		return fail(stderr, err)
	}
	t, err := resolveGuidance(root, name)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown role or skill") {
			return usagef(stderr, "%v", err)
		}
		return fail(stderr, err)
	}
	if err := wiringSafePath(root, t.rel); err != nil {
		return fail(stderr, err)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(t.rel)))
	if err != nil {
		return fail(stderr, fmt.Errorf("read %s: %w; brigada init writes it", t.rel, err))
	}
	version, digest, yours, generated := splitGenerated(string(data), t.skill)
	fmt.Fprintf(stdout, "%s (%s)\n", t.rel, t.what)
	var reason string
	var base string
	if !generated {
		reason = "the file has no brigada header, so what it was generated from is unknown"
	} else if base, err = archivedDefault(t, name, version, digest); err != nil {
		reason = err.Error()
	}
	if reason != "" {
		fmt.Fprintf(stdout, "two-way diff, because %s\n", reason)
		fmt.Fprintf(stdout, "current default is version %s\n\n", defaults.Version)
		printTextDiff(stdout, "your file", "new default", yours, t.text)
		return 0
	}
	fmt.Fprintf(stdout, "generated from version %s; current default is version %s\n\n", version, defaults.Version)
	printTextDiff(stdout, "generated default (version "+version+")", "your file", base, yours)
	fmt.Fprintln(stdout)
	printTextDiff(stdout, "generated default (version "+version+")", "new default (version "+defaults.Version+")", base, t.text)
	return 0
}

// printTextDiff prints a line diff of a against b under a heading naming
// both, or says they are the same.
func printTextDiff(w io.Writer, from, to, a, b string) {
	fmt.Fprintf(w, "--- %s\n+++ %s\n", from, to)
	lines := lineDiff(splitDiffLines([]byte(a)), splitDiffLines([]byte(b)), 2)
	if len(lines) == 0 {
		fmt.Fprintln(w, "(no differences)")
		return
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// lineDiff is a longest-common-subsequence diff of two line lists: removed
// lines "-", added "+", with ctx lines of context around each change and
// "..." where unchanged lines are left out. Empty when nothing differs.
func lineDiff(a, b []string, ctx int) []string {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var all []string
	changed := []bool{}
	push := func(s string, c bool) { all = append(all, s); changed = append(changed, c) }
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			push("  "+a[i], false)
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			push("+ "+b[j], true)
			j++
		default:
			push("- "+a[i], true)
			i++
		}
	}
	keep := make([]bool, len(all))
	differs := false
	for k, c := range changed {
		if !c {
			continue
		}
		differs = true
		for d := -ctx; d <= ctx; d++ {
			if k+d >= 0 && k+d < len(all) {
				keep[k+d] = true
			}
		}
	}
	if !differs {
		return nil
	}
	var out []string
	gap := false
	for k, l := range all {
		if !keep[k] {
			gap = true
			continue
		}
		if gap && len(out) > 0 {
			out = append(out, "  ...")
		}
		gap = false
		out = append(out, l)
	}
	return out
}

// restoreRoleKey puts a role's key back into the repository's
// project.yaml when the instructions map does not carry it (§5.9), so a
// file `--take-default` restores is configured again. The worker on the
// instructions_file alias, a key already present and a repository with
// no readable project.yaml keep what they have.
func restoreRoleKey(root, role string) (bool, error) {
	path := filepath.Join(root, filepath.FromSlash(engine.ProjectConfigPath))
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	cfg, err := engine.ParseProjectConfig(content)
	if err != nil {
		// A file init did not write is not take-default's to fix.
		return false, nil
	}
	if _, ok := cfg.RoleFiles[role]; ok {
		return false, nil
	}
	if role == engine.RoleWorker && cfg.InstructionsFile != "" {
		return false, nil // the alias is the worker's key (§5.9)
	}
	if err := initSafeFile(path); err != nil {
		return false, err
	}
	next, err := engine.ProjectConfigRoleKey(content, role)
	if err != nil {
		return false, err
	}
	if err := writeInitFile(path, content, next, true); err != nil {
		return false, err
	}
	return true, nil
}

// runTakeDefault is `brigada init --take-default <role|skill>` (§5.9): the
// file is reset to the new default under a fresh header, whatever was in
// it. A role whose key project.yaml's instructions map does not carry
// gets the key back at its default path, so the restored file is
// configured again; the worker on the instructions_file alias keeps that
// alias. It touches the one file and the map's one line, and no engine:
// `brigada project import` applies it.
func runTakeDefault(stdout, stderr io.Writer, name string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(stderr, err)
	}
	root, err := gitRoot(cwd)
	if err != nil {
		return fail(stderr, err)
	}
	t, err := resolveGuidance(root, name)
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown role or skill") {
			return usagef(stderr, "--take-default: %v", err)
		}
		return fail(stderr, err)
	}
	if err := wiringSafePath(root, t.rel); err != nil {
		return fail(stderr, err)
	}
	// A role left not configured gets its key back beside the file, so
	// the restore reaches the import (§5.9).
	if !t.skill {
		added, err := restoreRoleKey(root, t.name)
		if err != nil {
			return fail(stderr, err)
		}
		if added {
			fmt.Fprintf(stdout, "%s: instructions.%s key added at the default path\n", engine.ProjectConfigPath, t.name)
		}
	}
	path := filepath.Join(root, filepath.FromSlash(t.rel))
	if err := initSafeFile(path); err != nil {
		return fail(stderr, err)
	}
	old, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return fail(stderr, err)
	}
	next := generatedRoleFile(t.text)
	if t.skill {
		next = generatedSkillFile(t.text)
	}
	if exists {
		if string(old) == next {
			fmt.Fprintf(stdout, "%s: already the default of version %s; unchanged\n", t.rel, defaults.Version)
			return 0
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fail(stderr, err)
	}
	if err := writeInitFile(path, old, []byte(next), exists); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s: reset to the default of version %s under a fresh header\nbrigada project import applies it\n", t.rel, defaults.Version)
	return 0
}
