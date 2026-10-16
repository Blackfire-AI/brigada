package main

import (
	"context"
	"fmt"
	"io"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// runHold is the hold command's dispatch (§4.6): `hold release`, the
// user's forced release of a stuck interactive hold.
func runHold(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usagef(stderr, "usage: brigada hold release --repo URL")
	}
	switch args[0] {
	case "release":
		return runHoldRelease(ctx, args[1:], stdout, stderr)
	default:
		return usagef(stderr, "unknown hold subcommand %q; want release", args[0])
	}
}

// runHoldRelease is `brigada hold release --repo URL` (§4.6): the user's
// own session lost its way and still holds the foreman's slot, so the
// user releases it. Client authority, no hold credential; the engine
// revokes the credential and bumps the fence before it re-evaluates the
// wake, so the stuck session's next call meets stale_hold.
func runHoldRelease(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hold release")
	common := addCommon(fs)
	repo := fs.String("repo", "", "the project's repo URL")
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "hold release takes no positional arguments, got %q", extra)
	}
	if *repo == "" {
		return usagef(stderr, "hold release needs --repo URL")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", false, stderr)
	if code != 0 {
		return code
	}
	ans, err := client.ReleaseHold(ctx, *repo)
	if err != nil {
		if isNotFound(err) {
			if hint := knownProjectsHint(ctx, client); hint != "" {
				err = fmt.Errorf("%w; %s", err, hint)
			}
		}
		return fail(stderr, err)
	}
	if ans == engine.AnsGone {
		fmt.Fprintf(stdout, "no interactive hold on %s\n", *repo)
		return 0
	}
	fmt.Fprintf(stdout, "released the interactive hold on %s: its credential is revoked and pending events are re-evaluated for a wake\n", *repo)
	return 0
}
