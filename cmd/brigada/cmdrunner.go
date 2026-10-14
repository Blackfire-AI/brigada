package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/adapter/kimi"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runner"
)

// runRunner is the runner role (§5.1): it advertises one agent — a
// harness and a model on this machine — and long-polls the engine for
// jobs, one at a time: workspace, worker, result contract, check, push.
// The engine starts on demand here too (§13); before it is up the runner
// waits and retries, which is also how it survives an engine's restart.
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
	fs.StringVar(&harness, "harness", "claude", "the harness this runner offers: claude, kimi, or fake with --worker-script")
	fs.StringVar(&model, "model", "", "the model to run (claude default: sonnet; kimi default: the CLI's own; fake default: fake-1)")
	fs.StringVar(&workerScript, "worker-script", "", "with --harness fake: the scripted worker the fake adapter drives (tests)")
	fs.StringVar(&agentFile, "agent-file", "", "with --harness kimi: the agent definition file the CLI session starts under")
	fs.StringVar(&classes, "classes", "implement,review,answer", "the task classes this runner takes, comma-separated")
	fs.StringVar(&labels, "labels", "", "labels this runner offers on every lease poll, comma-separated")
	fs.IntVar(&maxParallel, "max-parallel", 0, "this runner's own cap on parallel jobs, advertised at register (0 = unset)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
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

	var ad adapter.Adapter
	switch harness {
	case "claude":
		if agentFile != "" {
			return usagef(stderr, "--agent-file is a --harness kimi flag; the claude harness takes none")
		}
		ad = &claude.Adapter{}
		if model == "" {
			model = "sonnet"
		}
	case "kimi":
		// The agent file is resolved and checked here, not at the first
		// job: a typo in the path is a startup failure, never a job's.
		if agentFile != "" {
			abs, err := filepath.Abs(agentFile)
			if err != nil {
				return fail(stderr, fmt.Errorf("--agent-file: %w", err))
			}
			if _, err := os.Stat(abs); err != nil {
				return fail(stderr, fmt.Errorf("--agent-file: %w", err))
			}
			agentFile = abs
		}
		ad = &kimi.Adapter{AgentFile: agentFile}
		if model == "" {
			model = kimi.DefaultModel
		}
	case "fake":
		if agentFile != "" {
			return usagef(stderr, "--agent-file is a --harness kimi flag; the fake harness takes none")
		}
		if workerScript == "" {
			return usagef(stderr, "--harness fake needs --worker-script: the scripted worker the fake adapter drives")
		}
		ad = &fake.Adapter{Script: workerScript}
		if model == "" {
			model = "fake-1"
		}
	default:
		return usagef(stderr, "unknown harness %q; want claude, kimi or fake", harness)
	}

	if code := ensureEngine(ctx, rt, noStart, stderr); code != 0 {
		return code
	}
	name, secret, err := lifecycle.TokenForRole(rt.paths.Tokens, "runner")
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "runner %s up on %s (harness %s, model %s)\n", name, rt.paths.Socket, harness, model)
	if err := runner.Run(ctx, runner.Config{
		Socket:   rt.paths.Socket,
		Token:    secret,
		StateDir: runnerStateDir(rt),
		Agents: []api.Agent{{
			Adapter: ad.Name(),
			Harness: harness,
			Model:   model,
			Family:  harness,
			Account: name,
			Classes: splitList(classes),
		}},
		Adapters:    []adapter.Adapter{ad},
		Labels:      splitList(labels),
		MaxParallel: maxParallel,
	}); err != nil {
		return fail(stderr, fmt.Errorf("runner: %w", err))
	}
	return 0
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
