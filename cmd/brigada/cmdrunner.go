package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
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
		classes      string
		labels       string
		noStart      bool
	)
	fs.StringVar(&harness, "harness", "claude", "the harness this runner offers: claude, or fake with --worker-script")
	fs.StringVar(&model, "model", "", "the model to run (claude default: sonnet; fake default: fake-1)")
	fs.StringVar(&workerScript, "worker-script", "", "with --harness fake: the scripted worker the fake adapter drives (tests)")
	fs.StringVar(&classes, "classes", "implement,review,answer", "the task classes this runner takes, comma-separated")
	fs.StringVar(&labels, "labels", "", "labels this runner offers on every lease poll, comma-separated")
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
		ad = &claude.Adapter{}
		if model == "" {
			model = "sonnet"
		}
	case "fake":
		if workerScript == "" {
			return usagef(stderr, "--harness fake needs --worker-script: the scripted worker the fake adapter drives")
		}
		ad = &fake.Adapter{Script: workerScript}
		if model == "" {
			model = "fake-1"
		}
	default:
		return usagef(stderr, "unknown harness %q; want claude or fake", harness)
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
		Adapters: []adapter.Adapter{ad},
		Labels:   splitList(labels),
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
