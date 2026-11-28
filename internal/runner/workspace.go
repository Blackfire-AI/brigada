package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/projecturl"
	"github.com/Blackfire-AI/brigada/internal/prompt"
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
	mirror string // the repo's cached bare mirror
	dir    string // the task's worktree
	branch string
	base   string // the commit the worktree was built on
	// holdsBranch is whether branch is registered as checked out by this
	// worktree (see holdBranch); guarded by the mirror's lock.
	holdsBranch bool
	// holdsDir is whether this worktree registered its directory as
	// occupied (see holdWorktreeDir); guarded by the mirror's lock.
	holdsDir bool
	repoURL  string
	gitEnv   []string
	// leased is whether leaseSHA was recorded; leaseSHA is the commit the
	// remote's branch held when the job's worktree was prepared ("" when
	// the remote had no such branch), the lease ensurePushed publishes
	// under (§5.4). A workspace built without it publishes with a plain
	// push.
	leased   bool
	leaseSHA string
	// baseRef is the job's base as resolved in the worktree, and
	// baseCommit the commit it named when the worktree was prepared,
	// pinned: the mirror's refs move under later jobs' fetches. The
	// branch guard (§5.2) holds HEAD to the base's history; "" when the
	// base could not be read, and the guard does not ask.
	baseRef    string
	baseCommit string
}

// prepareWorkspace materializes the workspace for a leased job (§5.1):
// the repo's mirror is cloned or fetched, then the worktree is added on
// the grant's branch — a work job's from the base ref, a fix round's
// from the head of the task's branch as the remote holds it, so the
// round builds on the work before it. A repo without a URL is refused
// outright (product §5 rule 4: repos are addressed by URL): no clone,
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
		dir:     worktreeDir(cfg.StateDir, grant),
		branch:  grantBranch(grant),
		repoURL: grant.RepoURL,
		gitEnv:  cfg.GitEnv,
	}
	// The mirror is shared by every job of the repo, and a concurrent
	// runner has several in flight: its clone, fetches and worktree
	// records are changed under the mirror's lock, held through the
	// worktree add and released before the job runs.
	//
	// A follow-up of a task waits first for the author's worktree to come
	// down: the branches it fetches or checks out (its own result
	// branch, the one it continues, a brigada/ base) cannot move while a
	// live worktree holds them. The wait gives up when the job's ctx ends.
	// The worktree's own directory is waited for the same way: the path
	// is the task's, not the job's (§5.1), so a run whose predecessor on
	// the path is still winding down — a lost lease re-taken, a follow-up
	// leased before the last worktree came down — waits for it.
	unlock, err := lockMirrorHolding(ctx, w.mirror, []string{w.branch, grant.Branch, grant.SeedBranch, brigadaRef(landTargetOf(grant.BaseRef))}, []string{w.dir})
	if err != nil {
		return nil, err
	}
	defer unlock()
	baseRef, err := w.materializeWithBase(ctx, cfg, grant)
	if err != nil {
		return nil, err
	}
	start := baseRef
	// leaseFromStart is whether the worktree starts at the remote head of
	// its own branch, which then is the lease; otherwise the worktree did
	// not start from the remote's branch, and the lease stays empty: only
	// a branch that does not exist yet is created, and one that exists is
	// refused rather than overwritten.
	leaseFromStart := false
	if isFixJob(grant) && grant.Branch != "" {
		// The round continues the task's branch (§3): the worktree
		// starts at the branch's head as the remote holds it, fetched by
		// name — the mirror's own fetch skips the runners' brigada/
		// namespace — so the round's work builds on the work before it
		// and its push is the lease push of §5.4. A remote that answers cleanly that it holds no such branch —
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
			leaseFromStart = true
		}
	} else if grant.SeedBranch != "" {
		// A stop-running restart's fresh attempt (§5.6) carries on the
		// stopped run's work: the worktree starts at the head of the
		// branch it pushed, fetched by name like a fix round's, while the
		// base ref stays the task's base for the diff and the landing. A
		// remote that holds no such branch leaves the attempt on the
		// base; one that cannot be asked fails the job, as for a round.
		if _, holds, err := w.remoteBranch(ctx, grant.SeedBranch); err != nil {
			return nil, err
		} else if holds {
			if err := w.fetchBranch(ctx, grant.SeedBranch); err != nil {
				return nil, err
			}
			start = grant.SeedBranch
		}
	}
	w.leased = true
	// A worktree left behind by a crashed run of the same job would make
	// the add below fail on the path; the path is the task's and one
	// live job holds its key (§12.1), so what sits at this path now is
	// dead or winding down — and the winding-down was waited out above.
	w.discardLocked(ctx)
	if out, err := w.git(ctx, "--git-dir", w.mirror, "worktree", "add", "-B", w.branch, w.dir, start); err != nil {
		w.discardLocked(ctx)
		return nil, fmt.Errorf("add the worktree for %s from %s: %w", w.branch, start, trimCmdError(out, err))
	}
	// The branch is checked out here from now until the worktree comes
	// down; the hold is taken under the mirror's lock, with no gap. So
	// is the directory's: the path outlives this job (§5.1), and the
	// next job on it waits for this worktree to come down first.
	holdBranch(w.mirror, w.branch)
	w.holdsBranch = true
	holdWorktreeDir(w.dir)
	w.holdsDir = true
	// The commit the worktree starts from: what holdsWork compares the
	// worker's HEAD against — a branch with no work on it is not a
	// result (§5.6).
	out, err := w.git(ctx, "-C", w.dir, "rev-parse", "HEAD")
	if err != nil {
		w.discardLocked(ctx)
		return nil, fmt.Errorf("read the worktree's base commit: %w", trimCmdError(out, err))
	}
	w.base = strings.TrimSpace(string(out))
	w.baseRef = baseRef
	if out, err := w.git(ctx, "-C", w.dir, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err == nil {
		w.baseCommit = strings.TrimSpace(string(out))
	}
	if leaseFromStart {
		w.leaseSHA = w.base // the head the worktree really started from
	}
	named := baseRef
	if grant.SeedBranch != "" && start == grant.SeedBranch {
		named = start // the seeded attempt is told the branch it carries on
	}
	grant.Prompt = prompt.WithWorkerBase(grant.Prompt, named, w.base)
	return w, nil
}

// materializeWithBase materializes the mirror and returns the ref the
// job's worktree (or review diff) is based on: grant.BaseRef, which the
// engine sets to the project's land target when one is set (§6.1), fresh
// after the mirror's fetch. The land target may not exist on the remote
// yet — the runner creates it at land time (§7.1) — and then neither the
// by-name fetch of a brigada/ branch nor the worktree add could resolve
// it. Such a base falls back to the remote's default branch (the mirror's
// HEAD), the branch the land job creates the target from, and the
// fallback is logged. A remote that cannot be asked at all fails the job
// as an infrastructure error instead of reading as absence.
//
// A base may also be a target's waiting ref (refs/brigada/land/<target>,
// §6.1): the task was restarted onto a dependency whose landing waits at
// the push gate or sits local. The mirror's general fetch takes only
// refs/heads, so the ref is fetched by name, forced, and is the base —
// never checked out, so it takes no brigada/ branch hold. Once the waiting
// landings were pushed the remote no longer holds the ref; the base is then
// the target branch the ref names, with the usual fallback to the default
// branch when that is missing too (a warning is logged at each step).
func (w *workspace) materializeWithBase(ctx context.Context, cfg *Config, grant *api.JobGrant) (string, error) {
	base := grant.BaseRef
	waitRef := ""
	if strings.HasPrefix(base, landWaitPrefix) {
		holds, err := w.remoteRefHeld(ctx, base)
		if err != nil {
			return "", &infraError{err}
		}
		if holds {
			waitRef = base
		} else {
			target := landTargetOf(base)
			if cfg != nil && cfg.Log != nil {
				cfg.Log.WarnContext(ctx, "waiting ref is gone from the remote (its landings were pushed?); building on the land target branch",
					"job_id", grant.JobID, "task_id", grant.TaskID, "base_ref", base, "target_branch", target)
			}
			base = target
		}
	}
	fetchBase := base
	if waitRef != "" {
		fetchBase = waitRef
	} else if brigadaRef(base) != "" {
		if _, holds, err := w.remoteBranch(ctx, base); err != nil {
			return "", &infraError{err}
		} else if !holds {
			fetchBase = "" // nothing to fetch by name; judged below
		}
	}
	if err := w.materializeMirror(ctx, fetchBase); err != nil {
		return "", err
	}
	if waitRef != "" {
		return waitRef, nil
	}
	if w.refExists(ctx, "refs/heads/"+base) || (brigadaRef(base) == "" && w.refExists(ctx, base)) {
		return base, nil
	}
	out, err := w.git(ctx, "--git-dir", w.mirror, "symbolic-ref", "--short", "HEAD")
	def := strings.TrimSpace(string(out))
	if err != nil || def == "" {
		return "", fmt.Errorf("base %s is not on %s and its default branch is unknown: %w",
			base, redactURL(w.repoURL), trimCmdError(out, err))
	}
	if cfg != nil && cfg.Log != nil {
		cfg.Log.WarnContext(ctx, "base ref is not on the remote yet (an unlanded land target?); building on the default branch",
			"job_id", grant.JobID, "task_id", grant.TaskID, "base_ref", base, "default_branch", def)
	}
	return def, nil
}

// landWaitPrefix starts every target's waiting ref (see landWaitRef).
const landWaitPrefix = "refs/brigada/land/"

// landTargetOf is the land target branch a waiting ref names, and ref
// itself when it is not a waiting ref.
func landTargetOf(ref string) string {
	return strings.TrimPrefix(ref, landWaitPrefix)
}

// remoteRefHeld reports whether the remote holds exactly ref: false when
// it answers cleanly that it does not, an error when it could not be
// asked.
func (w *workspace) remoteRefHeld(ctx context.Context, ref string) (bool, error) {
	out, err := w.git(ctx, "ls-remote", w.repoURL, ref)
	if err != nil {
		return false, fmt.Errorf("ask %s for the ref %s: %w", redactURL(w.repoURL), ref, trimCmdError(out, err))
	}
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[1] == ref {
			return true, nil
		}
	}
	return false, nil
}

// refExists reports whether ref resolves to a commit in the mirror.
func (w *workspace) refExists(ctx context.Context, ref string) bool {
	_, err := w.git(ctx, "--git-dir", w.mirror, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// prepareReviewWorkspace materializes the reviewer's checkout (§7): the
// worktree sits at the task's result branch exactly as the remote holds
// it, on a detached HEAD — no branch is created, moved or pushed, for
// the review only reads. The branch is fetched by name: the mirror's
// general fetch skips the runners' brigada/ namespace, and the runner
// that reviews the work may never have run it.
func prepareReviewWorkspace(ctx context.Context, cfg *Config, grant *api.JobGrant) (*workspace, error) {
	if isPlanReviewJob(grant) {
		return preparePlanReviewWorkspace(ctx, cfg, grant)
	}
	if grant.RepoURL == "" {
		return nil, fmt.Errorf("task %d: no repo URL; a review must name a repo to read", grant.TaskID)
	}
	if grant.ReviewBranch == "" {
		return nil, fmt.Errorf("task %d: no result branch to review; the work round left none", grant.TaskID)
	}
	w := &workspace{
		mirror:  mirrorPath(cfg.StateDir, grant.RepoURL),
		dir:     worktreeDir(cfg.StateDir, grant),
		branch:  grant.ReviewBranch,
		repoURL: grant.RepoURL,
		gitEnv:  cfg.GitEnv,
	}
	// See prepareWorkspace: the result branch is the author's, possibly
	// still checked out in a worktree that is on its way down. The
	// directory is waited for the same way — the review path is the
	// task's too (§5.1), shared with the re-review that resumes this
	// one's session.
	unlock, err := lockMirrorHolding(ctx, w.mirror, []string{grant.ReviewBranch, brigadaRef(landTargetOf(grant.BaseRef))}, []string{w.dir})
	if err != nil {
		return nil, err
	}
	defer unlock()
	baseRef, err := w.materializeWithBase(ctx, cfg, grant)
	if err != nil {
		return nil, err
	}
	if err := w.fetchBranch(ctx, grant.ReviewBranch); err != nil {
		return nil, err
	}
	w.discardLocked(ctx)
	if out, err := w.git(ctx, "--git-dir", w.mirror, "worktree", "add", "--detach", w.dir, w.branch); err != nil {
		w.discardLocked(ctx)
		return nil, fmt.Errorf("add the review worktree at %s: %w", w.branch, trimCmdError(out, err))
	}
	// The directory hold, taken under the mirror's lock like a work
	// worktree's branch hold: a detached review moves no branch, but
	// its path is shared with the reviews of the same attempt.
	holdWorktreeDir(w.dir)
	w.holdsDir = true
	// The commit under review, kept for the record like a work
	// worktree's base; the review path itself never pushes, so nothing
	// compares against it.
	out, err := w.git(ctx, "-C", w.dir, "rev-parse", "HEAD")
	if err != nil {
		w.discardLocked(ctx)
		return nil, fmt.Errorf("read the review worktree's commit: %w", trimCmdError(out, err))
	}
	w.base = strings.TrimSpace(string(out))
	if err := w.writeReviewDiffSince(ctx, baseRef, grant.ReviewSince); err != nil {
		w.discardLocked(ctx)
		return nil, err
	}
	return w, nil
}

// infraError marks a failure of the job's own plumbing: a fetch or
// clone of the repository's remote that could not be done — the
// network not up yet in the seconds after a wake (issue #46: `Could
// not resolve host`), a remote that will not answer. The job completes
// with it as an infrastructure error, engine.ErrorKindInfra, the class
// a failed setup completes under (§5.7): the engine retries the job,
// capped, and the task is never parked on it while retries remain.
type infraError struct{ err error }

func (e *infraError) Error() string { return e.err.Error() }

func (e *infraError) Unwrap() error { return e.err }

// materializeMirror clones or fetches the repo's cached mirror (§5.1)
// and pins its fallback identity, fetching baseRef by name when it
// lives in the runners' own brigada/ namespace, or is a target's waiting
// ref (refs/brigada/land/...), which no general fetch brings: that one is
// fetched forced into the same name on a fresh clone as well as an
// existing mirror, and the caller has checked the remote holds it.
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
			return &infraError{fmt.Errorf("fetch the mirror of %s: %w", redactURL(w.repoURL), trimCmdError(out, err))}
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
			// A clone cut short (the job's lease lost, the runner stopping)
			// must not leave a half mirror the next job would fetch into.
			_ = os.RemoveAll(w.mirror)
			return &infraError{fmt.Errorf("clone %s: %w", redactURL(w.repoURL), trimCmdError(out, err))}
		}
	}
	if strings.HasPrefix(baseRef, landWaitPrefix) {
		if out, err := w.git(ctx, "--git-dir", w.mirror, "fetch", "origin", "+"+baseRef+":"+baseRef); err != nil {
			return &infraError{fmt.Errorf("fetch the ref %s from %s: %w", baseRef, redactURL(w.repoURL), trimCmdError(out, err))}
		}
	}
	// A fallback identity and no signing, so a fresh worktree can commit
	// whatever the host's global git config says; a signed-in CLI sets
	// its own identity per commit. Mirror mode is unset because it
	// refuses every refspec push while the fetches above pass explicit
	// refspecs and never needed it.
	//
	// The push URL is disabled: the task, review and plan-review
	// worktrees are linked worktrees of this mirror and share its
	// config, so a worker's `git push origin HEAD` is refused in all of
	// them and the runner is the only push path (§5.7). The fetch side
	// is untouched. No runner push goes through the mirror's origin: it
	// pushes by explicit repository URL (ensurePushed, the salvage
	// push), the landing from its own clone and the plan file from its
	// own scratch repository, and the mirror itself only fetches.
	for _, kv := range append(append([][2]string(nil), fallbackIdentity[:]...), [][2]string{
		{"commit.gpgsign", "false"},
		{"remote.origin.mirror", "false"},
		{"remote.origin.pushurl", "disabled://worker-push-refused"},
	}...) {
		if out, err := w.git(ctx, "--git-dir", w.mirror, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("set %s on the mirror: %w", kv[0], trimCmdError(out, err))
		}
	}
	return nil
}

// fallbackIdentity is the git identity the runner commits under where no
// other is configured: the mirror's, and the landing clone's when the
// host has none.
var fallbackIdentity = [2][2]string{
	{"user.name", "brigada runner"},
	{"user.email", "runner@brigada.invalid"},
}

// fetchBranch fetches one branch by name into the mirror (§5.1): a ref
// the general fetch skips — a base that lives in the runners' brigada/
// namespace, or a review's result branch — fetched for the job that
// names it, from a remote that holds it.
func (w *workspace) fetchBranch(ctx context.Context, branch string) error {
	if out, err := w.git(ctx, "--git-dir", w.mirror, "fetch", "origin",
		"+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return &infraError{fmt.Errorf("fetch the branch %s from %s: %w", branch, redactURL(w.repoURL), trimCmdError(out, err))}
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
	// The teardown must happen whatever ended the job, so it waits for
	// the lock on a context that cannot be cancelled.
	unlock, err := lockMirror(context.WithoutCancel(ctx), w.mirror)
	if err != nil {
		return
	}
	defer unlock()
	w.discardLocked(ctx)
}

// discardLocked is discardWorktree for a caller that already holds the
// mirror's lock. It runs on a context of its own: the set-up that fails
// because its job was aborted must still take back what it made.
func (w *workspace) discardLocked(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = w.git(ctx, "--git-dir", w.mirror, "worktree", "remove", "--force", w.dir)
	_, _ = w.git(ctx, "--git-dir", w.mirror, "worktree", "prune")
	_ = os.RemoveAll(w.dir)
	if w.holdsBranch {
		w.holdsBranch = false
		releaseBranch(w.mirror, w.branch)
	}
	if w.holdsDir {
		w.holdsDir = false
		releaseWorktreeDir(w.dir)
	}
}

// worktreeDir names the job's worktree path (§5.1). A task job's path
// is the task's, its role's and its session generation's: a harness
// session belongs to the directory it started in — opencode resumes
// only there, while Claude Code tolerates the move — so the jobs that
// share a session run at one directory: a work job and the fix rounds
// that resume it, a review and the re-review that resumes it. The
// generation is the attempt the engine counts in the task's restarts,
// so a restart's fresh session never reuses the directory of the one it
// does not resume. At most one live job holds the task's key (§12.1), so
// two runs never share the path at once on one runner: a re-leased job
// whose lost predecessor is still winding down is waited out by the
// branch and directory holds, and what a crashed run left at the path
// is discarded before the worktree is added (see prepareWorkspace). A
// job without a task — a standalone review, which never resumes (§7: a
// review writes no session back) — keeps the per-job path of job id
// and attempt: nothing it runs shares the directory.
func worktreeDir(stateDir string, grant *api.JobGrant) string {
	if grant.TaskID != 0 {
		role := "author"
		if isReviewJob(grant) {
			role = "review"
		}
		return filepath.Join(stateDir, "worktrees", fmt.Sprintf("task-%d-%s-s%d", grant.TaskID, role, grant.SessionGen))
	}
	return filepath.Join(stateDir, "worktrees", fmt.Sprintf("job-%d-a%d", grant.JobID, grant.Attempt))
}

// brigadaRef is ref when it lives in the runners' own brigada/ namespace
// — the one the mirror's general fetch skips, so a job fetches it by
// name — and "" otherwise.
func brigadaRef(ref string) string {
	if strings.HasPrefix(ref, "brigada/") {
		return ref
	}
	return ""
}

// leaseRefusedError is a publish the remote refused because the branch
// no longer holds the commit the job started from (§5.4): someone else
// moved it, and the runner's rewrite would have discarded their work.
type leaseRefusedError struct{ err error }

func (e *leaseRefusedError) Error() string {
	return "the branch moved on the remote since the job started: " + e.err.Error()
}

func (e *leaseRefusedError) Unwrap() error { return e.err }

// isLeaseRefused reports whether err is a refused lease push, as against
// any other failure to publish.
func isLeaseRefused(err error) bool {
	var l *leaseRefusedError
	return errors.As(err, &l)
}

// leaseRejected reads git's push output for the lease's refusal: a
// rejected ref whose stale info is the lease's own words. A hook's or a
// protected branch's "[remote rejected]" is another failure.
func leaseRejected(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "(stale info)") ||
		(strings.Contains(s, "[rejected]") && !strings.Contains(s, "[remote rejected]"))
}

// ensurePushed verifies the branch on the remote holds exactly the
// worktree's commit, and makes it so when it does not (§5.4). The
// template tells the worker to leave the commit and the push to the
// runner (§5.3), but a worker may push anyway — a run that ignored it,
// or a prompt from an older template — so ensurePushed verifies rather
// than trusts: a branch that merely exists is not enough, since an
// early push followed by more work, or edits the worker never
// committed, would leave the remote holding something other than what
// the runner is about to vouch for. So: leftovers the
// worker left uncommitted are committed, then the remote's SHA is
// compared with the worktree's HEAD, and a mismatch is pushed. A
// ls-remote that still disagrees after the push was no push.
//
// The push is a lease push: it replaces the remote branch only while
// the branch still holds the commit the job started from (or still does
// not exist, when it did not), so a worker that rewrote its own pushed
// branch — an amend, a rebuild on a new base — publishes, and a branch
// someone else moved meanwhile is refused with a *leaseRefusedError.
func (w *workspace) ensurePushed(ctx context.Context) error {
	if err := w.commitLeftovers(ctx, false); err != nil {
		return err
	}
	head, err := w.worktreeHead(ctx)
	if err != nil {
		return err
	}
	remote, remoteOK := w.remoteBranchSHA(ctx)
	if remoteOK && remote == head {
		w.leaseSHA = head
		return nil // the worker pushed exactly what sits in the worktree
	}
	lease := w.leaseSHA
	if remoteOK && remote != lease {
		// A remote head the worktree's own history holds — the worker's
		// early push, followed by more work — is a plain fast-forward:
		// nothing of anyone's is lost, so it is the lease. A head the
		// worktree lacks is another writer's, and the job's start stays
		// the lease.
		if _, err := w.git(ctx, "-C", w.dir, "merge-base", "--is-ancestor", remote, head); err == nil {
			lease = remote
		}
	}
	args := []string{"-C", w.dir, "push"}
	if w.leased {
		args = append(args, "--force-with-lease=refs/heads/"+w.branch+":"+lease)
	}
	args = append(args, w.repoURL, "HEAD:refs/heads/"+w.branch)
	if out, err := w.git(ctx, args...); err != nil {
		err = fmt.Errorf("push %s to %s: %w", w.branch, redactURL(w.repoURL), trimCmdError(out, err))
		if w.leased && leaseRejected(out) {
			return &leaseRefusedError{err}
		}
		return err
	}
	remote, ok := w.remoteBranchSHA(ctx)
	if !ok || remote != head {
		return fmt.Errorf("branch %s on %s does not hold the worktree's commit after pushing it",
			w.branch, redactURL(w.repoURL))
	}
	w.leaseSHA = head // a later push of this workspace leases on what this one published
	return nil
}

// branchGuardError is a worktree the branch guard refused (§5.2): the
// worker left the task branch, left the job's base, or — on a task whose
// lease carries tool grants — committed on its own. The runner commits
// and publishes nothing for it.
type branchGuardError struct{ what string }

func (e *branchGuardError) Error() string { return "branch guard: " + e.what }

// guardBranch checks the worktree before the runner commits a work or
// fix job's result (§5.2): HEAD is still the task branch — not detached,
// not another branch — and descends from the job's base's history: it
// shares a commit with the base as the job started. Sharing, not
// descending from the base's head: a dependent whose base is its
// dependency's branch may be rebuilt on the land target that dependency
// landed on as a squash (§7.1), and a fix round's base may have moved on
// since its branch left it. With noWorkerCommits, for a task whose
// lease carries tool grants, HEAD also descends from the job's start
// head, and the worker made no commit of its own since it: committing
// is the runner's, and its commit of the leftovers comes after this
// check. Without it the worker may commit,
// amend or rebase its own branch (§5.4). A violation is a
// *branchGuardError naming what was wrong; a git that cannot be asked is
// an error of its own.
func (w *workspace) guardBranch(ctx context.Context, noWorkerCommits bool) error {
	head, err := w.worktreeHead(ctx)
	if err != nil {
		return err
	}
	out, err := w.git(ctx, "-C", w.dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return &branchGuardError{fmt.Sprintf("the worktree's HEAD is detached at %s, not on the task branch %s", head, w.branch)}
		}
		return fmt.Errorf("read the worktree's branch: %w", trimCmdError(out, err))
	}
	if ref := strings.TrimSpace(string(out)); ref != "refs/heads/"+w.branch {
		return &branchGuardError{fmt.Sprintf("the worktree is on branch %s, not the task branch %s",
			strings.TrimPrefix(ref, "refs/heads/"), w.branch)}
	}
	if w.baseCommit != "" {
		// merge-base exits 1, printing nothing, for two histories that
		// share no commit.
		if out, err := w.git(ctx, "-C", w.dir, "merge-base", w.baseCommit, head); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return &branchGuardError{fmt.Sprintf("HEAD %s no longer descends from the job's base %s (%s): the two share no history",
					head, w.baseRef, w.baseCommit)}
			}
			return fmt.Errorf("check HEAD descends from the job's base: %w", trimCmdError(out, err))
		}
	}
	if !noWorkerCommits {
		return nil
	}
	// A granted task's history only grows: the job's start head stays
	// under HEAD. The shared-history check above passes for almost any
	// two commits of a repository — a branch reset to an older commit
	// included — so this one holds a granted task to its start.
	if out, err := w.git(ctx, "-C", w.dir, "merge-base", "--is-ancestor", w.base, head); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return &branchGuardError{fmt.Sprintf("HEAD %s does not descend from the job's start head %s, which a task with tool grants keeps under its branch",
				head, w.base)}
		}
		return fmt.Errorf("check HEAD descends from the job's start head: %w", trimCmdError(out, err))
	}
	out, err = w.git(ctx, "-C", w.dir, "rev-list", "--reverse", w.base+"..HEAD")
	if err != nil {
		return fmt.Errorf("read the worker's commits: %w", trimCmdError(out, err))
	}
	if shas := strings.Fields(string(out)); len(shas) > 0 {
		return &branchGuardError{fmt.Sprintf("the worker made commits of its own, which a task with tool grants leaves to the runner: %s",
			listPaths(shas, maxListedPaths))}
	}
	return nil
}

// salvage saves a failed worker's work on a separate branch, leaving
// the task's remote branch alone. A clean checkout with no commits
// beyond the task's remote (or its base, if never pushed) saves nothing.
func (w *workspace) salvage(ctx context.Context, branch string) (bool, error) {
	if err := w.commitLeftovers(ctx, true); err != nil {
		return false, err
	}
	head, err := w.worktreeHead(ctx)
	if err != nil {
		return false, err
	}
	remote, exists, err := w.remoteBranch(ctx, w.branch)
	if err != nil {
		return false, err
	}
	if !exists {
		if head == w.base {
			return false, nil
		}
	} else {
		if head == remote {
			return false, nil
		}
		// The remote may have advanced since this checkout was built.
		// Fetch its commit without moving any branch in the shared mirror.
		if out, err := w.git(ctx, "-C", w.dir, "fetch", "--no-tags", "--no-write-fetch-head", w.repoURL, remote); err != nil {
			return false, fmt.Errorf("fetch the task branch for salvage: %w", trimCmdError(out, err))
		}
		out, err := w.git(ctx, "-C", w.dir, "rev-list", "--max-count=1", "HEAD", "--not", remote)
		if err != nil {
			return false, fmt.Errorf("read the worker's unpushed commits: %w", trimCmdError(out, err))
		}
		if strings.TrimSpace(string(out)) == "" {
			return false, nil
		}
	}
	if out, err := w.git(ctx, "-C", w.dir, "push", w.repoURL, "HEAD:refs/heads/"+branch); err != nil {
		return false, fmt.Errorf("push %s to %s: %w", branch, redactURL(w.repoURL), trimCmdError(out, err))
	}
	if remote, exists, err := w.remoteBranch(ctx, branch); err != nil {
		return false, err
	} else if !exists || remote != head {
		return false, fmt.Errorf("salvage branch %s does not hold the worktree's commit after pushing it", branch)
	}
	return true, nil
}

// commitLeftovers commits whatever the worker left uncommitted, so a
// done run cannot ship a branch that is missing its own edits. A clean
// tree is left alone. A merge the worker left in progress is finished by
// the commit when it is finished — no unmerged index entries, and none of
// the merge's conflicted paths still carrying a conflict marker — and git
// records the two-parent merge. An unfinished merge is handled by the
// caller's mode: on the done path (salvage false) it fails the job,
// naming the files, and nothing is committed; on the salvage path it is
// aborted first, so the original failure is not masked, and the commit
// records the abort.
func (w *workspace) commitLeftovers(ctx context.Context, salvage bool) error {
	aborted := false
	// Git's own unmerged index entries are checked whether or not a merge
	// is in progress: a cherry-pick, stash pop, checkout -m or restore
	// --merge conflict leaves them with no MERGE_HEAD, and git add -A
	// would clear them and commit the markers.
	merging, err := w.merging(ctx)
	if err != nil {
		return err
	}
	unfinished, err := w.unfinishedMerge(ctx, merging)
	if err != nil {
		return err
	}
	if unfinished != "" {
		if !salvage {
			return fmt.Errorf("the worker left an unfinished merge (%s); resolve or abort it before the runner commits", unfinished)
		}
		abort := []string{"-C", w.dir, "merge", "--abort"}
		if !merging {
			abort = []string{"-C", w.dir, "reset", "--merge"}
		}
		if out, err := w.git(ctx, abort...); err != nil {
			return fmt.Errorf("abort the unfinished merge: %w", trimCmdError(out, err))
		}
		aborted = true
	}
	out, err := w.git(ctx, "-C", w.dir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("read the worktree's status: %w", trimCmdError(out, err))
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil
	}
	if out, err := w.git(ctx, "-C", w.dir, "status", "--porcelain=v1", "-z", "--untracked-files=all"); err != nil {
		return fmt.Errorf("read the worktree's status: %w", trimCmdError(out, err))
	} else if bad := suspectLeftovers(string(out)); len(bad) > 0 {
		return fmt.Errorf("the worker left files that look like credentials or hidden configuration, "+
			"which the runner will not commit and push: %s; remove them or add them to .gitignore",
			listPaths(bad, maxListedPaths))
	}
	if out, err := w.git(ctx, "-C", w.dir, "add", "-A"); err != nil {
		return fmt.Errorf("stage the worker's uncommitted changes: %w", trimCmdError(out, err))
	}
	msg := "brigada runner: the worker's uncommitted changes"
	if aborted {
		msg = "brigada runner: the worker's uncommitted changes (an unfinished merge was aborted)"
	}
	if out, err := w.git(ctx, "-C", w.dir, "commit", "-m", msg); err != nil {
		return fmt.Errorf("commit the worker's uncommitted changes: %w", trimCmdError(out, err))
	}
	return nil
}

// merging reports whether a merge is in progress in the worktree: git
// sets MERGE_HEAD when `git merge` began and has not yet been committed
// or aborted.
func (w *workspace) merging(ctx context.Context) (bool, error) {
	out, err := w.git(ctx, "-C", w.dir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil // no MERGE_HEAD: no merge in progress
		}
		return false, fmt.Errorf("look for a merge in progress: %w", trimCmdError(out, err))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// unfinishedMerge describes why the merge in progress is unfinished, or
// returns "" when it is finished. The gate is git's own signal: any
// unmerged index entry (unmergedPaths) leaves the merge
// unfinished, whatever its content — a modify/delete, binary or add/add
// conflict carries no marker at all. A path the worker staged clears its
// entry, so a git add of an unresolved file would hide it: for that case
// only the merge's own conflicted paths (mergeConflictPaths) are read for
// a conflict marker, never the whole tree, where a legitimate file may
// hold one.
func (w *workspace) unfinishedMerge(ctx context.Context, merging bool) (string, error) {
	unmerged, err := w.unmergedPaths(ctx)
	if err != nil {
		return "", err
	}
	if len(unmerged) > 0 {
		return "unmerged: " + listPaths(unmerged, maxListedPaths), nil
	}
	if !merging {
		return "", nil
	}
	conflicted, err := w.mergeConflictPaths(ctx)
	if err != nil {
		return "", err
	}
	if marked := pathsWithMarkers(w.dir, conflicted); len(marked) > 0 {
		return "conflict markers staged in: " + listPaths(marked, maxListedPaths), nil
	}
	return "", nil
}

// unmergedPaths names the paths git holds unmerged in the index. A git
// that cannot be asked is an error, never an empty list that would read
// as nothing unmerged. Only stdout is read: stderr's warnings are not
// paths.
func (w *workspace) unmergedPaths(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", w.dir, "diff", "--name-only", "-z", "--diff-filter=U")
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), w.gitEnv...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read the unmerged paths: %w", trimCmdError([]byte(stderr.String()), err))
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// mergeConflictPaths names the paths the merge in progress conflicted on
// — the paths git held unmerged before the worker staged them — by
// replaying the merge of HEAD and MERGE_HEAD in memory with
// `git merge-tree --write-tree --name-only`, which touches neither the
// index nor the tree. It exits 1 when the merge conflicts, listing the
// paths after the result tree's line.
func (w *workspace) mergeConflictPaths(ctx context.Context) ([]string, error) {
	out, err := w.git(ctx, "-C", w.dir, "merge-tree", "--write-tree", "--name-only", "--no-messages", "HEAD", "MERGE_HEAD")
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("read the merge's conflicted paths: %w", trimCmdError(out, err))
		}
	} else {
		return nil, nil // the merge is clean: nothing conflicted
	}
	lines := strings.Split(string(out), "\n")
	var files []string
	for _, l := range lines[1:] { // the first line is the result tree
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// pathsWithMarkers returns the subset of files whose content still
// carries a git conflict marker. A file that is gone (a resolved
// modify/delete) carries none.
func pathsWithMarkers(dir string, files []string) []string {
	var out []string
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		if hasConflictMarkers(data) {
			out = append(out, f)
		}
	}
	return out
}

// hasConflictMarkers reports whether a file's content carries a git
// conflict marker — a line that begins with <<<<<<<.
func hasConflictMarkers(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("<<<<<<<")) {
			return true
		}
	}
	return false
}

// worktreeHead is the commit the worktree's branch points at.
func (w *workspace) worktreeHead(ctx context.Context) (string, error) {
	out, err := w.git(ctx, "-C", w.dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read the worktree's HEAD: %w", trimCmdError(out, err))
	}
	return strings.TrimSpace(string(out)), nil
}

// changedPaths is the task's own diff's paths, base...HEAD (§6.3,
// §7.1): what the land gate's protected-path rule matches, recorded
// with the work job's result so the rule reads the real paths the work
// touched, not the spec's words.
func (w *workspace) changedPaths(ctx context.Context, base string) ([]string, error) {
	if base == "" {
		return nil, fmt.Errorf("no base ref to diff the task's paths from")
	}
	out, err := w.git(ctx, "-C", w.dir, "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return nil, trimCmdError(out, err)
	}
	var paths []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
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
	// A remote that wants credentials the runner lacks fails at once
	// instead of waiting on a prompt nobody will answer.
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), w.gitEnv...)
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

// redactURL replaces any userinfo in the URLs a text quotes with ***:
// git mostly redacts credentials itself, but the runner quotes URLs and
// git output in its errors, and one leak through a job record is one too
// many. A lone token (https://<token>@host) goes with a user:password
// pair, through the same redaction the engine uses for repo URLs.
func redactURL(s string) string {
	return projecturl.RedactText(s)
}
