package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
	"github.com/Blackfire-AI/brigada/internal/logo"
)

// Tests can exercise the real authenticated API handler without a listener.
var connectClient = connect

// runStatus is the client's status (§13, §10): the engine — which
// process, up how long — the jobs by status, then the plans with their
// tasks and the "needs you" queue, plain text, one line each, the
// sections rendered by api.StatusSections like the foreman's MCP tool.
// The plans list keeps the newest finished plans only; --all lists
// every finished plan, the MCP tool's all=true.
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status",
		"brigada status [--line] [--all] [--no-start]",
		"Show the engine, jobs, plans and what needs your attention.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	line := fs.Bool("line", false, "print one short line for a terminal status bar; never starts the engine")
	all := fs.Bool("all", false, "list every finished plan, not only the three most recent")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "status takes no arguments, got %q", extra)
	}
	if *line {
		return statusLine(ctx, common, stdout)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connectClient(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	var st api.Status
	var err error
	if *all {
		st, err = client.StatusAll(ctx, false)
	} else {
		st, err = client.Status(ctx)
	}
	if err != nil {
		return fail(stderr, err)
	}
	uptime := "0s"
	if started := parseStartedAt(st.StartedAt); !started.IsZero() {
		uptime = formatUptime(time.Since(started))
	}
	fmt.Fprintf(stdout, "engine pid %d up %s\n", st.Pid, uptime)
	counts := make([]string, 0, len(jobStatusOrder))
	for _, s := range jobStatusOrder {
		counts = append(counts, fmt.Sprintf("%s %d", s, st.Jobs[s]))
	}
	fmt.Fprintf(stdout, "jobs: %s\n", strings.Join(counts, " "))
	fmt.Fprintln(stdout, api.StatusSections(st))
	if hint := agentsHint(ctx, rt, true); hint != "" {
		fmt.Fprintln(stdout, hint)
	}
	reportProjectConfigStatus(ctx, client, stdout)
	return 0
}

// statusLine is `brigada status --line` (§10): the status as one short
// line, for a terminal status bar that runs it every few seconds — the
// attached foreman's, above all. A status bar must never start an
// engine nor print an error page, so it never starts one, and anything
// that keeps it from reading the status becomes a one-line answer on
// stdout with exit 0: the bar shows it, and the next tick tries again.
// Every line starts with the one-line logo.
func statusLine(ctx context.Context, common *commonFlags, stdout io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	say := func(line string) int {
		fmt.Fprintln(stdout, withLogo(stdout, line))
		return 0
	}
	rt, code := resolveRuntime(common, io.Discard)
	if code != 0 {
		return say("brigada: no state directory")
	}
	client, code := connect(ctx, rt, "client", true, io.Discard)
	if code != 0 {
		return say("brigada: engine not running")
	}
	st, err := client.Status(ctx)
	if err != nil {
		return say("brigada: status unavailable")
	}
	return say(api.StatusLine(st, time.Now()))
}

// withLogo turns the leading "brigada" of a status line into the one-line
// logo, "▙ brigada", when the locale can print it. A status bar reads the
// line from a pipe, so the ▙ is never colored.
func withLogo(stdout io.Writer, line string) string {
	caps := logo.Caps{UTF8: detectCaps(stdout).UTF8}
	mark := strings.TrimSuffix(logo.String(logo.OneLine, caps), "\n")
	rest, ok := strings.CutPrefix(line, "brigada")
	if mark == "" || !ok {
		return line
	}
	return mark + rest
}

// runTask is the task command's dispatch: `task submit`, the debugging
// path that submits a task without the foreman (§12.1), and `task
// retire`, the user's settle of tasks that will not land (§6.2).
func runTask(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "task", "usage: brigada task submit --repo URL --spec FILE|- [--base REF] [--check CMD] [--title TITLE] [--harness HARNESS] | brigada task retire --reason TEXT <task-id>...", "submit or retire", taskCommands)
}

// taskRetirer is what task retire asks of the API client; tests stub it.
type taskRetirer interface {
	RetireTasks(ctx context.Context, tasks []int64, reason, idemKey string) (engine.RetireResult, error)
}

// runTaskRetire is `brigada task retire [--reason TEXT] [--no-start]
// <task-id>...` (§6.2): the user settles tasks that will not land —
// their live jobs stop, their land holds drop, their branches stay
// untouched, and they never land by any path — with the reason recorded
// verbatim. Their plans close once nothing else holds them open.
func runTaskRetire(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("task retire",
		"brigada task retire --reason TEXT [--no-start] <task-id>...",
		"Retire tasks that will not land; their branches stay and their plans close.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	reason := fs.String("reason", "", "why the tasks are retired, recorded verbatim with each")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if strings.TrimSpace(*reason) == "" {
		return usagef(stderr, "task retire needs --reason TEXT")
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usagef(stderr, "usage: brigada task retire --reason TEXT <task-id>...")
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, s := range rest {
		id, ok := parseID(stderr, "task id", s)
		if !ok {
			return 2
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	return printTaskRetire(ctx, client, ids, *reason, stdout, stderr)
}

// printTaskRetire retires through the client and says what it did: each
// retired task, and the state each touched plan reached.
func printTaskRetire(ctx context.Context, client taskRetirer, ids []int64, reason string, stdout, stderr io.Writer) int {
	res, err := client.RetireTasks(ctx, ids, reason, "")
	if err != nil {
		return fail(stderr, err)
	}
	for _, id := range res.Retired {
		fmt.Fprintf(stdout, "task %d retired: %s\n", id, reason)
	}
	for _, p := range res.Plans {
		fmt.Fprintf(stdout, "plan %d is %s\n", p.PlanID, p.State)
	}
	return 0
}

// runTaskSubmit is the debug submit (§12.1): one task created whole —
// repo, base ref, spec, check — with its first work job queued, the
// worker prompt composed by the engine from the versioned template plus
// the spec verbatim (§1.4, §5.3).
func runTaskSubmit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("task submit",
		"brigada task submit --repo URL --spec FILE|- [--base REF] [--check CMD] [--title TITLE] [--harness HARNESS] [--no-start]",
		"Submit one task directly, without the foreman.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		repo    string
		base    string
		spec    string
		check   string
		title   string
		harness string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL the task works in")
	fs.StringVar(&base, "base", "main", "the ref the worktree starts from")
	fs.StringVar(&spec, "spec", "", "the task spec file, passed to the worker verbatim; - reads stdin")
	fs.StringVar(&check, "check", "", "the check command the runner runs after the worker reports done; empty = none")
	fs.StringVar(&title, "title", "", "the task's title; default is the spec's first line")
	fs.StringVar(&harness, "harness", "", "the harness that must run the job (default: any)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "task submit takes no positional arguments, got %q", extra)
	}
	if repo == "" {
		return usagef(stderr, "task submit needs --repo URL")
	}
	if spec == "" {
		return usagef(stderr, "task submit needs --spec FILE (or - for stdin)")
	}
	specBody, err := readSpec(spec)
	if err != nil {
		return fail(stderr, err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "admin", noStart, stderr)
	if code != 0 {
		return code
	}
	res, err := client.SubmitTask(ctx, api.TaskSubmit{
		RepoURL:  repo,
		BaseRef:  base,
		Title:    title,
		Spec:     specBody,
		CheckCmd: check,
		Harness:  harness,
	})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "task %d job %d: %s (run %d)\n", res.TaskID, res.JobID, res.Answer, res.Run)
	return 0
}

// readSpec reads the task spec: a file, or stdin when the path is -.
func readSpec(path string) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read the spec from stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the spec %s: %w", path, err)
	}
	return string(data), nil
}

// runHistory is the §9 timeline as plain text: events and messages
// merged, oldest first, every page followed to the end.
func runHistory(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("history",
		"brigada history [--task ID] [--job ID] [--limit N] [--no-start]",
		"Show the timeline, optionally filtered by task or job.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		task    int64
		job     int64
		limit   int
		noStart bool
	)
	fs.Int64Var(&task, "task", 0, "show one task's timeline")
	fs.Int64Var(&job, "job", 0, "show one job's timeline")
	fs.IntVar(&limit, "limit", 100, "page size, 1..500")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "history takes no positional arguments, got %q", extra)
	}
	if task < 0 || job < 0 {
		return usagef(stderr, "--task and --job must be non-negative ids")
	}
	if limit < 1 || limit > 500 {
		return usagef(stderr, "--limit must be between 1 and 500")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connectClient(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	since := ""
	for {
		page, err := client.History(ctx, api.HistoryQuery{TaskID: task, JobID: job, Since: since, Limit: limit})
		if err != nil {
			return fail(stderr, err)
		}
		if err := history.Render(stdout, page.Items); err != nil {
			return fail(stderr, err)
		}
		if page.NextCursor == "" {
			return 0
		}
		since = page.NextCursor
	}
}

// runCancel stops a live job (§12.1).
func runCancel(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("cancel",
		"brigada cancel [--no-start] <job-id>",
		"Cancel a live job.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usagef(stderr, "usage: brigada cancel <job-id>")
	}
	id, ok := parseID(stderr, "job id", rest[0])
	if !ok {
		return 2
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	ans, err := client.CancelJob(ctx, id)
	if err != nil {
		return fail(stderr, err)
	}
	switch ans {
	case engine.AnsOK:
		fmt.Fprintf(stdout, "job %d: cancelled\n", id)
		return 0
	case engine.AnsAlready:
		fmt.Fprintf(stdout, "job %d: already cancelled\n", id)
		return 0
	case engine.AnsGone:
		fmt.Fprintf(stderr, "brigada: job %d: no such job\n", id)
		return 1
	default:
		fmt.Fprintf(stdout, "job %d: %s\n", id, ans)
		return 0
	}
}
