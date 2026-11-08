package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// runPause is `brigada pause` (§8.3): the user stops dispatch for the
// whole engine, or with --repo for one project. Running jobs finish; what
// waits stays queued until `brigada resume`.
func runPause(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// The flag set is built with a literal name in each function: the
	// docs verifier reads flag definitions from the source.
	fs := newFlagSet("pause",
		"brigada pause [--repo URL] [--no-start]",
		"Pause dispatch for the engine, or for one project with --repo; running jobs finish.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	repo := fs.String("repo", "", "the project's repo URL; absent or empty names the engine")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	client, code := dispatchSession(ctx, fs, common, args, noStart, stdout, stderr)
	if client == nil {
		return code
	}
	res, err := client.Pause(ctx, *repo, engine.PauseOriginUser)
	if err != nil {
		return failDispatch(ctx, client, stderr, err)
	}
	fmt.Fprintln(stdout, pauseLine(res))
	return 0
}

// runResume is `brigada resume` (§8.3): it lifts the pause of the scope
// it names and no other.
func runResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("resume",
		"brigada resume [--repo URL] [--no-start]",
		"Resume dispatch for the engine, or for one project with --repo; the other scope's pause stays.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	repo := fs.String("repo", "", "the project's repo URL; absent or empty names the engine")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	client, code := dispatchSession(ctx, fs, common, args, noStart, stdout, stderr)
	if client == nil {
		return code
	}
	res, err := client.Resume(ctx, *repo)
	if err != nil {
		return failDispatch(ctx, client, stderr, err)
	}
	fmt.Fprintln(stdout, resumeLine(res))
	return 0
}

// dispatchSession parses the flags and connects: it returns a nil client
// with the exit code when the command is to end there. An absent or empty
// --repo is the engine's scope. It is never the working directory's
// origin: a bare pause must not become a project's, so askRepo's fallback
// is not used; a given URL goes to the engine as it is.
func dispatchSession(ctx context.Context, fs *flag.FlagSet, common *commonFlags, args []string, noStart *bool, stdout, stderr io.Writer) (*api.Client, int) {
	if code, stop := common.parse(stdout, stderr, args); stop {
		return nil, code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return nil, usagef(stderr, "%s takes no arguments, got %q; name a project with --repo URL", fs.Name(), extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return nil, code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return nil, code
	}
	return client, 0
}

func failDispatch(ctx context.Context, client *api.Client, stderr io.Writer, err error) int {
	if isNotFound(err) {
		if hint := knownProjectsHint(ctx, client); hint != "" {
			err = fmt.Errorf("%w; %s", err, hint)
		}
	}
	return fail(stderr, err)
}

// scopeName names a pause's scope in the command's words.
func scopeName(project int64, repo string) string {
	if project == 0 {
		return "the engine"
	}
	return "project " + repo
}

func pauseLine(res api.PauseReply) string {
	scope := scopeName(res.ProjectID, res.RepoURL)
	if res.Started {
		return fmt.Sprintf("dispatch paused for %s (origin %s); running jobs finish, queued jobs wait", scope, res.Origin)
	}
	return fmt.Sprintf("dispatch was already paused for %s (origin %s, since %s); nothing started", scope, res.Origin, res.StartedAt)
}

func resumeLine(res api.ResumeReply) string {
	scope := scopeName(res.ProjectID, res.RepoURL)
	switch {
	case res.Resumed && res.StillPaused != "":
		return fmt.Sprintf("dispatch resumed for %s; it is still paused by the %s's pause", scope, res.StillPaused)
	case res.Resumed:
		return fmt.Sprintf("dispatch resumed for %s (the pause was origin %s)", scope, res.Origin)
	case res.StillPaused != "":
		return fmt.Sprintf("%s has no pause of its own; the %s's pause still holds it", scope, res.StillPaused)
	}
	return fmt.Sprintf("nothing was paused for %s", scope)
}
