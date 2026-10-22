package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/landing"
)

// A land job's execution (§7.1): no worker, no worktree — the runner's
// own git, in a clone of the target branch under its state directory;
// the task's setup and extra checkouts serve the check alone. In order:
// fetch the target (cut from the default branch when the remote has
// none yet) and the task's result
// branch by exact name; squash the task's own diff (base...branch,
// three-way) as one commit through internal/landing, with the message
// the hand-driven land builds; run the project's default check on the
// result; then by the push gate push, record waiting, or record the
// push command; after_land after the push, or after the landing when
// the gate is never. Never a force-push, never another branch touched.
// Under the ask gate a landing waits on the remote, not in the clone:
// its commit is pushed, fast-forward only, to the target's waiting ref
// (landWaitRef), and the next landing squashes onto that ref while it
// is ahead of the target, so the waiting landings form one stack — a
// dependent on top of its dependency — that any runner can push. Under
// the never gate the commit waits on the same ref, so the recorded push
// command runs from the user's own repository. A push phase — the land
// job reused once the gate is released — pushes the recorded commit, no
// new squash.

// landRecordJSON is the landing record a land job's completion carries
// (§7.1), the shape the engine keeps: the target branch, the resulting
// commit, whether the check on the landed result passed, the push state
// the gate made, and — when the landing failed — the conflicting files
// or the check log.
type landRecordJSON struct {
	TargetBranch  string   `json:"target_branch"`
	Commit        string   `json:"commit"`
	CheckPassed   bool     `json:"check_passed"`
	Pushed        string   `json:"pushed"` // 'pushed', 'waiting' or 'local'
	PushCmd       string   `json:"push_cmd,omitempty"`
	AfterLandErr  string   `json:"after_land_error,omitempty"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
	CheckLog      string   `json:"check_log,omitempty"`
}

// pushRecordJSON is a push phase's completion (§7.1): the landing
// record already exists; this reports the push of the commit it named.
type pushRecordJSON struct {
	TargetBranch string `json:"target_branch"`
	Commit       string `json:"commit"`
	Pushed       string `json:"pushed"` // 'pushed'
	AfterLandErr string `json:"after_land_error,omitempty"`
}

// isLandJob reports whether the grant is a land job (§7.1): no worker
// and no agent, only the git label's promise.
func isLandJob(grant *api.JobGrant) bool {
	return engine.Kind(grant.Kind) == engine.KindLand
}

// runLandJob runs a land job (§7.1) under the same heartbeat cover as
// a worker's run — a check on the landed result can run for minutes,
// and the lease must survive it — and completes it with the landing
// record or an error, whatever the engine's answers make of it.
func (r *jobRun) runLandJob(ctx context.Context) {
	runCtx, abort := context.WithCancel(ctx)
	beatsDone := make(chan struct{})
	go r.heartbeats(runCtx, abort, beatsDone)
	defer func() {
		abort()
		<-beatsDone
	}()
	if r.grant.Push {
		r.runLandPushPhase(runCtx)
		return
	}
	r.runLandSquashPhase(runCtx)
}

// runLandSquashPhase is the landing itself (§7.1): one attempt, and —
// when the target moved under the push — one retry from a fresh fetch.
// A conflict or a failed check is the attempt's outcome, not the
// runner's failure: the record carries the files or the check log,
// nothing is pushed, and the task parks for the foreman.
func (r *jobRun) runLandSquashPhase(ctx context.Context) {
	grant := r.grant
	switch {
	case grant.RepoURL == "":
		r.finishWithError(ctx, "the land job names no repo; nothing to land from")
		return
	case grant.LandTarget == "" || grant.Branch == "" || grant.BaseRef == "":
		r.finishWithError(ctx, fmt.Sprintf("the land job is incomplete: target %q, branch %q, base %q",
			grant.LandTarget, grant.Branch, grant.BaseRef))
		return
	case grant.LandMessage == nil:
		r.finishWithError(ctx, "the land job carries no message data for the squash commit")
		return
	}
	dir := landCloneDir(r.cfg.StateDir, grant)
	if err := r.prepareLandClone(ctx, dir, grant.RepoURL); err != nil {
		r.finishWithError(ctx, err.Error())
		return
	}
	// The landing check sees what a work's check sees (§5.7, §5.8): the
	// extra checkouts, then the setup, run in the clone before the check.
	defer r.cleanupSetup()
	defer r.cleanupExtras()
	if why := r.prepareExtras(ctx); why != "" {
		if !r.gone(ctx, "checking out the extra repositories") {
			r.finishWithSetupError(ctx, why)
		}
		return
	}
	out, err := r.landAttempt(ctx, dir)
	if err != nil {
		r.finishLandErr(ctx, err)
		return
	}
	if out.pushRejected {
		// The target — or the waiting ref, under the ask and never
		// gates — moved under the push (§7.1): fetch and retry the
		// squash once, then a second rejection is a conflict-like
		// failure.
		r.cfg.Log.WarnContext(ctx, "the landing push was rejected; retrying once from a fresh fetch",
			"job_id", grant.JobID, "err", out.pushErr)
		out, err = r.landAttempt(ctx, dir)
		if err != nil {
			r.finishLandErr(ctx, err)
			return
		}
		if out.pushRejected {
			r.finishWithError(ctx, "the push was rejected again after one retry (the target moved): "+out.pushErr)
			return
		}
	}
	r.finishLand(ctx, out.record())
}

// landSetupError is a failed setup before the landing check: an
// infrastructure failure of the job (§5.7), delivered like a work job's.
type landSetupError struct{ why string }

func (e *landSetupError) Error() string { return e.why }

// finishLandErr completes the land job with an attempt's error.
func (r *jobRun) finishLandErr(ctx context.Context, err error) {
	var setup *landSetupError
	if errors.As(err, &setup) {
		r.finishWithSetupError(ctx, setup.why)
		return
	}
	r.finishWithError(ctx, err.Error())
}

// landOutcome is one squash attempt's result (§7.1): the landing
// record's fields — conflict files and check log included, for the two
// outcomes that park the task — plus the push rejection the caller
// retries once. An error return is an infrastructure failure the runner
// could not turn into any landing at all.
type landOutcome struct {
	target       string
	commit       string
	checkOK      bool
	checkLog     string
	pushed       string
	pushCmd      string
	afterErr     string
	conflict     []string
	pushRejected bool
	pushErr      string
}

func (o *landOutcome) record() landRecordJSON {
	return landRecordJSON{
		TargetBranch:  o.target,
		Commit:        o.commit,
		CheckPassed:   o.checkOK,
		Pushed:        o.pushed,
		PushCmd:       o.pushCmd,
		AfterLandErr:  o.afterErr,
		ConflictFiles: o.conflict,
		CheckLog:      o.checkLog,
	}
}

// landAttempt runs one full landing attempt in the clone (§7.1):
// fetch, squash the task's own diff, check, then the push gate. The
// clone is reset clean first, so a rejected attempt never leaks into
// its retry.
func (r *jobRun) landAttempt(ctx context.Context, dir string) (*landOutcome, error) {
	grant := r.grant
	o := &landOutcome{target: grant.LandTarget}
	refs := []string{grant.LandTarget, grant.Branch}
	if grant.BaseRef != grant.LandTarget && grant.BaseRef != grant.Branch {
		refs = append(refs, grant.BaseRef)
	}
	if err := r.landFetch(ctx, dir, refs...); err != nil {
		return nil, err
	}
	onto := "refs/heads/" + grant.LandTarget
	if grant.GatePush == engine.GateAsk || grant.GatePush == engine.GateNever {
		// Landings that wait at the push gate — or land under never,
		// whose push is the user's — stack (§7.1): while the waiting
		// ref is ahead of the target — some landing still waits for
		// its push — this one squashes onto it, so it lands after them
		// and on top of any dependency among them. Once the target
		// holds the ref, the target is the base again.
		if err := r.landFetchWaiting(ctx, dir); err != nil {
			return nil, err
		}
		wait := landWaitRef(grant.LandTarget)
		if r.landHasCommit(ctx, dir, wait) {
			pushed, err := r.landIsAncestor(ctx, dir, wait, onto)
			if err != nil {
				return nil, fmt.Errorf("compare the waiting landings with %s: %v", grant.LandTarget, err)
			}
			if !pushed {
				onto = wait
			}
		}
	}
	// The landing clone sits detached at the fetched target, or at the
	// waiting ref: no branch of its own is ever checked out, so the
	// fetches move no ref under a checkout and the squash commits ride a
	// detached HEAD.
	if out, err := r.landGit(ctx, dir, "checkout", "--quiet", "--force", "--detach", onto); err != nil {
		return nil, fmt.Errorf("check out %s: %v", onto, trimCmdError(out, err))
	}
	head, err := r.landText(ctx, dir, "rev-parse", "refs/heads/"+grant.Branch)
	if err != nil {
		return nil, fmt.Errorf("read the fetched head of %s: %v", grant.Branch, err)
	}
	commit, err := r.landText(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	o.commit = strings.TrimSpace(commit)
	// The task's own diff, base...branch (§7.1). The recorded base can
	// be a dependency's branch, and once that dependency landed as a
	// squash — its branch never joining the target's history — or the
	// task's own branch was rebuilt on the target, the base is no longer
	// an ancestor of the branch: diffed against it, the three-dot diff
	// would start at the stale merge-base and carry every change the
	// target made since, conflicting in files the task never touched. A
	// base the branch no longer sits on gives way to the base the squash
	// lands on, whose merge-base with the branch is the task's own point
	// of departure; a branch still stacked on its dependency's keeps it,
	// for the base below (the target, or the waiting stack) already
	// carries the dependency's work.
	diffBase := onto
	if grant.BaseRef == grant.LandTarget || grant.BaseRef == grant.Branch {
		diffBase = "refs/heads/" + grant.BaseRef
	} else if stacked, err := r.landIsAncestor(ctx, dir, "refs/heads/"+grant.BaseRef, "refs/heads/"+grant.Branch); err != nil {
		return nil, fmt.Errorf("compare %s with %s: %v", grant.BaseRef, grant.Branch, err)
	} else if stacked {
		diffBase = "refs/heads/" + grant.BaseRef
	}
	patch, err := landing.Diff(ctx, dir, diffBase, grant.Branch, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("diff %s...%s: %v", diffBase, grant.Branch, err)
	}
	if len(patch) > 0 {
		// The message the hand-driven land builds (§7.1), from the data
		// the grant carries — the worker's own commit_message (§5.4)
		// included, which Message puts in place of the built subject and
		// body when the worker wrote one; the head is the fetched one,
		// the runner's to say.
		msg, _ := landing.Message(landing.Task{
			ID:            grant.TaskID,
			Title:         grant.LandMessage.Title,
			Branch:        grant.Branch,
			Head:          strings.TrimSpace(head),
			Spec:          grant.LandMessage.Spec,
			Summary:       grant.LandMessage.Summary,
			CommitMessage: grant.LandMessage.CommitMessage,
			Family:        grant.LandMessage.Family,
			Model:         grant.LandMessage.Model,
		})
		res, err := landing.Squash(ctx, dir, grant.TaskID, msg, patch, io.Discard)
		if err != nil {
			var conflict *landing.Conflict
			if errors.As(err, &conflict) {
				// §7.1: the attempt is discarded — nothing resolved,
				// nothing pushed — and the conflicting files are the
				// job's record. Git itself named none: its own words
				// carry the failure instead.
				r.discardLandAttempt(ctx, dir)
				if len(conflict.Files) > 0 {
					o.conflict = conflict.Files
					return o, nil
				}
				return nil, fmt.Errorf("the task's diff no longer applies onto %s: %v", grant.LandTarget, conflict.Err)
			}
			return nil, fmt.Errorf("squash the task's diff onto %s: %v", grant.LandTarget, err)
		}
		if !res.Empty {
			commit, err := r.landText(ctx, dir, "rev-parse", "HEAD")
			if err != nil {
				return nil, err
			}
			o.commit = strings.TrimSpace(commit)
		}
		// An empty patch made no commit (§7.1): the record names the
		// head it was squashed onto, for the landing is already entirely
		// there.
	}
	if grant.CheckCmd != "" {
		if why := r.runSetup(ctx, dir, false); why != "" {
			if r.gone(ctx, "running the setup before the landing check") {
				return nil, errors.New("the run was no longer ours while the landing setup ran")
			}
			return nil, &landSetupError{why}
		}
		if r.gone(ctx, "running the setup before the landing check") {
			return nil, errors.New("the run was no longer ours while the landing setup ran")
		}
		check := runCheck(ctx, dir, grant.CheckCmd, r.cfg.CheckTimeout, r.extraEnvVars()...)
		if r.gone(ctx, "running the landing check") {
			return nil, errors.New("the run was no longer ours while the landing check ran")
		}
		if check.Failed() {
			// §7.1: the check's failure is the landing's outcome; the
			// log is the job's artifact, nothing is pushed, and the
			// attempt is discarded.
			if err := r.saveRecord(ctx, "check_log", fmt.Sprintf("job-%d-check.log", grant.JobID), []byte(check.Log)); err != nil {
				r.cfg.Log.WarnContext(ctx, "save the landing check log", "job_id", grant.JobID, "err", err)
			}
			r.discardLandAttempt(ctx, dir)
			o.checkLog = check.Output
			return o, nil
		}
	}
	o.checkOK = true
	switch grant.GatePush {
	case engine.GateAsk:
		// §6.3: the landing waits at the push gate; the push phase,
		// queued by the user's release, pushes the recorded commit and
		// runs after_land. The commit waits on the remote's waiting ref,
		// fast-forward only, so it outlives this clone and any runner can
		// push it; a rejection means another landing moved the ref first,
		// and the caller retries once from a fresh fetch and squash.
		if err := r.landPushWaiting(ctx, dir, grant.LandTarget); err != nil {
			r.discardLandAttempt(ctx, dir)
			o.pushRejected = true
			o.pushErr = err.Error()
			return o, nil
		}
		o.pushed = "waiting"
	case engine.GateNever:
		// §6.3: brigada never pushes the target; after_land runs now,
		// after the landing. The commit waits on the remote's waiting
		// ref — brigada's own, no branch of the user's (§7.1) — so the
		// recorded command is exact and runs from the user's own
		// repository: fetch the durable ref, push it to the target.
		if err := r.landPushWaiting(ctx, dir, grant.LandTarget); err != nil {
			r.discardLandAttempt(ctx, dir)
			o.pushRejected = true
			o.pushErr = err.Error()
			return o, nil
		}
		o.pushed = "local"
		o.pushCmd = landPushCmd(grant.LandTarget)
		o.afterErr = r.runAfterLand(ctx, dir)
	default:
		if out, err := r.landGit(ctx, dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+grant.LandTarget); err != nil {
			// The target moved (§7.1): the attempt is discarded and the
			// caller retries once, from a fresh fetch and squash.
			r.discardLandAttempt(ctx, dir)
			o.pushRejected = true
			o.pushErr = trimCmdError(out, err).Error()
			return o, nil
		}
		o.pushed = "pushed"
		o.afterErr = r.runAfterLand(ctx, dir)
	}
	return o, nil
}

// runLandPushPhase performs the push phase of an already-recorded
// landing (§7.1): the push gate was released, and the runner pushes
// the recorded commit — no new squash — then runs after_land. The
// commit waits on the remote's waiting ref (landWaitRef), so whichever
// runner leases the phase fetches it there, its own clone fresh or not;
// a commit found on neither is reported, never re-made. The waiting
// landings form one stack, so pushing a later one pushes the earlier
// ones with it: a commit the target already holds is pushed already.
func (r *jobRun) runLandPushPhase(ctx context.Context) {
	grant := r.grant
	switch {
	case grant.RepoURL == "":
		r.finishWithError(ctx, "the push job names no repo; nothing to push to")
		return
	case grant.LandTarget == "" || grant.LandCommit == "":
		r.finishWithError(ctx, "the push job names no landing: no target branch or commit")
		return
	}
	dir := landCloneDir(r.cfg.StateDir, grant)
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
	// The recorded commit, exactly (§7.1): verified present, checked
	// out, pushed.
	if out, err := r.landGit(ctx, dir, "cat-file", "-e", grant.LandCommit+"^{commit}"); err != nil {
		r.finishWithError(ctx, fmt.Sprintf("the recorded landing commit %s is neither on %s nor in this runner's landing clone: %v",
			grant.LandCommit, landWaitRef(grant.LandTarget), trimCmdError(out, err)))
		return
	}
	if out, err := r.landGit(ctx, dir, "checkout", "--quiet", "--force", "--detach", grant.LandCommit); err != nil {
		r.finishWithError(ctx, fmt.Sprintf("check out the recorded commit %s: %v", grant.LandCommit, trimCmdError(out, err)))
		return
	}
	// Two tries: the stack's pushes run at once, so a rejection may be a
	// later landing's push that already carried this commit — the fresh
	// fetch tells, and only a target that still lacks it is a failure.
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
			r.finishWithError(ctx, fmt.Sprintf("push %s to %s: %v", grant.LandCommit, grant.LandTarget, trimCmdError(out, err)))
			return
		}
		if err := r.landFetch(ctx, dir, grant.LandTarget); err != nil {
			r.finishWithError(ctx, err.Error())
			return
		}
	}
	r.finishLand(ctx, pushRecordJSON{
		TargetBranch: grant.LandTarget,
		Commit:       grant.LandCommit,
		Pushed:       "pushed",
		AfterLandErr: r.runAfterLand(ctx, dir),
	})
}

// runAfterLand runs the after_land command (§6.3) in the landing
// clone, in the user's shell like a check: its failure is reported —
// in the record and the history — and never undone, never a change to
// what landed. "" when none ran or it passed.
func (r *jobRun) runAfterLand(ctx context.Context, dir string) string {
	if r.grant.AfterLand == "" {
		return ""
	}
	res := runCheck(ctx, dir, r.grant.AfterLand, r.cfg.CheckTimeout)
	if !res.Failed() {
		return ""
	}
	why := fmt.Sprintf("exit %d: %s", res.ExitCode, res.Output)
	if res.TimedOut {
		why = fmt.Sprintf("timed out after %s: %s", r.cfg.CheckTimeout, res.Output)
	}
	return tailOf(strings.TrimSpace(why), 2<<10)
}

// finishLand completes the land job with a record's JSON (§7.1): the
// landing record, or the push phase's.
func (r *jobRun) finishLand(ctx context.Context, rec any) {
	b, err := json.Marshal(rec)
	if err != nil {
		r.finishWithError(ctx, "encode the landing record: "+err.Error())
		return
	}
	r.deliver(ctx, api.Completion{Result: string(b)})
}

// landCloneDir names the task's landing clone under the runner's state
// directory (§7.1): one per task, created by its landing and reused by
// its push phase and any retry. Never the user's own worktree.
func landCloneDir(stateDir string, grant *api.JobGrant) string {
	return filepath.Join(stateDir, "lands", fmt.Sprintf("task-%d", grant.TaskID))
}

// prepareLandClone creates or reuses the task's landing clone (§7.1)
// and leaves it clean and detached: a hard reset and a clean, so no
// attempt's leftovers — conflict markers, staged files, a check's
// build — reach the next job, and no branch checked out, so the
// fetches never move a ref under a checkout.
func (r *jobRun) prepareLandClone(ctx context.Context, dir, repoURL string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return fmt.Errorf("create the landing clones directory: %w", err)
		}
		// A clone cut short must not leave a half clone the next job
		// would treat as reused.
		_ = os.RemoveAll(dir)
		out, err := r.landGit(ctx, filepath.Dir(dir), "clone", "--quiet", repoURL, dir)
		if err != nil {
			return fmt.Errorf("clone %s: %v", redactURL(repoURL), trimCmdError(out, err))
		}
		if out, err := r.landGit(ctx, dir, "checkout", "--quiet", "--detach"); err != nil {
			return fmt.Errorf("detach the landing clone: %v", trimCmdError(out, err))
		}
	}
	// The squash commits in this clone: a host with no identity of its
	// own gets the runner's fallback, one with an identity keeps it.
	for _, kv := range fallbackIdentity {
		if _, err := r.landGit(ctx, dir, "config", "--get", kv[0]); err == nil {
			continue
		}
		if out, err := r.landGit(ctx, dir, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("set %s on the landing clone: %v", kv[0], trimCmdError(out, err))
		}
	}
	r.discardLandAttempt(ctx, dir)
	return nil
}

// discardLandAttempt wipes an attempt's traces from the landing clone
// (§7.1): a conflict's markers and index, a stopped check's build, a
// rejected push's tree. Nothing is resolved, nothing pushed.
func (r *jobRun) discardLandAttempt(ctx context.Context, dir string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = r.landGit(ctx, dir, "reset", "--quiet", "--hard")
	_, _ = r.landGit(ctx, dir, "clean", "--quiet", "-fdx")
}

// landFetch fetches refs into the landing clone by exact name (§7.1),
// as the hand-driven land fetches: no tags, no prune, nothing but the
// named branches, forced only into the clone's own local refs.
//
// A land target the remote does not have yet — a fresh staging branch —
// is created from the default branch on first use: the clone's own ref
// is cut from the default branch, and the landing's push creates it on
// the remote.
func (r *jobRun) landFetch(ctx context.Context, dir string, refs ...string) error {
	target := r.grant.LandTarget
	fresh := false
	for i, ref := range refs {
		if ref != target || target == "" {
			continue
		}
		out, err := r.landGit(ctx, dir, "ls-remote", "--heads", "origin", "refs/heads/"+target)
		if err != nil {
			return fmt.Errorf("look for %s on %s: %v", target, redactURL(r.grant.RepoURL), trimCmdError(out, err))
		}
		if strings.TrimSpace(string(out)) != "" {
			break
		}
		if r.grant.DefaultBranch == "" || r.grant.DefaultBranch == target {
			return fmt.Errorf("%s does not exist on %s and there is no default branch to create it from", target, redactURL(r.grant.RepoURL))
		}
		fresh = true
		refs = append(append([]string(nil), refs[:i]...), refs[i+1:]...)
		refs = append(refs, r.grant.DefaultBranch)
		break
	}
	args := []string{"fetch", "--quiet", "--no-tags", "origin"}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		args = append(args, fmt.Sprintf("+refs/heads/%s:refs/heads/%s", ref, ref))
	}
	if out, err := r.landGit(ctx, dir, args...); err != nil {
		return fmt.Errorf("fetch %s from %s: %v", strings.Join(refs, ", "), redactURL(r.grant.RepoURL), trimCmdError(out, err))
	}
	if fresh {
		if out, err := r.landGit(ctx, dir, "update-ref", "refs/heads/"+target, "refs/heads/"+r.grant.DefaultBranch); err != nil {
			return fmt.Errorf("create %s from %s: %v", target, r.grant.DefaultBranch, trimCmdError(out, err))
		}
	}
	return nil
}

// landWaitRef names the remote ref the target's waiting landings sit on
// (§7.1, §6.3): the newest landing that waits at the push gate, with
// every earlier one beneath it. Outside refs/heads, so it is no branch
// of the user's, and moved only by fast-forward pushes, so no waiting
// commit ever drops off it.
func landWaitRef(target string) string {
	return "refs/brigada/land/" + target
}

// landPushCmd is the exact command a gate_push never landing records for
// the user (§6.3, §7.1): the commit waits on the remote's waiting ref,
// so the command runs from the user's own repository — fetch the
// durable ref, push it to the target — never from the runner's clone.
func landPushCmd(target string) string {
	return "git fetch origin " + landWaitRef(target) + " && git push origin FETCH_HEAD:refs/heads/" + target
}

// landPushWaiting pushes the landing clone's HEAD to the target's
// waiting ref (§7.1), fast-forward only: the commit then outlives the
// clone, for the push phase of any runner — or, under gate_push never,
// the user's own repository.
func (r *jobRun) landPushWaiting(ctx context.Context, dir, target string) error {
	out, err := r.landGit(ctx, dir, "push", "--quiet", "origin", "HEAD:"+landWaitRef(target))
	if err != nil {
		return trimCmdError(out, err)
	}
	return nil
}

// landFetchWaiting fetches the waiting refs into the landing clone
// (§7.1), pruning the ones the remote no longer has. A remote with none
// is no failure: the glob then matches nothing.
func (r *jobRun) landFetchWaiting(ctx context.Context, dir string) error {
	if out, err := r.landGit(ctx, dir, "fetch", "--quiet", "--no-tags", "--prune", "origin",
		"+refs/brigada/land/*:refs/brigada/land/*"); err != nil {
		return fmt.Errorf("fetch the waiting landings from %s: %v", redactURL(r.grant.RepoURL), trimCmdError(out, err))
	}
	return nil
}

// landHasCommit reports whether ref names a commit in the landing clone.
func (r *jobRun) landHasCommit(ctx context.Context, dir, ref string) bool {
	_, err := r.landGit(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// landIsAncestor reports whether commit is reachable from ref in the
// landing clone: git's own answer, an error only when git could not
// give one.
func (r *jobRun) landIsAncestor(ctx context.Context, dir, commit, ref string) (bool, error) {
	out, err := r.landGit(ctx, dir, "merge-base", "--is-ancestor", commit, ref)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, trimCmdError(out, err)
}

// landGit runs one git command in the landing clone with the runner's
// own environment, where its credentials live (§5.1) — the same rule
// the workspace's git follows. Both streams come back; git names its
// failures on stderr.
func (r *jobRun) landGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, r.cfg.GitEnv...)
	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		return []byte(combined.String()), err
	}
	return []byte(combined.String()), nil
}

// landText is landGit for a command whose stdout is the answer.
func (r *jobRun) landText(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := r.landGit(ctx, dir, args...)
	if err != nil {
		return "", trimCmdError(out, err)
	}
	return string(out), nil
}
