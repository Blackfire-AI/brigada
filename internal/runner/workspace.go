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
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// A runner's workspace (§5.1, product rule 6): a per-repo mirror cache
// under the runner's state directory, and one git worktree per task,
// on the branch the job's grant names, built from the job's base ref —
// or, for a fix round, from the head of the branch it continues. The
// mirror is cloned once and fetched afterwards; the worktree is removed
// after the job completes. The runner pushes result branches to the
// repo's remote itself, with its own credentials (config or
// environment), which never reach the worker's argv or prompt.

// taskBranch is the runner's own fallback naming for a result branch
// (§5.1): brigada/task-<id>, with an attempt suffix once a job was
// re-leased. It names the branch only when the grant carries none — an
// engine that does not name branches itself.
func taskBranch(taskID, attempt int64) string {
	if attempt <= 1 {
		return fmt.Sprintf("brigada/task-%d", taskID)
	}
	return fmt.Sprintf("brigada/task-%d-a%d", taskID, attempt)
}

// grantBranch is the branch the job's grant names it to work on: the
// engine's choice, made once at the job's first lease and kept by its
// later leases. A grant without one — an older engine — falls back to
// the runner's own naming above.
func grantBranch(grant *api.JobGrant) string {
	if grant.Branch != "" {
		return grant.Branch
	}
	return taskBranch(grant.TaskID, grant.Attempt)
}

// isFixJob reports whether the grant is a fix round (§3): the round
// that continues the task's existing branch — after a failed check, a
// changes verdict, or as the answer that resumes the author's session —
// instead of starting one from the base ref.
func isFixJob(grant *api.JobGrant) bool {
	return engine.Kind(grant.Kind) == engine.KindFix
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
// the grant's branch — a work job's from the base ref, a fix round's
// from the head of the task's branch as the remote holds it, so the
// round builds on the work before it. A repo without a URL is refused
// outright (product §5 rule 6: repos are addressed by URL): no clone,
// no worktree, no worker. A review job takes the other worktree (§7):
// the task's result branch, read-only.
func prepareWorkspace(ctx context.Context, cfg *Config, grant *api.JobGrant) (*workspace, error) {
	if isReviewJob(grant) {
		return prepareReviewWorkspace(ctx, cfg, grant)
	}
	if grant.RepoURL == "" {
		return nil, fmt.Errorf("task %d: no repo URL; a task must name a repo to work in", grant.TaskID)
	}
	if grant.BaseRef == "" {
		return nil, fmt.Errorf("task %d: no base ref to build the worktree on", grant.TaskID)
	}
	w := &workspace{
		mirror:  mirrorPath(cfg.StateDir, grant.RepoURL),
		dir:     filepath.Join(cfg.StateDir, "worktrees", fmt.Sprintf("job-%d", grant.JobID)),
		branch:  grantBranch(grant),
		repoURL: grant.RepoURL,
		gitEnv:  cfg.GitEnv,
	}
	if err := w.materializeMirror(ctx, grant.BaseRef); err != nil {
		return nil, err
	}
	start := grant.BaseRef
	if isFixJob(grant) && grant.Branch != "" {
		// The round continues the task's branch (§3): the worktree
		// starts at the branch's head as the remote holds it, fetched by
		// name — the mirror's own fetch skips the runners' brigada/
		// namespace — so the round's work builds on the work before it
		// and its push is a fast-forward of the branch, never a force.
		// A remote that answers cleanly that it holds no such branch —
		// nothing was ever pushed — leaves the round on the base ref,
		// where the work itself starts. A remote that cannot be asked at
		// all fails the job instead: reading that as absence would
		// silently rebuild the round from the base and throw away the
		// work it was sent to continue.
		if _, holds, err := w.remoteBranch(ctx, grant.Branch); err != nil {
			return nil, err
		} else if holds {
			if err := w.fetchBranch(ctx, grant.Branch); err != nil {
				return nil, err
			}
			start = grant.Branch
		}
	}
	// A worktree left behind by a crashed run of the same job would make
	// the add below fail on the path; the runner is sequential, so what
	// sits there now is dead.
	w.discardWorktree(ctx)
	if out, err := w.git(ctx, "--git-dir", w.mirror, "worktree", "add", "-B", w.branch, w.dir, start); err != nil {
		w.discardWorktree(ctx)
		return nil, fmt.Errorf("add the worktree for %s from %s: %w", w.branch, start, trimCmdError(out, err))
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

// prepareReviewWorkspace materializes the reviewer's checkout (§7): the
// worktree sits at the task's result branch exactly as the remote holds
// it, on a detached HEAD — no branch is created, moved or pushed, for
// the review only reads. The branch is fetched by name: the mirror's
// general fetch skips the runners' brigada/ namespace, and the runner
// that reviews the work may never have run it.
func prepareReviewWorkspace(ctx context.Context, cfg *Config, grant *api.JobGrant) (*workspace, error) {
	if grant.RepoURL == "" {
		return nil, fmt.Errorf("task %d: no repo URL; a review must name a repo to read", grant.TaskID)
	}
	if grant.ReviewBranch == "" {
		return nil, fmt.Errorf("task %d: no result branch to review; the work round left none", grant.TaskID)
	}
	w := &workspace{
		mirror:  mirrorPath(cfg.StateDir, grant.RepoURL),
		dir:     filepath.Join(cfg.StateDir, "worktrees", fmt.Sprintf("job-%d", grant.JobID)),
		branch:  grant.ReviewBranch,
		repoURL: grant.RepoURL,
		gitEnv:  cfg.GitEnv,
	}
	if err := w.materializeMirror(ctx, grant.BaseRef); err != nil {
		return nil, err
	}
	if err := w.fetchBranch(ctx, grant.ReviewBranch); err != nil {
		return nil, err
	}
	w.discardWorktree(ctx)
	if out, err := w.git(ctx, "--git-dir", w.mirror, "worktree", "add", "--detach", w.dir, w.branch); err != nil {
		w.discardWorktree(ctx)
		return nil, fmt.Errorf("add the review worktree at %s: %w", w.branch, trimCmdError(out, err))
	}
	// The commit under review, kept for the record like a work
	// worktree's base; the review path itself never pushes, so nothing
	// compares against it.
	out, err := w.git(ctx, "-C", w.dir, "rev-parse", "HEAD")
	if err != nil {
		w.discardWorktree(ctx)
		return nil, fmt.Errorf("read the review worktree's commit: %w", trimCmdError(out, err))
	}
	w.base = strings.TrimSpace(string(out))
	return w, nil
}

// materializeMirror clones or fetches the repo's cached mirror (§5.1)
// and pins its fallback identity, fetching baseRef by name when it
// lives in the runners' own brigada/ namespace.
func (w *workspace) materializeMirror(ctx context.Context, baseRef string) error {
	if _, err := os.Stat(w.mirror); err == nil {
		// The fetch takes every branch but the runners' own result
		// branches: those sit in this mirror checked out by live
		// worktrees, and git refuses to move a ref under a checkout.
		// The mirror never needs them from the remote — the runner
		// pushes them, and the worktrees create them here — and the
		// negative refspec keeps them out of pruning too.
		if out, err := w.git(ctx, "--git-dir", w.mirror, "fetch", "--prune", "origin",
			"+refs/heads/*:refs/heads/*", "^refs/heads/brigada/*"); err != nil {
			return fmt.Errorf("fetch the mirror of %s: %w", redactURL(w.repoURL), trimCmdError(out, err))
		}
		// A follow-up job may build on an earlier attempt's result
		// branch (§5.3: feedback rounds resume the work). The fetch
		// above skips the brigada/ namespace wholesale, and a runner
		// that never ran the earlier attempt — or re-cloned its mirror —
		// would not have the ref. So the base branch is fetched by name
		// when it lives there: this runner holds at most one brigada/
		// ref checked out at a time, its own, and it is not this job's
		// base.
		if strings.HasPrefix(baseRef, "brigada/") {
			if err := w.fetchBranch(ctx, baseRef); err != nil {
				return err
			}
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(w.mirror), 0o755); err != nil {
			return fmt.Errorf("create the mirror directory: %w", err)
		}
		if out, err := w.git(ctx, "clone", "--mirror", w.repoURL, w.mirror); err != nil {
			return fmt.Errorf("clone %s: %w", redactURL(w.repoURL), trimCmdError(out, err))
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
			return fmt.Errorf("set %s on the mirror: %w", kv[0], trimCmdError(out, err))
		}
	}
	return nil
}

// fetchBranch fetches one branch by name into the mirror (§5.1): a ref
// the general fetch skips — a base that lives in the runners' brigada/
// namespace, or a review's result branch — fetched for the job that
// names it, from a remote that holds it.
func (w *workspace) fetchBranch(ctx context.Context, branch string) error {
	if out, err := w.git(ctx, "--git-dir", w.mirror, "fetch", "origin",
		"+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return fmt.Errorf("fetch the branch %s from %s: %w", branch, redactURL(w.repoURL), trimCmdError(out, err))
	}
	return nil
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

// remoteBranch is the commit the remote holds for one branch: false
// when the remote answers cleanly that it has no such ref, and an
// error when the question itself failed — the remote could not be
// asked, a network or credentials fault. The two are not the same
// absence, and the fix round's workspace builds on the difference
// (§3): the callers that cannot use it take remoteBranchSHA below.
func (w *workspace) remoteBranch(ctx context.Context, branch string) (string, bool, error) {
	out, err := w.git(ctx, "ls-remote", w.repoURL, "refs/heads/"+branch)
	if err != nil {
		return "", false, fmt.Errorf("ask %s for the branch %s: %w",
			redactURL(w.repoURL), branch, trimCmdError(out, err))
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[1] != "refs/heads/"+branch {
		return "", false, nil
	}
	return fields[0], true, nil
}

// remoteBranchSHA is the commit the remote holds for the worktree's
// branch; false when the remote does not have it (§5.4). A failure to
// ask reads the same as an absent branch here: the push path that
// uses this pushes first and asks again, so the difference buys it
// nothing.
func (w *workspace) remoteBranchSHA(ctx context.Context) (string, bool) {
	sha, ok, err := w.remoteBranch(ctx, w.branch)
	if err != nil {
		return "", false
	}
	return sha, ok
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
