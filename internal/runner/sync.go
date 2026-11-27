package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// A sync job's execution (§7.1): no worker, no worktree, no check, no
// after_land and no land_ready — the sync lands no task's work. In a
// clone of the target the runner fetches both heads and the waiting ref,
// counts the default branch's commits the target does not hold, and
// either finds the target current (nothing merges, nothing pushes),
// queues behind a waiting push stack (outcome behind stack), or merges
// the default branch into the target — a true merge commit, never a
// squash or rebase — and brings it to the remote by the push gate. Under
// auto the merge pushes to the target itself, fast-forward; under ask it
// waits on the target's waiting ref alone; under never it waits on the
// same ref with the exact hand push recorded. A push phase — the sync
// job `brigada push approve` queues — pushes the recorded merge commit,
// no new merge.

// syncRecordJSON is the sync record a sync job's completion carries
// (§7.1), the shape the engine keeps: the outcome, both heads, the count
// behind, and the merge with its push state or the conflicting files.
type syncRecordJSON struct {
	Outcome       string   `json:"outcome"`
	DefaultHead   string   `json:"default_head,omitempty"`
	TargetHead    string   `json:"target_head,omitempty"`
	Behind        int      `json:"behind,omitempty"`
	MergeCommit   string   `json:"merge_commit,omitempty"`
	Pushed        string   `json:"pushed,omitempty"`
	PushCmd       string   `json:"push_cmd,omitempty"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
}

// The sync's outcomes (§7.1), the engine's own words.
const (
	syncOutcomeCurrent     = "current"
	syncOutcomeMerged      = "merged"
	syncOutcomeConflict    = "conflict"
	syncOutcomeBehindStack = "behind stack"
)

// isSyncJob reports whether the grant is a sync job (§7.1): no worker
// and no agent, only the git label's promise.
func isSyncJob(grant *api.JobGrant) bool {
	return engine.Kind(grant.Kind) == engine.KindSync
}

// runSyncJob runs a sync job (§7.1) under the same heartbeat cover as a
// landing — a merge and its push can take a moment, and the lease must
// survive them — and completes it with the sync record or an error.
func (r *jobRun) runSyncJob(ctx context.Context) {
	runCtx, abort := context.WithCancel(ctx)
	beatsDone := make(chan struct{})
	go r.heartbeats(runCtx, abort, beatsDone)
	defer func() {
		abort()
		<-beatsDone
	}()
	if r.grant.Push {
		r.runSyncPushPhase(runCtx)
		return
	}
	r.runSyncAttempt(runCtx)
}

// syncCloneDir names the project's sync clone under the runner's state
// directory (§7.1): one per project, reused by every attempt and the
// push phase. Never the user's own worktree.
func syncCloneDir(stateDir string, grant *api.JobGrant) string {
	return filepath.Join(stateDir, "lands", fmt.Sprintf("sync-%d", grant.ProjectID))
}

// runSyncAttempt runs the sync's full attempt (§7.1): fetch both heads
// and the waiting ref, drop a stale ref, count behind, and merge or
// queue behind the stack. A push the target's movement keeps rejecting
// retries from a fresh fetch up to the landing's own attempt bound.
func (r *jobRun) runSyncAttempt(ctx context.Context) {
	grant := r.grant
	switch {
	case grant.RepoURL == "":
		r.finishWithError(ctx, "the sync job names no repo; nothing to sync from")
		return
	case grant.LandTarget == "" || grant.DefaultBranch == "":
		r.finishWithError(ctx, fmt.Sprintf("the sync job is incomplete: target %q, default %q", grant.LandTarget, grant.DefaultBranch))
		return
	}
	dir := syncCloneDir(r.cfg.StateDir, grant)
	if err := r.prepareLandClone(ctx, dir, grant.RepoURL); err != nil {
		r.finishWithError(ctx, err.Error())
		return
	}
	// A target the remote does not hold yet is current (§7.1): the first
	// landing cuts it from the default branch; nothing merges here.
	has, err := r.landRemoteHas(ctx, dir, grant.LandTarget)
	if err != nil {
		r.finishWithError(ctx, err.Error())
		return
	}
	if !has {
		r.finishSync(ctx, syncRecordJSON{Outcome: syncOutcomeCurrent})
		return
	}
	for attempt := 1; ; attempt++ {
		out, err := r.syncAttempt(ctx, dir)
		if err != nil {
			r.finishWithError(ctx, err.Error())
			return
		}
		if !out.pushRejected {
			r.finishSync(ctx, out.record())
			return
		}
		if attempt == landPushAttempts {
			r.finishWithError(ctx, fmt.Sprintf("the sync push was rejected after %d attempts (the target moved): %s", attempt, out.pushErr))
			return
		}
		r.cfg.Log.WarnContext(ctx, "the sync push was rejected; retrying from a fresh fetch",
			"job_id", grant.JobID, "attempt", attempt, "err", out.pushErr)
		select {
		case <-time.After(landRetryPause(attempt)):
		case <-ctx.Done():
			r.gone(ctx, "waiting to retry the sync push")
			r.finishWithError(ctx, "the run was no longer ours while the sync waited to retry its push")
			return
		}
	}
}

// syncOutcome is one sync attempt's result (§7.1): the record's fields
// plus the push rejection the caller retries from a fresh fetch.
type syncOutcome struct {
	outcome      string
	defaultHead  string
	targetHead   string
	behind       int
	mergeCommit  string
	pushed       string
	pushCmd      string
	conflict     []string
	pushRejected bool
	pushErr      string
}

func (o *syncOutcome) record() syncRecordJSON {
	return syncRecordJSON{
		Outcome:       o.outcome,
		DefaultHead:   o.defaultHead,
		TargetHead:    o.targetHead,
		Behind:        o.behind,
		MergeCommit:   o.mergeCommit,
		Pushed:        o.pushed,
		PushCmd:       o.pushCmd,
		ConflictFiles: o.conflict,
	}
}

// syncAttempt runs one sync attempt in the clone (§7.1): fetch both
// heads and the waiting ref, drop a stale ref, count the default commits
// the target lacks, then merge onto the target's head or queue behind a
// waiting stack. The clone is reset clean first, so a rejected attempt
// never leaks into its retry.
func (r *jobRun) syncAttempt(ctx context.Context, dir string) (*syncOutcome, error) {
	grant := r.grant
	target, defaultBranch := grant.LandTarget, grant.DefaultBranch
	o := &syncOutcome{}
	if err := r.landFetch(ctx, dir, defaultBranch, target); err != nil {
		return nil, err
	}
	if err := r.landFetchWaiting(ctx, dir); err != nil {
		return nil, err
	}
	// The engine's stale-ref mark, then the positional drop (§7.1): a
	// waiting ref the target moved past goes before anything merges, and
	// the drop is reported on the completion.
	if err := r.landDropMarkedWaiting(ctx, dir); err != nil {
		return nil, err
	}
	if err := r.landDropStaleWaiting(ctx, dir, target); err != nil {
		return nil, err
	}
	defHead, err := r.landText(ctx, dir, "rev-parse", "refs/heads/"+defaultBranch)
	if err != nil {
		return nil, fmt.Errorf("read the head of %s: %v", defaultBranch, err)
	}
	tgtHead, err := r.landText(ctx, dir, "rev-parse", "refs/heads/"+target)
	if err != nil {
		return nil, fmt.Errorf("read the head of %s: %v", target, err)
	}
	o.defaultHead = strings.TrimSpace(defHead)
	o.targetHead = strings.TrimSpace(tgtHead)
	count, err := r.landText(ctx, dir, "rev-list", "--count", "refs/heads/"+target+"..refs/heads/"+defaultBranch)
	if err != nil {
		return nil, fmt.Errorf("count the commits %s is behind %s: %v", target, defaultBranch, err)
	}
	o.behind, _ = strconv.Atoi(strings.TrimSpace(count))
	if o.behind == 0 {
		o.outcome = syncOutcomeCurrent
		return o, nil
	}
	// A waiting push stack blocks the merge (§7.1): while the waiting ref
	// is ahead of the target, a sync push would move the target past every
	// waiting commit. The sync queues behind the stack instead.
	wait := landWaitRef(target)
	if r.landHasCommit(ctx, dir, wait) {
		held, err := r.landIsAncestor(ctx, dir, wait, "refs/heads/"+target)
		if err != nil {
			return nil, fmt.Errorf("compare the waiting stack with %s: %v", target, err)
		}
		if !held {
			o.outcome = syncOutcomeBehindStack
			return o, nil
		}
	}
	// Merge the default branch into the target — a true merge commit,
	// never a squash or rebase (§7.1) — onto whatever head the target is at.
	if out, err := r.landGit(ctx, dir, "checkout", "--quiet", "--force", "--detach", "refs/heads/"+target); err != nil {
		return nil, fmt.Errorf("check out %s: %v", target, trimCmdError(out, err))
	}
	msg := fmt.Sprintf("Merge %s into %s\n\nBrigada-Sync: %s@%s", defaultBranch, target, defaultBranch, o.defaultHead)
	if out, err := r.landGit(ctx, dir, "merge", "--quiet", "--no-ff", "--no-edit", "-m", msg, "refs/heads/"+defaultBranch); err != nil {
		files := r.syncConflictFiles(ctx, dir)
		r.discardLandAttempt(ctx, dir)
		if len(files) > 0 {
			o.outcome = syncOutcomeConflict
			o.conflict = files
			return o, nil
		}
		return nil, fmt.Errorf("merge %s into %s: %v", defaultBranch, target, trimCmdError(out, err))
	}
	merge, err := r.landText(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read the merge commit: %v", err)
	}
	o.mergeCommit = strings.TrimSpace(merge)
	o.outcome = syncOutcomeMerged
	switch grant.GatePush {
	case engine.GateAsk:
		if err := r.landPushWaiting(ctx, dir, target); err != nil {
			return nil, fmt.Errorf("push the sync merge to the waiting ref of %s: %v", target, err)
		}
		o.pushed = "waiting"
	case engine.GateNever:
		if err := r.landPushWaiting(ctx, dir, target); err != nil {
			return nil, fmt.Errorf("push the sync merge to the waiting ref of %s: %v", target, err)
		}
		o.pushed = "local"
		o.pushCmd = landPushCmd(target)
	default: // auto: push the merge to the target itself, fast-forward
		if out, err := r.landGit(ctx, dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+target); err != nil {
			o.pushRejected = true
			o.pushErr = trimCmdError(out, err).Error()
			return o, nil
		}
		o.pushed = "pushed"
		o.behind = 0
	}
	return o, nil
}

// syncConflictFiles reads the paths a failed merge left unmerged (§7.1).
func (r *jobRun) syncConflictFiles(ctx context.Context, dir string) []string {
	out, err := r.landGit(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// runSyncPushPhase pushes the recorded sync merge (§7.1): the commit the
// waiting record names, checked out and pushed to the target, no new
// merge, no check, no after_land. A push the target can no longer
// fast-forward to is reported as an error; the engine forgets the waiting
// merge and the re-queued sync settles the target.
func (r *jobRun) runSyncPushPhase(ctx context.Context) {
	grant := r.grant
	switch {
	case grant.RepoURL == "":
		r.finishWithError(ctx, "the sync push job names no repo; nothing to push to")
		return
	case grant.LandTarget == "" || grant.LandCommit == "":
		r.finishWithError(ctx, "the sync push job names no merge: no target branch or commit")
		return
	}
	dir := syncCloneDir(r.cfg.StateDir, grant)
	if err := r.prepareLandClone(ctx, dir, grant.RepoURL); err != nil {
		r.finishWithError(ctx, err.Error())
		return
	}
	if err := r.landFetch(ctx, dir, grant.LandTarget); err != nil {
		r.finishWithError(ctx, err.Error())
		return
	}
	if err := r.landFetchWaiting(ctx, dir); err != nil {
		r.finishWithError(ctx, err.Error())
		return
	}
	if out, err := r.landGit(ctx, dir, "cat-file", "-e", grant.LandCommit+"^{commit}"); err != nil {
		r.finishWithError(ctx, fmt.Sprintf("the recorded sync merge %s is neither on %s nor in this runner's sync clone: %v",
			grant.LandCommit, landWaitRef(grant.LandTarget), trimCmdError(out, err)))
		return
	}
	if out, err := r.landGit(ctx, dir, "checkout", "--quiet", "--force", "--detach", grant.LandCommit); err != nil {
		r.finishWithError(ctx, fmt.Sprintf("check out the recorded sync merge %s: %v", grant.LandCommit, trimCmdError(out, err)))
		return
	}
	// Two tries, as a landing's push: a rejection may be a push that
	// already carried this commit — the fresh fetch tells.
	for try := 1; ; try++ {
		landed, err := r.landIsAncestor(ctx, dir, grant.LandCommit, "refs/heads/"+grant.LandTarget)
		if err != nil {
			r.finishWithError(ctx, fmt.Sprintf("compare %s with %s: %v", grant.LandCommit, grant.LandTarget, err))
			return
		}
		if landed {
			break
		}
		out, err := r.landGit(ctx, dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+grant.LandTarget)
		if err == nil {
			break
		}
		if try == 2 {
			r.finishWithError(ctx, fmt.Sprintf("push the sync merge %s to %s: %v", grant.LandCommit, grant.LandTarget, trimCmdError(out, err)))
			return
		}
		if err := r.landFetch(ctx, dir, grant.LandTarget); err != nil {
			r.finishWithError(ctx, err.Error())
			return
		}
	}
	r.finishSync(ctx, syncRecordJSON{Outcome: syncOutcomeMerged, MergeCommit: grant.LandCommit, Pushed: "pushed"})
}

// finishSync completes the sync job with a record's JSON (§7.1).
func (r *jobRun) finishSync(ctx context.Context, rec syncRecordJSON) {
	r.finishLand(ctx, rec)
}
