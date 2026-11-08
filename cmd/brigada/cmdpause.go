package main

import (
	"context"
	"fmt"
	"io"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// Tests can exercise pause and resume over an in-process API without a
// listener.
var connectPause = connect

// runPause is `brigada pause` (§8.3): pause dispatch — the whole engine
// bare, one project with --repo. While the pause stands the lease hands
// out none of the scope's jobs; running ones run to their end, and what
// they queue next waits.
func runPause(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("pause",
		"brigada pause [--repo URL] [--no-start]",
		"Pause dispatch: the whole engine, or one project with --repo.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		repo    string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the project's repo URL; empty pauses the whole engine")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "pause takes no positional arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connectPause(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	// An empty --repo is the engine's scope, never resolved to the
	// working directory's origin: a bare pause stays the engine's.
	res, err := client.Pause(ctx, repo, engine.PauseOriginUser)
	if err != nil {
		return failRepo(stderr, ctx, client, err)
	}
	switch {
	case res.ProjectID == 0 && res.Started:
		fmt.Fprintln(stdout, "engine paused")
	case res.ProjectID == 0:
		fmt.Fprintf(stdout, "engine already paused (origin %s)\n", res.Origin)
	case res.Started:
		fmt.Fprintf(stdout, "paused %s\n", repo)
	default:
		fmt.Fprintf(stdout, "%s already paused (origin %s)\n", repo, res.Origin)
	}
	return 0
}

// runResume is `brigada resume` (§8.3): lift the pause of one scope —
// the engine's bare, one project's with --repo — and no other. The
// engine evaluates the wakes the pause held; a scope still covered by
// the other one's pause is said to stay paused.
func runResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("resume",
		"brigada resume [--repo URL] [--no-start]",
		"Lift a dispatch pause: the engine's, or one project's with --repo.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		repo    string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the project's repo URL; empty lifts the engine's pause")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "resume takes no positional arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connectPause(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	res, err := client.Resume(ctx, repo)
	if err != nil {
		return failRepo(stderr, ctx, client, err)
	}
	switch {
	case res.ProjectID == 0 && res.Resumed:
		fmt.Fprintln(stdout, "engine resumed")
	case res.ProjectID == 0:
		fmt.Fprintln(stdout, "engine is not paused")
	case res.Resumed:
		fmt.Fprintf(stdout, "resumed %s\n", repo)
	default:
		if holdsPauseOf(ctx, client, 0) {
			fmt.Fprintf(stdout, "%s is still paused by the engine's pause\n", repo)
		} else {
			fmt.Fprintf(stdout, "%s is not paused\n", repo)
		}
	}
	return 0
}

// holdsPauseOf reports whether a pause of the scope still stands: the
// engine's with project 0, one project's with its id. Resume's answer
// for a scope another pause still covers reads it.
func holdsPauseOf(ctx context.Context, client *api.Client, project int64) bool {
	st, err := client.Status(ctx)
	if err != nil {
		return false
	}
	for _, p := range st.Pauses {
		if p.ProjectID == project {
			return true
		}
	}
	return false
}

// failRepo reports a pause or resume the engine refused, naming the
// known projects when the repo URL matched none of them.
func failRepo(stderr io.Writer, ctx context.Context, client *api.Client, err error) int {
	if isNotFound(err) {
		if hint := knownProjectsHint(ctx, client); hint != "" {
			err = fmt.Errorf("%w; %s", err, hint)
		}
	}
	return fail(stderr, err)
}
