package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/codex"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/adapter/kimi"
	"github.com/Blackfire-AI/brigada/internal/adapter/opencode"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runner"
)

// runRunner is the runner role (§5.1): it advertises one or several
// agents — each a harness and a model on this machine, with the task
// classes it takes — and long-polls the engine for jobs, one at a
// time: workspace, worker, result contract, check, push. The engine
// starts on demand here too (§13); before it is up the runner waits
// and retries, which is also how it survives an engine's restart.
func runRunner(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runner",
		"brigada runner [--agent SPEC]... [--harness HARNESS] [--model MODEL] [--classes CLASSES] [--agent-file PATH] [--worker-script PATH] [--labels LABELS] [--max-parallel N] [--no-start]",
		"Run a runner that leases jobs, runs workers and checks, and pushes task branches.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		harness      string
		model        string
		workerScript string
		agentFile    string
		classes      string
		labels       string
		maxParallel  int
		noStart      bool
	)
	fs.StringVar(&harness, "harness", "claude", "the harness this runner offers: claude, kimi, codex, opencode, or fake with --worker-script")
	fs.StringVar(&model, "model", "", "the model to run (claude default: sonnet; kimi, codex and opencode default: the CLI's own; opencode model: provider/model; fake default: fake-1)")
	fs.StringVar(&workerScript, "worker-script", "", "with --harness fake: the scripted worker the fake adapter drives (tests)")
	fs.StringVar(&agentFile, "agent-file", "", "with --harness kimi: the agent definition file the CLI session starts under")
	fs.StringVar(&classes, "classes", defaultClasses, "the task classes this runner takes, comma-separated")
	fs.StringVar(&labels, "labels", "", "labels this runner offers on every lease poll, comma-separated")
	fs.IntVar(&maxParallel, "max-parallel", 0, "how many concurrent jobs this runner runs, advertised at register (0 = unset, one at a time)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	agents := &agentSpecs{}
	fs.Var(agents, "agent", "one offered agent, repeatable: harness=<h>[,model=<m>][,classes=<c1>+<c2>][,account=<label>][,agent-file=<path>][,effort=<level>][,tools=<p1>+<p2>|none]; account defaults to the harness name; effort is validated per harness (claude, codex, opencode; kimi has none); tools= (claude only) replaces the Go shell scope with Claude Code tool patterns, none = file tools only; replaces --harness/--model/--classes/--agent-file")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "runner takes no arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	if code := moveToStateDir(rt, stderr); code != 0 {
		return code
	}

	// The shorthand flags describe one agent; --agent describes every
	// one itself. Mixing the two forms is a usage error, so an argument
	// cannot be read as overriding half of another.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case len(agents.specs) > 0:
		for _, name := range []string{"harness", "model", "classes", "agent-file"} {
			if set[name] {
				return usagef(stderr, "--agent and --%s cannot be mixed: --agent describes every agent itself", name)
			}
		}
	default:
		agents.specs = append(agents.specs, agentSpec{
			harness: harness, model: model, classes: splitList(classes), agentFile: agentFile,
		})
	}

	ads, adverts, err := resolveRunnerAgents(rt, agents.specs, workerScript)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return fail(stderr, err)
		}
		return usagef(stderr, "%v", err)
	}
	if err := writeKimiReviewAgentFiles(ads); err != nil {
		return fail(stderr, err)
	}

	if code := ensureEngine(ctx, rt, noStart, stderr); code != 0 {
		return code
	}
	name, secret, err := lifecycle.TokenForRole(rt.paths.Tokens, "runner")
	if err != nil {
		return fail(stderr, err)
	}
	parts := make([]string, len(adverts))
	for i, a := range adverts {
		model := a.Model
		if a.Effort != "" {
			model += "@" + a.Effort
		}
		// This is the table's tier: the runner does not read the engine's override.
		parts[i] = fmt.Sprintf("%s (model %s, tier %s, classes %s)", a.Harness, model, adapter.AgentTier(a.Harness, a.Model), strings.Join(a.Classes, ","))
		if a.Tools != "" {
			parts[i] += " tools " + a.Tools
		}
	}
	// The startup line prints from inside Run, once the runner holds its
	// state directory's lock: printed here, a second process on the same
	// state directory would announce itself up and then be refused.
	if err := runner.Run(ctx, runner.Config{
		Socket:      rt.paths.Socket,
		Token:       secret,
		StateDir:    runnerStateDir(rt),
		Agents:      adverts,
		AgentSpecs:  runnerSpecStrings(agents.specs),
		Adapters:    ads,
		Labels:      splitList(labels),
		MaxParallel: maxParallel,
		Ready: func() {
			fmt.Fprintf(stdout, "runner %s up on %s offering %s\n", name, rt.paths.Socket, strings.Join(parts, "; "))
			for _, a := range adverts {
				if a.Family == "" {
					fmt.Fprintf(stdout, "%s/%s: model family unknown; it takes no review that needs another family (give provider/model to fix)\n", a.Harness, a.Model)
				}
			}
		},
	}); err != nil {
		return fail(stderr, fmt.Errorf("runner: %w", err))
	}
	return 0
}

// agentSpec is one offered agent as the --agent flag describes it.
type agentSpec struct {
	harness   string
	model     string
	classes   []string
	account   string
	agentFile string
	effort    string
	// tools is the explicit tools= scope: nil when unset, an empty
	// non-nil list for none. toolsText is the claude agent's effective
	// scope as registered, filled once the runner resolves it.
	tools     []string
	toolsText string
}

// accountLabel is the account the agent's quota is shared under (§8): the
// explicit account= label, else the harness name — one login per CLI on
// a runner is the common shape, so the account is the harness's login.
func (s agentSpec) accountLabel() string {
	if s.account != "" {
		return s.account
	}
	return s.harness
}

// defaultClasses is what an agent takes when nothing narrows it — the
// shorthand --classes default and the --agent form's own, kept one
// constant so the two forms cannot drift.
const defaultClasses = "implement,review,answer"

// agentSpecs is the repeatable --agent flag: one agentSpec per value.
type agentSpecs struct {
	specs []agentSpec
}

func (s *agentSpecs) String() string {
	if len(s.specs) == 0 {
		return ""
	}
	out := make([]string, len(s.specs))
	for i, spec := range s.specs {
		out[i] = spec.String()
	}
	return strings.Join(out, " ")
}

// Set parses one --agent value: harness=<h>[,model=<m>][,classes=<c1>+<c2>]
// [,account=<label>][,agent-file=<path>][,effort=<level>][,tools=<p1>+<p2>|none]. The harness
// is required; the
// rest keeps its harness's default. Everything but existence is validated here, so a
// bad agent is a startup failure, never a job's.
func (s *agentSpecs) Set(v string) error {
	spec := agentSpec{classes: splitList(defaultClasses)}
	sawClasses := false
	for _, field := range strings.Split(v, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return fmt.Errorf("--agent %q: want harness=<h>[,model=<m>][,classes=<c1>+<c2>][,account=<label>][,agent-file=<path>][,effort=<level>][,tools=<p1>+<p2>|none]", v)
		}
		switch key {
		case "harness":
			spec.harness = value
		case "model":
			spec.model = value
		case "classes":
			sawClasses = true
			spec.classes = splitPlusList(value)
		case "account":
			spec.account = value
		case "agent-file":
			spec.agentFile = value
		case "effort":
			spec.effort = value
		case "tools":
			tools, err := lifecycle.ParseTools(value)
			if err != nil {
				return fmt.Errorf("--agent %q: tools: %w", v, err)
			}
			spec.tools = tools
		default:
			return fmt.Errorf("--agent %q: unknown field %q; want harness, model, classes, account, agent-file, effort or tools", v, key)
		}
	}
	if spec.harness == "" {
		return fmt.Errorf("--agent %q: harness=<h> is required", v)
	}
	if sawClasses && len(spec.classes) == 0 {
		return fmt.Errorf("--agent %q: classes is empty; name at least one, + -separated", v)
	}
	s.specs = append(s.specs, spec)
	return nil
}

func (s agentSpec) String() string {
	fields := []string{"harness=" + s.harness}
	if s.model != "" {
		fields = append(fields, "model="+s.model)
	}
	if len(s.classes) > 0 {
		fields = append(fields, "classes="+strings.Join(s.classes, "+"))
	}
	if s.account != "" {
		fields = append(fields, "account="+s.account)
	}
	if s.agentFile != "" {
		fields = append(fields, "agent-file="+s.agentFile)
	}
	if s.effort != "" {
		fields = append(fields, "effort="+s.effort)
	}
	if s.tools != nil {
		text := "none"
		if len(s.tools) > 0 {
			text = strings.Join(s.tools, "+")
		}
		fields = append(fields, "tools="+text)
	}
	return strings.Join(fields, ",")
}

// splitList splits a comma-separated flag value into a non-nil list,
// dropping empty fields.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

// splitPlusList splits a plus-separated --agent classes value, dropping
// empty fields.
func splitPlusList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, "+") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

// runnerDefaultModel is the model recorded when a harness has no model flag.
func runnerDefaultModel(harness string) string {
	switch harness {
	case "claude":
		return "sonnet"
	case "codex":
		return codex.DefaultModel
	case "kimi":
		return kimi.DefaultModel
	case "opencode":
		return opencode.DefaultModel
	case "fake":
		return "fake-1"
	}
	return ""
}

// resolveRunnerAgents is shared by runner and init: validation, defaults and
// effective advertisements must agree before init starts a detached runner.
func resolveRunnerAgents(rt *runtime, specs []agentSpec, workerScript string) ([]adapter.Adapter, []api.Agent, error) {
	var ads []adapter.Adapter
	var adverts []api.Agent
	seen := map[string]bool{}
	for _, spec := range specs {
		if seen[spec.harness] {
			return nil, nil, fmt.Errorf("harness %q is offered twice: at most one agent per harness per runner", spec.harness)
		}
		seen[spec.harness] = true
		if len(spec.classes) == 0 {
			return nil, nil, fmt.Errorf("the agent for harness %q takes no classes: --classes must name at least one", spec.harness)
		}
		if spec.model == "" {
			spec.model = runnerDefaultModel(spec.harness)
		}
		var ad adapter.Adapter
		if spec.tools != nil && spec.harness != "claude" {
			return nil, nil, fmt.Errorf("--agent tools=: %s", lifecycle.ToolsRefusal(spec.harness))
		}
		switch spec.harness {
		case "claude":
			if spec.agentFile != "" {
				return nil, nil, fmt.Errorf("--agent-file is a --harness kimi flag; the claude harness takes none")
			}
			// The flag wins over the config key; neither is the Go
			// default (nil), which is not the same as none (empty).
			tools := spec.tools
			if tools == nil {
				tools = rt.file.Tools["claude/"+spec.model]
			}
			ad = &claude.Adapter{AllowedTools: tools}
			spec.toolsText = claude.ScopeText(tools)
		case "kimi":
			// The agent file is resolved and checked here, not at the
			// first job: a typo in the path is a startup failure, never
			// a job's.
			if spec.agentFile != "" {
				abs, err := filepath.Abs(spec.agentFile)
				if err != nil {
					return nil, nil, fmt.Errorf("agent-file: %w", err)
				}
				if _, err := os.Stat(abs); err != nil {
					return nil, nil, fmt.Errorf("agent-file: %w", err)
				}
				spec.agentFile = abs
			}
			ad = &kimi.Adapter{AgentFile: spec.agentFile, ReadOnlyAgentFile: filepath.Join(runnerStateDir(rt), "kimi-readonly.md")}
		case "codex":
			if spec.agentFile != "" {
				return nil, nil, fmt.Errorf("--agent-file is a --harness kimi flag; the codex harness takes none")
			}
			ad = &codex.Adapter{}
		case "opencode":
			if spec.agentFile != "" {
				return nil, nil, fmt.Errorf("--agent-file is a --harness kimi flag; the opencode harness takes none")
			}
			ad = &opencode.Adapter{}
		case "fake":
			if spec.agentFile != "" {
				return nil, nil, fmt.Errorf("--agent-file is a --harness kimi flag; the fake harness takes none")
			}
			if workerScript == "" {
				return nil, nil, fmt.Errorf("--harness fake needs --worker-script: the scripted worker the fake adapter drives")
			}
			ad = &fake.Adapter{Script: workerScript}
		default:
			return nil, nil, fmt.Errorf("unknown harness %q; want claude, kimi, codex, opencode or fake", spec.harness)
		}
		if err := adapter.ValidateEffort(spec.harness, spec.effort); err != nil {
			return nil, nil, fmt.Errorf("--agent harness=%s: %v", spec.harness, err)
		}
		ads = append(ads, ad)
		// Unknown custom aliases retain AgentFamily's harness fallback;
		// OpenCode's CLI default stays empty because its provider is unknown.
		family, _ := adapter.AgentFamily(spec.harness, spec.model)
		adverts = append(adverts, api.Agent{
			Effort:  spec.effort,
			Adapter: ad.Name(),
			Harness: spec.harness,
			Model:   spec.model,
			Family:  family,
			Account: spec.accountLabel(),
			Classes: spec.classes,
			Tools:   spec.toolsText,
		})
	}

	return ads, adverts, nil
}

// writeKimiReviewAgentFiles runs once at runner startup, never while init is
// displaying its plan or per job. Explicit agent files remain caller-owned.
func writeKimiReviewAgentFiles(ads []adapter.Adapter) error {
	for _, ad := range ads {
		k, ok := ad.(*kimi.Adapter)
		if !ok || k.AgentFile != "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(k.ReadOnlyAgentFile), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(k.ReadOnlyAgentFile, []byte(kimi.ReadOnlyAgentDefinition), 0600); err != nil {
			return err
		}
		if err := os.Chmod(k.ReadOnlyAgentFile, 0600); err != nil {
			return err
		}
	}
	return nil
}

func runnerSpecStrings(specs []agentSpec) []string {
	out := make([]string, len(specs))
	for i, spec := range specs {
		out[i] = spec.String()
	}
	return out
}
