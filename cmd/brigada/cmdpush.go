package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// pushApprover is the one call the push command needs from the API
// client; tests stub it.
type pushApprover interface {
	PushApprove(ctx context.Context, repoURL string) (engine.PushApproveResult, error)
}

var pushCommands = map[string]command{
	"approve": {name: "approve", description: "Release the push gate and queue the waiting landings' pushes.", run: runPushApprove},
}

const pushUsage = "usage: brigada push approve [--repo URL] [--no-start]"

func runPush(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "push", pushUsage, "approve", pushCommands)
}

// runPushApprove is the user's yes on the push gate (§6.3): every
// landing recorded as waiting gets a push job — the land job reused, its
// push phase — which a runner performs. Only the client token can say
// it: the foreman has no way to pass the gate.
func runPushApprove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("push approve", "brigada push approve [--repo URL] [--no-start]",
		"Release the push gate and queue the waiting landings' pushes.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		repo    string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL (default: the working directory's origin)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "push approve takes no positional arguments")
	}
	if repo == "" {
		dir, err := os.Getwd()
		if err != nil {
			return fail(stderr, fmt.Errorf("find the working directory: %w", err))
		}
		if repo, err = gitOriginURL(dir); err != nil {
			return fail(stderr, err)
		}
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	return printPushApprove(ctx, client, repo, stdout, stderr)
}

func printPushApprove(ctx context.Context, client pushApprover, repo string, stdout, stderr io.Writer) int {
	res, err := client.PushApprove(ctx, repo)
	if err != nil {
		return fail(stderr, err)
	}
	if res.Released == 0 {
		fmt.Fprintln(stdout, "nothing waits to push")
		return 0
	}
	fmt.Fprintf(stdout, "push approved: %d landing(s) queued to push\n", res.Released)
	return 0
}
