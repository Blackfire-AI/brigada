package main

import (
	"context"
	"fmt"
	"io"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// runPause is `brigada pause` (§8.3): the user stops dispatch for the
// whole engine, or for one project with --repo. Running jobs are not
// touched; nothing new leases until `brigada resume` lifts the pause.
func runPause(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("pause",
		"brigada pause [--repo URL] [--no-start]",
		"Pause dispatch for the whole engine, or for one project with --repo: running jobs finish, nothing new starts.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	repo := fs.String("repo", "", "the project's repo URL; empty or absent pauses the whole engine")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "pause takes no arguments, got %q; name a project with --repo URL", extra)
	}
	scopeRepo, err := dispatchRepo(*repo)
	if err != nil {
		return fail(stderr, err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	res, err := client.Pause(ctx, scopeRepo, engine.PauseOriginUser)
	if err != nil {
		return fail(stderr, withProjectsHint(ctx, client, err))
	}
	scope := scopeName(scopeRepo)
	if res.Started {
		fmt.Fprintf(stdout, "paused %s: nothing new is leased, running jobs finish; brigada resume lifts it\n", scope)
		return 0
	}
	fmt.Fprintf(stdout, "%s was already paused (origin %s, since %s)\n", scope, res.Origin, res.StartedAt)
	return 0
}

// runResume is `brigada resume` (§8.3): it lifts the pause of the scope it
// names and no other, and says what still holds a project paused.
func runResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("resume",
		"brigada resume [--repo URL] [--no-start]",
		"Lift the pause of the whole engine, or of one project with --repo, and no other.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	repo := fs.String("repo", "", "the project's repo URL; empty or absent resumes the whole engine")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "resume takes no arguments, got %q; name a project with --repo URL", extra)
	}
	scopeRepo, err := dispatchRepo(*repo)
	if err != nil {
		return fail(stderr, err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	res, err := client.Resume(ctx, scopeRepo)
	if err != nil {
		return fail(stderr, withProjectsHint(ctx, client, err))
	}
	scope := scopeName(scopeRepo)
	if res.Resumed {
		fmt.Fprintf(stdout, "resumed %s\n", scope)
	} else {
		fmt.Fprintf(stdout, "%s was not paused\n", scope)
	}
	// A lift frees nothing of a project another scope still pauses.
	st, err := client.Status(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("read the pauses that remain: %w", err))
	}
	for _, line := range remainingPauseLines(st, scopeRepo) {
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// dispatchRepo is the project a pause or a resume names: empty — absent,
// or given empty — is the engine; a given URL is sent as given, as
// `brigada ask` passes one on. askRepo is not called for an empty value,
// for its fallback to the working directory's origin would turn an engine
// pause into a project pause.
func dispatchRepo(repo string) (string, error) {
	if repo == "" {
		return "", nil
	}
	return askRepo(repo)
}

// scopeName names a scope in the command's answers.
func scopeName(repo string) string {
	if repo == "" {
		return "the engine"
	}
	return "project " + repo
}

// withProjectsHint adds the known projects to a not-found answer, as ask
// does for a repo the engine has no project for.
func withProjectsHint(ctx context.Context, client *api.Client, err error) error {
	if isNotFound(err) {
		if hint := knownProjectsHint(ctx, client); hint != "" {
			return fmt.Errorf("%w; %s", err, hint)
		}
	}
	return err
}

// remainingPauseLines says what still pauses after a resume of the scope
// repo names (§8.3): a project is still paused by the engine's pause, and
// the engine's resume leaves every project's own pause standing.
func remainingPauseLines(st api.Status, repo string) []string {
	var lines []string
	for _, p := range st.Pauses {
		switch {
		case repo != "" && p.ProjectID == 0:
			lines = append(lines, fmt.Sprintf(
				"project %s is still paused by the engine's pause; brigada resume lifts it", repo))
		case repo == "" && p.ProjectID != 0:
			lines = append(lines, fmt.Sprintf(
				"project %s is still paused by its own pause; brigada resume --repo %s lifts it", p.RepoURL, p.RepoURL))
		}
	}
	return lines
}
