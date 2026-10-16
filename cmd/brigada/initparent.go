package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

// Init in a folder that is not a repository (design §13.1 "Parent folder",
// §4.6 mode B): scan the direct children, print what is there, register
// only what the user selects, and wire the coordinator in the folder's own
// project scope. Nothing is written into a child repository.

const (
	coordinatorServerName  = "brigada-coordinator"
	coordinatorSkillFile   = ".claude/skills/brigada-coordinator/SKILL.md"
	coordinatorSkillMarker = "<!-- brigada-managed: coordinator-instructions"
)

// codexCoordinatorText is what init prints for Codex in a parent folder:
// like the foreman's, a user-level command that init never runs.
const codexCoordinatorText = `  codex: no verified project scope; init writes nothing for it and never runs this. To register the coordinator, run it yourself (user-level):
      codex mcp add brigada-coordinator -- brigada mcp --coordinator
`

// parentChild is one direct child of the scanned folder.
type parentChild struct {
	name   string
	path   string
	origin string // the origin URL as configured; never printed (it may hold a token)
	canon  string
	skip   string // why the child is not a candidate; empty = a candidate
	known  bool
}

// scanParent lists the direct children of dir that could be projects. It
// reads the folder one level deep only: hidden entries are left out,
// symlinks are listed as skipped and never followed, a child counts as a
// repository only when its own top level is the child itself (so a plain
// folder inside some enclosing repository is not mistaken for one), and a
// repository is never descended into. The result is in name order; the
// second child with a canonical URL already seen is a duplicate of the
// first, never a second project.
func scanParent(ctx context.Context, dir string) ([]parentChild, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []parentChild
	first := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		c := parentChild{name: name, path: filepath.Join(dir, name)}
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			c.skip = "symlink, not followed"
		case !e.IsDir():
			continue
		default:
			classifyChild(ctx, &c)
		}
		if c.skip == "" {
			if other, dup := first[c.canon]; dup {
				c.skip = "duplicate of " + other + " (one project)"
			} else {
				first[c.canon] = name
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// classifyChild decides whether a directory child is a repository with a
// usable origin, setting skip when it is not.
func classifyChild(ctx context.Context, c *parentChild) {
	st, err := os.Lstat(filepath.Join(c.path, ".git"))
	switch {
	case err != nil:
		c.skip = "not a git repository (not descended into)"
		return
	case st.Mode()&fs.ModeSymlink != 0:
		c.skip = ".git is a symlink, not followed"
		return
	}
	top, err := gitRoot(c.path)
	if err != nil || !sameDir(top, c.path) {
		c.skip = "not a git repository (not descended into)"
		return
	}
	origin, canon, err := initOrigin(ctx, c.path)
	switch {
	case err == nil:
		c.origin, c.canon = origin, canon
	case strings.HasPrefix(err.Error(), "no origin"):
		c.skip = "no origin remote"
	default:
		c.skip = "ambiguous origin"
	}
}

// sameDir reports whether two paths name one directory once their
// symlinked prefixes (a temp dir under a symlinked parent) are resolved.
func sameDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

// parentOptions are the init flags the parent-folder run uses.
type parentOptions struct {
	yes, probe, all, noWiring bool
	foreman                   string
}

// coordinatorSkillContent is the brigada-owned coordinator skill.
func coordinatorSkillContent() []byte {
	return []byte(fmt.Sprintf(`---
name: brigada-coordinator
description: Rules for coordinating several repositories through brigada from this folder. Use when the user asks to plan, hand off, check or approve work across these projects, or asks about brigada plans, tasks or status.
---
%s v%d -->

These are brigada's coordinator rules for your own session (mode B). Brigada
owns this file and replaces it on "brigada init"; put your own instructions
elsewhere.

%s`, coordinatorSkillMarker, foreman.CoordinatorInstructionsVersion, foreman.CoordinatorInstructions()))
}

// planCoordinatorWiring computes the folder's project-scope changes; it
// reads files and writes nothing. No hook and no permission entry is
// written: the two approval tools stay on the harness's own "ask".
func planCoordinatorWiring(dir string) []wireFile {
	skill := coordinatorSkillContent()
	return planSteps(dir, []wireStep{
		{wiringMCPFile, func(d []byte, exists bool) ([]byte, string, error) {
			return mergeMCPEntry(d, exists, coordinatorServerName, []string{"mcp", "--coordinator"})
		}},
		{coordinatorSkillFile, func(d []byte, exists bool) ([]byte, string, error) {
			return mergeSkillMarked(d, exists, skill, coordinatorSkillMarker)
		}},
	})
}

func printCoordinatorWiringPlan(w io.Writer, files []wireFile) {
	fmt.Fprintln(w, "Coordinator wiring (mode B; the folder's own project scope, written only on apply, never committed or pushed):")
	for _, f := range files {
		switch {
		case f.err != nil:
			fmt.Fprintf(w, "    %s: left alone, not changed (%v)\n", f.rel, f.err)
		case f.state == "unchanged":
			fmt.Fprintf(w, "    %s: already wired; unchanged\n", f.rel)
		default:
			fmt.Fprintf(w, "    %s: %s\n", f.rel, f.state)
			if f.rel == coordinatorSkillFile && !f.exists {
				fmt.Fprintf(w, "      new brigada-owned file, %d bytes, coordinator instructions v%d\n", len(f.next), foreman.CoordinatorInstructionsVersion)
				continue
			}
			printFileDiff(w, f.rel, f.old, f.next, f.exists)
		}
	}
}

// runInitParent is `brigada init` outside a repository.
func runInitParent(ctx context.Context, dir string, opts parentOptions, common *commonFlags, reader *bufio.Reader, stdout, stderr io.Writer) int {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return fail(stderr, err)
	}
	children, err := scanParent(ctx, dir)
	if err != nil {
		return fail(stderr, err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	rt.stateDir, err = filepath.Abs(rt.stateDir)
	if err != nil {
		return fail(stderr, err)
	}
	rt.paths = lifecycle.PathsOf(rt.stateDir)
	if rt.configPath != "" {
		rt.configPath, err = filepath.Abs(rt.configPath)
		if err != nil {
			return fail(stderr, err)
		}
	}
	canAsk := !opts.yes && !opts.probe
	ask := func(prompt string) (string, error) {
		fmt.Fprintln(stdout, strings.TrimSpace(prompt))
		v, err := reader.ReadString('\n')
		if err == io.EOF && len(v) == 0 {
			return "", fmt.Errorf("no input available; select with --all")
		}
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("cannot read answer (%v); select with --all", err)
		}
		return strings.TrimSpace(v), nil
	}

	// Known or new is read from a running engine; init starts none to look.
	var engineProjects []engine.ProjectInfo
	engineUp := false
	if lifecycle.EngineRunning(ctx, rt.paths.Socket) {
		if _, secret, err := lifecycle.TokenForRole(rt.paths.Tokens, "client"); err == nil {
			if list, err := api.NewClient(rt.paths.Socket, secret).Projects(ctx); err == nil {
				engineProjects, engineUp = list, true
			}
		}
	}
	for i := range children {
		for _, p := range engineProjects {
			if children[i].skip == "" && p.RepoURL == children[i].canon {
				children[i].known = true
			}
		}
	}

	fmt.Fprintf(stdout, "Init plan for the folder %s (not a repository)\n", dir)
	fmt.Fprintln(stdout, "Direct children only; hidden directories are skipped, symlinks are not followed, repositories are not descended into.")
	var register []int // children[i] the user can select
	for i, c := range children {
		switch {
		case c.skip != "":
			fmt.Fprintf(stdout, "  [-] %s: skipped: %s\n", c.path, c.skip)
		case c.known:
			fmt.Fprintf(stdout, "  [-] %s  %s  known\n", c.path, c.canon)
		default:
			register = append(register, i)
			state := "new"
			if !engineUp {
				state = "unknown (engine not running), treated as new"
			}
			fmt.Fprintf(stdout, "  [%d] %s  %s  %s\n", len(register), c.path, c.canon, state)
		}
	}
	if len(children) == 0 {
		fmt.Fprintln(stdout, "  (no directories here)")
	}

	selected := map[int]bool{}
	switch {
	case opts.all:
		for _, i := range register {
			selected[i] = true
		}
	case canAsk && len(register) > 0:
		answer, err := ask("Register which? Numbers (1,3), all, or blank for none:")
		if err != nil {
			return fail(stderr, err)
		}
		picks, err := parseSelection(answer, len(register))
		if err != nil {
			return fail(stderr, err)
		}
		for _, n := range picks {
			selected[register[n-1]] = true
		}
	}
	var picked []parentChild
	for i, c := range children {
		if selected[i] {
			picked = append(picked, c)
		}
	}

	harness, model := "", ""
	if len(picked) > 0 {
		harness, model, err = parentForeman(opts.foreman, engineProjects)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "Register %d project(s) with foreman %s/%s:\n", len(picked), harness, model)
		for _, c := range picked {
			fmt.Fprintf(stdout, "  %s\n", c.canon)
		}
	} else {
		fmt.Fprintln(stdout, "No project selected; nothing is registered (select with --all or at the prompt).")
	}
	fmt.Fprintln(stdout, "Nothing is written into the child repositories.")

	var wiring []wireFile
	if !opts.noWiring {
		wiring = planCoordinatorWiring(dir)
		printCoordinatorWiringPlan(stdout, wiring)
		fmt.Fprint(stdout, codexCoordinatorText)
	}
	if opts.probe {
		fmt.Fprintln(stdout, "Probe only; nothing written or started. Re-run without --probe to confirm, or with --yes to apply.")
		return 0
	}
	if !opts.yes {
		answer, err := ask("Apply this plan? [y/N]")
		if err != nil {
			return fail(stderr, err)
		}
		if a := strings.ToLower(answer); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Declined; nothing written or started.")
			return 1
		}
	}

	if err := applyWiring(wiring); err != nil {
		return fail(stderr, fmt.Errorf("partial init: %w; fix the error and rerun init", err))
	}
	for _, f := range wiring {
		if f.changed() {
			fmt.Fprintf(stdout, "Wired %s (%s); no git commit or push.\n", f.rel, f.state)
		}
	}
	if len(picked) > 0 {
		if err := lifecycle.EnsureEngine(ctx, lifecycle.StartOptions{Socket: rt.paths.Socket, StateDir: rt.stateDir, ConfigPath: rt.configForStart(), Binary: initBinary}); err != nil {
			return fail(stderr, fmt.Errorf("partial init: the coordinator is wired; %w; rerun init", err))
		}
		_, secret, err := lifecycle.TokenForRole(rt.paths.Tokens, "client")
		if err != nil {
			return fail(stderr, fmt.Errorf("partial init: the coordinator is wired; %w", err))
		}
		client := api.NewClient(rt.paths.Socket, secret)
		now, err := client.Projects(ctx)
		if err != nil {
			return fail(stderr, fmt.Errorf("partial init: the coordinator is wired; read the projects: %w", err))
		}
		for _, c := range picked {
			if projectKnown(now, c.canon) {
				fmt.Fprintf(stdout, "Already known: %s\n", c.canon)
				continue
			}
			if _, err := client.SetProjectForeman(ctx, c.origin, harness, model, ""); err != nil {
				return fail(stderr, fmt.Errorf("partial init: the coordinator is wired; register %s: %w", c.canon, err))
			}
			fmt.Fprintf(stdout, "Registered %s\n", c.canon)
		}
	}
	fmt.Fprintln(stdout, "Coordinator ready: open your harness in this folder. Registered projects have no runner agents from this command: run `brigada init` inside a repository for those (it also wires that repository for mode A).")
	return 0
}

func projectKnown(list []engine.ProjectInfo, canon string) bool {
	for _, p := range list {
		if p.RepoURL == canon {
			return true
		}
	}
	return false
}

// parentForeman picks the foreman a newly registered project gets: the
// --foreman flag (harness/model, a harness that resumes headless), else
// the foreman of a project the engine already has. With neither, init
// refuses before it writes anything.
func parentForeman(flag string, existing []engine.ProjectInfo) (string, string, error) {
	if flag != "" {
		harness, model, ok := strings.Cut(flag, "/")
		if !ok || harness == "" || model == "" {
			return "", "", fmt.Errorf("--foreman wants harness/model, got %q", flag)
		}
		if !adapter.ResumeCapable(harness) {
			return "", "", fmt.Errorf("--foreman must name a harness that resumes headless (claude or codex); %q does not", harness)
		}
		return harness, model, nil
	}
	for _, p := range existing {
		if p.Harness != "" && p.Model != "" {
			return p.Harness, p.Model, nil
		}
	}
	return "", "", fmt.Errorf("no foreman to give new projects: pass --foreman harness/model (no existing project has one to reuse); nothing written or started")
}

// parseSelection reads the user's answer to the selection prompt: blank or
// none selects nothing, all selects every number, otherwise numbers split
// by commas or spaces, each 1..max.
func parseSelection(answer string, max int) ([]int, error) {
	answer = strings.ToLower(strings.TrimSpace(answer))
	switch answer {
	case "", "none", "n":
		return nil, nil
	case "all", "a":
		all := make([]int, max)
		for i := range all {
			all[i] = i + 1
		}
		return all, nil
	}
	var picks []int
	seen := map[int]bool{}
	for _, f := range strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > max {
			return nil, fmt.Errorf("cannot select %q: give numbers from 1 to %d, all, or blank for none", f, max)
		}
		if !seen[n] {
			seen[n] = true
			picks = append(picks, n)
		}
	}
	return picks, nil
}
