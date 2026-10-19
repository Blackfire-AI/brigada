package planfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The identity the mirror commits under: stable, so the plans branch
// reads as one author's log, and not anyone's own.
const (
	authorName  = "brigada"
	authorEmail = "brigada@brigada.invalid"
)

// Target is where a plan file goes: a branch of the repo's remote.
type Target struct {
	URL    string // the remote, as the project names it (product §5 rule 6)
	Branch string
	// Default marks Branch as the repo's default branch (§6.4's option):
	// the file is committed there, and a remote without it is an error —
	// the mirror never creates the default branch. Any other branch is
	// created, as an orphan, when the remote lacks it.
	Default bool
}

// ErrNoDefaultBranch is the push that committed to the default branch of a
// remote that has none yet.
var ErrNoDefaultBranch = errors.New("the remote has no default branch yet")

// Push puts the plan's file on the target branch: it fetches the branch
// into a scratch repository (the engine's own environment, §5: no
// assumption about the host beyond git and the remote's credentials),
// writes the file, commits it with a stable identity and a message naming
// the plan and what changed, and pushes. A file already identical on the
// branch is not committed again. Any failure is the caller's to log: the
// mirror is best-effort, and the next change pushes again.
//
// The scratch repository names the project URL as its remote "origin" and
// talks to that name, never to the bare URL: git config scoped by remote URL
// (includeIf "hasconfig:remote.*.url:...") and the hooks, credential helpers
// and other settings set through it then apply here as they do in a clone.
func Push(ctx context.Context, t Target, p Plan) error {
	data, err := Render(p)
	if err != nil {
		return err
	}
	if t.URL == "" {
		return errors.New("no remote URL to push the plan file to")
	}
	if err := checkBranch(ctx, t.Branch); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "brigada-planfile-")
	if err != nil {
		return fmt.Errorf("scratch repository: %w", err)
	}
	defer os.RemoveAll(dir)
	g := func(args ...string) (string, error) { return git(ctx, dir, args...) }

	if _, err := g("init", "-q"); err != nil {
		return err
	}
	if _, err := g("remote", "add", "origin", "--", t.URL); err != nil {
		return err
	}
	heads, err := g("ls-remote", "--heads", "origin", "refs/heads/"+t.Branch)
	if err != nil {
		return err
	}
	var old *Plan
	switch {
	case strings.TrimSpace(heads) != "":
		if _, err := g("fetch", "-q", "--depth=1", "origin", "+refs/heads/"+t.Branch+":refs/remotes/mirror/"+t.Branch); err != nil {
			return err
		}
		if _, err := g("checkout", "-q", "-B", t.Branch, "refs/remotes/mirror/"+t.Branch); err != nil {
			return err
		}
		if prev, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(Path(p.ID)))); err == nil {
			if bytes.Equal(prev, data) {
				return nil // the mirror is already current
			}
			if parsed, err := Parse(prev); err == nil {
				old = &parsed
			}
		}
	case t.Default:
		return fmt.Errorf("%w: branch %s of %s", ErrNoDefaultBranch, t.Branch, t.URL)
	default:
		if _, err := g("checkout", "-q", "--orphan", t.Branch); err != nil {
			return err
		}
	}

	file := filepath.Join(dir, filepath.FromSlash(Path(p.ID)))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return fmt.Errorf("scratch repository: %w", err)
	}
	if err := os.WriteFile(file, data, 0o644); err != nil {
		return fmt.Errorf("scratch repository: %w", err)
	}
	if _, err := g("add", "--", Path(p.ID)); err != nil {
		return err
	}
	msg := fmt.Sprintf("plan %d: %s", p.ID, Describe(old, p))
	if _, err := g("commit", "-q", "-m", msg); err != nil {
		return err
	}
	_, err = g("push", "-q", "origin", "HEAD:refs/heads/"+t.Branch)
	return err
}

// checkBranch refuses a branch name git would not take, so a bad setting
// reads as one clear error.
func checkBranch(ctx context.Context, branch string) error {
	if branch == "" {
		return errors.New("no branch to push the plan file to")
	}
	if _, err := git(ctx, "", "check-ref-format", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("plan file branch %q: not a valid branch name", branch)
	}
	return nil
}

// git runs one git command in dir with prompts off — a remote that wants
// credentials the engine lacks fails instead of hanging — and returns its
// stdout. The error carries git's own stderr.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{
		"-c", "user.name=" + authorName, "-c", "user.email=" + authorEmail,
		"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true",
		"GIT_AUTHOR_NAME="+authorName, "GIT_AUTHOR_EMAIL="+authorEmail,
		"GIT_COMMITTER_NAME="+authorName, "GIT_COMMITTER_EMAIL="+authorEmail)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
