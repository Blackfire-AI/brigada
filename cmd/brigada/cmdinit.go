package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/caps"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"github.com/Blackfire-AI/brigada/internal/term"
	"github.com/mattn/go-isatty"
)

// initBinary is empty in production (this executable); integration tests for
// init and start use the built CLI for real detached engine and runner startup.
var initBinary string

var initCPUCount = goruntime.NumCPU

// Per-harness account defaults, displayed in initHarnesses order.
// Every cap is clamped to tasks at once.
var initAccountDefaults = map[string]int{
	"claude": 3, "codex": 2, "kimi": 1, "opencode": 2, "dsh": 1,
}

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) (code int) {
	fs := newFlagSet("init",
		"brigada init [--probe | --yes] [--verbose] [--now] [options] (inside one repository, or in a folder holding several: then --all selects them)",
		"Probe and confirm setup of one repository or a folder of repositories.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	yes := fs.Bool("yes", false, "apply the displayed plan without prompting (does not confirm authentication)")
	now := fs.Bool("now", false, "restart the runner at once when its agents change, cutting its running jobs; without it the restart waits out running jobs")
	probe := fs.Bool("probe", false, "print the plan only; write nothing and start nothing")
	verbose := fs.Bool("verbose", false, "also print the full contents of new repository setup files")
	limited := fs.Bool("accept-limited", false, "acknowledge fewer than two review families; matters only under review.family=require, where reviews wait for another family; under the default prefer it changes nothing")
	check := fs.String("check", "", "default check command, or none / - for no check (never executed by init)")
	repo := fs.String("repo", "", "repository URL or canonical identity to register instead of the selected remote")
	remote := fs.String("remote", "", "git remote to use as the project's repository")
	tools := fs.String("tools", "", "worker scope in runner tools= syntax; scopes claude's tool list and opencode's bash commands")
	foreman := fs.String("foreman", "", "choose the foreman, a signed-in claude or codex model, as harness/model[@effort]; without @effort an interactive run asks for the effort")
	noWiring := fs.Bool("no-wiring", false, "in a parent folder, skip the coordinator's wiring; in a repository, leave the harness untouched: init writes no harness wiring, and an older init's stays")
	parallel := fs.Int("parallel", 0, "tasks at once on this machine (1..64)")
	all := fs.Bool("all", false, "in a folder that is not a repository: register every new repository found, without asking")
	overwrite := fs.Bool("overwrite-config", false, "replace config caps and saved agent lines that already hold another value; without it init adds only missing keys and reports each value it keeps and changes")
	takeDefault := fs.String("take-default", "", "reset one role file or default skill (a role: foreman, plan_review, worker, reviewer; or a skill name) to the new default under a fresh header; touches that file only")
	agents := &agentSpecs{}
	fs.Var(agents, "agent", "--agent once per harness and model, same complete syntax and model defaults as brigada runner")
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
	if *takeDefault != "" {
		// Resets one generated file; no probe, plan or engine (§5.9).
		return runTakeDefault(stdout, stderr, *takeDefault)
	}
	// askedHarness names every harness whose worker questions this run
	// asks; an interactive answer replaces every saved line of an asked
	// harness, while a run that does not ask keeps lines per pair below.
	askedHarness := map[string]bool{}
	// Setup refusals retain init’s exit code 1; flag and argument errors above use 2.
	defer func() {
		if code != 0 {
			code = 1
		}
	}()
	parallelProvided := false
	parallelAnswered := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "parallel" {
			parallelProvided = true
		}
	})
	if parallelProvided && (*parallel < 1 || *parallel > 64) {
		return usagef(stderr, "--parallel N: want an integer from 1 to 64")
	}
	toolsProvided := *tools != ""
	if *yes && *probe {
		return usagef(stderr, "--yes and --probe are mutually exclusive")
	}
	if !*yes && !*probe {
		printInitBanner(stdout)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(stderr, err)
	}
	root, err := gitRoot(cwd)
	if err != nil {
		// Not a repository: the parent-folder run (design §13.1, §4.6).
		if parallelProvided {
			return usagef(stderr, "--parallel sets up one repository; this folder is not one")
		}
		if *repo != "" || *remote != "" || *check != "" || toolsProvided || len(agents.specs) > 0 || *limited || len(signedIn) > 0 {
			return usagef(stderr, "--repo, --remote, --check, --tools, --agent, --signed-in and --accept-limited set up one repository; this folder is not one (parent-folder init takes --all, --foreman, --yes, --probe and --no-wiring)")
		}
		return runInitParent(ctx, cwd, parentOptions{yes: *yes, probe: *probe, all: *all, noWiring: *noWiring, foreman: *foreman}, common, bufio.NewReader(projectStdin), stdout, stderr)
	}
	if *all {
		return usagef(stderr, "--all selects projects in a parent folder; this is one repository")
	}
	foremanWant, foremanHasEffort, foremanOK := parseInitForeman(*foreman)
	if *foreman != "" && !foremanOK {
		return usagef(stderr, "--foreman: want HARNESS/MODEL[@EFFORT], e.g. claude/sonnet@high")
	}
	if foremanHasEffort {
		if err := adapter.ValidateEffort(foremanWant.harness, foremanWant.effort); err != nil {
			return usagef(stderr, "--foreman: %v", err)
		}
	}
	// A remote must exist; a real local bare remote is valid. Multiple URLs
	// of the chosen remote must still name one identity.
	reader := bufio.NewReader(projectStdin)
	var askRemote func([]gitRemote) (string, error)
	if !*yes && !*probe {
		askRemote = func(remotes []gitRemote) (string, error) {
			fmt.Fprintln(stdout, "Choose the repository's remote:")
			for _, r := range remotes {
				shown := "(no URL)"
				if len(r.urls) > 0 {
					shown = engine.RedactRepoURL(r.urls[0])
				}
				fmt.Fprintf(stdout, "  %s  %s\n", r.name, shown)
			}
			fmt.Fprint(stdout, "Remote name (--remote NAME): ")
			answer, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				return "", err
			}
			return strings.TrimSpace(answer), nil
		}
	}
	selection, selectedRemote, err := selectInitOriginWithRemote(ctx, root, *repo, *remote, askRemote)
	if err != nil {
		return fail(stderr, err)
	}
	if selectedRemote != "origin" || *remote != "" {
		fmt.Fprintf(stdout, "Using git remote %s\n", selectedRemote)
	}
	origin, canon := selection.transport, selection.canon
	accepted, err := confirmInitSelection(selection, *yes, *probe, reader, stdout)
	if err != nil {
		return fail(stderr, err)
	}
	if !accepted {
		return 1
	}
	caps := initDetectCaps(stdout)
	styled := caps.TTY && caps.Color != logo.NoColor
	if !styled {
		// Plain text keeps its existing symbols, but terminal width still
		// controls layout independently of color support.
		caps.Color = logo.NoColor
		caps.UTF8 = false
	}
	stdout = initOutput{Writer: stdout, style: term.New(caps), styled: styled, utf8: caps.UTF8}
	style := initPresentation(stdout).style
	if styled {
		printInitWrapped(stdout, "", "  ", style.Bold("brigada init")+initSeparator(stdout)+canon)
		fmt.Fprintln(stdout)
	} else {
		fmt.Fprintf(stdout, "brigada init: set up %s\n\n", canon)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	requireFamily := rt.file.Review.Family == engine.ReviewFamilyRequire
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
	configPath := rt.configPath
	if configPath == "" {
		configPath = lifecycle.DefaultConfigPath()
	}
	if err := lifecycle.CheckConfigWritePath(configPath); err != nil {
		return fail(stderr, err)
	}
	cores := initCPUCount()
	if !parallelProvided {
		savedParallel, err := runnerstate.LoadParallel(runnerStateDir(rt))
		if err != nil {
			return fail(stderr, err)
		}
		*parallel = savedParallel
		if *parallel == 0 {
			*parallel = min(4, max(1, cores/2))
		}
	}
	canAsk := !*yes && !*probe
	// A rerun defaults to the saved foreman and to the saved agents, one spec
	// per model. A foreman-only spec (no classes) is not a task choice.
	var saved map[string][]agentSpec
	var savedForeman agentSpec
	rerun := false
	if canAsk {
		value, err := runnerstate.LoadForeman(runnerStateDir(rt))
		if err != nil {
			return fail(stderr, err)
		}
		if spec, _, ok := parseInitForeman(value); ok {
			savedForeman = spec
		}
	}
	if canAsk && len(agents.specs) == 0 {
		lines, err := runnerstate.LoadAgents(runnerStateDir(rt))
		if err != nil {
			return fail(stderr, err)
		}
		if lines != nil {
			var choices agentSpecs
			for _, line := range lines {
				if err := choices.Set(line); err != nil {
					return fail(stderr, fmt.Errorf("saved agents: %w", err))
				}
			}
			rerun = true
			saved = make(map[string][]agentSpec)
			for _, spec := range choices.specs {
				if len(spec.classes) > 0 {
					saved[spec.harness] = append(saved[spec.harness], spec)
				}
			}
		}
	}
	if canAsk {
		fmt.Fprint(stdout, initDim(stdout, "brigada runs coding tasks on the agent CLIs you are signed in to. A foreman\n(an agent) turns your request into a plan of tasks; workers (other agents)\nwrite the code in their own git worktrees; your check command tests it; and\nan agent of another model family reviews it\n"+initReviewIntro(requireFamily)+" Four short steps follow.\nThe engine mirrors plan files (titles, task states, dependencies,\nbranches, open questions, extra repositories) to the brigada/plans\nbranch of your remote; turn that off with\n`brigada project set plan_files=off`.\nNothing is written or started until you confirm at the end.\n\n"))
	}
	terminal := false
	if f, ok := projectStdin.(*os.File); ok {
		terminal = isatty.IsTerminal(f.Fd())
	}
	askLine := func(prompt, flags string) (string, error) {
		if !canAsk {
			return "", nil
		}
		if !terminal {
			prompt = strings.TrimRight(prompt, " \t")
		}
		prompt = initPrompt(stdout, prompt)
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
		case "dsh":
			source = initDshModels(ctx)
		}
		// Only OpenCode's picks are individual choices: a saved model its
		// list no longer shows stays pickable. The other CLIs offer every model.
		if h == "opencode" {
			for _, spec := range saved[h] {
				source = initCurrentModel(source, spec)
			}
		}
		sources[h] = source
		probes[h] = source.signIn
	}
	confirmedCLI := func(h string) bool { return probes[h].signedIn || signedIn[h] }
	if canAsk {
		printInitSignedIn(stdout, probes, signedIn)
		if len(agents.specs) == 0 {
			fmt.Fprintln(stdout)
		}
	} else {
		fmt.Fprintln(stdout, "Agent CLIs found:")
		for _, h := range initHarnesses {
			printInitWrapped(stdout, fmt.Sprintf("  %-9s ", initHarnessLabel(h)), "  ", probes[h].status)
		}
		fmt.Fprintln(stdout)
	}
	shown := map[string]bool{}
	authenticate := func(spec agentSpec, defaultYes ...bool) (bool, error) {
		h := spec.harness
		confirmed := probes[h].signedIn || signedIn[h]
		if canAsk && !shown[h] {
			shown[h] = true
			status := probes[h].status
			if signedIn[h] && !probes[h].signedIn {
				status = "signed in"
			}
			switch {
			case styled:
				fmt.Fprintln(stdout)
				initCLIStatus(stdout, h, probes[h], confirmed)
			case !confirmed && h == "kimi":
				fmt.Fprint(stdout, "\n  kimi: init can't check sign-in. If you're not signed in, run `kimi login`\n  first, or leave kimi out.\n")
			case !confirmed && h == "opencode" && !probes[h].checked:
				fmt.Fprint(stdout, "\n  opencode: init can't check sign-in. If you're not signed in, run\n  `opencode auth login` first, or leave opencode out.\n")
			default:
				fmt.Fprintln(stdout)
				printInitWrapped(stdout, "  ", "  ", initHarnessLabel(h)+": "+status)
			}
			if h == "dsh" {
				printInitDshKeyNote(stdout)
			}
		}
		if confirmed {
			return true, nil
		}
		name := h
		if spec.accountLabel() != h {
			name += " account " + spec.accountLabel()
		}
		if h == "kimi" || h == "opencode" || h == "dsh" {
			// A rerun that used this CLI keeps using it on Enter.
			if len(defaultYes) > 0 && defaultYes[0] {
				v, err := ask("    Use "+name+"? [Y/n]: ", "--signed-in "+h)
				return !strings.EqualFold(v, "n") && !strings.EqualFold(v, "no"), err
			}
			v, err := ask("    Use "+name+"? [y/N]: ", "--signed-in "+h)
			return strings.EqualFold(v, "y") || strings.EqualFold(v, "yes"), err
		}
		return confirm("    Are you signed in to "+name+"?", "--signed-in "+h)
	}
	var foremanSpec agentSpec
	foremanEffort, foremanPicked := "", false
	// pickForeman is step 1: the foreman's model among those of the signed-in
	// claude and codex CLIs, then its effort. The foreman is a model, so it is
	// chosen before the workers.
	pickForeman := func() (agentSpec, string, int) {
		initStep(stdout, 1, "the foreman", "  The foreman plans the work and decides what happens when a task needs\n  judgment. It runs whenever work finishes, so it uses that agent's quota.\n  Only claude and codex can be the foreman.\n")
		candidates := initForemanCandidates(sources, confirmedCLI)
		if len(candidates) == 0 {
			return agentSpec{}, "", fail(stderr, fmt.Errorf("no foreman: only a signed-in claude or codex can plan; sign in to one and confirm it with --signed-in; nothing written or started"))
		}
		defaultChoice := 1
		for i, c := range candidates {
			if savedForeman.harness != "" && c.matches(savedForeman.harness, savedForeman.model) {
				defaultChoice = i + 1
			}
		}
		resolve := func(answer string) (initForemanChoice, error) {
			if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(candidates) {
				return candidates[n-1], nil
			}
			harness, model, ok := strings.Cut(answer, "/")
			if !ok {
				return initForemanChoice{}, fmt.Errorf("%q is neither a number from the menu nor harness/model", answer)
			}
			return initResolveForeman(candidates, sources, harness, model)
		}
		var choice initForemanChoice
		switch {
		case *foreman != "":
			c, err := initResolveForeman(candidates, sources, foremanWant.harness, foremanWant.model)
			if err != nil {
				return agentSpec{}, "", usagef(stderr, "--foreman must name a usable agent that resumes headless (claude/model or codex/model); none matched")
			}
			choice = c
		case len(candidates) == 1:
			choice = candidates[0]
			fmt.Fprintf(stdout, "  Foreman: %s (the only model that can plan)\n", choice.name())
		default:
			fmt.Fprintln(stdout, initDim(stdout, "    Which model should the foreman run?"))
			printInitForemanMenu(stdout, candidates, defaultChoice, savedForeman)
			for attempt := 0; ; attempt++ {
				answer, err := ask(fmt.Sprintf("    Pick %s, or type harness/model [%d]: ", initPickRange(len(candidates)), defaultChoice), "--foreman HARNESS/MODEL")
				if err != nil {
					return agentSpec{}, "", fail(stderr, err)
				}
				if answer == "" {
					answer = strconv.Itoa(defaultChoice)
				}
				c, err := resolve(answer)
				if err == nil {
					choice = c
					break
				}
				if attempt == 2 {
					return agentSpec{}, "", usagef(stderr, "--foreman HARNESS/MODEL: %v; pick %s, or type harness/model", err, initPickRange(len(candidates)))
				}
				fmt.Fprintf(stdout, "  %v; try again.\n", err)
				fmt.Fprintln(stdout, "  pick a numbered entry, or type harness/model")
			}
		}
		effort := foremanWant.effort
		if !foremanHasEffort {
			var savedEffort []string
			if savedForeman.harness != "" && choice.matches(savedForeman.harness, savedForeman.model) {
				savedEffort = []string{savedForeman.effort}
			}
			var err error
			effort, err = initPickEffort(stdout, choice.harness, choice.entry, ask, savedEffort...)
			if err != nil {
				return agentSpec{}, "", fail(stderr, err)
			}
		}
		initChangeLater(stdout, "brigada project foreman --harness H --model M --effort E, or run brigada init again")
		return agentSpec{harness: choice.harness, model: choice.entry.value}, effort, 0
	}
	if len(agents.specs) == 0 && canAsk {
		asked := false
		for _, h := range []string{"claude", "codex"} {
			if !probes[h].installed || confirmedCLI(h) {
				continue
			}
			asked = true
			askedHarness[h] = true
			if probes[h].checked {
				printInitWrapped(stdout, "", "  ", h+": "+probes[h].status+".")
			}
			ok, err := confirm("    Are you signed in to "+h+"?", "--signed-in "+h)
			if err != nil {
				return fail(stderr, err)
			}
			signedIn[h] = ok
		}
		if asked {
			fmt.Fprintln(stdout)
		}
		var exit int
		if foremanSpec, foremanEffort, exit = pickForeman(); exit != 0 {
			return exit
		}
		foremanPicked = true
		fmt.Fprintln(stdout)
		initStep(stdout, 2, "workers", "  Each agent CLI below can run tasks. brigada offers every model the CLI\n  knows: for each task the foreman says how heavy a model it needs (light,\n  standard, heavy) and how hard it should think, and brigada picks the\n  model of that tier with the best record on this repo."+initReviewWorkers(requireFamily))
		for _, h := range initHarnesses {
			if !probes[h].installed || ((h == "claude" || h == "codex") && !confirmedCLI(h)) {
				continue
			}
			authenticated, err := authenticate(agentSpec{harness: h}, len(saved[h]) > 0)
			if err != nil {
				return fail(stderr, err)
			}
			askedHarness[h] = true
			if !authenticated {
				continue
			}
			signedIn[h] = true
			entries := initEligibleEntries(h, sources[h])
			if len(entries) == 0 && h != "opencode" {
				continue
			}
			var models []string
			var classes []string
			switch h {
			case "claude", "codex":
				names := initUseNames(h, entries, rt.file.Caps.TierOf)
				def := !rerun || len(saved[h]) > 0
				hint := "[Y/n]"
				if !def {
					hint = "[y/N]"
				}
				answer, err := ask(fmt.Sprintf("    Use %s's models for tasks (%s)? %s: ", h, names, hint), "--agent harness="+h+",model=MODEL")
				if err != nil {
					return fail(stderr, err)
				}
				use := def
				switch strings.ToLower(answer) {
				case "y", "yes":
					use = true
				case "n", "no":
					use = false
				}
				if !use {
					continue
				}
				for _, entry := range entries {
					models = append(models, entry.value)
				}
			case "kimi":
				if styled {
					printInitWrapped(stdout, "    ", "    ", style.Warn(style.Warning()+" kimi has no sandbox: when it writes code it can run any command on this machine without asking. As a reviewer it only reads."))
				} else {
					fmt.Fprint(stdout, "    kimi has no sandbox: when it writes code it can run any command on this\n    machine without asking. As a reviewer it only reads.\n")
				}
				writes := len(saved[h]) > 0 && slices.Contains(saved[h][0].classes, "implement")
				hint := "y/N"
				if writes {
					hint = "Y/n"
				}
				answer, err := ask("    Let kimi write code too, not only review? ["+hint+"]: ", "--agent harness=kimi,classes=review (or implement+review+answer to allow writing)")
				if err != nil {
					return fail(stderr, err)
				}
				classes = []string{"review"}
				if answer == "" && len(saved[h]) > 0 {
					classes = slices.Clone(saved[h][0].classes)
				} else if strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes") {
					classes = splitList(defaultClasses)
				}
				for _, entry := range entries {
					models = append(models, entry.value)
				}
			case "dsh":
				for _, entry := range entries {
					models = append(models, entry.value)
				}
			case "opencode":
				printInitOpenCodeMenu(stdout, entries, sources[h].unknownCount, len(saved[h]) > 0)
				defaultAnswer := "-"
				if ids := initCurrentIDs(entries); ids != "" {
					defaultAnswer = ids
				}
				prompt := fmt.Sprintf("    Pick numbers (1,3) or -, or type provider/model [%s]: ", defaultAnswer)
				for attempt := 0; attempt < 3; attempt++ {
					answer, err := askLine(prompt, "--agent harness=opencode,model=PROVIDER/MODEL")
					if err != nil {
						return fail(stderr, err)
					}
					if strings.TrimSpace(answer) == "" {
						answer = defaultAnswer
					}
					picks, err := parseInitOpenCodePicks(entries, answer)
					if err == nil {
						models = picks
						break
					}
					if attempt == 2 {
						return usagef(stderr, "--agent %s: %v; pick a numbered entry, or type a model whose name says its family", h, err)
					}
					fmt.Fprintf(stdout, "  %v; try again.\n", err)
					fmt.Fprintln(stdout, "  pick a numbered entry, or type a model whose name says its family")
				}
			}
			for _, model := range models {
				spec := agentSpec{harness: h, model: model, classes: splitList(defaultClasses)}
				if i := slices.IndexFunc(saved[h], func(s agentSpec) bool { return s.model == model }); i >= 0 {
					spec = saved[h][i]
				}
				if classes != nil {
					spec.classes = slices.Clone(classes)
				}
				agents.specs = append(agents.specs, spec)
			}
		}
		initChangeLater(stdout, initAgentChangeLater(sources))
	}
	var usable []agentSpec
	var available []agentSpec
	seen := map[string]bool{}
	families := map[string]bool{}
	for _, spec := range agents.specs {
		if !slices.Contains(initHarnesses, spec.harness) {
			return usagef(stderr, "unsupported init harness %q", spec.harness)
		}
		if spec.model == "" {
			spec.model = runnerDefaultModel(spec.harness)
		}
		pair := spec.harness + "/" + spec.model
		if seen[pair] {
			return usagef(stderr, "agent %s is offered twice: one agent per harness and model per runner", pair)
		}
		seen[pair] = true
		_, err := initModelFamily(spec.harness, spec.model)
		if err != nil {
			return usagef(stderr, "--agent %s: %v", spec.harness, err)
		}
		if len(spec.classes) > 0 {
			if err := engine.ValidateSettingValue(engine.SettingApprovalClasses, strings.Join(spec.classes, "+")); err != nil {
				return usagef(stderr, "agent classes: %v", err)
			}
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
	// Fewer than two review families stops setup only under review.family=require;
	// under prefer reviews fall back to the same family or the author's agent.
	isLimited := len(families) < 2 && requireFamily
	if len(families) < 2 {
		if requireFamily {
			printInitWrapped(stdout, "LIMITED SETUP: ", "  ", "fewer than two distinct review-capable model families, and review.family=require. Sign in to a harness of another family, or add a model of another family with classes=review. Reviews that cannot be served by another family will wait; inspect brigada status.")
			if !*limited {
				return fail(stderr, fmt.Errorf("setup stopped; add a second review family or explicitly acknowledge with --accept-limited (review.family=require); nothing written or started"))
			}
		} else {
			printInitWrapped(stdout, "WARNING: ", "  ", "fewer than two distinct review-capable model families. review.family=prefer (the default): reviews prefer another family and fall back to the same family or the author's own agent, labelled in history. Add a model of another family with classes=review for cross-family reviews, or set review.family=require in the engine config to insist on them.")
		}
	}
	// The foreman is the model picked above, picked now when --agent skipped
	// the worker questions, or the first agent that resumes when nothing is asked.
	foremanIndex := -1
	if !canAsk {
		for i := range usable {
			s := &usable[i]
			if adapter.ResumeCapable(s.harness) && (*foreman == "" || foremanWant.harness+"/"+foremanWant.model == s.harness+"/"+s.model) {
				foremanIndex = i
				break
			}
		}
		if foremanIndex < 0 {
			return usagef(stderr, "--foreman must name a usable agent that resumes headless (claude/model or codex/model); none matched")
		}
		foremanEffort = usable[foremanIndex].effort
		if foremanHasEffort {
			foremanEffort = foremanWant.effort
		}
	} else {
		if !foremanPicked {
			fmt.Fprintln(stdout)
			var exit int
			if foremanSpec, foremanEffort, exit = pickForeman(); exit != 0 {
				return exit
			}
			fmt.Fprintln(stdout)
			initStep(stdout, 2, "workers", "  Set by your --agent flags.\n")
		}
		foremanIndex = slices.IndexFunc(usable, func(s agentSpec) bool {
			return s.harness == foremanSpec.harness && s.model == foremanSpec.model
		})
		if foremanIndex < 0 {
			// The foreman's CLI is not used for tasks: it plans and nothing else.
			usable = append(usable, agentSpec{harness: foremanSpec.harness, model: foremanSpec.model, classes: []string{}})
			foremanIndex = len(usable) - 1
		}
	}
	chosen := &usable[foremanIndex]
	// A saved agent line stays unless this run's answers replace it: an
	// interactive answer about a CLI replaces every saved line of that
	// harness (declining drops it), and anywhere a chosen harness and
	// model pair replaces its line. Kept lines stay in the runner's
	// agents file and the runner serves them; --overwrite-config replaces
	// the whole file with this run's choices.
	var keptAgents []string
	var keptSpecs []agentSpec
	if !*overwrite {
		lines, err := runnerstate.LoadAgents(runnerStateDir(rt))
		if err != nil {
			return fail(stderr, err)
		}
		chosenPairs := map[string]bool{}
		for _, spec := range usable {
			chosenPairs[spec.harness+"/"+spec.model] = true
		}
		for _, line := range lines {
			var one agentSpecs
			if err := one.Set(line); err == nil {
				spec := one.specs[0]
				if askedHarness[spec.harness] || chosenPairs[spec.harness+"/"+spec.model] {
					continue
				}
				keptSpecs = append(keptSpecs, spec)
			}
			keptAgents = append(keptAgents, line)
		}
	}

	if canAsk {
		fmt.Fprintln(stdout)
		initStep(stdout, 3, "tasks at once", "")
		var defaults []string
		for _, h := range initHarnesses {
			if probes[h].installed {
				defaults = append(defaults, fmt.Sprintf("%s %d", h, initAccountDefaults[h]))
			}
		}
		printInitWrapped(stdout, "  ", "  ", initDim(stdout,
			fmt.Sprintf("Each running task is one agent CLI working in its own copy of the repo, plus your check command, so this is bounded by your machine. This one has %d cores. Each CLI's account also gets its own limit (%s, brigada's defaults, never above this number); change any of them later with brigada config.", cores, strings.Join(defaults, ", "))), 74)
		if !parallelProvided {
			for attempt := 0; attempt < 3; attempt++ {
				answer, err := ask(fmt.Sprintf("    Tasks at once [%d]: ", *parallel), "--parallel N")
				if err != nil {
					return fail(stderr, err)
				}
				if answer == "" {
					break
				}
				n, err := strconv.Atoi(answer)
				if err == nil && n >= 1 && n <= 64 {
					*parallel = n
					parallelAnswered = true
					break
				}
				if attempt == 2 {
					return usagef(stderr, "--parallel N: want an integer from 1 to 64")
				}
				fmt.Fprintln(stdout, "    enter an integer from 1 to 64; try again.")
			}
		}
	}
	capValues := map[string]string{
		"caps.global": strconv.Itoa(*parallel), "caps.plan": "3",
	}
	for _, spec := range usable {
		key := "caps.account." + spec.accountLabel()
		n := min(initAccountDefaults[spec.harness], *parallel)
		if previous, ok := capValues[key]; ok {
			old, _ := strconv.Atoi(previous)
			n = min(n, old)
		}
		capValues[key] = strconv.Itoa(n)
	}
	// A cap the config file already names keeps its value: init adds only
	// missing keys, so a rerun or a second repository never resets a cap
	// the user tuned. An explicit tasks-at-once answer (--parallel or a
	// typed number) replaces caps.global; --overwrite-config replaces
	// every existing cap. The plan reports each kept and each changed
	// value.
	var capNotes []string
	for _, key := range slices.Sorted(maps.Keys(capValues)) {
		old, ok := initExistingCap(rt.file.Caps, key)
		if !ok || strconv.Itoa(old) == capValues[key] {
			continue
		}
		if *overwrite || (key == "caps.global" && (parallelProvided || parallelAnswered)) {
			capNotes = append(capNotes, fmt.Sprintf("%s changes from %d to %s", key, old, capValues[key]))
			continue
		}
		capNotes = append(capNotes, fmt.Sprintf("%s stays %d; this run would set %s (--overwrite-config replaces it)", key, old, capValues[key]))
		capValues[key] = strconv.Itoa(old)
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
	projectCheck, projectTools := "", ""
	if exists {
		cfg, err := engine.ParseProjectConfig(old)
		if err != nil {
			return fail(stderr, err)
		}
		if cfg.Repo != canon {
			return fail(stderr, fmt.Errorf("project.yaml names another repository; correct it before init"))
		}
		_ = json.Unmarshal(cfg.Values[engine.SettingDefaultCheck], &projectCheck)
		_ = json.Unmarshal(cfg.Values[engine.SettingWorkerTools], &projectTools)
	}
	checkProvided := *check != ""
	if *check == "none" || *check == "-" {
		*check = ""
	}
	if !checkProvided || !toolsProvided {
		candidates, err := checkCandidates(root)
		if err != nil {
			return fail(stderr, err)
		}
		entries := initCheckEntries(candidates, projectCheck, projectTools, exists)
		if !checkProvided {
			if canAsk {
				command, exhausted, err := askInitCheck(stdout, ask, entries, candidates)
				if err != nil {
					return fail(stderr, err)
				}
				if exhausted {
					return usagef(stderr, "--check: no usable answer; type a command or - for no check, or pass --check COMMAND")
				}
				*check = command
			} else {
				*check = entries[initCheckDefault(entries)].Command
			}
		}
		if !toolsProvided {
			*tools = initCheckTools(*check, entries, candidates)
		}
	}

	proposed, err := engine.ProjectConfigDefaults(old, canon, *check, *tools)
	if err != nil {
		return fail(stderr, err)
	}
	// The defaults' role files and skills (§5.9, §5.10) are named in the
	// file init writes.
	if proposed, err = engine.ProjectConfigGuidance(proposed); err != nil {
		return fail(stderr, err)
	}
	proposedCfg, err := engine.ParseProjectConfig(proposed)
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
	// The runner serves the kept saved agents too, and the restart
	// comparison must see them, so an already-running runner is neither
	// restarted without them nor left serving a narrower set than the
	// saved file. A kept line that no longer resolves stays in the file
	// alone; the runner cannot serve it.
	runnerAgents := slices.Clone(usable)
	runnerAdverts := slices.Clone(adverts)
	for _, spec := range keptSpecs {
		_, keptAdverts, err := resolveRunnerAgents(rt, []agentSpec{spec}, "")
		if err != nil {
			continue
		}
		runnerAgents = append(runnerAgents, spec)
		runnerAdverts = append(runnerAdverts, keptAdverts...)
	}
	if canAsk {
		fmt.Fprintln(stdout)
	}
	planForeman := *chosen
	planForeman.effort = foremanEffort
	printInitAgents(stdout, usable, len(families), requireFamily, planForeman, sources, rt.file.Caps.TierOf)
	var accounts []string
	seenAccounts := map[string]bool{}
	for _, h := range initHarnesses {
		for _, spec := range usable {
			label := spec.accountLabel()
			if spec.harness == h && !seenAccounts[label] {
				seenAccounts[label] = true
				accounts = append(accounts, label+" "+capValues["caps.account."+label])
			}
		}
	}
	printInitWrapped(stdout, "  "+style.Bold("At once:")+" ", "    ", fmt.Sprintf("%d tasks on this machine; per account: %s (change: brigada config)", *parallel, strings.Join(accounts, ", ")), 80)
	for _, note := range capNotes {
		printInitWrapped(stdout, "  ", "    ", note)
	}
	if len(keptAgents) > 0 {
		names := make([]string, len(keptAgents))
		for i, line := range keptAgents {
			names[i] = line
			var one agentSpecs
			if one.Set(line) == nil && one.specs[0].model != "" {
				names[i] = one.specs[0].harness + "/" + one.specs[0].model
			}
		}
		printInitWrapped(stdout, "  ", "    ", fmt.Sprintf("kept saved agents this run does not offer: %s (--overwrite-config replaces them)", strings.Join(names, ", ")))
	}
	checkSummary := "none (work goes straight to review)"
	if *check != "" {
		checkSummary = *check + " (init doesn't run it; change it later in .brigada/project.yaml)"
	}
	printInitWrapped(stdout, "  "+style.Bold("Check:")+" ", "    ", checkSummary)
	fmt.Fprintln(stdout, "  "+style.Bold("What workers may do:"))
	listed := map[string]bool{}
	for _, a := range adverts {
		if !slices.Contains(a.Classes, "implement") || listed[a.Harness] {
			continue
		}
		listed[a.Harness] = true
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
			permissions = "edit files; run only the commands of the worker tools scope"
		case "dsh":
			permissions = "edit files and run commands inside dsh's workspace sandbox; reviews run read-only"
		}
		prefix := fmt.Sprintf("    %-9s ", a.Harness)
		printInitWrapped(stdout, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), permissions)
	}
	// Init writes no harness wiring; a rerun strips the wiring an older
	// init left behind, unless --no-wiring holds it back.
	var wiring []wireFile
	if !*noWiring {
		wiring = planLegacyWiring(root)
	}
	// The operator guide and the generated role files and skills are
	// written whole, under the same plan, confirmation and recheck.
	operator := append(planOperatorFiles(root), planGuidance(root, proposedCfg)...)
	files := append(append([]wireFile{{rel: engine.ProjectConfigPath, old: old, next: proposed, exists: exists}}, operator...), wiring...)
	printInitFiles(stdout, files, *verbose)
	for _, note := range guidanceNotes(proposedCfg) {
		printInitWrapped(stdout, "  ", "    ", note)
	}
	printInitOperatorNote(stdout)
	if *noWiring {
		printInitWrapped(stdout, "", "  ", initDim(stdout, "--no-wiring: init in a repository no longer wires or unwires the harness; the flag matters only in a parent folder"))
	}
	printInitWrapped(stdout, "  "+style.Bold("In the background:")+" ", "    ", "a runner starts now and keeps going (log: "+initHomePath(filepath.Join(rt.stateDir, "runner.log"))+"; stop it with: "+style.Accent("brigada stop")+"). The engine starts when needed and stops after 30 idle minutes.")
	fmt.Fprintln(stdout)
	if *probe {
		printInitWrapped(stdout, "", "  ", "Probe only; nothing written or started. Re-run without --probe to confirm, or with --yes to apply.")
		return 0
	}
	if !*yes {
		files := 0
		if !exists || !bytes.Equal(old, proposed) {
			files++
		}
		files += countChanged(operator)
		for _, f := range wiring {
			if f.changed() {
				files++
			}
		}
		rec, running, err := runnerstate.Running(runnerStateDir(rt))
		if err != nil {
			return fail(stderr, err)
		}
		prompt := initApplyPrompt(files, running)
		if running && initAgentsDiffer(rec, runnerAdverts, runnerAgents, *parallel) {
			noun := "files"
			if files == 1 {
				noun = "file"
			}
			prompt = fmt.Sprintf(
				"Write %d %s and restart the runner with the new agents?",
				files, noun)
		}
		answer, err := ask(prompt+" [y/N]: ", "--yes to apply the displayed plan")
		if err != nil {
			return fail(stderr, err)
		}
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Fprintln(stdout, "Declined; nothing written or started.")
			return 1
		}
	}
	engineRunning := lifecycle.EngineRunning(ctx, rt.paths.Socket)
	// Recheck before the first write, in case the path changed during prompts.
	if err := lifecycle.CheckConfigWritePath(configPath); err != nil {
		return fail(stderr, err)
	}
	// The first mutation is after the complete plan and confirmation. A
	// concurrent edit must not be silently overwritten after the review.
	if err := writeInitFile(file, old, proposed, exists); err != nil {
		return fail(stderr, err)
	}
	partial := func(err error) int {
		return fail(stderr, fmt.Errorf("partial init: project.yaml is in place; %w; fix the error and rerun init", err))
	}
	capsChanged, err := lifecycle.SetConfigValues(configPath, capValues)
	if err != nil {
		return partial(fmt.Errorf("save caps: %w", err))
	}
	if err := runnerstate.SaveParallel(runnerStateDir(rt), *parallel); err != nil {
		return partial(fmt.Errorf("save tasks at once: %w", err))
	}
	if err := applyWiring(append(slices.Clone(operator), wiring...)); err != nil {
		return partial(err)
	}
	savedSpecs := append(runnerSpecStrings(usable), keptAgents...)
	if err := runnerstate.SaveAgents(
		runnerStateDir(rt), savedSpecs); err != nil {
		return partial(fmt.Errorf("save agent choices: %w", err))
	}
	if err := runnerstate.SaveForeman(runnerStateDir(rt), agentName(chosen.harness, chosen.model, foremanEffort)); err != nil {
		return partial(fmt.Errorf("save foreman choice: %w", err))
	}
	written := 0
	if !exists || !bytes.Equal(old, proposed) {
		written++
	}
	written += countChanged(operator)
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
	if currentForeman.Harness != chosen.harness || currentForeman.Model != chosen.model || currentForeman.Effort != foremanEffort {
		if _, err := client.SetProjectForeman(ctx, origin, chosen.harness, chosen.model, foremanEffort); err != nil {
			return partial(fmt.Errorf("import succeeded but foreman registration failed: %w", err))
		}
	}

	runnerSpecs := runnerSpecStrings(runnerAgents)
	result, err := startDetachedRunner(ctx, rt, runnerSpecs, *parallel, "")
	if err != nil {
		return partial(fmt.Errorf("project registered and imported, but runner startup failed: %w", err))
	}
	restarted := false
	if result.Existing && initAgentsDiffer(result.Record, runnerAdverts, runnerAgents, *parallel) {
		// The handoff is the runner's own protocol: init leaves a
		// restart request it honours between jobs, so no running job is
		// cut — unless --now asked for exactly that.
		mode := runnerstate.RestartDrain
		if *now {
			mode = runnerstate.RestartNow
		}
		if result.Existing && !*now {
			fmt.Fprintln(stdout, "runner restarting: it finishes its running jobs first; --now cuts them")
		}
		result, err = startDetachedRunner(ctx, rt, runnerSpecs, *parallel, mode)
		if err != nil {
			return partial(fmt.Errorf("restart runner: %w", err))
		}
		restarted = true
		if result.Existing && initAgentsDiffer(result.Record, runnerAdverts, runnerAgents, *parallel) {
			return partial(fmt.Errorf("another runner started with different agents; rerun init"))
		}
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
		}
		fmt.Fprintf(&done, " Runner already running (pid %d); no second runner started; %s.", result.Record.PID, difference)
	} else if restarted {
		fmt.Fprintf(&done, " Runner restarted with the new agents (pid %d).", result.Record.PID)
	} else {
		fmt.Fprintf(&done, " Detached runner started (pid %d).", result.Record.PID)
	}
	if isLimited {
		done.WriteString(" Limited setup applied (review.family=require); reviews wait for another family.")
	} else if agentsMatch {
		done.WriteString(" Setup ready.")
	} else {
		done.WriteString(" Project configured; inspect the existing runner agents above before sending work.")
	}
	printInitWrapped(stdout, "", "  ", done.String())
	if engineRunning && capsChanged {
		printInitWrapped(stdout, "", "  ", "the engine reads caps when it starts: run brigada stop --engine, then any command starts it with the new values")
	}
	fmt.Fprintln(stdout, style.Bold("Next:"))
	fmt.Fprintf(stdout, "  1. Talk to the foreman: %s\n", style.Accent("brigada attach"))
	fmt.Fprintf(stdout, "  2. Or, without a conversation: %s\n", style.Accent("brigada ask \"what you want built\""))
	fmt.Fprintln(stdout, "  Watch progress: "+style.Accent("brigada status")+"    Stop the runner: "+style.Accent("brigada stop"))
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

// A typed resolved default name has the same metadata as its menu entry.
func initPickedModel(harness, model string, sources map[string]initModelSource) initModelEntry {
	for _, entry := range sources[harness].entries {
		if entry.value == model || entry.name == model {
			return entry
		}
	}
	entry := initModelEntry{value: model, name: initModelName(model)}
	if harness == "claude" {
		entry.effortLevels = adapter.EffortLevels(harness)
	}
	return entry
}

// Use the numbered menu's defaultID for saved models, including models no
// longer returned by the CLI's catalog.
func initCurrentModel(source initModelSource, spec agentSpec) initModelSource {
	source.entries = slices.Clone(source.entries)
	index := slices.IndexFunc(source.entries, func(entry initModelEntry) bool {
		return entry.value == spec.model
	})
	if index < 0 {
		entry := initPickedModel(spec.harness, spec.model,
			map[string]initModelSource{spec.harness: source})
		entry.value = spec.model
		entry.id = len(source.entries) + 1
		source.entries = append(source.entries, entry)
		index = len(source.entries) - 1
	}
	entry := &source.entries[index]
	entry.isCurrent = true
	source.defaultID = entry.id
	return source
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

// printInitWrapped keeps prose within the terminal width. Continuations either
// align with a table cell or use the paragraph's continuation indent.
func printInitWrapped(w io.Writer, prefix, continuation, text string, maxWidth ...int) {
	var words []initWord
	for _, word := range strings.Fields(text) {
		words = append(words, initWord{" ", word})
	}
	printInitWords(w, prefix, continuation, words, maxWidth...)
}

// initWord is text that is never split at a space, with the separator that
// precedes it unless it starts a line.
type initWord struct{ sep, text string }

func printInitWords(w io.Writer, prefix, continuation string, words []initWord, maxWidth ...int) {
	limit := initPresentation(w).style.Width()
	if len(maxWidth) > 0 {
		limit = min(limit, maxWidth[0])
	}
	if term.VisibleWidth(prefix) >= limit-1 {
		printInitWrapped(w, "    ", "      ", strings.TrimSpace(prefix))
		prefix, continuation = "      ", "      "
	}
	line := prefix
	for _, next := range words {
		word := next.text
		separator := ""
		if line != prefix {
			separator = next.sep
		}
		if term.VisibleWidth(line+separator+word) >= limit && line != prefix {
			fmt.Fprintln(w, line)
			prefix, line, separator = continuation, continuation, ""
		}
		// Long identifiers and paths have no word boundary to wrap at.
		for term.VisibleWidth(line+separator+word) >= limit {
			room := limit - 1 - term.VisibleWidth(line+separator)
			if room <= 0 {
				fmt.Fprintln(w, line)
				prefix, line, separator = continuation, continuation, ""
				continue
			}
			part, rest := initSplitVisible(word, room)
			fmt.Fprintln(w, line+separator+part)
			word = rest
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
	return strings.Join(commands, ", ")
}

func initAgentRoles(classes []string) string {
	if len(classes) == 0 {
		return "foreman only"
	}
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
	case "xiaomi":
		return "Xiaomi"
	case "meituan":
		return "Meituan"
	case "nvidia":
		return "NVIDIA"
	default:
		return family
	}
}

// printInitAgents is the Plan's foreman and agent table. The tier is what the
// foreman asks for per task; each row names the harness, model, family, tier
// and what the agent may do.
func printInitAgents(w io.Writer, agents []agentSpec, families int, requireFamily bool, foreman agentSpec, sources map[string]initModelSource, tier func(harness, model string) string) {
	out := initPresentation(w)
	style := out.style
	heading := "Plan"
	if out.styled {
		heading = style.Rule() + " Plan"
	}
	fmt.Fprintln(w, style.Bold(heading))
	foremanText := initForemanName(agentSpec{harness: foreman.harness, model: foreman.model}, sources)
	if foreman.effort != "" {
		foremanText += ", effort " + foreman.effort
	}
	printInitWrapped(w, "  "+style.Bold("Foreman:")+" ", "    ", foremanText)
	fmt.Fprintln(w, "  "+style.Bold("Agents (every model of each CLI; the foreman picks the tier per task):"))
	modelWidth, familyWidth, tierWidth := 0, 0, 0
	familyNames := make([]string, len(agents))
	modelCells := make([]string, len(agents))
	tiers := make([]string, len(agents))
	roles := make([]string, len(agents))
	roleWordWidth := 0
	// The phrases the table can print, besides the roles actually present.
	roleNeed := 0
	for _, role := range []string{initAgentRoles(nil), initAgentRoles([]string{"implement", "review", "answer"}), initAgentRoles([]string{"review"})} {
		roleNeed = max(roleNeed, initRoleWidth(role, 2))
	}
	for i, s := range agents {
		family, _ := adapter.AgentFamily(s.harness, s.model)
		family = initFamilyName(family)
		familyNames[i] = family
		modelCells[i] = initAgentModelName(s, sources)
		if s.effort != "" {
			modelCells[i] += style.Dim(" @" + s.effort)
		}
		tiers[i] = tier(s.harness, s.model)
		modelWidth = max(modelWidth, term.VisibleWidth(modelCells[i]))
		familyWidth = max(familyWidth, term.VisibleWidth(family))
		tierWidth = max(tierWidth, term.VisibleWidth(tiers[i]))
		roles[i] = initAgentRoles(s.classes)
		roleNeed = max(roleNeed, initRoleWidth(roles[i], 2))
		for _, word := range strings.Fields(roles[i]) {
			roleWordWidth = max(roleWordWidth, term.VisibleWidth(word))
		}
	}
	// Keep every row in the same layout. Rows stay on one line when the
	// role column can hold every role in at most two lines.
	stacked := 14+modelWidth+2+familyWidth+2+tierWidth+2+roleNeed >= style.Width()
	for i, s := range agents {
		// If the fixed columns leave no room for roles, put family, tier and
		// roles on a second line under the model column for every row. Very
		// long identifiers wrap within that column, with the rest on separate
		// lines.
		if stacked {
			indent := strings.Repeat(" ", 14)
			printInitWrapped(w, fmt.Sprintf("    %-9s ", s.harness), indent, modelCells[i])
			if 14+familyWidth+2+tierWidth+2+roleWordWidth >= style.Width() {
				printInitWrapped(w, indent, indent, style.Dim(familyNames[i]))
				printInitWrapped(w, indent, indent, tiers[i])
				printInitWrapped(w, indent, indent, roles[i])
			} else {
				prefix := indent + initPad(style.Dim(familyNames[i]), familyWidth) + "  " + initPad(tiers[i], tierWidth) + "  "
				printInitWrapped(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), roles[i])
			}
			continue
		}
		prefix := fmt.Sprintf("    %-9s ", s.harness) + initPad(modelCells[i], modelWidth) + "  " + initPad(style.Dim(familyNames[i]), familyWidth) + "  " + initPad(tiers[i], tierWidth) + "  "
		printInitWrapped(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), roles[i])
	}
	printInitWrapped(w, "  "+style.Bold("Reviews:")+" ", "    ", initReviewsLine(families, requireFamily))
	if slices.ContainsFunc(agents, func(s agentSpec) bool { return s.harness == "dsh" }) {
		printInitWrapped(w, "  ", "  ", style.Dim("dsh is experimental and may change in any release."))
	}
}

// initRoleWidth is the narrowest column in which text wraps to at most lines
// lines.
func initRoleWidth(text string, lines int) int {
	words := strings.Fields(text)
	width := 0
	for _, word := range words {
		width = max(width, term.VisibleWidth(word))
	}
	for ; ; width++ {
		used, line := 1, 0
		for _, word := range words {
			switch {
			case line == 0:
				line = term.VisibleWidth(word)
			case line+1+term.VisibleWidth(word) <= width:
				line += 1 + term.VisibleWidth(word)
			default:
				used++
				line = term.VisibleWidth(word)
			}
		}
		if used <= lines {
			return width
		}
	}
}

// initPad right-pads text, which may carry escapes, to a visible width.
func initPad(text string, width int) string {
	return text + strings.Repeat(" ", max(0, width-term.VisibleWidth(text)))
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

type initRepoSelection struct {
	transport string
	canon     string
	alias     string
	override  bool
}

func confirmInitSelection(s initRepoSelection, yes, probe bool, reader *bufio.Reader, out io.Writer) (bool, error) {
	if s.alias == "" && !s.override {
		return true, nil
	}
	if s.alias != "" {
		fmt.Fprintf(out, "SSH alias %s resolves to %s; registration transport: %s\n", s.alias, s.canon, engine.RedactRepoURL(s.transport))
	} else {
		fmt.Fprintf(out, "--repo selects %s; registration transport: %s\n", s.canon, engine.RedactRepoURL(s.transport))
	}
	if yes || probe {
		return true, nil
	}
	fmt.Fprint(out, "Use this repository? [y/N]: ")
	answer, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Fprintln(out, "Declined; nothing written or started.")
		return false, nil
	}
	return true, nil
}

// initSSHConfig is replaced in tests; only hostname and port are read.
var initSSHConfig = func(ctx context.Context, alias string) ([]byte, error) {
	return exec.CommandContext(ctx, "ssh", "-G", alias).Output()
}

type gitRemote struct {
	name string
	urls []string
}

var (
	errNoGitRemote     = errors.New("no git remote")
	errChooseGitRemote = errors.New("several git remotes without origin")
)

func listGitRemotes(ctx context.Context, root string) ([]gitRemote, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "remote").Output()
	if err != nil {
		return nil, fmt.Errorf("list git remotes: %w", err)
	}
	var remotes []gitRemote
	for _, name := range strings.Fields(string(out)) {
		r := gitRemote{name: name}
		urls, err := exec.CommandContext(ctx, "git", "-C", root, "remote", "get-url", "--all", name).Output()
		if err == nil {
			for _, raw := range strings.Split(strings.TrimSpace(string(urls)), "\n") {
				if raw != "" {
					r.urls = append(r.urls, raw)
				}
			}
		}
		remotes = append(remotes, r)
	}
	return remotes, nil
}

func chooseGitRemote(remotes []gitRemote, requested string) (gitRemote, error) {
	if requested != "" {
		for _, r := range remotes {
			if r.name == requested {
				return r, nil
			}
		}
		return gitRemote{}, fmt.Errorf("--remote %q does not name a git remote", requested)
	}
	for _, r := range remotes {
		if r.name == "origin" {
			return r, nil
		}
	}
	switch len(remotes) {
	case 0:
		return gitRemote{}, errNoGitRemote
	case 1:
		return remotes[0], nil
	default:
		return gitRemote{}, errChooseGitRemote
	}
}

func initOrigin(ctx context.Context, root string) (string, string, error) {
	s, err := selectInitOrigin(ctx, root, "")
	return s.transport, s.canon, err
}

func selectInitOrigin(ctx context.Context, root, override string) (initRepoSelection, error) {
	s, _, err := selectInitOriginWithRemote(ctx, root, override, "", nil)
	return s, err
}

func selectInitOriginWithRemote(ctx context.Context, root, override, requested string, ask func([]gitRemote) (string, error)) (initRepoSelection, string, error) {
	remotes, err := listGitRemotes(ctx, root)
	if err != nil {
		return initRepoSelection{}, "", err
	}
	chosenRemote, err := chooseGitRemote(remotes, requested)
	if errors.Is(err, errChooseGitRemote) && ask != nil {
		answer, askErr := ask(remotes)
		if askErr != nil {
			return initRepoSelection{}, "", askErr
		}
		chosenRemote, err = chooseGitRemote(remotes, answer)
	}
	if err != nil {
		if errors.Is(err, errNoGitRemote) {
			return initRepoSelection{}, "", fmt.Errorf("no remote: configure a git remote before init; a repository without a remote is unsupported")
		}
		if errors.Is(err, errChooseGitRemote) {
			return initRepoSelection{}, "", fmt.Errorf("several git remotes and none named origin; choose one with --remote NAME")
		}
		return initRepoSelection{}, "", err
	}
	if len(chosenRemote.urls) == 0 {
		return initRepoSelection{}, "", fmt.Errorf("git remote %q has no URL; configure it before init", chosenRemote.name)
	}
	raws := chosenRemote.urls
	if override != "" {
		id, err := engine.Canonicalize(override)
		if err != nil {
			return initRepoSelection{}, "", fmt.Errorf("--repo: %w", err)
		}
		transport := override
		if initBareIdentity(override) {
			user, _, _, ok := initSSHOrigin(raws[0])
			if !ok {
				return initRepoSelection{}, "", fmt.Errorf("--repo: a bare identity needs an ssh-like remote; give a full URL")
			}
			if strings.HasSuffix(id, ".git") {
				return initRepoSelection{}, "", fmt.Errorf("--repo: a bare identity ending in .git cannot keep its path in an SSH transport; give a full URL")
			}
			transport = initSSHTransport(user, id)
		}
		if err := checkInitTransport(transport, id); err != nil {
			return initRepoSelection{}, "", fmt.Errorf("--repo: %w", err)
		}
		return initRepoSelection{transport: transport, canon: id, override: true}, chosenRemote.name, nil
	}
	var selected initRepoSelection
	for _, raw := range raws {
		s, err := initOriginURL(ctx, raw)
		if err != nil {
			return initRepoSelection{}, "", fmt.Errorf("remote %q is ambiguous or invalid: %w; use --repo with a full URL or canonical identity", chosenRemote.name, err)
		}
		if selected.canon != "" && selected.canon != s.canon {
			return initRepoSelection{}, "", fmt.Errorf("remote %q has multiple canonical identities; use --repo to select one", chosenRemote.name)
		}
		if selected.canon == "" {
			selected = s
		}
	}
	return selected, chosenRemote.name, nil
}

func initOriginURL(ctx context.Context, raw string) (initRepoSelection, error) {
	user, host, path, sshLike := initSSHOrigin(raw)
	if sshLike && !strings.Contains(host, ".") && host != "localhost" {
		out, err := initSSHConfig(ctx, host)
		if err != nil {
			return initRepoSelection{}, fmt.Errorf("SSH alias %s could not be resolved; give --repo", host)
		}
		hostname, port := "", "22"
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			switch strings.ToLower(fields[0]) {
			case "hostname":
				hostname = fields[1]
			case "port":
				port = fields[1]
			}
		}
		if hostname == "" || !strings.Contains(hostname, ".") {
			return initRepoSelection{}, fmt.Errorf("SSH alias %s did not resolve to a full hostname; give --repo", host)
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return initRepoSelection{}, fmt.Errorf("SSH alias %s has an invalid port; give --repo", host)
		}
		hp := hostname
		if portNumber != 22 {
			hp += ":" + strconv.Itoa(portNumber)
		}
		transport := initSSHTransport(user, hp+"/"+strings.TrimLeft(path, "/"))
		id, err := engine.Canonicalize(transport)
		if err != nil {
			return initRepoSelection{}, fmt.Errorf("SSH alias %s: %w; give --repo", host, err)
		}
		if err := checkInitTransport(transport, id); err != nil {
			return initRepoSelection{}, err
		}
		return initRepoSelection{transport: transport, canon: id, alias: host}, nil
	}
	id, err := engine.Canonicalize(raw)
	if err != nil {
		return initRepoSelection{}, err
	}
	if initBareIdentity(raw) {
		return initRepoSelection{}, fmt.Errorf("origin names an identity without a transport; give --repo with a full URL")
	}
	return initRepoSelection{transport: raw, canon: id}, nil
}

// initBareIdentity recognizes the host/path and host:port/path forms,
// including spellings that Canonicalize normalizes.
func initBareIdentity(raw string) bool {
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "/") || strings.HasPrefix(strings.ToLower(raw), "file:") || strings.Contains(raw, "@") {
		return false
	}
	before, _, slash := strings.Cut(raw, "/")
	if !slash {
		return false
	}
	_, port, colon := strings.Cut(before, ":")
	if !colon {
		return true
	}
	if port == "" {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

var initSSHUserRe = regexp.MustCompile(`^[A-Za-z0-9._~%+-]+$`)

func initSSHOrigin(raw string) (user, host, path string, ok bool) {
	if strings.HasPrefix(strings.ToLower(raw), "file:") {
		return "", "", "", false
	}
	if strings.HasPrefix(strings.ToLower(raw), "ssh://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", "", false
		}
		if u.User != nil {
			if _, password := u.User.Password(); password {
				return "", "", "", false
			}
			user = u.User.Username()
			if !initSSHUserRe.MatchString(user) {
				return "", "", "", false
			}
		}
		return user, u.Hostname(), u.Path, true
	}
	if strings.Contains(raw, "://") {
		return "", "", "", false
	}
	rest := raw
	if before, after, found := strings.Cut(raw, "@"); found {
		user, rest = before, after
		if !initSSHUserRe.MatchString(user) || strings.Contains(rest, "@") {
			return "", "", "", false
		}
	}
	host, path, ok = strings.Cut(rest, ":")
	return user, host, path, ok && host != "" && !strings.ContainsAny(host, "/[]") && !strings.HasPrefix(path, "/")
}

func initSSHTransport(user, identity string) string {
	if user != "" {
		return "ssh://" + user + "@" + identity
	}
	return "ssh://" + identity
}

func checkInitTransport(transport, identity string) error {
	id, err := engine.Canonicalize(transport)
	if err != nil {
		return err
	}
	if id != identity {
		return fmt.Errorf("transport resolves to %s instead of %s", id, identity)
	}
	return nil
}

// printInitFileLine wraps displayed file contents without normalizing whitespace.
func printInitFileLine(w io.Writer, prefix, line string) {
	printInitWords(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), []initWord{{text: line}})
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
		printInitFileLine(w, "      -", line)
	}
	for _, line := range b {
		printInitFileLine(w, "      +", line)
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

func initAgentsDiffer(rec runnerstate.Record, agents []api.Agent,
	specs []agentSpec, parallel int) bool {
	if rec.MaxParallel != parallel {
		return true
	}
	if rec.Specs != nil && !sameInitSpecs(rec.Specs, runnerSpecStrings(specs)) {
		return true
	}
	return rec.Agents != nil && !sameInitAgents(rec.Agents, agents)
}

// initExistingCap reads one caps key from the loaded config file; false
// when the file does not name the layer.
func initExistingCap(l caps.Limits, key string) (int, bool) {
	rest, _ := strings.CutPrefix(key, "caps.")
	switch rest {
	case "global":
		if l.Global != nil {
			return *l.Global, true
		}
	case "plan":
		if l.Plan != nil {
			return *l.Plan, true
		}
	}
	if label, ok := strings.CutPrefix(rest, "account."); ok {
		if n, ok := l.Account[label]; ok {
			return n, true
		}
	}
	return 0, false
}
