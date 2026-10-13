package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

// runEngine is the engine role (§13): the store, the rules and the API
// on the Unix socket, one instance per state directory, until it is
// stopped, it fails, or it has been idle long enough. Meant to run in
// the background — a client that needs it starts it detached (§13).
func runEngine(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("engine")
	common := addCommon(fs)
	var idleMinutes float64
	fs.Float64Var(&idleMinutes, "idle-minutes", 0,
		"exit after this many minutes with no live jobs and no connected requests (default 30; config file's idle_minutes in between)")
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	if args := fs.Args(); len(args) > 0 {
		return usagef(stderr, "engine takes no arguments, got %q", args)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	idle := time.Duration(0)
	switch {
	case idleMinutes > 0:
		idle = time.Duration(idleMinutes * float64(time.Minute))
	case rt.file.IdleMinutes > 0:
		idle = time.Duration(rt.file.IdleMinutes * float64(time.Minute))
	}
	err := lifecycle.Run(ctx, lifecycle.Config{
		StateDir: rt.stateDir,
		Idle:     idle,
		Caps:     rt.file.Caps,
		// The §10 notifications arrive in a later phase; the hook is the
		// engine's promise to flush them before an idle exit.
		FlushNotifications: nil,
	})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "engine stopped")
	return 0
}
