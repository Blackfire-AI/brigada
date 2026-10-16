package runner

import (
	"context"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
	"sync"
)

// mirrorLocks serializes the mutations of one repo's shared mirror
// across the jobs of a concurrent runner: its clone and fetch, and the
// worktree adds, removes and prunes that rewrite its worktree records.
// One mutex per mirror path, so jobs of different repos never wait on
// each other. The lock covers a job's workspace set-up and tear-down
// only — never its execution — so independent worktrees of one repo
// run side by side.
var mirrorLocks sync.Map // mirror path → chan struct{} (capacity 1)

// lockMirror takes the mirror's lock and returns its release, or ctx's
// error when the job is aborted while it waits — a job queued behind a
// slow clone must still notice that it lost its lease. It is not
// re-entrant: a holder uses the *Locked workspace methods.
func lockMirror(ctx context.Context, mirror string) (func(), error) {
	c, _ := mirrorLocks.LoadOrStore(mirror, make(chan struct{}, 1))
	ch := c.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Branches checked out by a live worktree of this process. Git refuses
// to move a branch ref, or to check it out a second time, while a
// worktree holds it, so a follow-up job of the same task (a review
// fetching the result branch, a fix continuing it) must wait for the
// author's worktree to come down. The author's completion is published
// before its worktree is removed, so the follow-up can be leased in
// between; this registry is what makes it wait, deterministically.
var branchHolds struct {
	mu sync.Mutex
	m  map[string]chan struct{} // mirror + "\x00" + branch → closed on release
}

func branchHoldKey(mirror, branch string) string { return mirror + "\x00" + branch }

// holdBranch records that a worktree of the mirror has branch checked out.
func holdBranch(mirror, branch string) {
	branchHolds.mu.Lock()
	defer branchHolds.mu.Unlock()
	if branchHolds.m == nil {
		branchHolds.m = map[string]chan struct{}{}
	}
	k := branchHoldKey(mirror, branch)
	if _, ok := branchHolds.m[k]; !ok {
		branchHolds.m[k] = make(chan struct{})
	}
}

// releaseBranch ends the hold, waking whoever waits for the branch.
func releaseBranch(mirror, branch string) {
	branchHolds.mu.Lock()
	defer branchHolds.mu.Unlock()
	k := branchHoldKey(mirror, branch)
	if ch, ok := branchHolds.m[k]; ok {
		close(ch)
		delete(branchHolds.m, k)
	}
}

// heldBranch returns the release channel of the first of branches a live
// worktree holds, or nil when none is held.
func heldBranch(mirror string, branches []string) chan struct{} {
	branchHolds.mu.Lock()
	defer branchHolds.mu.Unlock()
	for _, b := range branches {
		if ch, ok := branchHolds.m[branchHoldKey(mirror, b)]; ok {
			return ch
		}
	}
	return nil
}

// lockMirrorFor takes the mirror's lock once none of branches is checked
// out by another live worktree, waiting for them to come down first. The
// check is repeated under the lock, because the holder's removal needs
// the lock too: waiting for a branch while holding it would deadlock.
func lockMirrorFor(ctx context.Context, mirror string, branches ...string) (func(), error) {
	for {
		if ch := heldBranch(mirror, branches); ch != nil {
			select {
			case <-ch:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		unlock, err := lockMirror(ctx, mirror)
		if err != nil {
			return nil, err
		}
		if heldBranch(mirror, branches) == nil {
			return unlock, nil
		}
		unlock()
	}
}

// One runner process per state directory (§5.1). The `runner` role's
// token is one per state dir, so two runner processes on it register
// under the same name and the second's advertisement replaces the
// first's — each process then believes it offers what it was started
// with, while the engine believes whatever registered last. A lock
// file in the state directory fails the second process at once,
// before it can register or touch the shared worktrees.
//
// Why a lock file and not an engine-side check at register: the
// engine may be down or restarting exactly when a second runner
// starts — register is retried with a backoff, so an engine-side
// refusal would come late, after the second process already holds the
// token and shares the state directory's worktrees and mirrors. The
// lock needs no engine at all, and the kernel releases it when the
// process dies however it dies, so a killed runner never wedges its
// state directory. The pid written into the file is for the message a
// second runner prints, not for the decision itself.

// lockStateDir takes the exclusive runner lock inside stateDir and
// returns its release. A lock another runner process holds is refused
// with its pid named.
func lockStateDir(stateDir string) (func(), error) {
	return runnerstate.Acquire(stateDir)
}
