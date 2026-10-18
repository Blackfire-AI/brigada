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
	"strconv"
	"strings"
	"unicode/utf8"

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
	if canAsk {
		fmt.Fprint(stdout, "brigada runs coding tasks on the agent CLIs you are signed in to. A foreman\n(an agent) turns your request into a plan of tasks; workers (other agents)\nwrite the code in their own git worktrees; your check command tests it; and\nan agent of a different model family reviews it. Three short steps follow.\nNothing is written or started until you confirm at the end.\n\n")
	}
	terminal := false
	if f, ok := projectStdin.(*os.File); ok {
		terminal = isatty.IsTerminal(f.Fd())
	}
	askLine := func(prompt, flags string) (string, error) {
		if !canAsk {
			return "", nil
		}
		if terminal {
			fmt.Fprint(stdout, prompt)
		} else {
			fmt.Fprintln(stdout, strings.TrimRight(prompt, " \t"))
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
	sources := map[string]initModelSource{}
	for _, h := range initHarnesses {
		var source initModelSource
		switch h {
		case "claude":
			source = initClaudeModels(ctx)
		case "codex":
			source = initCodexModels(ctx)
		case "kimi":
			source = initKimiModels(ctx)
		case "opencode":
			source = initOpenCodeModels(ctx)
		}
		sources[h] = source
		probes[h] = source.signIn
	}
	if canAsk {
		fmt.Fprint(stdout, "Step 1 of 3: agents\n  Each agent CLI below can work for brigada. For each one, pick the model it\n  should run, or leave it out. Reviews always go to a different model family\n  than the author's, so you need at least two families (e.g. claude + codex).\n")
	} else {
		fmt.Fprintln(stdout, "Agent CLIs found:")
		for _, h := range initHarnesses {
			printInitWrapped(stdout, fmt.Sprintf("  %-9s ", h), "  ", probes[h].status)
		}
		fmt.Fprintln(stdout)
	}
	shown := map[string]bool{}
	authenticate := func(spec agentSpec) (bool, error) {
		h := spec.harness
		confirmed := probes[h].signedIn || signedIn[h]
		if canAsk && !shown[h] {
			shown[h] = true
			status := probes[h].status
			if signedIn[h] && !probes[h].signedIn {
				status = "signed in"
			}
			switch {
			case !confirmed && h == "kimi":
				fmt.Fprint(stdout, "\n  kimi: init can't check sign-in. If you're not signed in, run `kimi login`\n  first, or leave kimi out.\n")
			case !confirmed && h == "opencode" && !probes[h].checked:
				fmt.Fprint(stdout, "\n  opencode: init can't check sign-in. If you're not signed in, run\n  `opencode auth login` first, or leave opencode out.\n")
			default:
				fmt.Fprintln(stdout)
				printInitWrapped(stdout, "  ", "  ", h+": "+status)
			}
		}
		if confirmed {
			return true, nil
		}
		name := h
		if spec.accountLabel() != h {
			name += " account " + spec.accountLabel()
		}
		if h == "kimi" || h == "opencode" {
			v, err := ask("    Use "+name+"? [y/N]: ", "--signed-in "+h)
			return strings.EqualFold(v, "y") || strings.EqualFold(v, "yes"), err
		}
		return confirm("    Are you signed in to "+name+"?", "--signed-in "+h)
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
			entries := sources[h].entries
			fmt.Fprintf(stdout, "    Which model should %s run?\n", h)
			width := 0
			for _, entry := range entries {
				width = max(width, len(initMenuModelName(entry)))
			}
			for _, entry := range entries {
				family, _ := initModelFamily(h, entry.value)
				prefix := fmt.Sprintf("      %2d  %-*s  %s", entry.id, width, initMenuModelName(entry), initFamilyName(family))
				note := entry.note
				if entry.note != "" {
					if home, err := os.UserHomeDir(); err == nil && home != "" {
						note = strings.ReplaceAll(note, "("+home+string(os.PathSeparator), "(~/")
					}
					prefix += "  "
				}
				printInitWrapped(stdout, prefix, "          ", note)
			}
			if h != "kimi" {
				fmt.Fprintf(stdout, "       -  don't use %s\n", h)
			}
			if sources[h].unknownCount > 0 {
				command := "opencode models"
				if h == "codex" {
					command = "codex debug models"
				}
				printInitWrapped(stdout, "    ", "    ", fmt.Sprintf("%d more from `%s` aren't listed: brigada can't tell their model family, which it needs to pick reviewers.", sources[h].unknownCount, command))
			}
			defaultAnswer := "-"
			if len(entries) > 0 && h != "opencode" {
				defaultAnswer = "1"
			}
			pick := initPickRange(len(entries))
			if h != "kimi" {
				if pick != "" {
					pick += " or "
				}
				pick += "-"
			}
			identifier := "a model id"
			if h == "opencode" {
				identifier = "provider/model"
			}
			prompt := fmt.Sprintf("    Pick %s, or type %s [%s]: ", pick, identifier, defaultAnswer)
			for attempt := 0; attempt < 3; attempt++ {
				model, err := askLine(prompt, "--agent harness="+h+",model=MODEL")
				if err != nil {
					return fail(stderr, err)
				}
				if model == "" {
					model = defaultAnswer
				}
				if model == "-" {
					break
				}
				for _, entry := range entries {
					if model == strconv.Itoa(entry.id) {
						model = entry.value
						break
					}
				}
				if _, err := initModelFamily(h, model); err != nil {
					if attempt == 2 {
						return usagef(stderr, "--agent %s: %v; pick a numbered entry, or type a model whose name says its family", h, err)
					}
					fmt.Fprintf(stdout, "  %v; try again.\n", err)
					fmt.Fprintln(stdout, "  pick a numbered entry, or type a model whose name says its family")
					continue
				}
				classes := splitList(defaultClasses)
				if h == "kimi" {
					fmt.Fprint(stdout, "    kimi has no sandbox: when it writes code it can run any command on this\n    machine without asking. As a reviewer it only reads.\n")
					answer, err := ask("    Let kimi write code too, not only review? [y/N]: ", "--agent harness=kimi,classes=review (or implement+review+answer to allow writing)")
					if err != nil {
						return fail(stderr, err)
					}
					if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
						classes = []string{"review"}
					}
				}
				agents.specs = append(agents.specs, agentSpec{harness: h, model: model, classes: classes})
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
	if canAsk {
		fmt.Fprint(stdout, "\nStep 2 of 3: the foreman\n  The foreman plans the work and decides what happens when a task needs\n  judgment. It runs whenever work finishes, so it uses that agent's quota.\n  Only claude and codex can be the foreman; it uses the model picked above.\n")
		var candidates []*agentSpec
		defaultChoice := 1
		for _, h := range initHarnesses {
			for i := range usable {
				if usable[i].harness == h && adapter.ResumeCapable(h) {
					candidates = append(candidates, &usable[i])
					if chosen == &usable[i] {
						// Preserve the first resume-capable --agent in flag order as
						// the default, even when the CLI-ordered menu starts elsewhere.
						defaultChoice = len(candidates)
					}
				}
			}
		}
		if len(candidates) == 1 {
			fmt.Fprintf(stdout, "  Foreman: %s (the only agent that can plan)\n", initForemanName(*chosen, sources))
		} else {
			for i, candidate := range candidates {
				fmt.Fprintf(stdout, "    %d  %s\n", i+1, initForemanName(*candidate, sources))
			}
			if *foreman == "" {
				for attempt := 0; attempt < 3; attempt++ {
					answer, err := ask(fmt.Sprintf("  Pick %s [%d]: ", initPickRange(len(candidates)), defaultChoice), "--foreman HARNESS/MODEL")
					if err != nil {
						return fail(stderr, err)
					}
					if answer == "" {
						answer = strconv.Itoa(defaultChoice)
					}
					n, err := strconv.Atoi(answer)
					if err == nil && n >= 1 && n <= len(candidates) {
						chosen = candidates[n-1]
						break
					}
					if attempt == 2 {
						return usagef(stderr, "--foreman: pick %s", initPickRange(len(candidates)))
					}
					fmt.Fprintf(stdout, "  pick %s; try again.\n", initPickRange(len(candidates)))
				}
			}
		}
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
			if canAsk {
				fmt.Fprint(stdout, "\nStep 3 of 3: the check\n  A command that must succeed (exit 0) before a task's work is accepted.\n")
				prompt := "  Check command: "
				if suggestion != "" {
					fmt.Fprintf(stdout, "  Found %s, so the suggestion is:\n", source)
					prompt = fmt.Sprintf("  Check command [%s]: ", suggestion)
				}
				answer, err := ask(prompt, "--check COMMAND")
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
	if canAsk {
		fmt.Fprintln(stdout)
	}
	printInitAgents(stdout, usable, len(families), *chosen, sources)
	printInitWrapped(stdout, "  Check: ", "    ", *check+" (init doesn't run it; change it later in .brigada/project.yaml)")
	fmt.Fprintln(stdout, "  What workers may do:")
	for _, a := range adverts {
		if !slices.Contains(a.Classes, "implement") {
			continue
		}
		var permissions string
		switch a.Harness {
		case "claude":
			permissions = "edit files; no commands"
			if a.Tools != "none" {
				permissions = "edit files; run only: " + initToolCommands(a.Tools)
			}
		case "codex":
			permissions = "edit files and run commands inside codex's sandbox"
		case "kimi":
			permissions = "edit files and run any command without asking"
		case "opencode":
			permissions = "edit files; no commands"
		}
		prefix := fmt.Sprintf("    %-9s ", a.Harness)
		printInitWrapped(stdout, prefix, strings.Repeat(" ", len(prefix)), permissions)
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
	}
	printInitWrapped(stdout, "  In the background: ", "    ", "a runner starts now and keeps going (log: "+initHomePath(filepath.Join(rt.stateDir, "runner.log"))+"; stop it with: brigada stop). The engine starts when needed and stops after 30 idle minutes.")
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

func initPickRange(size int) string {
	if size == 0 {
		return ""
	}
	if size == 1 {
		return "1"
	}
	return fmt.Sprintf("1-%d", size)
}

func initMenuModelName(entry initModelEntry) string {
	if entry.name != "" {
		return entry.name
	}
	return initModelName(entry.value)
}

func initForemanName(spec agentSpec, sources map[string]initModelSource) string {
	return agentName(spec.harness, initAgentModelName(spec, sources), spec.effort)
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

// printInitWrapped keeps plan prose under 80 columns. Continuations either
// align with a table cell or use the paragraph's continuation indent.
func printInitWrapped(w io.Writer, prefix, continuation, text string) {
	if utf8.RuneCountInString(prefix) >= 79 {
		printInitWrapped(w, "    ", "      ", strings.TrimSpace(prefix))
		prefix, continuation = "      ", "      "
	}
	line := prefix
	for _, word := range strings.Fields(text) {
		separator := ""
		if line != prefix {
			separator = " "
		}
		if utf8.RuneCountInString(line+separator+word) >= 80 && line != prefix {
			fmt.Fprintln(w, line)
			prefix, line, separator = continuation, continuation, ""
		}
		// Long identifiers and paths have no word boundary to wrap at.
		for utf8.RuneCountInString(line+separator+word) >= 80 {
			room := 79 - utf8.RuneCountInString(line+separator)
			if room <= 0 {
				fmt.Fprintln(w, line)
				prefix, line, separator = continuation, continuation, ""
				continue
			}
			runes := []rune(word)
			fmt.Fprintln(w, line+separator+string(runes[:room]))
			word = string(runes[room:])
			prefix, line, separator = continuation, continuation, ""
		}
		line += separator + word
	}
	fmt.Fprintln(w, line)
}

func initHomePath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if path == home {
			return "~"
		}
		if rel, ok := strings.CutPrefix(path, home+string(os.PathSeparator)); ok {
			return filepath.Join("~", rel)
		}
	}
	return path
}

// Describe command scopes without exposing Claude's Bash(...) syntax. Other
// tool patterns retain their names so a custom scope is still visible in full.
func initToolCommands(tools string) string {
	var commands []string
	for _, tool := range strings.Split(tools, "+") {
		if command, ok := strings.CutPrefix(tool, "Bash("); ok {
			command = strings.TrimSuffix(command, ")")
			commands = append(commands, strings.TrimSuffix(command, ":*"))
		} else {
			commands = append(commands, tool)
		}
	}
	return strings.Join(commands, "; ")
}

func initAgentRoles(classes []string) string {
	if len(classes) == 3 && slices.Contains(classes, "implement") && slices.Contains(classes, "review") && slices.Contains(classes, "answer") {
		return "writes code, reviews"
	}
	var roles []string
	for _, class := range classes {
		switch class {
		case "implement":
			roles = append(roles, "writes code")
		case "review":
			roles = append(roles, "reviews")
		case "answer":
			roles = append(roles, "answers questions")
		default:
			roles = append(roles, "handles "+class+" tasks")
		}
	}
	if len(roles) == 1 {
		return roles[0] + " only"
	}
	return strings.Join(roles, ", ")
}

func initAgentModelName(s agentSpec, sources map[string]initModelSource) string {
	for _, entry := range sources[s.harness].entries {
		if entry.value == s.model {
			return initMenuModelName(entry)
		}
	}
	return initModelName(s.model)
}

func initFamilyName(family string) string {
	switch family {
	case "":
		return "unknown family"
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	case "moonshot":
		return "Moonshot"
	case "zhipu":
		return "Zhipu (GLM)"
	case "google":
		return "Google"
	case "deepseek":
		return "DeepSeek"
	case "alibaba":
		return "Alibaba"
	case "meta":
		return "Meta"
	case "mistral":
		return "Mistral"
	default:
		return family
	}
}

func printInitAgents(w io.Writer, agents []agentSpec, families int, foreman agentSpec, sources map[string]initModelSource) {
	fmt.Fprintln(w, "Plan\n  Agents:")
	modelWidth, familyWidth := 0, 0
	familyNames := make([]string, len(agents))
	for i, s := range agents {
		family, _ := adapter.AgentFamily(s.harness, s.model)
		family = initFamilyName(family)
		familyNames[i] = family
		modelWidth = max(modelWidth, utf8.RuneCountInString(initAgentModelName(s, sources)))
		familyWidth = max(familyWidth, utf8.RuneCountInString(family))
	}
	for i, s := range agents {
		// If the fixed columns leave no room for roles, put family and roles
		// on a second line under the model column for every row. Very long
		// identifiers wrap within that column, with roles on a separate line.
		if 14+modelWidth+2+familyWidth+2 >= 79 {
			indent := strings.Repeat(" ", 14)
			printInitWrapped(w, fmt.Sprintf("    %-9s ", s.harness), indent, initAgentModelName(s, sources))
			if 14+familyWidth+2 >= 79 {
				printInitWrapped(w, indent, indent, familyNames[i])
				printInitWrapped(w, indent, indent, initAgentRoles(s.classes))
			} else {
				prefix := fmt.Sprintf("%s%-*s  ", indent, familyWidth, familyNames[i])
				printInitWrapped(w, prefix, strings.Repeat(" ", utf8.RuneCountInString(prefix)), initAgentRoles(s.classes))
			}
			continue
		}
		prefix := fmt.Sprintf("    %-9s %-*s  %-*s  ", s.harness, modelWidth, initAgentModelName(s, sources), familyWidth, familyNames[i])
		printInitWrapped(w, prefix, strings.Repeat(" ", utf8.RuneCountInString(prefix)), initAgentRoles(s.classes))
	}
	printInitWrapped(w, "  Reviews: ", "    ", fmt.Sprintf("%d model families, so every task can get a reviewer of another one.", families))
	printInitWrapped(w, "  Foreman: ", "    ", initForemanName(foreman, sources))
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
