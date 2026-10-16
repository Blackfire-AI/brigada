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
	"github.com/mattn/go-isatty"
)

// initBinary is empty in production (this executable); integration tests use
// the built CLI to exercise real detached engine and runner startup.
var initBinary string

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) (code int) {
	// Init has one refusal status, including invalid or incomplete setup input.
	defer func() {
		if code != 0 {
			code = 1
		}
	}()
	fs := newFlagSet("init")
	common := addCommon(fs)
	yes := fs.Bool("yes", false, "apply the displayed plan without prompting (does not confirm authentication)")
	probe := fs.Bool("probe", false, "print the plan only; write nothing and start nothing")
	limited := fs.Bool("accept-limited", false, "acknowledge fewer than two review families; reviews still require another family")
	check := fs.String("check", "", "default check command (suggested from build files; never executed by init)")
	tools := fs.String("tools", "", "worker scope in runner tools= syntax; applied only to Claude workers")
	foreman := fs.String("foreman", "", "choose a usable resume-capable agent as harness/model")
	noWiring := fs.Bool("no-wiring", false, "skip harness wiring (the bridge, hooks and foreman skill for mode A)")
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
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(stdout, "Usage: brigada init [--probe | --yes] [options] (inside one repository)")
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		return 0
	}
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "init takes no positional arguments; run it inside one repository")
	}
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
		return fail(stderr, fmt.Errorf("init needs one repository with an origin; parent-folder discovery is not implemented: %w", err))
	}
	// An origin must exist; a real local bare remote is valid. Multiple origin
	// URLs with different identities are ambiguous and must be fixed by the user.
	origin, canon, err := initOrigin(ctx, root)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Init plan for %s\n", canon)
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
	terminal := false
	if f, ok := projectStdin.(*os.File); ok {
		terminal = isatty.IsTerminal(f.Fd())
	}
	ask := func(prompt, flags string) (string, error) {
		if !canAsk {
			return "", nil
		}
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
		return strings.TrimSpace(v), nil
	}
	confirm := func(prompt, flags string) (bool, error) {
		v, err := ask(prompt+" [y/N] ", flags)
		v = strings.ToLower(v)
		return v == "y" || v == "yes", err
	}
	probes := map[string]harnessProbe{}
	for _, h := range initHarnesses {
		p := probeHarness(ctx, h)
		probes[h] = p
		if !p.installed {
			fmt.Fprintf(stdout, "  %s: not on PATH\n", h)
			continue
		}
		fmt.Fprintf(stdout, "  %s: installed; %s\n", h, p.evidence)
	}
	if len(agents.specs) == 0 && canAsk {
		for _, h := range initHarnesses {
			if !probes[h].installed {
				continue
			}
			model, err := ask(fmt.Sprintf("Model served by %s account [%s] (- to skip): ", h, runnerDefaultModel(h)), "--agent harness="+h+",model=MODEL")
			if err != nil {
				return fail(stderr, err)
			}
			if model == "-" {
				continue
			}
			if err := agents.Set("harness=" + h + ",model=" + model); err != nil {
				return usagef(stderr, "%v", err)
			}
		}
	}
	var usable []agentSpec
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
		family, err := adapter.AgentFamily(spec.harness, spec.model)
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
		authenticated := p.signedIn || signedIn[spec.harness]
		if !authenticated {
			authenticated, err = confirm("Confirm "+spec.harness+" account "+spec.accountLabel()+" is signed in and serves "+spec.model+"?", "--signed-in "+spec.harness)
			if err != nil {
				return fail(stderr, err)
			}
		}
		if !authenticated {
			fmt.Fprintf(stdout, "  skipping %s: authentication unconfirmed; sign in and pass --signed-in %s or confirm interactively\n", spec.harness, spec.harness)
			continue
		}
		usable = append(usable, spec)
		if slices.Contains(spec.classes, "review") {
			families[family] = true
		}
		fmt.Fprintf(stdout, "  agent %s: family=%s classes=%s\n", spec.String(), family, strings.Join(spec.classes, "+"))
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
	fmt.Fprintf(stdout, "Foreman: %s\n", agentName(chosen.harness, chosen.model, chosen.effort))
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
		suggestion, scope, err := suggestInitDefaults(root)
		if err != nil {
			return fail(stderr, err)
		}
		if *check == "" {
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
	fmt.Fprintf(stdout, "Default check: %s (suggested only; not executed)\nWorker tools: %s (Claude workers only)\n", *check, *tools)
	for _, a := range adverts {
		if a.Tools != "" {
			fmt.Fprintf(stdout, "  %s/%s effective worker tools: %s\n", a.Harness, a.Model, a.Tools)
		}
	}
	fmt.Fprintf(stdout, "Register project and foreman; import %s; start engine on demand and one detached runner in %s.\n", engine.ProjectConfigPath, rt.stateDir)
	if exists {
		printInitDiff(stdout, old, proposed)
	} else {
		fmt.Fprintf(stdout, "New %s:\n%s", engine.ProjectConfigPath, proposed)
	}
	var wiring []wireFile
	if !*noWiring {
		if probes["claude"].installed {
			wiring = planWiring(root)
			printWiringPlan(stdout, wiring)
		}
		if probes["codex"].installed {
			fmt.Fprint(stdout, codexWiringText)
		}
	}
	if *probe {
		fmt.Fprintln(stdout, "Probe only; nothing written or started. Re-run without --probe to confirm, or with --yes to apply.")
		return 0
	}
	if !*yes {
		apply, err := confirm("Apply this plan (including the file diff above)?", "--yes to apply the displayed plan")
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
	if exists && bytes.Equal(old, proposed) {
		fmt.Fprintf(stdout, "Unchanged file %s.\n", engine.ProjectConfigPath)
	} else {
		fmt.Fprintf(stdout, "Applied file %s; no git commit or push.\n", engine.ProjectConfigPath)
	}
	partial := func(err error) int {
		return fail(stderr, fmt.Errorf("partial init: project.yaml is in place; %w; fix the error and rerun init", err))
	}
	if err := applyWiring(wiring); err != nil {
		return partial(err)
	}
	for _, f := range wiring {
		if f.changed() {
			fmt.Fprintf(stdout, "Wired %s (%s); no git commit or push.\n", f.rel, f.state)
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
	if result.Existing {
		difference := "agents match the chosen ones"
		if result.Record.Agents == nil {
			agentsMatch = false
			difference = "existing agents unavailable; cannot compare to chosen ones"
		} else if !sameInitAgents(result.Record.Agents, adverts) || (result.Record.Specs != nil && !sameInitSpecs(result.Record.Specs, runnerSpecStrings(usable))) {
			agentsMatch = false
			difference = "agents differ from the chosen ones; existing agents kept"
		}
		fmt.Fprintf(stdout, "Runner already running (pid %d); no second runner started; %s.\n", result.Record.PID, difference)
	} else {
		fmt.Fprintf(stdout, "Detached runner started (pid %d); log: %s\n", result.Record.PID, filepath.Join(rt.stateDir, "runner.log"))
	}
	if isLimited {
		fmt.Fprintln(stdout, "Limited setup applied; cross-family reviews may wait.")
	} else if agentsMatch {
		fmt.Fprintln(stdout, "Setup ready.")
	} else {
		fmt.Fprintln(stdout, "Project configured; inspect the existing runner agents above before sending work.")
	}
	fmt.Fprintln(stdout, "Next: brigada ask \"your task\" / brigada attach; brigada status. Open your harness in the repo to use mode A where it is wired (the hold-only bootstrap credential of design §12.1 is not built yet).")
	return 0
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
	fmt.Fprintf(w, "Diff for %s:\n--- existing\n+++ proposed\n", engine.ProjectConfigPath)
	if bytes.Equal(old, next) {
		fmt.Fprintln(w, "(unchanged)")
		return
	}
	// A complete replacement hunk makes every retained comment/setting visible.
	a, b := strings.Split(strings.TrimSuffix(string(old), "\n"), "\n"), strings.Split(strings.TrimSuffix(string(next), "\n"), "\n")
	fmt.Fprintf(w, "@@ -1,%d +1,%d @@\n", len(a), len(b))
	for _, line := range a {
		fmt.Fprintln(w, "-"+line)
	}
	for _, line := range b {
		fmt.Fprintln(w, "+"+line)
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
