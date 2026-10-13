// Command brigada is the one binary of the brigada engine: the client,
// the engine, the runner and the foreman's MCP bridge are all roles of
// this same binary (design §2). Every role shares the state directory —
// the store, the socket, the tokens — so a plain `brigada status` can
// start the engine it asks (§13).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/Blackfire-AI/brigada/internal/version"
)

const usage = `brigada is the single binary of the brigada engine: client,
engine, runner and foreman are all this binary in different roles.

Usage:

	brigada <command> [arguments]

Commands:

	engine          Run the engine: store and API on the Unix socket.
	runner          Run a runner: lease jobs, run workers, run checks.
	mcp             Run the foreman's MCP bridge over stdio (§12.2).
	status          Show the engine and its jobs by status.
	task submit     Submit a task (debug path until the foreman exists).
	history         Show the timeline.
	cancel <job>    Cancel a job.
	version         Print the brigada version and exit.
	help            Print usage and exit.

Common flags, before a command's own:

	--state-dir DIR   State directory (default: $XDG_STATE_HOME/brigada,
	                  else ~/.local/state/brigada).
	--config PATH     Config file (default: $XDG_CONFIG_HOME/brigada/config,
	                  else ~/.config/brigada/config).

Exit codes: 0 ok, 1 error, 2 usage.
`

// command is one subcommand: its flags, parsed against args, and its
// body. Commands print their own usage errors (exit 2) and their own
// failures (exit 1); run only draws the map.
type command struct {
	name string
	// run parses args and executes. The ctx ends on SIGINT/SIGTERM.
	run func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

var commands = map[string]command{
	"engine":  {name: "engine", run: runEngine},
	"runner":  {name: "runner", run: runRunner},
	"mcp":     {name: "mcp", run: runMCP},
	"status":  {name: "status", run: runStatus},
	"task":    {name: "task", run: runTask},
	"history": {name: "history", run: runHistory},
	"cancel":  {name: "cancel", run: runCancel},
}

// run is the entry point of the brigada command. It writes to stdout and
// stderr and returns the process exit code, so it can be tested without
// spawning a process.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "brigada %s\n", version.Version)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "brigada: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	return cmd.run(ctx, args[1:], stdout, stderr)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// usagef prints a usage error and returns exit code 2.
func usagef(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "brigada: "+format+"\n", args...)
	return 2
}

// fail prints an error and returns exit code 1.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "brigada: %v\n", err)
	return 1
}

// jobStatusOrder is the statuses' print order (§13's status): live
// first, then terminal.
var jobStatusOrder = []string{
	"queued", "leased", "running", "done", "failed", "cancelled", "stalled",
}

// parseID reads a job id argument.
func parseID(stderr io.Writer, what, s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		usagef(stderr, "%s must be a positive integer, got %q", what, s)
		return 0, false
	}
	return id, true
}
