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
	buildversion "github.com/Blackfire-AI/brigada/internal/version"
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
		return guidanceTarget{rel: rel, text: text, what: "role " + role}, nil
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
		return guidanceTarget{rel: dir + "/" + skill + "/SKILL.md", text: text, skill: true, what: "skill " + skill}, nil
	}
	return guidanceTarget{}, fmt.Errorf("unknown role or skill %q; want one of: %s", name, guidanceNames())
}

// archivedDefault is the exact text a file was generated from: the archive
// directory the header's version names, its text verified against the
// header's digest; else the archived text of that role or skill whose
// digest is the header's, from any version (an untagged build's version
// names no directory). The error says why no base can be had.
func archivedDefault(t guidanceTarget, name, version, digest string) (text, found string, err error) {
	file := name + ".md"
	if t.skill {
		file = "skills/" + name + "/SKILL.md"
	}
	text, err = defaults.ArchiveText(version, file)
	if err == nil && defaults.Digest(text) == digest {
		return text, version, nil
	}
	if text, found, ok := defaults.FindByDigest(file, digest); ok {
		return text, found, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("the header names version %s, which this brigada does not ship, and no shipped text has its digest", version)
	}
	return "", "", fmt.Errorf("the header's digest does not match version %s's shipped text, nor any other shipped text", version)
}

// runInstructionsDiff is `brigada instructions diff <role|skill>` (§5.9):
// one three-way diff of the project's file, the default it was generated
// from (found in the archive by the version in its header, else by the
// header's digest) and the new default, so your edits and brigada's change
// read together. A header whose default cannot be found, or no header at
// all, falls back to a two-way diff of the file against the new default and
// says why.
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
	var base, baseVersion string
	if !generated {
		reason = "the file has no brigada header, so what it was generated from is unknown"
	} else if base, baseVersion, err = archivedDefault(t, name, version, digest); err != nil {
		reason = err.Error()
	}
	if reason != "" {
		fmt.Fprintf(stdout, "two-way diff, because %s\n", reason)
		fmt.Fprintf(stdout, "current default is from version %s\n\n", brigadaVersion())
		printTextDiff(stdout, "your file", "new default", yours, t.text)
		return 0
	}
	fmt.Fprintf(stdout, "generated by version %s; this is version %s\n\n", version, brigadaVersion())
	printThreeWayDiff(stdout, "your file", "generated default (version "+baseVersion+")", "new default (version "+brigadaVersion()+")", yours, base, t.text)
	return 0
}

func brigadaVersion() string { return buildversion.String() }

// printThreeWayDiff prints one diff3-style diff of yours against base and
// theirs: each region that differs shows your lines, the base's and the
// new default's, so your edits and the default's change read together.
func printThreeWayDiff(w io.Writer, yoursName, baseName, theirsName, yours, base, theirs string) {
	fmt.Fprintf(w, "three-way diff: %s, %s, %s\n", yoursName, baseName, theirsName)
	lines := diff3(splitDiffLines([]byte(base)), splitDiffLines([]byte(yours)), splitDiffLines([]byte(theirs)), yoursName, baseName, theirsName, 2)
	if len(lines) == 0 {
		fmt.Fprintln(w, "(no differences)")
		return
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// lcsMatch maps each line of a to the line of b it pairs with in a
// longest common subsequence, -1 for a line with no pair.
func lcsMatch(a, b []string) []int {
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
	match := make([]int, n)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			match[i] = j
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			match[i] = -1
			i++
		default:
			j++
		}
	}
	for ; i < n; i++ {
		match[i] = -1
	}
	return match
}

// diff3 is a three-way line diff in diff3 style. Lines all three texts
// share print as context; every other region prints as
//
//	<<<<<<< yours
//	||||||| base
//	=======
//	>>>>>>> theirs
//
// with the three versions of the region. Empty when the texts agree.
func diff3(base, yours, theirs []string, yoursName, baseName, theirsName string, ctx int) []string {
	my, mt := lcsMatch(base, yours), lcsMatch(base, theirs)
	var all []string
	var changed []bool
	push := func(s string, c bool) { all = append(all, s); changed = append(changed, c) }
	i, y, t := 0, 0, 0
	for i <= len(base) {
		k := i
		for k < len(base) && (my[k] < 0 || mt[k] < 0) {
			k++
		}
		ye, te := len(yours), len(theirs)
		if k < len(base) {
			ye, te = my[k], mt[k]
		}
		by, bb, bt := yours[y:ye], base[i:k], theirs[t:te]
		if slicesEqual(by, bb) && slicesEqual(bb, bt) {
			for _, l := range bb {
				push("  "+l, false)
			}
		} else {
			push("<<<<<<< "+yoursName, true)
			for _, l := range by {
				push(l, true)
			}
			push("||||||| "+baseName, true)
			for _, l := range bb {
				push(l, true)
			}
			push("=======", true)
			for _, l := range bt {
				push(l, true)
			}
			push(">>>>>>> "+theirsName, true)
		}
		if k == len(base) {
			break
		}
		push("  "+base[k], false)
		i, y, t = k+1, ye+1, te+1
	}
	return withContext(all, changed, ctx)
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// withContext keeps the changed lines with ctx lines around them and "..."
// where unchanged lines are left out. Empty when nothing changed.
func withContext(all []string, changed []bool, ctx int) []string {
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
	return withContext(all, changed, ctx)
}

// runTakeDefault is `brigada init --take-default <role|skill>` (§5.9): the
// file is reset to the new default under a fresh header, whatever was in
// it. It touches that one file, and no engine: `brigada project import`
// applies it.
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
			fmt.Fprintf(stdout, "%s: already the default of version %s; unchanged\n", t.rel, brigadaVersion())
			return 0
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fail(stderr, err)
	}
	if err := writeInitFile(path, old, []byte(next), exists); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s: reset to the default of version %s under a fresh header\nbrigada project import applies it\n", t.rel, brigadaVersion())
	return 0
}
