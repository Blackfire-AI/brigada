package main

import (
	"context"
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
	fs := newFlagSet("runner")
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
	fs.IntVar(&maxParallel, "max-parallel", 0, "this runner's own cap on parallel jobs, advertised at register (0 = unset)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	agents := &agentSpecs{}
	fs.Var(agents, "agent", "one offered agent, repeatable: harness=<h>[,model=<m>][,classes=<c1>+<c2>][,agent-file=<path>]; replaces --harness/--model/--classes/--agent-file")
	if code := common.parse(stderr, args); code != 0 {
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

	var ads []adapter.Adapter
	var adverts []api.Agent
	seen := map[string]bool{}
	for _, spec := range agents.specs {
		if seen[spec.harness] {
			return usagef(stderr, "harness %q is offered twice: at most one agent per harness per runner", spec.harness)
		}
		seen[spec.harness] = true
		if len(spec.classes) == 0 {
			return usagef(stderr, "the agent for harness %q takes no classes: --classes must name at least one", spec.harness)
		}
		var ad adapter.Adapter
		switch spec.harness {
		case "claude":
			if spec.agentFile != "" {
				return usagef(stderr, "--agent-file is a --harness kimi flag; the claude harness takes none")
			}
			ad = &claude.Adapter{}
			if spec.model == "" {
				spec.model = "sonnet"
			}
		case "kimi":
			// The agent file is resolved and checked here, not at the
			// first job: a typo in the path is a startup failure, never
			// a job's.
			if spec.agentFile != "" {
				abs, err := filepath.Abs(spec.agentFile)
				if err != nil {
					return fail(stderr, fmt.Errorf("agent-file: %w", err))
				}
				if _, err := os.Stat(abs); err != nil {
					return fail(stderr, fmt.Errorf("agent-file: %w", err))
				}
				spec.agentFile = abs
			}
			ad = &kimi.Adapter{AgentFile: spec.agentFile}
			if spec.model == "" {
				spec.model = kimi.DefaultModel
			}
		case "codex":
			if spec.agentFile != "" {
				return usagef(stderr, "--agent-file is a --harness kimi flag; the codex harness takes none")
			}
			ad = &codex.Adapter{}
			if spec.model == "" {
				spec.model = codex.DefaultModel
			}
		case "opencode":
			if spec.agentFile != "" {
				return usagef(stderr, "--agent-file is a --harness kimi flag; the opencode harness takes none")
			}
			ad = &opencode.Adapter{}
			if spec.model == "" {
				spec.model = opencode.DefaultModel
			}
		case "fake":
			if spec.agentFile != "" {
				return usagef(stderr, "--agent-file is a --harness kimi flag; the fake harness takes none")
			}
			if workerScript == "" {
				return usagef(stderr, "--harness fake needs --worker-script: the scripted worker the fake adapter drives")
			}
			ad = &fake.Adapter{Script: workerScript}
			if spec.model == "" {
				spec.model = "fake-1"
			}
		default:
			return usagef(stderr, "unknown harness %q; want claude, kimi, codex, opencode or fake", spec.harness)
		}
		ads = append(ads, ad)
		adverts = append(adverts, api.Agent{
			Adapter: ad.Name(),
			Harness: spec.harness,
			Model:   spec.model,
			Family:  spec.harness,
			Classes: spec.classes,
		})
	}

	if code := ensureEngine(ctx, rt, noStart, stderr); code != 0 {
		return code
	}
	name, secret, err := lifecycle.TokenForRole(rt.paths.Tokens, "runner")
	if err != nil {
		return fail(stderr, err)
	}
	for i := range adverts {
		adverts[i].Account = name
	}
	parts := make([]string, len(adverts))
	for i, a := range adverts {
		parts[i] = fmt.Sprintf("%s (model %s, classes %s)", a.Harness, a.Model, strings.Join(a.Classes, ","))
	}
	// The startup line prints from inside Run, once the runner holds its
	// state directory's lock: printed here, a second process on the same
	// state directory would announce itself up and then be refused.
	if err := runner.Run(ctx, runner.Config{
		Socket:      rt.paths.Socket,
		Token:       secret,
		StateDir:    runnerStateDir(rt),
		Agents:      adverts,
		Adapters:    ads,
		Labels:      splitList(labels),
		MaxParallel: maxParallel,
		Ready: func() {
			fmt.Fprintf(stdout, "runner %s up on %s offering %s\n", name, rt.paths.Socket, strings.Join(parts, "; "))
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
	agentFile string
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
// [,agent-file=<path>]. The harness is required; the rest keeps its
// harness's default. Everything but existence is validated here, so a
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
			return fmt.Errorf("--agent %q: want harness=<h>[,model=<m>][,classes=<c1>+<c2>][,agent-file=<path>]", v)
		}
		switch key {
		case "harness":
			spec.harness = value
		case "model":
			spec.model = value
		case "classes":
			sawClasses = true
			spec.classes = splitPlusList(value)
		case "agent-file":
			spec.agentFile = value
		default:
			return fmt.Errorf("--agent %q: unknown field %q; want harness, model, classes or agent-file", v, key)
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
	if s.agentFile != "" {
		fields = append(fields, "agent-file="+s.agentFile)
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
