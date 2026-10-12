package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// A runner's workspace (§5.1, product rule 6): a per-repo mirror cache
// under the runner's state directory, and one git worktree per task,
// on a branch of its own, built from the job's base ref. The mirror is
// cloned once and fetched afterwards; the worktree is removed after the
// job completes. The runner pushes result branches to the repo's remote
// itself, with its own credentials (config or environment), which never
// reach the worker's argv or prompt.

// taskBranch is the result branch a task's attempt works on (§5.1):
// brigada/task-<id>, with an attempt suffix once a job was re-leased —
// a re-dispatched attempt never reuses the branch name of the one
// before it (§6.4).
func taskBranch(taskID, attempt int64) string {
	if attempt <= 1 {
		return fmt.Sprintf("brigada/task-%d", taskID)
	}
	return fmt.Sprintf("brigada/task-%d-a%d", taskID, attempt)
}

// workspace is one task's materialized checkout.
type workspace struct {
	mirror  string // the repo's cached bare mirror
	dir     string // the task's worktree
	branch  string
	base    string // the commit the worktree was built on
	repoURL string
	gitEnv  []string
}

// prepareWorkspace materializes the workspace for a leased job (§5.1):
// the repo's mirror is cloned or fetched, then the worktree is added on
// its own branch from the base ref. A repo without a URL is refused
// outright (product §5 rule 6: repos are addressed by URL): no clone,
// no worktree, no worker.
func prepareWorkspace(ctx context.Context, cfg *Config, grant *api.JobGrant) (*workspace, error) {
	if grant.RepoURL == "" {
		return nil, fmt.Errorf("task %d: no repo URL; a task must name a repo to work in", grant.TaskID)
	}
	if grant.BaseRef == "" {
		return nil, fmt.Errorf("task %d: no base ref to build the worktree on", grant.TaskID)
	}
	w := &workspace{
		mirror:  mirrorPath(cfg.StateDir, grant.RepoURL),
		dir:     filepath.Join(cfg.StateDir, "worktrees", fmt.Sprintf("job-%d", grant.JobID)),
		branch:  taskBranch(grant.TaskID, grant.Attempt),
		repoURL: grant.RepoURL,
		gitEnv:  cfg.GitEnv,
	}
	if _, err := os.Stat(w.mirror); err == nil {
		// The fetch takes every branch but the runners' own result
		// branches: those sit in this mirror checked out by live
		// worktrees, and git refuses to move a ref under a checkout.
		// The mirror never needs them from the remote — the runner
		// pushes them, and the worktrees create them here — and the
		// negative refspec keeps them out of pruning too.
		if out, err := w.git(ctx, "--git-dir", w.mirror, "fetch", "--prune", "origin",
			"+refs/heads/*:refs/heads/*", "^refs/heads/brigada/*"); err != nil {
			return nil, fmt.Errorf("fetch the mirror of %s: %w", redactURL(grant.RepoURL), trimCmdError(out, err))
		}
		// A follow-up job may build on an earlier attempt's result
		// branch (§5.3: feedback rounds resume the work). The fetch
		// above skips the brigada/ namespace wholesale, and a runner
		// that never ran the earlier attempt — or re-cloned its mirror —
		// would not have the ref. So the base branch is fetched by name
		// when it lives there: this runner holds at most one brigada/
		// ref checked out at a time, its own, and it is not this job's
		// base.
		if strings.HasPrefix(grant.BaseRef, "brigada/") {
			if out, err := w.git(ctx, "--git-dir", w.mirror, "fetch", "origin",
				"+refs/heads/"+grant.BaseRef+":refs/heads/"+grant.BaseRef); err != nil {
				return nil, fmt.Errorf("fetch the base branch %s from %s: %w",
					grant.BaseRef, redactURL(grant.RepoURL), trimCmdError(out, err))
			}
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(w.mirror), 0o755); err != nil {
			return nil, fmt.Errorf("create the mirror directory: %w", err)
		}
		if out, err := w.git(ctx, "clone", "--mirror", grant.RepoURL, w.mirror); err != nil {
			return nil, fmt.Errorf("clone %s: %w", redactURL(grant.RepoURL), trimCmdError(out, err))
		}
	}
	// A fallback identity and no signing, so a fresh worktree can commit
	// whatever the host's global git config says; a signed-in CLI sets
	// its own identity per commit. Mirror mode is unset because it
	// refuses every refspec push — the worker's `git push origin HEAD`
	// (§5.3) and the runner's own both carry refspecs — while the
	// fetches above pass explicit refspecs and never needed it.
	for _, kv := range [][2]string{
		{"user.name", "brigada runner"},
		{"user.email", "runner@brigada.invalid"},
		{"commit.gpgsign", "false"},
		{"remote.origin.mirror", "false"},
	} {
		if out, err := w.git(ctx, "--git-dir", w.mirror, "config", kv[0], kv[1]); err != nil {
			return nil, fmt.Errorf("set %s on the mirror: %w", kv[0], trimCmdError(out, err))
		}
	}
	// A worktree left behind by a crashed run of the same job would make
	// the add below fail on the path; the runner is sequential, so what
	// sits there now is dead.
	w.discardWorktree(ctx)
	if out, err := w.git(ctx, "--git-dir", w.mirror, "worktree", "add", "-B", w.branch, w.dir, grant.BaseRef); err != nil {
		w.discardWorktree(ctx)
		return nil, fmt.Errorf("add the worktree for %s from %s: %w", w.branch, grant.BaseRef, trimCmdError(out, err))
	}
	// The commit the worktree starts from: what holdsWork compares the
	// worker's HEAD against — a branch with no work on it is not a
	// result (§5.6).
	out, err := w.git(ctx, "-C", w.dir, "rev-parse", "HEAD")
	if err != nil {
		w.discardWorktree(ctx)
		return nil, fmt.Errorf("read the worktree's base commit: %w", trimCmdError(out, err))
	}
	w.base = strings.TrimSpace(string(out))
	return w, nil
}

// holdsWork reports whether the worktree holds anything beyond the base
// it was built on: a commit the worker made, or edits it left behind. A
// run the runner stopped early pushes only when it does (§5.6) — the
// work is not lost, but an empty result branch is noise, not a result.
func (w *workspace) holdsWork(ctx context.Context) bool {
	if out, err := w.git(ctx, "-C", w.dir, "status", "--porcelain"); err == nil && strings.TrimSpace(string(out)) != "" {
		return true
	}
	head, err := w.worktreeHead(ctx)
	return err == nil && head != w.base
}

// remove takes the worktree down (§5.1: removed after complete), with
// the mirror left cached for the next task of the repo.
func (w *workspace) remove(ctx context.Context) {
	if w == nil {
		return
	}
	w.discardWorktree(ctx)
}

// discardWorktree removes the worktree and prunes the mirror's records.
// A worktree that is already gone is success here.
func (w *workspace) discardWorktree(ctx context.Context) {
	_, _ = w.git(ctx, "--git-dir", w.mirror, "worktree", "remove", "--force", w.dir)
	_, _ = w.git(ctx, "--git-dir", w.mirror, "worktree", "prune")
	_ = os.RemoveAll(w.dir)
}

// ensurePushed verifies the branch on the remote holds exactly the
// worktree's commit, and makes it so when it does not (§5.4). The
// worker usually pushes its own branch, but a branch that merely exists
// is not enough: an early push followed by more work, or edits the
// worker never committed, would leave the remote holding something
// other than what the runner is about to vouch for. So: leftovers the
// worker left uncommitted are committed, then the remote's SHA is
// compared with the worktree's HEAD, and a mismatch is pushed. A
// ls-remote that still disagrees after the push was no push.
func (w *workspace) ensurePushed(ctx context.Context) error {
	if err := w.commitLeftovers(ctx); err != nil {
		return err
	}
	head, err := w.worktreeHead(ctx)
	if err != nil {
		return err
	}
	if remote, ok := w.remoteBranchSHA(ctx); ok && remote == head {
		return nil // the worker pushed exactly what sits in the worktree
	}
	if out, err := w.git(ctx, "-C", w.dir, "push", w.repoURL, "HEAD:refs/heads/"+w.branch); err != nil {
		return fmt.Errorf("push %s to %s: %w", w.branch, redactURL(w.repoURL), trimCmdError(out, err))
	}
	remote, ok := w.remoteBranchSHA(ctx)
	if !ok || remote != head {
		return fmt.Errorf("branch %s on %s does not hold the worktree's commit after pushing it",
			w.branch, redactURL(w.repoURL))
	}
	return nil
}

// commitLeftovers commits whatever the worker left uncommitted, so a
// done run cannot ship a branch that is missing its own edits. A clean
// tree is left alone.
func (w *workspace) commitLeftovers(ctx context.Context) error {
	out, err := w.git(ctx, "-C", w.dir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("read the worktree's status: %w", trimCmdError(out, err))
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil
	}
	if out, err := w.git(ctx, "-C", w.dir, "add", "-A"); err != nil {
		return fmt.Errorf("stage the worker's uncommitted changes: %w", trimCmdError(out, err))
	}
	if out, err := w.git(ctx, "-C", w.dir, "commit", "-m", "brigada runner: the worker's uncommitted changes"); err != nil {
		return fmt.Errorf("commit the worker's uncommitted changes: %w", trimCmdError(out, err))
	}
	return nil
}

// worktreeHead is the commit the worktree's branch points at.
func (w *workspace) worktreeHead(ctx context.Context) (string, error) {
	out, err := w.git(ctx, "-C", w.dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read the worktree's HEAD: %w", trimCmdError(out, err))
	}
	return strings.TrimSpace(string(out)), nil
}

// remoteBranchSHA is the commit the remote holds for the branch; false
// when the remote does not have it (§5.4).
func (w *workspace) remoteBranchSHA(ctx context.Context) (string, bool) {
	out, err := w.git(ctx, "ls-remote", w.repoURL, "refs/heads/"+w.branch)
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[1] != "refs/heads/"+w.branch {
		return "", false
	}
	return fields[0], true
}

// git runs one git command with the runner's own environment, where its
// credentials live (config or environment — never the worker's argv or
// prompt). Both streams come back: git names its failures on stderr.
func (w *workspace) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), w.gitEnv...)
	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		return []byte(combined.String()), err
	}
	return []byte(combined.String()), nil
}

// mirrorPath names the repo's mirror under state: a hash of the URL,
// because the URL itself is not a filename.
func mirrorPath(stateDir, repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return filepath.Join(stateDir, "mirrors", hex.EncodeToString(sum[:])[:32]+".git")
}

// trimCmdError folds a command's captured output into its error: git
// writes what went wrong to stdout/stderr, and the error that reaches
// the job record should name it, bounded and with any userinfo the URL
// carried redacted — a URL like https://user:pass@host/repo.git rides
// the runner's git configuration and must not land in job records.
func trimCmdError(out []byte, err error) error {
	detail := redactURL(strings.TrimSpace(string(out)))
	if len(detail) > 400 {
		detail = detail[:400] + "…"
	}
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

// credURLRe matches the userinfo of a URL: scheme://user:pass@. The
// user and password parts cannot contain '/', ':' before the separator
// or '@', so the pattern stays anchored at the authority.
var credURLRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@:\s]+:[^/@\s]+@`)

// redactURL replaces any user:password in a URL with ***: git mostly
// redacts credentials itself, but the runner quotes URLs and git output
// in its errors, and one leak through a job record is one too many.
func redactURL(s string) string {
	return credURLRe.ReplaceAllString(s, "$1***@")
}
