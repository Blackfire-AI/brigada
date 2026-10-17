package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/mcp"
)

// runHold is the hold command's dispatch (§4.6): `hold release`, the
// user's forced release of a stuck interactive hold.
func runHold(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "hold", "usage: brigada hold release --repo URL", "confirm, release or yield", holdCommands)
}

// runHoldConfirm is init's SessionStart hook (§4.6 "Identity"). The session
// id is correlation only, never authority, and the engine has no API to
// receive it yet: the bridge already sends the id it reads from its own
// environment at hold_acquire. So this accepts the hook's input, changes
// nothing and always exits zero, so the hook is installed and safe until
// that API exists. Every failure is swallowed as in hold yield.
func runHoldConfirm(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hold confirm",
		"brigada hold confirm [--session ID]",
		"Accept SessionStart hook input for correlation; always exit successfully.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	fs.String("session", os.Getenv("CLAUDE_CODE_SESSION_ID"), "harness session id (correlation only)")
	var diagnostics bytes.Buffer
	if code, stop := common.parse(stdout, &diagnostics, args); stop && code != 0 {
		fmt.Fprintf(stderr, "brigada hold confirm: invalid hook arguments\n")
	}
	return 0
}

// runHoldRelease is `brigada hold release --repo URL` (§4.6): the user's
// own session lost its way and still holds the foreman's slot, so the
// user releases it. Client authority, no hold credential; the engine
// revokes the credential and bumps the fence before it re-evaluates the
// wake, so the stuck session's next call meets stale_hold.
func runHoldRelease(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hold release",
		"brigada hold release --repo URL",
		"Release a stuck interactive hold on the foreman.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	repo := fs.String("repo", "", "the project's repo URL")
	if code, stop := common.parse(stdout, stderr, args); stop {
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

// runHoldYield is safe in a harness Stop hook: every failure, including flags
// and local state resolution, exits zero and leaves a best-effort local log.
func runHoldYield(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hold yield",
		"brigada hold yield [--session ID]",
		"Yield only the bound bridge hold from a Stop hook; always exit successfully.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	session := fs.String("session", os.Getenv("CLAUDE_CODE_SESSION_ID"), "harness session id (correlation only)")
	var diagnostics bytes.Buffer
	report := func(err error) int {
		state := common.stateDir
		if state == "" {
			path := common.config
			if path == "" {
				path = lifecycle.DefaultConfigPath()
			}
			if config, err := lifecycle.LoadConfigFile(path); err == nil {
				state = config.StateDir
			}
		}
		if state == "" {
			state = lifecycle.DefaultStateDir()
		}
		if diagnostics.Len() > 0 {
			err = fmt.Errorf("%w: %s", err, diagnostics.String())
		}
		mcp.LogHookFailure(state, err)
		fmt.Fprintf(stderr, "brigada hold yield: %v\n", err)
		return 0
	}
	if code, stop := common.parse(stdout, &diagnostics, args); stop {
		if code != 0 {
			return report(errors.New("invalid hook arguments"))
		}
		return 0
	}
	if len(fs.Args()) > 0 {
		return report(errors.New("hold yield takes no positional arguments"))
	}
	rt, code := resolveRuntime(common, &diagnostics)
	if code != 0 {
		return report(errors.New("resolve hook state directory"))
	}
	common.stateDir = rt.stateDir
	hookCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := mcp.YieldFromHook(hookCtx, rt.stateDir, *session); err != nil {
		return report(err)
	}
	return 0
}
