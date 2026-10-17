package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// agentResetter is the one call the agent reset command needs from the
// API client; tests stub it.
type agentResetter interface {
	ResetAgent(ctx context.Context, harness, model string) (int, error)
}

func runAgent(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "agent", "usage: brigada agent reset <harness>/<model>", "reset", agentCommands)
}

// runAgentReset clears a broken or cooling agent (§8: "or the user
// resets it"). The agent is named by the harness/model pair that
// `brigada status` and `brigada stats` print, not by id: a model that
// several runners share resets on all of them, which is what a user who
// has fixed a login or has waited out a quota means. The harness is the
// part before the first slash, so a model name may hold slashes itself.
func runAgentReset(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agent reset",
		"brigada agent reset [--no-start] <harness>/<model>",
		"Clear a broken or cooling agent on every runner that offers it.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usagef(stderr, "usage: brigada agent reset <harness>/<model>")
	}
	harness, model, ok := strings.Cut(rest[0], "/")
	if !ok || harness == "" || model == "" {
		return usagef(stderr, "agent must be <harness>/<model>, got %q", rest[0])
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	return printAgentReset(ctx, client, harness, model, stdout, stderr)
}

func printAgentReset(ctx context.Context, client agentResetter, harness, model string, stdout, stderr io.Writer) int {
	n, err := client.ResetAgent(ctx, harness, model)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "agent %s/%s: reset (%d matched)\n", harness, model, n)
	return 0
}
