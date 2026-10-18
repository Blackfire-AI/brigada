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
	                   and chat.
	brigada <command> [arguments]

Commands:

	init            Set up this repository, or, in a folder that is not
	                one, the repositories under it: probe, plan, confirm.
	remove          Remove brigada from this repository; keep its history.
	attach          Attach to this repo's foreman and chat (what a bare
	                brigada runs).
	ask <message>   Send the foreman a message without attaching.
	hold release    Release a stuck interactive hold on the foreman (--repo).
	hold yield      Stop hook: yield only the bound bridge hold; always exits 0.
	hold confirm    Kept for old SessionStart hooks; does nothing, exits 0.
	review --branch <x>
	                Review a branch with an agent of another family than
	                its author's, with no plan or task.
	engine          Run the engine: store and API on the Unix socket.
	runner          Run a runner: lease jobs, run workers, run checks.
	start           Start the engine and detached runner with saved agents.
	config          Show every setting, where it lives and how to change it.
	stop            Stop the detached runner (--engine: the engine too).
	mcp             Serve brigada's tools over stdio: the foreman's
	                (--foreman), the coordinator's (--coordinator) or the
	                client's (--client); Claude Code runs it from the
	                wiring init writes.
	status          Show the engine, its jobs by status, plans and what
	                needs you (--line: one short line).
	task submit     Submit one task directly, without the foreman (a
	                debugging path).
	project foreman Show or set a repo's foreman harness, model and effort
	                (--repo defaults to this repo's origin).
	project list    List the projects: repo, foreman, active plans, needs-you.
	project set     Set project settings: key=value ... (approval limit,
	                default_check, setup, extra_repos, hold bounds,
	                instructions and the rest).
	project show    Show a project's settings and where each comes from
	                (default, set locally, or imported).
	project import  Import this repo's .brigada/project.yaml; --yes
	                accepts replacing hand-set values on a first import.
	history         Show the timeline.
	diff <task-id>  Show what a task changed: git diff base...branch, run
	                inside the repo (--stat, --repo URL).
	land <task-id>...
	                Squash approved tasks onto this branch, one commit each,
	                rerun the default check, print the push command; never
	                pushes (--onto, --repo, --landed, --no-check).
	logs <job-id>   Show what a job's worker did: the transcript as
	                readable lines (--check, --tail N, --follow, --task N).
	stats           Show attempts, successes, rate and confidence per
	                agent, class and repo.
	agent reset <harness>/<model>
	                Clear a broken or cooling agent.
	plan approve <id>
	                Approve a plan waiting over the approval limit.
	plan reject <id>
	                Reject such a plan and cancel its tasks.
	budget [--repo URL] [--wakes N]
	                Show the project's foreman wake budget: today's use
	                and the reset time; --wakes sets it.
	cancel <job-id> Cancel a live job.
	recover         Rebuild a lost store from the newest snapshot and the
	                plan files; the engine must be stopped.
	version         Print the brigada version and exit.
	help [command [subcommand]]
	                Print top-level or command usage and exit.

Common flags, after the command name and before its arguments:

	--state-dir DIR   State directory (default: $XDG_STATE_HOME/brigada,
	                  else ~/.local/state/brigada).
	--config PATH     Config file (default: $XDG_CONFIG_HOME/brigada/config,
	                  else ~/.config/brigada/config).

Exit codes: 0 ok, 1 error, 2 usage.

Use brigada <command> --help for its flags and arguments, or
brigada <command> <subcommand> --help for a subcommand.
`

// command is one subcommand: its flags, parsed against args, and its
// body. Commands print their own usage errors (exit 2) and their own
// failures (exit 1); run only draws the map.
type command struct {
	name        string
	description string
	subcommands map[string]command
	// run parses args and executes. The ctx ends on SIGINT/SIGTERM.
	run func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

var commands = map[string]command{
	"init":    {name: "init", run: runInit},
	"remove":  {name: "remove", run: runRemove},
	"attach":  {name: "attach", run: runAttach},
	"ask":     {name: "ask", run: runAsk},
	"hold":    {name: "hold", run: runHold, subcommands: holdCommands},
	"review":  {name: "review", run: runReview},
	"engine":  {name: "engine", run: runEngine},
	"runner":  {name: "runner", run: runRunner},
	"start":   {name: "start", run: runStart},
	"config":  {name: "config", run: runConfig},
	"stop":    {name: "stop", run: runStop},
	"mcp":     {name: "mcp", run: runMCP},
	"status":  {name: "status", run: runStatus},
	"task":    {name: "task", run: runTask, subcommands: taskCommands},
	"project": {name: "project", run: runProject, subcommands: projectCommands},
	"history": {name: "history", run: runHistory},
	"stats":   {name: "stats", run: runStats},
	"cancel":  {name: "cancel", run: runCancel},
	"agent":   {name: "agent", run: runAgent, subcommands: agentCommands},
	"plan":    {name: "plan", run: runPlan, subcommands: planCommands},
	"budget":  {name: "budget", run: runBudget},
	"recover": {name: "recover", run: runRecover},
	"diff":    {name: "diff", run: runDiff},
	"land":    {name: "land", run: runLand},
	"logs":    {name: "logs", run: runLogs},
	"version": {name: "version", run: runVersion},
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
	case "help":
		if len(args) > 1 {
			return run(ctx, append(append([]string{}, args[1:]...), "--help"), stdout, stderr)
		}
		fmt.Fprint(stdout, usage)
		return 0
	case "-h", "--help", "-help":
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

func runVersion(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("version", "brigada version", "Print the brigada version and exit.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "version takes no positional arguments")
	}
	fmt.Fprintf(stdout, "brigada %s\n", version.Version)
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// usagef prints a usage error and returns exit code 2.
func usagef(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "brigada: "+format+"\n", args...)
	name := ""
	if w, ok := stderr.(commandErrorWriter); ok {
		name = " " + w.name
	}
	fmt.Fprintf(stderr, "Run 'brigada%s --help' for usage.\n", name)
	return 2
}

// commandErrorWriter carries the command name to usagef, including errors
// reported by shared helpers such as parseID.
type commandErrorWriter struct {
	io.Writer
	name string
}

func commandStderr(stderr io.Writer, name string) io.Writer {
	return commandErrorWriter{Writer: stderr, name: name}
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
