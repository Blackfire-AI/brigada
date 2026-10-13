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
)

// runStatus is the client's status (§13, §10): the engine — which
// process, up how long — the jobs by status, then the plans with their
// tasks and the "needs you" queue, plain text, one line each, the
// sections rendered by api.StatusSections like the foreman's MCP tool.
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status")
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "status takes no arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	st, err := client.Status(ctx)
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
	return 0
}

// runTask is the task command's dispatch: `task submit`, the debug
// submit path until the foreman exists (§12.1).
func runTask(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usagef(stderr, "usage: brigada task submit --repo URL --base REF --spec FILE|- --check CMD [--harness claude]")
	}
	switch args[0] {
	case "submit":
		return runTaskSubmit(ctx, args[1:], stdout, stderr)
	default:
		return usagef(stderr, "unknown task subcommand %q; want submit", args[0])
	}
}

// runTaskSubmit is the debug submit (§12.1): one task created whole —
// repo, base ref, spec, check — with its first work job queued, the
// worker prompt composed by the engine from the versioned template plus
// the spec verbatim (§1.4, §5.3).
func runTaskSubmit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("task submit")
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
	fs.StringVar(&repo, "repo", "", "the repository URL the task works in (product §5 rule 6)")
	fs.StringVar(&base, "base", "main", "the ref the worktree starts from")
	fs.StringVar(&spec, "spec", "", "the task spec file, verbatim (§1.4); - reads stdin")
	fs.StringVar(&check, "check", "", "the check command the runner runs after a done (§5.5); empty = none")
	fs.StringVar(&title, "title", "", "the task's title; default is the spec's first line")
	fs.StringVar(&harness, "harness", "", "the harness that must run the job (default: any)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code := common.parse(stderr, args); code != 0 {
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
	fs := newFlagSet("history")
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
	if code := common.parse(stderr, args); code != 0 {
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
	client, code := connect(ctx, rt, "client", noStart, stderr)
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
	fs := newFlagSet("cancel")
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code := common.parse(stderr, args); code != 0 {
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
