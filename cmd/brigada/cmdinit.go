package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/mattn/go-isatty"
)

// initBinary is empty in production (this executable); integration tests use
// the built CLI to exercise real detached engine and runner startup.
var initBinary string

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) (code int) {
	fs := newFlagSet("init",
		"brigada init [--probe | --yes] [--verbose] [options] (inside one repository, or in a folder holding several: then --all selects them)",
		"Probe and confirm setup of one repository or a folder of repositories.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	yes := fs.Bool("yes", false, "apply the displayed plan without prompting (does not confirm authentication)")
	probe := fs.Bool("probe", false, "print the plan only; write nothing and start nothing")
	verbose := fs.Bool("verbose", false, "also print the full contents of new repository setup files")
	limited := fs.Bool("accept-limited", false, "acknowledge fewer than two review families; reviews still require another family")
	check := fs.String("check", "", "default check command (suggested from build files; never executed by init)")
	tools := fs.String("tools", "", "worker scope in runner tools= syntax; applied only to Claude workers")
	foreman := fs.String("foreman", "", "choose a usable resume-capable agent as harness/model")
	noWiring := fs.Bool("no-wiring", false, "skip harness wiring (the bridge, hooks and foreman skill for mode A; in a parent folder, the coordinator's)")
	all := fs.Bool("all", false, "in a folder that is not a repository: register every new repository found, without asking")
	agents := &agentSpecs{}
	fs.Var(agents, "agent", "repeatable, same complete syntax and model defaults as brigada runner")
	signedIn := map[string]bool{}
	fs.Func("signed-in", "confirm an installed harness is authenticated (repeatable or comma-separated); local only", func(v string) error {
		for _, h := range splitList(v) {
			if !slices.Contains(initHarnesses, h) {
				return fmt.Errorf("unknown harness %q", h)
			}
			signedIn[h] = true
		}
		return nil
	})
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "init takes no positional arguments; run it inside one repository")
	}
	// Setup refusals retain init’s exit code 1; flag and argument errors above use 2.
	defer func() {
		if code != 0 {
			code = 1
		}
	}()
	toolsProvided := *tools != ""
	if *yes && *probe {
		return usagef(stderr, "--yes and --probe are mutually exclusive")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(stderr, err)
	}
	root, err := gitRoot(cwd)
	if err != nil {
		// Not a repository: the parent-folder run (design §13.1, §4.6 mode B).
		if *check != "" || toolsProvided || len(agents.specs) > 0 || *limited || len(signedIn) > 0 {
			return usagef(stderr, "--check, --tools, --agent, --signed-in and --accept-limited set up one repository; this folder is not one (parent-folder init takes --all, --foreman, --yes, --probe and --no-wiring)")
		}
		return runInitParent(ctx, cwd, parentOptions{yes: *yes, probe: *probe, all: *all, noWiring: *noWiring, foreman: *foreman}, common, bufio.NewReader(projectStdin), stdout, stderr)
	}
	if *all {
		return usagef(stderr, "--all selects projects in a parent folder; this is one repository")
	}
	// An origin must exist; a real local bare remote is valid. Multiple origin
	// URLs with different identities are ambiguous and must be fixed by the user.
	origin, canon, err := initOrigin(ctx, root)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "brigada init: set up %s\n\n", canon)
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
	reader := bufio.NewReader(projectStdin)
	canAsk := !*yes && !*probe
	asked := false
	if canAsk {
		fmt.Fprint(stdout, "brigada runs coding tasks on the agent CLIs you are signed in to. A foreman\n(an agent session) turns your request into a plan of tasks; a runner in the\nbackground runs each task in its own git worktree, runs your check, and has\nan agent of another model family review it. Init asks a few questions, shows\nwhat it will write and start, and changes nothing until you confirm.\n\n")
	}
	terminal := false
	if f, ok := projectStdin.(*os.File); ok {
		terminal = isatty.IsTerminal(f.Fd())
	}
	askLine := func(prompt, flags string) (string, error) {
		if !canAsk {
			return "", nil
		}
		asked = true
		if terminal {
			fmt.Fprint(stdout, prompt)
		} else {
			fmt.Fprintln(stdout, strings.TrimSpace(prompt))
		}
		v, err := reader.ReadString('\n')
		if err == io.EOF && len(v) == 0 {
			return "", fmt.Errorf("no input available; supply %s; use --agent for accounts and --signed-in for authentication", flags)
		}
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("cannot read answer (%v); supply %s; use --agent for accounts and --signed-in for authentication", err, flags)
		}
		return strings.TrimSuffix(strings.TrimSuffix(v, "\n"), "\r"), nil
	}
	ask := func(prompt, flags string) (string, error) {
		v, err := askLine(prompt, flags)
		return strings.TrimSpace(v), err
	}
	confirm := func(prompt, flags string) (bool, error) {
		v, err := ask(prompt+" [y/N] ", flags)
		v = strings.ToLower(v)
		return v == "y" || v == "yes", err
	}
	probes := map[string]harnessProbe{}
	fmt.Fprintln(stdout, "Agent CLIs found:")
	for _, h := range initHarnesses {
		p := probeHarness(ctx, h)
		probes[h] = p
		fmt.Fprintf(stdout, "  %-9s %s\n", h, p.status)
	}
	fmt.Fprintln(stdout)
	authenticate := func(spec agentSpec) (bool, error) {
		if probes[spec.harness].signedIn || signedIn[spec.harness] {
			return true, nil
		}
		name := spec.harness
		if spec.accountLabel() != spec.harness {
			name += " account " + spec.accountLabel()
		}
		return confirm("Are you signed in to "+name+"?", "--signed-in "+spec.harness)
	}
	if len(agents.specs) == 0 && canAsk {
		for _, h := range initHarnesses {
			if !probes[h].installed {
				continue
			}
			authenticated, err := authenticate(agentSpec{harness: h})
			if err != nil {
				return fail(stderr, err)
			}
			if !authenticated {
				continue
			}
			signedIn[h] = true
			question := h + " model"
			if h == "opencode" {
				question += " as provider/model, e.g. zai-coding-plan/glm-5.3"
			}
			prompt := fmt.Sprintf("%s [%s] (Enter = default, - = don't use %s): ", question, initModelName(runnerDefaultModel(h)), h)
			for attempt := 0; attempt < 3; attempt++ {
				model, err := askLine(prompt, "--agent harness="+h+",model=MODEL")
				if err != nil {
					return fail(stderr, err)
				}
				if model == "-" {
					break
				}
				if model == "" {
					model = runnerDefaultModel(h)
				}
				if _, err := initModelFamily(h, model); err != nil {
					if attempt == 2 {
						return usagef(stderr, "--agent %s: %v", h, err)
					}
					fmt.Fprintf(stdout, "  %v; try again.\n", err)
					continue
				}
				agents.specs = append(agents.specs, agentSpec{harness: h, model: model, classes: splitList(defaultClasses)})
				break
			}
		}
	}
	var usable []agentSpec
	var available []agentSpec
	seen := map[string]bool{}
	families := map[string]bool{}
	for _, spec := range agents.specs {
		if !slices.Contains(initHarnesses, spec.harness) {
			return usagef(stderr, "unsupported init harness %q", spec.harness)
		}
		if seen[spec.harness] {
			return usagef(stderr, "harness %q is offered twice: at most one agent per harness per runner", spec.harness)
		}
		seen[spec.harness] = true
		if spec.model == "" {
			spec.model = runnerDefaultModel(spec.harness)
		}
		_, err := initModelFamily(spec.harness, spec.model)
		if err != nil {
			return usagef(stderr, "--agent %s: %v", spec.harness, err)
		}
		if err := engine.ValidateSettingValue(engine.SettingApprovalClasses, strings.Join(spec.classes, "+")); err != nil {
			return usagef(stderr, "agent classes: %v", err)
		}
		if spec.agentFile != "" {
			spec.agentFile, err = filepath.Abs(spec.agentFile)
			if err != nil {
				return fail(stderr, err)
			}
		}
		// Validate even agents that turn out to be unavailable, before any writes.
		if _, _, err := resolveRunnerAgents(rt, []agentSpec{spec}, ""); err != nil {
			return usagef(stderr, "%v", err)
		}
		p := probes[spec.harness]
		if !p.installed {
			fmt.Fprintf(stdout, "  skipping %s: not on PATH\n", spec.harness)
			continue
		}
		available = append(available, spec)
	}
	// Keep all questions in CLI order, even when --agent flags use another order.
	for _, h := range initHarnesses {
		for _, spec := range available {
			if spec.harness != h {
				continue
			}
			authenticated, err := authenticate(spec)
			if err != nil {
				return fail(stderr, err)
			}
			signedIn[h] = authenticated
		}
	}
	for _, spec := range available {
		if !signedIn[spec.harness] {
			fmt.Fprintf(stdout, "  skipping %s: authentication unconfirmed; sign in and pass --signed-in %s or confirm interactively\n", spec.harness, spec.harness)
			continue
		}
		usable = append(usable, spec)
		if slices.Contains(spec.classes, "review") {
			family, _ := adapter.AgentFamily(spec.harness, spec.model)
			if family != "" {
				families[family] = true
			}
		}
	}
	if len(usable) == 0 {
		return fail(stderr, fmt.Errorf("zero usable agents; install and sign in to a supported harness, then name it with --agent and confirm authentication with --signed-in; nothing written or started"))
	}
	isLimited := len(families) < 2
	if isLimited {
		fmt.Fprintln(stdout, "LIMITED SETUP: fewer than two distinct review-capable model families. Sign in to a harness of another family, or add a model of another family with classes=review. Cross-family reviews that cannot be served will wait; inspect brigada status.")
		if !*limited {
			return fail(stderr, fmt.Errorf("setup stopped; add a second review family or explicitly acknowledge with --accept-limited; nothing written or started"))
		}
	}
	var chosen *agentSpec
	for i := range usable {
		s := &usable[i]
		if adapter.ResumeCapable(s.harness) && (*foreman == "" || *foreman == s.harness+"/"+s.model) {
			chosen = s
			break
		}
	}
	if chosen == nil {
		return usagef(stderr, "--foreman must name a usable agent that resumes headless (claude/model or codex/model); none matched")
	}
	file := filepath.Join(root, filepath.FromSlash(engine.ProjectConfigPath))
	if err := initSafeFile(file); err != nil {
		return fail(stderr, err)
	}
	old, err := os.ReadFile(file)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return fail(stderr, err)
	}
	if exists {
		cfg, err := engine.ParseProjectConfig(old)
		if err != nil {
			return fail(stderr, err)
		}
		if cfg.Repo != canon {
			return fail(stderr, fmt.Errorf("project.yaml names another repository; correct it before init"))
		}
		if *check == "" {
			_ = json.Unmarshal(cfg.Values[engine.SettingDefaultCheck], check)
		}
		if *tools == "" {
			_ = json.Unmarshal(cfg.Values[engine.SettingWorkerTools], tools)
		}
	}
	if *check == "" || *tools == "" {
		suggestion, scope, source, err := suggestInitDefaultsWithSource(root)
		if err != nil {
			return fail(stderr, err)
		}
		if *check == "" {
			if canAsk && suggestion != "" {
				answer, err := ask(fmt.Sprintf("Check to run after every task [%s] (from %s): ", suggestion, source), "--check COMMAND")
				if err != nil {
					return fail(stderr, err)
				}
				if answer != "" {
					suggestion = answer
				}
			}
			*check = suggestion
		}
		if *tools == "" {
			*tools = scope
		}
	}

	if *check == "" {
		*check, err = ask("Default check (required; --check COMMAND): ", "--check COMMAND")
		if err != nil {
			return fail(stderr, err)
		}
	}
	if *tools == "" {
		*tools, err = ask("Worker tools (required; --tools PATTERN+PATTERN or none): ", "--tools PATTERN+PATTERN or none")
		if err != nil {
			return fail(stderr, err)
		}
	}
	if *check == "" || *tools == "" {
		return usagef(stderr, "no build-file defaults matched; supply --check and --tools, or answer interactively")
	}
	proposed, err := engine.ProjectConfigDefaults(old, canon, *check, *tools)
	if err != nil {
		return fail(stderr, err)
	}
	parsedTools, err := lifecycle.ParseTools(*tools)
	if err != nil {
		return usagef(stderr, "--tools: %v", err)
	}
	for i := range usable {
		if usable[i].harness == "claude" && usable[i].tools == nil && (toolsProvided || rt.file.Tools["claude/"+usable[i].model] == nil) {
			usable[i].tools = parsedTools
		}
	}
	_, adverts, err := resolveRunnerAgents(rt, usable, "")
	if err != nil {
		return usagef(stderr, "%v", err)
	}
	if asked {
		fmt.Fprintln(stdout)
	}
	printInitAgents(stdout, usable, len(families), *chosen)
	fmt.Fprintf(stdout, "  Check after every task: %s (init does not run it; edit .brigada/project.yaml to change it)\n  Worker permissions:\n", *check)
	for _, a := range adverts {
		var permissions string
		switch a.Harness {
		case "claude":
			permissions = "edits files; no shell"
			if a.Tools != "none" {
				permissions = "edits files; shell limited to: " + a.Tools
			}
		case "codex":
			permissions = "edits files and runs commands inside codex's workspace-write sandbox"
		case "kimi":
			permissions = "edits files and runs commands headless, with no approval prompts"
		case "opencode":
			permissions = "edits files; no shell"
		}
		fmt.Fprintf(stdout, "    %-9s %s\n", a.Harness, permissions)
	}
	var wiring []wireFile
	if !*noWiring && probes["claude"].installed {
		wiring = planWiring(root)
	}
	files := append([]wireFile{{rel: engine.ProjectConfigPath, old: old, next: proposed, exists: exists}}, wiring...)
	printInitFiles(stdout, files, *verbose)
	if !*noWiring {
		if probes["codex"].installed {
			fmt.Fprint(stdout, codexWiringText)
		}
		var workers []string
		for _, h := range []string{"kimi", "opencode"} {
			if probes[h].installed {
				workers = append(workers, h)
			}
		}
		if len(workers) > 0 {
			fmt.Fprintf(stdout, "  %s: work as workers only; nothing to set up.\n", strings.Join(workers, ", "))
		}
	}
	fmt.Fprintln(stdout, "  Background: the engine starts when a command needs it and exits after 30 idle minutes.")
	fmt.Fprintf(stdout, "    The runner starts now and keeps running; log: %s; stop it with: brigada stop\n", filepath.Join(rt.stateDir, "runner.log"))
	fmt.Fprintln(stdout)
	if *probe {
		fmt.Fprintln(stdout, "Probe only; nothing written or started. Re-run without --probe to confirm, or with --yes to apply.")
		return 0
	}
	if !*yes {
		files := 0
		if !exists || !bytes.Equal(old, proposed) {
			files++
		}
		for _, f := range wiring {
			if f.changed() {
				files++
			}
		}
		_, running, _ := runnerstate.Running(runnerStateDir(rt))
		apply, err := confirm(initApplyPrompt(files, running), "--yes to apply the displayed plan")
		if err != nil {
			return fail(stderr, err)
		}
		if !apply {
			fmt.Fprintln(stdout, "Declined; nothing written or started.")
			return 1
		}
	}
	// The first mutation is after the complete plan and confirmation. A
	// concurrent edit must not be silently overwritten after the review.
	if err := writeInitFile(file, old, proposed, exists); err != nil {
		return fail(stderr, err)
	}
	partial := func(err error) int {
		return fail(stderr, fmt.Errorf("partial init: project.yaml is in place; %w; fix the error and rerun init", err))
	}
	if err := applyWiring(wiring); err != nil {
		return partial(err)
	}
	written := 0
	if !exists || !bytes.Equal(old, proposed) {
		written++
	}
	for _, f := range wiring {
		if f.changed() {
			written++
		}
	}
	if err := lifecycle.EnsureEngine(ctx, lifecycle.StartOptions{Socket: rt.paths.Socket, StateDir: rt.stateDir, ConfigPath: rt.configForStart(), Binary: initBinary}); err != nil {
		return partial(err)
	}
	// Reuse project import's validation, first-import confirmation and conflict
	// reporting, rather than implementing a second provenance path.
	importArgs := []string{"--repo", origin, "--file", file, "--state-dir", rt.stateDir, "--no-start"}
	if rt.explicitCfg {
		importArgs = append(importArgs, "--config", rt.configPath)
	}
	if *yes {
		importArgs = append(importArgs, "--yes")
	}
	code = runProjectImportWithConfirm(ctx, importArgs, stdout, stderr, func(prompt string) (bool, error) {
		return confirm(prompt, "--yes to accept replacing hand-set values on import")
	})
	if code != 0 {
		return partial(fmt.Errorf("project import did not finish; the engine may be running; no runner was started"))
	}
	client, code := connect(ctx, rt, "client", true, stderr)
	if code != 0 {
		return partial(fmt.Errorf("connect after import failed"))
	}
	currentForeman, err := client.ProjectForeman(ctx, origin)
	if err != nil {
		return partial(fmt.Errorf("import succeeded but reading foreman failed: %w", err))
	}
	if currentForeman.Harness != chosen.harness || currentForeman.Model != chosen.model || currentForeman.Effort != chosen.effort {
		if _, err := client.SetProjectForeman(ctx, origin, chosen.harness, chosen.model, chosen.effort); err != nil {
			return partial(fmt.Errorf("import succeeded but foreman registration failed: %w", err))
		}
	}

	runnerArgs := []string{}
	for _, s := range usable {
		runnerArgs = append(runnerArgs, "--agent", s.String())
	}
	result, err := lifecycle.EnsureRunner(ctx, lifecycle.RunnerStartOptions{StateDir: rt.stateDir, ConfigPath: rt.configForStart(), Binary: initBinary, Args: runnerArgs})
	if err != nil {
		return partial(fmt.Errorf("project registered and imported, but runner startup failed: %w", err))
	}
	agentsMatch := true
	var done strings.Builder
	done.WriteString("Done: ")
	switch written {
	case 0:
		done.WriteString("no files needed writing.")
	case 1:
		done.WriteString("wrote 1 file (not committed).")
	default:
		fmt.Fprintf(&done, "wrote %d files (not committed).", written)
	}
	if result.Existing {
		difference := "agents match the chosen ones"
		if result.Record.Agents == nil {
			agentsMatch = false
			difference = "existing agents unavailable; cannot compare to chosen ones"
		} else if !sameInitAgents(result.Record.Agents, adverts) || (result.Record.Specs != nil && !sameInitSpecs(result.Record.Specs, runnerSpecStrings(usable))) {
			agentsMatch = false
			difference = "agents differ from the chosen ones; existing agents kept"
		}
		fmt.Fprintf(&done, " Runner already running (pid %d); no second runner started; %s.", result.Record.PID, difference)
	} else {
		fmt.Fprintf(&done, " Detached runner started (pid %d).", result.Record.PID)
	}
	if isLimited {
		done.WriteString(" Limited setup applied; cross-family reviews may wait.")
	} else if agentsMatch {
		done.WriteString(" Setup ready.")
	} else {
		done.WriteString(" Project configured; inspect the existing runner agents above before sending work.")
	}
	fmt.Fprintln(stdout, done.String())
	fmt.Fprintln(stdout, "Next:")
	step := 1
	if !*noWiring && probes["claude"].installed {
		fmt.Fprintf(stdout, "  %d. Open Claude Code here (`claude`), approve the brigada MCP server when asked, and say what you want built. Your session plans; you approve the plan.\n", step)
		step++
	}
	fmt.Fprintf(stdout, "  %d. Or, without a session: brigada ask \"what you want built\"\n", step)
	fmt.Fprintln(stdout, "  Watch progress: brigada status    Stop the runner: brigada stop")
	return 0
}

func initModelName(model string) string {
	if model == "default" {
		return "CLI default"
	}
	return model
}

func initModelFamily(harness, model string) (string, error) {
	if harness == "opencode" && model != "default" && !strings.Contains(model, "/") {
		return "", fmt.Errorf("opencode needs provider/model, e.g. zai-coding-plan/glm-5.3")
	}
	return adapter.AgentFamily(harness, model)
}

func printInitAgents(w io.Writer, agents []agentSpec, families int, foreman agentSpec) {
	fmt.Fprintln(w, "Plan\n  Agents (each implements, reviews and answers questions):")
	for _, s := range agents {
		family, _ := adapter.AgentFamily(s.harness, s.model)
		switch family {
		case "":
			family = "unknown (give provider/model to count it for review)"
		case "anthropic":
			family = "Anthropic"
		case "openai":
			family = "OpenAI"
		case "moonshot":
			family = "Moonshot"
		case "zhipu":
			family = "Zhipu (GLM)"
		case "google":
			family = "Google"
		}
		classes := ""
		if len(s.classes) != 3 || !slices.Contains(s.classes, "implement") || !slices.Contains(s.classes, "review") || !slices.Contains(s.classes, "answer") {
			classes = " (" + strings.Join(s.classes, ", ") + " only)"
		}
		fmt.Fprintf(w, "    %-9s %-25s family: %s%s\n", s.harness, initModelName(s.model), family, classes)
	}
	fmt.Fprintf(w, "  Review: %d model families; a task is always reviewed by a family other than its author's.\n", families)
	fmt.Fprintf(w, "  Foreman: %s (the first agent that can resume a session headless; change it with --foreman HARNESS/MODEL)\n", agentName(foreman.harness, foreman.model, foreman.effort))
}

func initApplyPrompt(files int, running bool) string {
	noun := "files"
	if files == 1 {
		noun = "file"
	}
	switch {
	case files > 0 && !running:
		return fmt.Sprintf("Write %d %s and start the runner?", files, noun)
	case files > 0:
		return fmt.Sprintf("Write %d %s?", files, noun)
	case !running:
		return "Start the runner?"
	default:
		return "Register the project?"
	}
}

func initOrigin(ctx context.Context, root string) (string, string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "remote", "get-url", "--all", "origin").Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return "", "", fmt.Errorf("no origin: configure an origin remote before init; a repository without a remote is unsupported")
	}
	var origin, canon string
	for _, raw := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, err := engine.Canonicalize(raw)
		if err != nil {
			return "", "", fmt.Errorf("origin is ambiguous or invalid: %w; configure an unambiguous origin URL before init", err)
		}
		if canon != "" && canon != id {
			return "", "", fmt.Errorf("origin has multiple canonical identities; select one origin URL before init")
		}
		if canon == "" {
			origin, canon = raw, id
		}
	}
	return origin, canon, nil
}

func printInitDiff(w io.Writer, old, next []byte) {
	fmt.Fprintf(w, "      Diff for %s:\n      --- existing\n      +++ proposed\n", engine.ProjectConfigPath)
	if bytes.Equal(old, next) {
		fmt.Fprintln(w, "      (unchanged)")
		return
	}
	// A complete replacement hunk makes every retained comment/setting visible.
	a, b := strings.Split(strings.TrimSuffix(string(old), "\n"), "\n"), strings.Split(strings.TrimSuffix(string(next), "\n"), "\n")
	fmt.Fprintf(w, "      @@ -1,%d +1,%d @@\n", len(a), len(b))
	for _, line := range a {
		fmt.Fprintln(w, "      -"+line)
	}
	for _, line := range b {
		fmt.Fprintln(w, "      +"+line)
	}
}

func initSafeFile(file string) error {
	for _, path := range []string{filepath.Dir(file), file} {
		s, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if s.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink at %s; keep project configuration inside the repository", path)
		}
	}
	return nil
}
func writeInitFile(file string, old, next []byte, existed bool) error {
	if err := initSafeFile(file); err != nil {
		return err
	}
	current, err := os.ReadFile(file)
	if (existed && (err != nil || !bytes.Equal(current, old))) || (!existed && !os.IsNotExist(err)) {
		return fmt.Errorf("%s changed after the plan was printed; rerun init to review it", filepath.Base(file))
	}
	if existed && bytes.Equal(old, next) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if existed {
		s, err := os.Stat(file)
		if err != nil {
			return err
		}
		mode = s.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".brigada-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(next); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
func sameInitAgents(a, b []api.Agent) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	sort := func(items []api.Agent) {
		slices.SortFunc(items, func(a, b api.Agent) int { return strings.Compare(a.Harness+"/"+a.Model, b.Harness+"/"+b.Model) })
	}
	sort(a)
	sort(b)
	return reflect.DeepEqual(a, b)
}

func sameInitSpecs(a, b []string) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
