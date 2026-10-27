package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// taskReader is the one thing diff asks the engine: which refs. The
// engine runs no git for a client (product §5 rule 1).
type taskReader interface {
	Task(ctx context.Context, id int64) (engine.TaskInfo, error)
}

// runDiff is `brigada diff <task-id> [--stat] [--repo URL]`: the task's
// change as git shows it, base...branch, on stdout so it pipes to a
// pager. It runs inside the project's repository, fetches the two refs
// from origin (refs only, no checkout) and diffs them locally.
func runDiff(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("diff",
		"brigada diff [--stat] [--repo URL] [--no-start] <task-id>",
		"Show a task’s changes as git diff base...branch inside its repository.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	stat := fs.Bool("stat", false, "print the diffstat instead of the whole diff")
	repo := fs.String("repo", "", "the repository URL the working directory stands for (default: its origin's URL)")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	rest, code, stop := parseWithPositional(common, stdout, stderr, args)
	if stop {
		return code
	}
	if len(rest) != 1 {
		return usagef(stderr, "usage: brigada diff <task-id> [--stat] [--repo URL]")
	}
	id, ok := parseID(stderr, "task id", rest[0])
	if !ok {
		return 2
	}
	dir, err := os.Getwd()
	if err != nil {
		return fail(stderr, fmt.Errorf("find the working directory: %w", err))
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	return printDiff(ctx, client, dir, id, *repo, *stat, stdout, stderr)
}

// parseWithPositional parses args that carry one leading positional
// (`diff 7 --stat`), which the flag package would otherwise stop at: the
// flags before it and the flags after it both count. It returns what is
// left over once every flag is taken.
func parseWithPositional(c *commonFlags, stdout, stderr io.Writer, args []string) ([]string, int, bool) {
	if code, stop := c.parse(stdout, stderr, args); stop {
		return nil, code, true
	}
	rest := c.fs.Args()
	if len(rest) == 0 {
		return nil, 0, false
	}
	first := rest[0]
	if code, stop := c.parse(stdout, stderr, rest[1:]); stop {
		return nil, code, true
	}
	return append([]string{first}, c.fs.Args()...), 0, false
}

// printDiff reads the task's refs, checks the working directory is the
// task's repository, fetches the two refs and prints the diff.
func printDiff(ctx context.Context, client taskReader, dir string, id int64, repo string, stat bool, stdout, stderr io.Writer) int {
	task, err := client.Task(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return fail(stderr, fmt.Errorf("task %d: no such task", id))
		}
		return fail(stderr, err)
	}
	if task.Branch == "" {
		return fail(stderr, fmt.Errorf("task %d has no result branch yet (state %s); nothing to diff", id, task.State))
	}
	here := repo
	if here == "" {
		origin, err := gitOriginURL(dir)
		if err != nil {
			return fail(stderr, err)
		}
		here = origin
	}
	// Use the engine's project identity rule, just as attach and ask do.
	// Invalid URLs must be refused even when their raw spellings match.
	here, err = engine.Canonicalize(here)
	if err != nil {
		return fail(stderr, fmt.Errorf("canonicalize this directory's repository URL: %w", err))
	}
	taskRepo, err := engine.Canonicalize(task.RepoURL)
	if err != nil {
		return fail(stderr, fmt.Errorf("canonicalize task %d's repository URL: %w", id, err))
	}
	if here != taskRepo {
		return fail(stderr, fmt.Errorf("task %d belongs to %s, but this directory is %s; run diff inside that repository", id, taskRepo, here))
	}
	// Private refs under refs/brigada/diff: a base that is a tag or a
	// branch, and a result branch, land somewhere the diff can name
	// without touching the user's own branches or checkout.
	baseRef := fmt.Sprintf("refs/brigada/diff/%d/base", id)
	branchRef := fmt.Sprintf("refs/brigada/diff/%d/branch", id)
	remote, err := projectRemote(ctx, dir, here)
	if err != nil {
		return fail(stderr, err)
	}
	if err := runGit(ctx, dir, nil, stderr, "fetch", "--quiet", remote,
		"+"+task.BaseRef+":"+baseRef, "+"+task.Branch+":"+branchRef); err != nil {
		return fail(stderr, fmt.Errorf("fetch %s and %s from %s: %w", task.BaseRef, task.Branch, remote, err))
	}
	gitArgs := []string{"--no-pager", "diff"}
	if stat {
		gitArgs = append(gitArgs, "--stat")
	}
	gitArgs = append(gitArgs, baseRef+"..."+branchRef)
	if err := runGit(ctx, dir, stdout, stderr, gitArgs...); err != nil {
		return fail(stderr, fmt.Errorf("diff %s...%s: %w", task.BaseRef, task.Branch, err))
	}
	return 0
}

// runGit runs git in dir with the given output sinks; a nil stdout
// discards it. A failure carries git's own words when stderr is not a
// sink the caller already shows.
func runGit(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if stdout != nil {
		cmd.Stdout = stdout
	}
	err := cmd.Run()
	if err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	if errb.Len() > 0 {
		stderr.Write(errb.Bytes())
	}
	return nil
}
