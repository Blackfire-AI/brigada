package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/landing"
)

// The batch run (§7.1 "Running a batch", §12.3): one land job lands every
// member the batch owns, in batch order, as one stack of squash commits
// with one push. In its landing clone the runner fetches the target, the
// waiting ref and every member's result branch by exact name, reconciles
// the members whose trailers already sit on the target or the waiting
// ref, and runs land_ready once before the first squash. For each
// remaining member it verifies the approved head, squashes the member's
// own diff as its own commit with its own message and trailers, and runs
// the default check on the whole stack through that commit; only a
// passing member becomes the base of the next. A conflict, a failed check
// or a moved head parks that member alone, its attempt discarded back to
// the last passing stack, and its dependents in the batch drop to wait;
// earlier and independent members go on. The push gate then publishes the
// survivors with one fast-forward push — the target under auto, the
// waiting ref under ask and never — and only the published members are
// landing records. A rejected push retries the remaining members from a
// fresh fetch, up to landPushAttempts pushes in all; a transport or
// credential failure is the environment's and stops the batch.

// The member outcomes a batch completion reports (§12.3), the engine's
// words.
const (
	landMemberLanded  = "landed"
	landMemberParked  = "parked"
	landMemberDropped = "dropped"
	// A mixed batch's gate answer for a member subject to it (§7.1
	// "Deciding a batch hold").
	landMemberHeld     = engine.OutcomeLandHeld
	landMemberNotReady = engine.OutcomeLandNotReady
)

// landBatchJSON is a batch land job's completion (§12.3): every granted
// member's outcome in batch order, and — when the run stopped partway —
// the job-level error that settles the members it reports nothing for.
type landBatchJSON struct {
	Batch     bool             `json:"batch"`
	Members   []landMemberJSON `json:"members"`
	Error     string           `json:"error,omitempty"`
	ErrorKind string           `json:"error_kind,omitempty"`
}

// landMemberJSON is one member's outcome.
type landMemberJSON struct {
	TaskID    int64           `json:"task_id"`
	Outcome   string          `json:"outcome"`
	Record    *landRecordJSON `json:"record,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	BlockedBy int64           `json:"blocked_by,omitempty"`
	Line      string          `json:"line,omitempty"`
	Suspects  string          `json:"suspects,omitempty"`
	Exit      int             `json:"exit,omitempty"`
}

// isLandBatch reports whether the grant is a batch land job's squash
// phase (§12.3).
func isLandBatch(grant *api.JobGrant) bool {
	return grant.LandBatch && !grant.Push
}

// landStackMember is one member's place in this attempt's stack: its
// squash commit, checked and passing.
type landStackMember struct {
	member engine.LandMember
	commit string
}

// landBatchAttempt is one attempt's result: the survivors a rejected
// push left unpublished, and the rejection's words. The members it
// settles — reconciled, published, parked or dropped — go straight into
// the caller's settled map.
type landBatchAttempt struct {
	survivors []landStackMember
	rejected  bool
	pushErr   string
}

// runLandBatch runs a batch land job's squash phase (§7.1).
func (r *jobRun) runLandBatch(ctx context.Context) {
	grant := r.grant
	switch {
	case grant.RepoURL == "":
		r.finishWithError(ctx, "the land job names no repo; nothing to land from")
		return
	case grant.LandTarget == "":
		r.finishWithError(ctx, "the land batch names no target")
		return
	}
	for _, m := range grant.LandMembers {
		if m.Branch == "" || m.BaseRef == "" || m.Message == nil {
			r.finishWithError(ctx, fmt.Sprintf("the land batch's member task %d is incomplete: branch %q, base %q", m.TaskID, m.Branch, m.BaseRef))
			return
		}
		if err := engine.CheckBaseFetchable(m.BaseRef); err != nil {
			r.finishWithError(ctx, err.Error())
			return
		}
	}
	dir := landCloneDir(r.cfg.StateDir, grant)
	if err := r.prepareLandClone(ctx, dir, grant.RepoURL); err != nil {
		r.finishWithError(ctx, err.Error())
		return
	}
	// land_ready runs once per land job, before the first squash (§7.1),
	// never again for a rejected push's retries; its answer covers the
	// members subject to it — every member but those whose gate an
	// approval or a re-check released.
	settled := map[int64]landMemberJSON{}
	if !r.runLandBatchReady(ctx, dir, settled) {
		return
	}
	defer r.cleanupSetup()
	defer r.cleanupExtras()
	if why := r.prepareExtras(ctx); why != "" {
		if !r.gone(ctx, "checking out the extra repositories") {
			r.finishWithSetupError(ctx, why)
		}
		return
	}
	errMsg, kind := r.landBatchLoop(ctx, dir, settled)
	r.finishLandBatch(ctx, settled, errMsg, kind)
}

// runLandBatchReady runs the batch's land_ready gate (§7.1 "Deciding a
// batch hold"). With every member subject to the command its answer is
// the batch's, completing the job as a one-task landing's does; with
// every member's gate released nothing runs. A mixed batch runs the
// command once: exit 0 lets every member go on, and a refusal settles
// only the members subject to it — each its own hold with the same line,
// or its own retry — while the released members go on in this batch,
// the dependents of a member it held dropping to wait. false when the
// gate completed the job.
func (r *jobRun) runLandBatchReady(ctx context.Context, dir string, settled map[int64]landMemberJSON) bool {
	grant := r.grant
	var subject []int64
	for _, m := range grant.LandMembers {
		if !m.SkipLandReady {
			subject = append(subject, m.TaskID)
		}
	}
	if len(subject) == len(grant.LandMembers) {
		return r.runLandReady(ctx, dir)
	}
	if grant.LandReady == "" || grant.SkipLandReady || len(subject) == 0 {
		return true
	}
	held, notReady := r.landReadyAnswer(ctx, dir)
	for _, id := range subject {
		switch {
		case held != nil:
			settled[id] = landMemberJSON{TaskID: id, Outcome: landMemberHeld, Line: held.Line, Suspects: held.Suspects}
		case notReady != nil:
			settled[id] = landMemberJSON{TaskID: id, Outcome: landMemberNotReady, Exit: notReady.Exit, Reason: notReady.Reason}
		}
	}
	return true
}

// landBatchLoop runs the batch's attempts in the prepared clone (§7.1)
// and returns every member's settled outcome and, when the batch stopped
// partway, the error and its kind that settle the rest.
func (r *jobRun) landBatchLoop(ctx context.Context, dir string, settled map[int64]landMemberJSON) (errMsg, kind string) {
	grant := r.grant
	pause := landRetryPause
	if r.cfg.landRetryPause != nil {
		pause = r.cfg.landRetryPause
	}
	// settled holds every member's final outcome as it is reached — the
	// gate's hold of a member subject to it, a content failure's park, a
	// dropped dependent, a published member — so a retry re-squashes only
	// the rest.
	for attempt := 1; ; attempt++ {
		res, err := r.landBatchAttempt(ctx, dir, settled)
		// The attempt writes each member's outcome into settled as it is
		// reached, so the members it reconciled or published stand even
		// when it stops on an error.
		if err != nil {
			// The batch stops (§7.1): every member it still owns and has
			// not settled takes the job's error — an environment failure
			// parks them all under its one cause.
			var setup *landSetupError
			if errors.As(err, &setup) {
				kind = engine.ErrorKindSetup
			}
			return err.Error(), kind
		}
		if !res.rejected {
			break
		}
		if attempt == landPushAttempts {
			// The bound is spent (§7.1): every surviving member still
			// unpublished parks with its own rejected-push failure.
			for _, s := range res.survivors {
				settled[s.member.TaskID] = landMemberJSON{TaskID: s.member.TaskID, Outcome: landMemberParked,
					Reason: fmt.Sprintf("the push was rejected after %d attempts (the target moved): %s", attempt, res.pushErr)}
			}
			break
		}
		r.cfg.Log.WarnContext(ctx, "the land batch's push was rejected; retrying from a fresh fetch",
			"job_id", grant.JobID, "attempt", attempt, "err", res.pushErr)
		select {
		case <-time.After(pause(attempt)):
		case <-ctx.Done():
			r.gone(ctx, "waiting to retry the land batch's push")
			return "the run was no longer ours while the land batch waited to retry its push", ""
		}
	}
	return "", ""
}

// finishLandBatch completes the batch with every settled member in batch
// order and, when the run stopped, the error that settles the rest.
func (r *jobRun) finishLandBatch(ctx context.Context, settled map[int64]landMemberJSON, errMsg, kind string) {
	rec := landBatchJSON{Batch: true, Members: []landMemberJSON{}, Error: errMsg, ErrorKind: kind}
	for _, m := range r.grant.LandMembers {
		if s, ok := settled[m.TaskID]; ok {
			rec.Members = append(rec.Members, s)
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		r.finishWithError(ctx, "encode the land batch's record: "+err.Error())
		return
	}
	r.deliver(ctx, api.Completion{Result: string(b)})
}

// landBatchAttempt runs one attempt over the members settled leaves
// (§7.1): fetch, reconcile, squash and check each in order, then one push.
func (r *jobRun) landBatchAttempt(ctx context.Context, dir string, settled map[int64]landMemberJSON) (*landBatchAttempt, error) {
	grant := r.grant
	target := grant.LandTarget
	res := &landBatchAttempt{}
	var todo []engine.LandMember
	for _, m := range grant.LandMembers {
		if _, ok := settled[m.TaskID]; !ok {
			todo = append(todo, m)
		}
	}
	if len(todo) == 0 {
		return res, nil
	}
	if err := r.landFetch(ctx, dir, target); err != nil {
		return nil, err
	}
	onto := "refs/heads/" + target
	wait := landWaitRef(target)
	stack := grant.GatePush == engine.GateAsk || grant.GatePush == engine.GateNever
	if stack || grant.DropWaitingRef {
		if err := r.landFetchWaiting(ctx, dir); err != nil {
			return nil, err
		}
	}
	if err := r.landDropMarkedWaiting(ctx, dir); err != nil {
		return nil, err
	}
	if stack {
		if err := r.landDropStaleWaiting(ctx, dir, target); err != nil {
			return nil, err
		}
		if r.landHasCommit(ctx, dir, wait) {
			pushed, err := r.landIsAncestor(ctx, dir, wait, onto)
			if err != nil {
				return nil, fmt.Errorf("compare the waiting landings with %s: %v", target, err)
			}
			if !pushed {
				onto = wait
			}
		}
	}
	// A member already published — a lost runner's push, or this job's
	// own push whose answer was lost — is recognized by its trailers
	// before anything squashes (§7.1): its task and branch-head on the
	// target land it pushed, on the waiting stack it waits, with its own
	// commit and no new squash.
	onTarget, err := landing.LandedCommits(ctx, dir, "refs/heads/"+target)
	if err != nil {
		return nil, fmt.Errorf("read the landings on %s: %v", target, err)
	}
	var onWait map[int64][]landing.Candidate
	if stack && r.landHasCommit(ctx, dir, wait) {
		if onWait, err = landing.LandedCommits(ctx, dir, wait); err != nil {
			return nil, fmt.Errorf("read the waiting landings: %v", err)
		}
	}
	var remaining []engine.LandMember
	for _, m := range todo {
		// A reconciled member settles at once, so an error later in this
		// attempt never leaves it to the job's error: it is landed, not
		// parked (§7.1).
		if sha := publishedSHA(onTarget[m.TaskID], m); sha != "" {
			settled[m.TaskID] = landMemberJSON{TaskID: m.TaskID, Outcome: landMemberLanded,
				Record: &landRecordJSON{TargetBranch: target, Commit: sha, CheckPassed: true, Pushed: "pushed"}}
			continue
		}
		if sha := publishedSHA(onWait[m.TaskID], m); sha != "" {
			rec := &landRecordJSON{TargetBranch: target, Commit: sha, CheckPassed: true, Pushed: "waiting"}
			if grant.GatePush == engine.GateNever {
				rec.Pushed, rec.PushCmd = "local", landPushCmd(target)
			}
			settled[m.TaskID] = landMemberJSON{TaskID: m.TaskID, Outcome: landMemberLanded, Record: rec}
			continue
		}
		remaining = append(remaining, m)
	}
	// Fetch every member still to squash — its branch and base by exact
	// name — after the reconciliation, for a published member's branch
	// family is pruned (§13.2). A base branch pruned once its task landed
	// is read from the history the squash lands on instead.
	var refs []string
	pruned := map[int64]bool{}
	for _, m := range remaining {
		refs = append(refs, m.Branch)
		if m.BaseRef == target || m.BaseRef == m.Branch {
			continue
		}
		if _, _, family := engine.ParseTaskBranchRef(m.BaseRef); family {
			holds, err := r.landRemoteHas(ctx, dir, m.BaseRef)
			if err != nil {
				return nil, err
			}
			pruned[m.TaskID] = !holds
		}
		if !pruned[m.TaskID] {
			refs = append(refs, m.BaseRef)
		}
	}
	if len(refs) > 0 {
		if err := r.landFetch(ctx, dir, dedupe(refs)...); err != nil {
			return nil, err
		}
	}
	if out, err := r.landGit(ctx, dir, "checkout", "--quiet", "--force", "--detach", onto); err != nil {
		return nil, fmt.Errorf("check out %s: %v", onto, trimCmdError(out, err))
	}
	last, err := r.landText(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	last = strings.TrimSpace(last)
	// failed names the members that will not publish in this batch — a
	// park, a drop, settled before or now — so their dependents drop.
	failed := func(id int64) bool {
		s, ok := settled[id]
		return ok && s.Outcome != landMemberLanded
	}
	for _, m := range remaining {
		blocker := int64(0)
		for _, d := range m.Deps {
			if failed(d) {
				blocker = d
				break
			}
		}
		if blocker != 0 {
			// A dependent of a member that will not land waits (§7.1): left
			// out of the push, nothing recorded, never parked.
			settled[m.TaskID] = landMemberJSON{TaskID: m.TaskID, Outcome: landMemberDropped, BlockedBy: blocker}
			continue
		}
		commit, conflict, refusal, err := r.landSquashOnto(ctx, dir, last, landSquashSpec{
			taskID:       m.TaskID,
			branch:       m.Branch,
			baseRef:      m.BaseRef,
			approvedHead: m.ApprovedHead,
			msg:          m.Message,
			basePruned:   pruned[m.TaskID],
		})
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			settled[m.TaskID] = landMemberJSON{TaskID: m.TaskID, Outcome: landMemberParked, Reason: refusal}
			r.landResetTo(ctx, dir, last)
			continue
		}
		if conflict != nil {
			settled[m.TaskID] = landMemberJSON{TaskID: m.TaskID, Outcome: landMemberParked,
				Record: &landRecordJSON{TargetBranch: target, Commit: last, ConflictFiles: conflict}}
			r.landResetTo(ctx, dir, last)
			continue
		}
		// The check runs on the whole stack through this member's commit;
		// a failure is the member just added, the stack's one addition
		// since the last passing check.
		header := fmt.Sprintf("land batch job %d: task %d, commit %s\n", grant.JobID, m.TaskID, commit)
		bad, output, err := r.landCheck(ctx, dir, fmt.Sprintf("job-%d-task-%d-check.log", grant.JobID, m.TaskID), header)
		if err != nil {
			return nil, err
		}
		if bad {
			settled[m.TaskID] = landMemberJSON{TaskID: m.TaskID, Outcome: landMemberParked,
				Record: &landRecordJSON{TargetBranch: target, Commit: commit, CheckLog: output}}
			r.landResetTo(ctx, dir, last)
			continue
		}
		res.survivors = append(res.survivors, landStackMember{member: m, commit: commit})
		last = commit
	}
	if len(res.survivors) == 0 {
		return res, nil // every member dropped out: nothing pushes, nothing records
	}
	// One fast-forward push publishes the stack (§7.1).
	var pushErr error
	if stack {
		pushErr = r.landPushWaiting(ctx, dir, target)
	} else if out, err := r.landGit(ctx, dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+target); err != nil {
		pushErr = trimCmdError(out, err)
	}
	if pushErr != nil && !landPushWasRejected(pushErr.Error()) {
		// A push whose response is lost may have landed (§7.1): the remote
		// is read again before any member is judged unlanded. A stack the
		// pushed ref now holds is published; otherwise the failure is the
		// transport's or the credentials', the environment's, not a
		// content rejection, and the batch stops.
		if !r.landStackPublished(ctx, dir, stack, last) {
			r.discardLandAttempt(ctx, dir)
			return res, fmt.Errorf("push the land batch to %s: %v", target, pushErr)
		}
		r.cfg.Log.WarnContext(ctx, "the land batch's push failed but the remote holds the stack; recording it",
			"job_id", grant.JobID, "err", pushErr)
		pushErr = nil
	}
	if pushErr != nil {
		r.discardLandAttempt(ctx, dir)
		res.rejected = true
		res.pushErr = pushErr.Error()
		return res, nil
	}
	for _, s := range res.survivors {
		rec := &landRecordJSON{TargetBranch: target, Commit: s.commit, CheckPassed: true}
		switch grant.GatePush {
		case engine.GateAsk:
			rec.Pushed = "waiting"
		case engine.GateNever:
			rec.Pushed, rec.PushCmd = "local", landPushCmd(target)
			rec.AfterLandErr = r.runAfterLand(ctx, dir)
		default:
			rec.Pushed = "pushed"
			r.landPruneFamily(ctx, dir, s.member.TaskID)
			rec.AfterLandErr = r.runAfterLand(ctx, dir)
		}
		settled[s.member.TaskID] = landMemberJSON{TaskID: s.member.TaskID, Outcome: landMemberLanded, Record: rec}
	}
	res.survivors = nil
	return res, nil
}

// landStackPublished re-reads the ref the batch pushed — the target, or
// the waiting ref under ask and never — and reports whether it now holds
// the stack's tip: a push that landed though its answer was lost. false
// when the remote cannot be read either.
func (r *jobRun) landStackPublished(ctx context.Context, dir string, stack bool, tip string) bool {
	ref := "refs/heads/" + r.grant.LandTarget
	if stack {
		if r.landFetchWaiting(ctx, dir) != nil {
			return false
		}
		ref = landWaitRef(r.grant.LandTarget)
	} else if r.landFetch(ctx, dir, r.grant.LandTarget) != nil {
		return false
	}
	if !r.landHasCommit(ctx, dir, ref) {
		return false
	}
	held, err := r.landIsAncestor(ctx, dir, tip, ref)
	return err == nil && held
}

// publishedSHA picks the candidate that is the member's own published
// landing (§7.1): its Brigada-Branch trailer naming the member's branch
// at the approved head. "" when none is.
func publishedSHA(cands []landing.Candidate, m engine.LandMember) string {
	for _, c := range cands {
		if c.Branch == m.Branch && (m.ApprovedHead == "" || c.Head == m.ApprovedHead) {
			return c.SHA
		}
	}
	return ""
}

// landPushWasRejected reports whether a push failure is git refusing a
// non-fast-forward — the target or the waiting ref moved — rather than
// the transport or the credentials failing.
func landPushWasRejected(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "rejected") || strings.Contains(m, "non-fast-forward") || strings.Contains(m, "fetch first")
}

// landResetTo discards a member's attempt (§7.1), restoring the last
// passing stack.
func (r *jobRun) landResetTo(ctx context.Context, dir, commit string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = r.landGit(ctx, dir, "reset", "--quiet", "--hard", commit)
	_, _ = r.landGit(ctx, dir, "clean", "--quiet", "-fdx")
}

// dedupe keeps the first of each ref.
func dedupe(refs []string) []string {
	seen := map[string]bool{}
	out := refs[:0:0]
	for _, r := range refs {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}
