package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/projecturl"
)

// runDoctor is `brigada doctor`: the pre-flight facts init gathers, on demand.
// It runs in a repository init registered — the registration check is that
// .brigada/project.yaml exists and parses — and reports the git identity, the
// project's remote, the push probe init runs (one implementation, runInitPushProbe,
// two surfaces) and the harness sign-in status. It writes no local files; the
// probe pushes a scratch ref to the remote and deletes it again.
func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor",
		"brigada doctor",
		"Check this registered repository's pre-flight facts: git identity, remote, push probe and harness sign-in. Experimental: the output will evolve.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if rest := fs.Args(); len(rest) > 0 {
		return usagef(stderr, "doctor takes no arguments, got %q", rest)
	}
	dir, err := os.Getwd()
	if err != nil {
		return fail(stderr, err)
	}
	root, err := gitRoot(dir)
	if err != nil {
		return usagef(stderr, "doctor runs inside a registered repository; run it in the repository, or brigada init to register one")
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(engine.ProjectConfigPath)))
	if errors.Is(err, os.ErrNotExist) {
		return usagef(stderr, "%s is not registered (no %s); run brigada init first", root, engine.ProjectConfigPath)
	}
	if err != nil {
		return fail(stderr, err)
	}
	cfg, err := engine.ParseProjectConfig(raw)
	if err != nil {
		return fail(stderr, err)
	}

	remotes, err := listGitRemotes(ctx, root)
	if err != nil {
		return fail(stderr, err)
	}
	remote, err := chooseGitRemote(remotes, "")
	switch {
	case errors.Is(err, errNoGitRemote):
		return fail(stderr, errors.New("no remote: configure a git remote; a repository without a remote is unsupported"))
	case errors.Is(err, errChooseGitRemote):
		return fail(stderr, errors.New("several git remotes and none named origin; doctor probes origin, or the only remote"))
	case err != nil:
		return fail(stderr, err)
	}

	fmt.Fprintf(stdout, "Project: %s\n", projecturl.Redact(cfg.Repo))
	for _, key := range []string{"user.name", "user.email"} {
		value, err := probeGitIdentity(ctx, root, key)
		if err != nil {
			value = "not set"
		}
		fmt.Fprintf(stdout, "Git identity: %s = %s\n", key, value)
	}
	redacted := make([]string, len(remote.urls))
	for i, u := range remote.urls {
		redacted[i] = projecturl.Redact(u)
	}
	fmt.Fprintf(stdout, "Remote: %s %s\n", remote.name, strings.Join(redacted, ", "))

	code := 0
	res, probeErr := runInitPushProbe(ctx, root, remote.name)
	switch {
	case probeErr != nil:
		fmt.Fprintf(stdout, "Push probe: FAILED: %v\n", probeErr)
		code = 1
	case res.outcome == probeOutcomeRefused:
		fmt.Fprintf(stdout, "Push probe: FAILED: %s refused the push\n", remote.name)
		if fix := initProbeFix(res.words); fix != "" {
			fmt.Fprintf(stdout, "  Fix: %s\n", fix)
		}
		fmt.Fprintln(stdout, res.words)
		code = 1
	case res.outcome == probeOutcomeUnreachable:
		fmt.Fprintf(stdout, "Push probe: WARNING: the push path could not be verified (%s)\n", res.words)
	default:
		fmt.Fprintf(stdout, "Push probe: ok (pushed and deleted a scratch ref on %s)\n", remote.name)
	}
	if res.deleteErr != nil {
		fmt.Fprintf(stdout, "WARNING: the probe could not delete its scratch ref %s (%v). Delete it with:\n  git push %s --delete %s\n",
			res.ref, res.deleteErr, remote.name, res.ref)
	}

	fmt.Fprintln(stdout, "Harness sign-in:")
	for _, h := range initHarnesses {
		fmt.Fprintf(stdout, "  %-9s %s\n", initHarnessLabel(h), probeHarness(ctx, h).status)
	}
	return code
}
