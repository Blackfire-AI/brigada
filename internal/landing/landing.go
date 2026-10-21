// Package landing is the git side of landing an approved task (§7.1),
// shared by `brigada land` and a runner's land job: the squash commit
// message, the task's own diff base...branch, applying that diff as one
// commit, and the conflicts that stop it. Functions take a repository
// directory and plain values, run git via os/exec, and return typed
// results and errors; none of it knows the user's working tree, the
// engine or the CLI.
package landing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Result is what Squash did.
type Result struct {
	SHA     string // the new commit's short sha; "" when Empty
	Empty   bool   // the patch changed nothing: no commit was made
	MsgPath string // the prepared message file; removed on success, left on a stop
}

// Conflict is a patch that did not apply cleanly three-way (§7.1):
// Files are the unmerged paths, Err is git's own words. The tree is
// left as git left it; nothing is resolved.
type Conflict struct {
	Files []string
	Err   error
}

func (c *Conflict) Error() string {
	if len(c.Files) > 0 {
		return fmt.Sprintf("does not apply cleanly: %s", strings.Join(c.Files, ", "))
	}
	return c.Err.Error()
}

func (c *Conflict) Unwrap() error { return c.Err }

// Diff is the task's own diff (§7.1): `git diff --binary base...branch`
// for its recorded base, so a task whose base was its dependency's
// branch diffs only its own work.
func Diff(ctx context.Context, dir, base, branch string, stderr io.Writer) ([]byte, error) {
	var out bytes.Buffer
	if err := run(ctx, dir, &out, stderr, "--no-pager", "diff", "--binary", base+"..."+branch); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Squash applies patch three-way to dir's index and tree and commits it
// as one commit with msg, on whatever branch is checked out. The
// message is written to a file inside .git and committed with -F, never
// a shell string (§7.1). A patch that changes nothing makes no commit
// and reports Empty. A patch that does not apply cleanly returns a
// *Conflict, names the unmerged files and leaves the message file and
// the tree as they are, for the caller's user to resolve; a commit
// git or a hook rejects returns the error and leaves the message file
// too.
func Squash(ctx context.Context, dir string, id int64, msg string, patch []byte, stderr io.Writer) (Result, error) {
	var res Result
	if len(patch) == 0 {
		res.Empty = true
		return res, nil
	}
	gitDir, err := text(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return res, err
	}
	path := filepath.Join(strings.TrimSpace(gitDir), fmt.Sprintf("BRIGADA_LAND_MSG_%d", id))
	if err := os.WriteFile(path, []byte(msg), 0o600); err != nil {
		return res, fmt.Errorf("write the commit message: %w", err)
	}
	res.MsgPath = path
	if err := apply3way(ctx, dir, patch); err != nil {
		return res, &Conflict{Files: unmergedFiles(ctx, dir), Err: err}
	}
	if succeeds(ctx, dir, "diff", "--cached", "--quiet") {
		os.Remove(path)
		res.Empty = true
		return res, nil
	}
	if err := run(ctx, dir, nil, stderr, "commit", "--quiet", "-F", path); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	sha, _ := text(ctx, dir, "rev-parse", "--short", "HEAD")
	res.SHA = strings.TrimSpace(sha)
	os.Remove(path)
	return res, nil
}

// Landed reports whether a commit in the checked-out branch's history
// carries Brigada-Task: <id> (§7.1): how a landed dependency is
// recognized.
func Landed(ctx context.Context, dir string, id int64) (bool, error) {
	out, err := text(ctx, dir, "log", "-n1", "--format=%H", "-E",
		fmt.Sprintf("--grep=^Brigada-Task: %d$", id), "HEAD")
	if err != nil {
		// An unborn branch has no history to carry a trailer.
		if !succeeds(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD") {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// LandedCommits maps each task id a commit in ref's history carries as a
// Brigada-Task trailer to that commit's full sha (§7.1): how the engine
// recognizes tasks landed outside the land job — a hand squash, or a
// brigada land in the user's clone. It is Landed's match run once for
// every task at once: the same -E grep over Brigada-Task, the value read
// from git's own trailer format, so the two stay one rule. A ref with no
// history maps nothing.
func LandedCommits(ctx context.Context, dir, ref string) (map[int64]string, error) {
	out, err := text(ctx, dir, "log", "--format=%H%x00%(trailers:key=Brigada-Task,valueonly)",
		"-E", "--grep=^Brigada-Task: [0-9]+$", ref)
	if err != nil {
		// An unborn branch has no history to carry a trailer.
		if !succeeds(ctx, dir, "rev-parse", "--verify", "--quiet", ref) {
			return nil, nil
		}
		return nil, err
	}
	landed := map[int64]string{}
	for _, line := range strings.Split(out, "\n") {
		sha, val, ok := strings.Cut(line, "\x00")
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		landed[id] = strings.TrimSpace(sha)
	}
	return landed, nil
}

// apply3way applies a patch three-way to the index and the tree.
func apply3way(ctx context.Context, dir string, patch []byte) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "apply", "--3way", "--index", "-")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = bytes.NewReader(patch)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// unmergedFiles names the unmerged paths; none can be named when git
// itself cannot be asked.
func unmergedFiles(ctx context.Context, dir string) []string {
	out, err := text(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files
}

// text runs git in dir and returns its stdout.
func text(ctx context.Context, dir string, args ...string) (string, error) {
	var out bytes.Buffer
	if err := run(ctx, dir, &out, io.Discard, args...); err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// run runs git in dir with the given output sinks; a nil stdout
// discards it. A failure carries git's own words; on success git's
// stderr is forwarded to stderr when it is not nil.
func run(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
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
	if errb.Len() > 0 && stderr != nil {
		stderr.Write(errb.Bytes())
	}
	return nil
}

// succeeds reports whether git exits 0.
func succeeds(ctx context.Context, dir string, args ...string) bool {
	return exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Run() == nil
}
