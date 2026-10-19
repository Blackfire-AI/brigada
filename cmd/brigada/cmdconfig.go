package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// runConfig gathers the three independently managed parts of setup. Agent
// choices survive a stopped runner; project values and provenance belong to
// the engine; machine values describe the file, not a daemon's flag overrides.
func runConfig(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("config",
		"brigada config [--state-dir DIR] [--config PATH]",
		"Show agents, foreman, project and machine settings, and how to change them.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "config takes no arguments, got %q", fs.Args())
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	saved, err := runnerstate.LoadAgents(runnerStateDir(rt))
	if err != nil {
		return fail(stderr, err)
	}
	var agents agentSpecs
	for _, spec := range saved {
		if err := agents.Set(spec); err != nil {
			return fail(stderr, fmt.Errorf("saved agents: %w", err))
		}
	}
	rec, running, err := runnerstate.Running(runnerStateDir(rt))
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "Agents (runner; change: brigada init)")
	models, efforts := make([]string, len(agents.specs)), make([]string, len(agents.specs))
	modelWidth, effortWidth := 0, 0
	for i, s := range agents.specs {
		models[i], efforts[i] = configAgentDetails(ctx, s)
		modelWidth = max(modelWidth, len([]rune(models[i])))
		effortWidth = max(effortWidth, len([]rune(efforts[i])))
	}
	for i, s := range agents.specs {
		prefix := fmt.Sprintf("  %-9s %-*s effort: %-*s ", s.harness, modelWidth, models[i], effortWidth, efforts[i])
		printInitWrapped(stdout, prefix, "    ", initAgentRoles(s.classes))
		// Show less common saved choices too, rather than hiding the scope or
		// account that will be used on the next start.
		if s.account != "" {
			printInitWrapped(stdout, "    account: ", "      ", s.account)
		}
		if s.agentFile != "" {
			printInitWrapped(stdout, "    agent-file: ", "      ", s.agentFile)
		}
		if s.tools != nil {
			tools := "none"
			if len(s.tools) > 0 {
				tools = strings.Join(s.tools, "+")
			}
			printInitWrapped(stdout, "    tools: ", "      ", tools)
		}
	}
	if len(agents.specs) == 0 {
		fmt.Fprintln(stdout, "  no saved agents; run brigada init")
	}
	state := "stopped"
	if running {
		state = fmt.Sprintf("running (pid %d)", rec.PID)
	}
	printInitWrapped(stdout, "  runner: ", "    ", state+" · after a reboot: brigada start")
	printInitWrapped(stdout, "  saved in: ", "    ", initHomePath(filepath.Join(runnerStateDir(rt), "agents")))

	if code := printConfigProject(ctx, rt, stdout, stderr); code != 0 {
		return code
	}
	return printConfigMachine(rt, common.stateDir, stdout, stderr)
}

func printConfigProject(ctx context.Context, rt *runtime, stdout, stderr io.Writer) int {
	const unknown = "This project: run inside a repository brigada knows (brigada init)"
	dir, err := os.Getwd()
	if err != nil {
		return fail(stderr, err)
	}
	if _, err := gitRoot(dir); err != nil {
		fmt.Fprintln(stdout, unknown)
		return 0
	}
	repo, err := gitOriginURL(dir)
	if err != nil {
		fmt.Fprintln(stdout, unknown)
		return 0
	}
	canon, err := engine.Canonicalize(repo)
	if err != nil {
		fmt.Fprintln(stdout, unknown)
		return 0
	}
	client, code := connect(ctx, rt, "client", false, stderr)
	if code != 0 {
		return code
	}
	// The active project list excludes archived repositories.
	projects, err := client.Projects(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if !slices.ContainsFunc(projects, func(p engine.ProjectInfo) bool { return p.RepoURL == canon }) {
		fmt.Fprintln(stdout, unknown)
		return 0
	}
	settings, err := client.ProjectSettings(ctx, repo)
	if err != nil {
		return fail(stderr, err)
	}
	foreman, err := client.ProjectForeman(ctx, repo)
	if err != nil {
		return fail(stderr, err)
	}
	printConfigProjectSettings(stdout, settings, foreman)
	return 0
}

func printConfigProjectSettings(stdout io.Writer, settings engine.ProjectSettings, foreman engine.ForemanSetting) {
	fmt.Fprintln(stdout, "Foreman (change: brigada project foreman --harness H --model M")
	fmt.Fprintln(stdout, "         [--effort E])")
	name := "none chosen"
	if foreman.Harness != "" {
		name = agentName(foreman.Harness, foreman.Model, foreman.Effort)
	}
	printInitWrapped(stdout, "  ", "    ", name)
	printInitWrapped(stdout, "This project (", "  ", settings.RepoURL+")")
	for _, s := range settings.Settings {
		source := "set by hand"
		switch {
		case s.Default || s.Source == engine.SourceDefault:
			source = "default"
		case s.Source == engine.SourceImported:
			source = "from " + engine.ProjectConfigPath
		}
		words := append(configValueWords(s.Key, s.Value), initWord{" ", source})
		printInitWords(stdout, "  "+s.Key+": ", "    ", words)
	}
	printInitWrapped(stdout, "  change: ", "    ", "edit .brigada/project.yaml, then brigada project import")
	printInitWrapped(stdout, "  ", "    ", "set by hand/local only: brigada project set key=value")
}

// configValueWords splits a value for wrapping. A tools value joins patterns
// with "+" and a pattern holds spaces ("Bash(go vet:*)"), so it wraps only
// after a "+"; any other value wraps at spaces.
func configValueWords(key, value string) []initWord {
	var words []initWord
	if key == engine.SettingWorkerTools || strings.HasPrefix(key, "tools.") {
		parts := strings.Split(value, "+")
		for i, part := range parts {
			if i < len(parts)-1 {
				part += "+"
			}
			words = append(words, initWord{"", part})
		}
		return words
	}
	for _, word := range strings.Fields(value) {
		words = append(words, initWord{" ", word})
	}
	return words
}

// Read only named, non-secret CLI metadata. A saved "default" stays a CLI
// default; resolving its display name never changes the saved agent choices.
func configAgentDetails(ctx context.Context, s agentSpec) (model, effort string) {
	model = s.model
	if model == "" {
		model = runnerDefaultModel(s.harness)
	}
	effort = s.effort
	home, _ := os.UserHomeDir()
	switch s.harness {
	case "claude":
		if effort == "" {
			effort = "Claude's default"
		}
	case "codex":
		path := codexConfigPath()
		if model == "default" && path != "" {
			if name, _ := readInitModelConfig(path, "model", false); name != "" {
				model = name
			}
		}
		if effort == "" {
			// The menu entries already hold the effort init would show as the
			// default: the user's own value when the model offers it.
			level := ""
			for _, entry := range initCodexModels(ctx).entries {
				if entry.name == model || entry.value == model {
					level = entry.defaultEffort
					break
				}
			}
			if level == "" {
				level = codexUserEffort(path)
			}
			effort = "codex's default"
			if level != "" {
				effort = level + " (codex default)"
			}
		}
	case "kimi":
		name, models := readInitModelConfig(filepath.Join(home, ".kimi-code", "config.toml"), "default_model", true)
		if model == "default" && name != "" {
			model = name
		}
		if effort == "" {
			effort = "kimi's default"
			for _, entry := range models {
				if entry.id == model && entry.defaultEffort != "" {
					effort = entry.defaultEffort + " (kimi config)"
				}
			}
		}
	}
	if model == "default" || model == "" {
		model = "CLI default"
	}
	if effort == "" {
		effort = s.harness + "'s default"
	}
	return model, effort
}

// printConfigMachine lists the machine config. flagStateDir is --state-dir;
// state_dir shows the directory in use and where it came from.
func printConfigMachine(rt *runtime, flagStateDir string, stdout, stderr io.Writer) int {
	stateDir, stateSource := initHomePath(absPath(rt.stateDir)), "(default)"
	switch {
	case flagStateDir != "":
		stateSource = "(--state-dir)"
	case rt.file.StateDir != "":
		stateSource = "(config file)"
	}
	// Keep these in the config loader's spelling. Wildcard rows describe the
	// fallback for unnamed accounts/models; concrete keys follow in order.
	defaults := [][2]string{
		{"notify", "auto"}, {"idle_minutes", "30"},
		{"state_dir", ""},
		{"snapshot_dir", "none"}, {"headroom", "true"},
		{"task_auto_retry", "true"}, {"foreman_wakes_per_day", "20"},
		{"default_reviewer", "none"},
		{"caps.global", "no cap"}, {"caps.plan", "no cap"}, {"caps.runner", "no cap"},
		{"caps.account.<label>", "no cap"}, {"caps.model.<model>", "no cap"},
		{"cost.<harness>/<model>", "1"}, {"tier.<harness>/<model>", "from the model name"},
		{"tools.claude/<model>", claude.ScopeText(nil)},
	}
	// The loader already validated this file. No accepted key holds a secret;
	// do not read harness config or credential files into this map.
	data, err := os.ReadFile(rt.configPath)
	if err != nil && !os.IsNotExist(err) {
		return fail(stderr, err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	printInitWrapped(stdout, "This machine (", "  ", initHomePath(rt.configPath)+"; edit the file)")
	for _, pair := range defaults {
		key, value := pair[0], pair[1]
		if key == "state_dir" {
			printInitWrapped(stdout, "  ", "    ", key+" = "+stateDir+" "+stateSource)
			delete(values, key)
			continue
		}
		if v, ok := values[key]; ok {
			if v == "" {
				v = "none"
			}
			if key == "idle_minutes" && rt.file.IdleMinutes == 0 {
				v = "30 (default; file has 0)"
			}
			printInitWords(stdout, "  "+key+" = ", "    ", configValueWords(key, v))
			delete(values, key)
		} else {
			printInitWords(stdout, "  "+key+" = ", "    ",
				append(configValueWords(key, value), initWord{" ", "(default)"}))
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		printInitWrapped(stdout, "  ", "    ", key+" = "+values[key])
	}
	return 0
}
