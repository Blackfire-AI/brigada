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
	"strings"
	"syscall"

	"github.com/Blackfire-AI/brigada/internal/version"
)

const usage = `brigada is the single binary of the brigada engine: client,
engine, runner and foreman are all this binary in different roles.

Usage:

	brigada            With no subcommand, attach to this repo's foreman
	                   and chat (§4.2).
	brigada <command> [arguments]

Commands:

	init            Probe and confirm setup of this repository (phase 6(a)).
	attach          Attach to this repo's foreman and chat (what a bare
	                brigada runs).
	ask <message>   Send the foreman a message without attaching.
	hold release    Release a stuck interactive hold on the foreman (--repo).
	hold yield      Stop hook: yield only the bound bridge hold; always exits 0.
	review --branch <x>
	                Review a branch with an agent of another family than
	                its author's, with no plan or task (§7).
	engine          Run the engine: store and API on the Unix socket.
	runner          Run a runner: lease jobs, run workers, run checks.
	mcp             Run the foreman's MCP bridge over stdio (§12.2).
	status          Show the engine and its jobs by status.
	task submit     Submit a task (debug path until the foreman exists).
	project foreman Show or set a repo's foreman harness and model
	                (--repo defaults to this repo's origin).
	project list    List the projects: repo, foreman, active plans, needs-you.
	project set     Set a project's approval limit: key=value ... (§6.3).
	project show    Show a project's settings and where each comes from
	                (default, set locally, or imported).
	project import  Import this repo's .brigada/project.yaml (§13.1); --yes
	                accepts replacing hand-set values on a first import.
	history         Show the timeline.
	diff <task-id>  Show what a task changed: git diff base...branch, run
	                inside the repo (--stat, --repo URL).
	land <task-id>...
	                Squash approved tasks onto this branch, one commit each,
	                rerun the default check, print the push command; never
	                pushes (--onto, --repo, --landed, --no-check; §7.1).
	logs <job-id>   Show what a job's worker did: the transcript as
	                readable lines (--check, --tail N, --follow, --task N).
	stats           Show which agent wins which class, per repo.
	agent reset <harness>/<model>
	                Clear a broken or cooling agent.
	plan approve <id>
	                Approve a plan waiting over the approval limit (§6.3).
	plan reject <id>
	                Reject such a plan and cancel its tasks.
	budget [--repo URL] [--wakes N]
	                Show the project's foreman wake budget: today's use
	                and the reset time; --wakes sets it (§4.3).
	cancel <job>    Cancel a job.
	recover         Rebuild a lost store from the newest snapshot and the
	                plan files; the engine must be stopped (§6.4).
	version         Print the brigada version and exit.
	help            Print usage and exit.

Common flags, after the command name and before its arguments:

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
	"init":    {name: "init", run: runInit},
	"attach":  {name: "attach", run: runAttach},
	"ask":     {name: "ask", run: runAsk},
	"hold":    {name: "hold", run: runHold},
	"review":  {name: "review", run: runReview},
	"engine":  {name: "engine", run: runEngine},
	"runner":  {name: "runner", run: runRunner},
	"mcp":     {name: "mcp", run: runMCP},
	"status":  {name: "status", run: runStatus},
	"task":    {name: "task", run: runTask},
	"project": {name: "project", run: runProject},
	"history": {name: "history", run: runHistory},
	"stats":   {name: "stats", run: runStats},
	"cancel":  {name: "cancel", run: runCancel},
	"agent":   {name: "agent", run: runAgent},
	"plan":    {name: "plan", run: runPlan},
	"budget":  {name: "budget", run: runBudget},
	"recover": {name: "recover", run: runRecover},
	"diff":    {name: "diff", run: runDiff},
	"land":    {name: "land", run: runLand},
	"logs":    {name: "logs", run: runLogs},
}

// run is the entry point of the brigada command. It writes to stdout and
// stderr and returns the process exit code, so it can be tested without
// spawning a process.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		// A bare brigada is the chat (§4.2 item 1): the user's way in.
		return runAttach(ctx, nil, stdout, stderr)
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
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprintln(stderr, "brigada: flags go after the command, for example brigada status --state-dir DIR")
			return 2
		}
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
