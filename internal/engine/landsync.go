package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The land-target sync (§7.1): a land target set off the default branch
// falls behind it whenever commits reach the default branch outside
// brigada. The sync keeps it current — a land job of its own, no task
// and no result branch, one per project at a time, leased by a runner
// like any landing. It runs at the two moments the target is consumed
// (before a landing's squash, before a task leases the target as a
// base); the engine holds those leases behind the one pending sync, and
// the mirror rule holds every landing and base lease while a sync is
// running, conflicting or waiting at the gate. The standing record lives
// in land_syncs (migration 0084); the job itself is task-less, on the
// key sync:<project>, so it belongs to no plan and never holds one open.

// syncKey is the one-live-job-per-key queue of a project's sync (§7.1).
func syncKey(project int64) string { return fmt.Sprintf("sync:%d", project) }

// The sync's outcomes (§7.1): current — the target holds every default
// commit, nothing merged or pushed; merged — a true merge commit was
// made and its push state recorded; conflict — the merge conflicts and
// the gate holds until the user resolves it by hand; behind stack — a
// waiting push stack blocks the merge, so the sync queues behind it.
const (
	syncOutcomeCurrent     = "current"
	syncOutcomeMerged      = "merged"
	syncOutcomeConflict    = "conflict"
	syncOutcomeBehindStack = "behind stack"
)

// The land_syncs states (§7.1): current opens the gate; waiting and
// conflict hold it (a merge at the push gate, or a conflicting one);
// behind_stack is the one exception — the sync merges nothing while a
// waiting stack stands, so landings and waves lease on the standing
// record.
const (
	syncStateCurrent     = "current"
	syncStateWaiting     = "waiting"
	syncStateConflict    = "conflict"
	syncStateBehindStack = "behind_stack"
)

// syncRecord is the payload a sync job's completion carries (§7.1): the
// outcome, both branches and the heads the attempt read, the count of
// default commits the target lacks, and — for a merge — the commit, its
// push state and the hand push under gate_push never, or the conflicting
// files. The engine fills the branches from the project's own setting;
// the runner reports the heads, the count and the merge.
type syncRecord struct {
	Outcome       string   `json:"outcome"`
	DefaultHead   string   `json:"default_head,omitempty"`
	TargetHead    string   `json:"target_head,omitempty"`
	Behind        int      `json:"behind,omitempty"`
	MergeCommit   string   `json:"merge_commit,omitempty"`
	Pushed        string   `json:"pushed,omitempty"` // 'pushed', 'waiting' or 'local' for a merge
	PushCmd       string   `json:"push_cmd,omitempty"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
}

// syncPayload is the stored form of a sync job's push-phase mark. The
// job has no task row; its prompt field carries this internal record
// instead of a worker prompt, as a sweep job carries its deletable set.
// Push marks the job `brigada push approve` queues to push a recorded
// merge — no new merge (§7.1).
type syncPayload struct {
	Push bool `json:"push,omitempty"`
}

// landSyncRow is one land_syncs row as the engine reads it (§7.1): the
// sync's standing record, the gate the mirror rule and the needs-you
// queue read.
type landSyncRow struct {
	ProjectID     int64
	State         string
	Behind        int
	DefaultBranch string
	TargetBranch  string
	DefaultHead   string
	TargetHead    string
	MergeCommit   string
	Pushed        string
	PushCmd       string
	ConflictFiles []string
}

// landSyncTx reads the project's sync record; nil, nil when it has none.
func landSyncTx(tx *sql.Tx, project int64) (*landSyncRow, error) {
	r := &landSyncRow{ProjectID: project}
	var files string
	err := tx.QueryRow(`SELECT state, behind, default_branch, target_branch, default_head, target_head,
		merge_commit, pushed, push_cmd, conflict_files FROM land_syncs WHERE project_id = ?`, project).
		Scan(&r.State, &r.Behind, &r.DefaultBranch, &r.TargetBranch, &r.DefaultHead, &r.TargetHead,
			&r.MergeCommit, &r.Pushed, &r.PushCmd, &files)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the sync of project %d: %w", project, err)
	}
	if files != "" {
		_ = json.Unmarshal([]byte(files), &r.ConflictFiles)
	}
	return r, nil
}

// writeLandSyncTx records the project's sync outcome, replacing any
// earlier record (§7.1).
func writeLandSyncTx(tx *sql.Tx, r *landSyncRow, now time.Time) error {
	files := r.ConflictFiles
	if files == nil {
		files = []string{}
	}
	b, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("encode the sync conflict files of project %d: %w", r.ProjectID, err)
	}
	if _, err := tx.Exec(`INSERT INTO land_syncs (project_id, state, behind, default_branch, target_branch,
		default_head, target_head, merge_commit, pushed, push_cmd, conflict_files, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id) DO UPDATE SET state = excluded.state, behind = excluded.behind,
			default_branch = excluded.default_branch, target_branch = excluded.target_branch,
			default_head = excluded.default_head, target_head = excluded.target_head,
			merge_commit = excluded.merge_commit, pushed = excluded.pushed, push_cmd = excluded.push_cmd,
			conflict_files = excluded.conflict_files, updated_at = excluded.updated_at`,
		r.ProjectID, r.State, r.Behind, r.DefaultBranch, r.TargetBranch, r.DefaultHead, r.TargetHead,
		r.MergeCommit, r.Pushed, r.PushCmd, string(b), fmtTime(now)); err != nil {
		return fmt.Errorf("record the sync of project %d: %w", r.ProjectID, err)
	}
	return nil
}

// forgetSyncMergeTx drops the project's sync record (§7.1): a waiting
// merge whose ref went stale, or whose push the target can no longer
// fast-forward to, is forgotten and the re-queued sync settles the
// target anew. No task parks, for the sync has none.
func forgetSyncMergeTx(tx *sql.Tx, project int64) error {
	if _, err := tx.Exec("DELETE FROM land_syncs WHERE project_id = ?", project); err != nil {
		return fmt.Errorf("forget the sync of project %d: %w", project, err)
	}
	return nil
}

// syncTargetTx reads the project's land target and default branch, and
// reports whether a sync applies (§7.1): only a land target set off the
// default branch can fall behind it. With no land target, or one equal
// to the default branch, the target is the default branch itself and
// nothing syncs.
func syncTargetTx(tx *sql.Tx, project int64) (wanted bool, target, defaultBranch string, err error) {
	if err := tx.QueryRow("SELECT default_branch FROM projects WHERE id = ?", project).Scan(&defaultBranch); err != nil {
		return false, "", "", fmt.Errorf("read the default branch of project %d: %w", project, err)
	}
	s, err := projectApprovalTx(tx, project)
	if err != nil {
		return false, "", "", err
	}
	target = s.LandTarget
	if target == "" || target == defaultBranch {
		return false, target, defaultBranch, nil
	}
	return true, target, defaultBranch, nil
}

// maybeQueueSyncTx queues the project's sync (§7.1) when a land target
// off the default is set and no sync is live: the one-live-job-per-key
// rule coalesces a repeat behind the running sync, so a plan's first
// wave, concurrent leases and an amendment's added tasks all wait on
// one sync. A live sync — queued, leased or running — is left alone.
func (e *Engine) maybeQueueSyncTx(tx *sql.Tx, project int64, now time.Time) error {
	wanted, _, _, err := syncTargetTx(tx, project)
	if err != nil || !wanted {
		return err
	}
	if live, err := liveJobTx(tx, syncKey(project)); err != nil {
		return err
	} else if live != nil {
		return nil
	}
	_, err = e.submitTx(tx, SubmitParams{
		Key:       syncKey(project),
		ProjectID: project,
		Kind:      KindSync,
		Mode:      ModeFresh,
		Prompt:    "", // no worker: nothing is composed (§1.4)
		Labels:    []string{"git"},
	}, now)
	return err
}

// maybeQueueSyncForBaseTx queues the project's sync when the task's
// resolved base is the land target (§7.1): the work-queue moment of the
// "before any task leases the target as a base" trigger. A task stacked
// on a dependency's branch consumes no target and queues nothing.
func (e *Engine) maybeQueueSyncForBaseTx(tx *sql.Tx, project, taskID int64, now time.Time) error {
	wanted, target, _, err := syncTargetTx(tx, project)
	if err != nil || !wanted {
		return err
	}
	var base string
	if err := tx.QueryRow("SELECT base_ref FROM tasks WHERE id = ?", taskID).Scan(&base); err != nil {
		return fmt.Errorf("read the base of task %d: %w", taskID, err)
	}
	if base != target {
		return nil
	}
	return e.maybeQueueSyncTx(tx, project, now)
}

// waitingStackTx reports whether any landing of the target waits at the
// push gate (§7.1) — ask (waiting) or never (local): the stack a sync
// must not merge under, and the behind-stack exception's condition.
func waitingStackTx(tx *sql.Tx, project int64, target string) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM landings WHERE project_id = ? AND target_branch = ?
		AND pushed IN ('waiting', 'local')`, project, target).Scan(&n); err != nil {
		return false, fmt.Errorf("read the waiting stack of %s: %w", target, err)
	}
	return n > 0, nil
}

// syncHoldsLeasesTx is the mirror rule (§7.1): while a sync is pending —
// running, conflicting or waiting at the gate — no landing is leased and
// no task leases the target as a base. A live sync job holds the gate;
// the one exception is a sync queued behind a waiting stack (its record
// behind_stack while the stack stands), which merges nothing, so
// landings and waves lease on the standing record.
func syncHoldsLeasesTx(tx *sql.Tx, project int64) (bool, error) {
	wanted, target, _, err := syncTargetTx(tx, project)
	if err != nil || !wanted {
		return false, err
	}
	live, err := liveJobTx(tx, syncKey(project))
	if err != nil || live == nil {
		return false, err
	}
	rec, err := landSyncTx(tx, project)
	if err != nil {
		return false, err
	}
	if rec != nil && rec.State == syncStateBehindStack {
		stack, err := waitingStackTx(tx, project, target)
		if err != nil {
			return false, err
		}
		if stack {
			return false, nil // the behind-stack exception
		}
	}
	return true, nil
}

// syncLeasableTx reports whether the project's sync may lease (§7.1): a
// landing already in flight finishes first, so the sync leases only once
// no land job of the project is leased or running. A waiting landing (a
// record at the push gate, no live push job) is not in flight: the sync
// still leases, reads the stack and completes behind it.
func syncLeasableTx(tx *sql.Tx, project int64) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = 'land'
		AND status IN ('leased', 'running')`, project).Scan(&n); err != nil {
		return false, fmt.Errorf("read the in-flight landings of project %d: %w", project, err)
	}
	return n == 0, nil
}

// holdForSyncTx stamps the queue-TTL hold on a job the sync holds (§7.1,
// §8.1): the job waits queued behind the sync, and the hold keeps the
// TTL clock counting from now so a sync that conflicts or waits at the
// gate never times out the leases queued behind it. The stamp is the
// cooling hold's own — cap_held_at with no cap layer.
func holdForSyncTx(tx *sql.Tx, j *jobRow, now time.Time) error {
	if _, err := tx.Exec(`UPDATE jobs SET cap_held_at = ?, cap_held_layer = NULL, cap_held_key = NULL, slot_hold = 0, updated_at = ?
		WHERE id = ?`, fmtTime(now), fmtTime(now), j.ID); err != nil {
		return fmt.Errorf("hold job %d behind the sync: %w", j.ID, err)
	}
	return nil
}

// landLeaseHeldTx reports whether a land job's lease is held behind the
// project's sync (§7.1): the mirror rule holds every landing while a sync
// is pending. The sync itself is queued when the land job is queued
// (queueLandJobTx), not per lease poll, so a current target leases freely.
func (e *Engine) landLeaseHeldTx(tx *sql.Tx, j *jobRow, now time.Time) (bool, error) {
	wanted, _, _, err := syncTargetTx(tx, j.ProjectID)
	if err != nil || !wanted {
		return false, err
	}
	held, err := syncHoldsLeasesTx(tx, j.ProjectID)
	if err != nil {
		return false, err
	}
	if held {
		if err := holdForSyncTx(tx, j, now); err != nil {
			return false, err
		}
	}
	return held, nil
}

// baseLeaseHeldTx reports whether a work or fix job leases the land
// target as its base (§6.1, §7.1) and is held behind the project's sync.
// A task stacked on a dependency's branch is not held: its base is not
// the target. The sync is queued when the work is queued
// (maybeQueueSyncForBaseTx), not per lease poll.
func (e *Engine) baseLeaseHeldTx(tx *sql.Tx, j *jobRow, now time.Time) (bool, error) {
	if j.TaskID == 0 {
		return false, nil
	}
	wanted, target, _, err := syncTargetTx(tx, j.ProjectID)
	if err != nil || !wanted {
		return false, err
	}
	var base string
	if err := tx.QueryRow("SELECT base_ref FROM tasks WHERE id = ?", j.TaskID).Scan(&base); err != nil {
		return false, fmt.Errorf("read the base of task %d: %w", j.TaskID, err)
	}
	if base != target {
		return false, nil // stacked on a dependency's branch, not the target
	}
	held, err := syncHoldsLeasesTx(tx, j.ProjectID)
	if err != nil {
		return false, err
	}
	if held {
		if err := holdForSyncTx(tx, j, now); err != nil {
			return false, err
		}
	}
	return held, nil
}

// syncLeaseGrantTx builds the grant a sync job's lease hands the runner
// (§7.1): the repo, the land target and the default branch, and the push
// gate the runner obeys. No worker, no agent, no prompt, no check, no
// after_land and no land_ready — the sync lands no task's work. A push
// phase (the job `brigada push approve` queues) carries the recorded
// merge commit alone, pushing it with no new merge. A full attempt whose
// target's waiting ref is marked stale says so, as a land job's does.
func (e *Engine) syncLeaseGrantTx(tx *sql.Tx, j *jobRow, owner int64, leaseID string, expires time.Time) (*LeaseGrant, error) {
	wanted, target, defaultBranch, err := syncTargetTx(tx, j.ProjectID)
	if err != nil {
		return nil, err
	}
	if !wanted {
		return nil, fmt.Errorf("sync job %d: project %d has no land target off its default branch", j.ID, j.ProjectID)
	}
	var repo string
	if err := tx.QueryRow(`SELECT COALESCE(NULLIF(transport_url, ''), repo_url) FROM projects WHERE id = ?`, j.ProjectID).Scan(&repo); err != nil {
		return nil, fmt.Errorf("read the repo of sync job %d: %w", j.ID, err)
	}
	s, err := projectApprovalTx(tx, j.ProjectID)
	if err != nil {
		return nil, err
	}
	gatePush := s.GatePush
	if gatePush == "" {
		gatePush = GateAuto
	}
	var payload syncPayload
	_ = json.Unmarshal([]byte(j.Prompt), &payload)
	rec, err := landSyncTx(tx, j.ProjectID)
	if err != nil {
		return nil, err
	}
	if payload.Push && rec != nil && rec.MergeCommit != "" {
		// The push phase: push the recorded merge, no new merge. A switch
		// of the gate to auto since the merge waited pushes it unasked
		// never — the approval is the user's yes (§7.1).
		return &LeaseGrant{
			JobID:          j.ID,
			LeaseOwner:     owner,
			ProjectID:      j.ProjectID,
			Kind:           j.Kind,
			Mode:           j.Mode,
			Key:            j.Key,
			LeaseID:        leaseID,
			LeaseExpiresAt: expires,
			RepoURL:        repo,
			LandTarget:     target,
			DefaultBranch:  defaultBranch,
			LandCommit:     rec.MergeCommit,
			Push:           true,
			GatePush:       gatePush,
			Attempt:        j.Attempts + 1,
		}, nil
	}
	if gatePush == GateAuto {
		// A sync re-made after its waiting ref went stale waits at ask
		// anew rather than pushing under the new auto (§7.1), as a
		// re-made landing does (heldPushTx).
		held, err := syncHeldPushTx(tx, j.ProjectID)
		if err != nil {
			return nil, err
		}
		if held {
			gatePush = GateAsk
		}
	}
	staleRef, err := staleLandRefTx(tx, j.ProjectID, target)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec("UPDATE jobs SET drop_waiting_ref = ? WHERE id = ?", boolInt(staleRef), j.ID); err != nil {
		return nil, fmt.Errorf("record the stale-ref mark on sync job %d: %w", j.ID, err)
	}
	return &LeaseGrant{
		JobID:          j.ID,
		LeaseOwner:     owner,
		ProjectID:      j.ProjectID,
		Kind:           j.Kind,
		Mode:           j.Mode,
		Key:            j.Key,
		LeaseID:        leaseID,
		LeaseExpiresAt: expires,
		RepoURL:        repo,
		LandTarget:     target,
		DefaultBranch:  defaultBranch,
		GatePush:       gatePush,
		DropWaitingRef: staleRef,
		Attempt:        j.Attempts + 1,
	}, nil
}

// syncHeldPushTx reports whether the project's last sync, since
// forgotten, recorded a merge waiting at the push gate or local under
// never, with no push after (§7.1): a sync re-made after its waiting ref
// went stale waits at ask again, as a re-made landing does (heldPushTx).
func syncHeldPushTx(tx *sql.Tx, project int64) (bool, error) {
	var kind, payload string
	err := tx.QueryRow(`SELECT kind, payload FROM events WHERE project_id = ? AND kind IN (?, ?)
		ORDER BY id DESC LIMIT 1`, project, evSyncLanded, evSyncPushed).Scan(&kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the last sync landing of project %d: %w", project, err)
	}
	if kind != evSyncLanded {
		return false, nil
	}
	var p eventPayload
	if json.Unmarshal([]byte(payload), &p) != nil {
		return false, nil
	}
	return p.Pushed == "waiting" || p.Pushed == "local", nil
}

// syncCompletedTx applies a sync job's completion (§7.1): the record the
// runner reported moves the project's standing record and the gate, and
// a merge that waits, conflicts or queues behind a stack re-queues the
// sync on the not-before backoff. No task parks, for the sync has none.
func (e *Engine) syncCompletedTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	if c.Error != "" {
		return e.syncErrorTx(tx, job, c, now)
	}
	var rec syncRecord
	if err := json.Unmarshal([]byte(c.Result), &rec); err != nil {
		return e.syncErrorTx(tx, job, Completion{Error: "unreadable sync record"}, now)
	}
	switch rec.Outcome {
	case syncOutcomeCurrent:
		return e.syncCurrentTx(tx, job, rec, now)
	case syncOutcomeMerged:
		return e.syncMergedTx(tx, job, rec, now)
	case syncOutcomeConflict:
		return e.syncConflictTx(tx, job, rec, now)
	case syncOutcomeBehindStack:
		return e.syncBehindStackTx(tx, job, rec, now)
	default:
		return e.syncErrorTx(tx, job, Completion{Error: fmt.Sprintf("unknown sync outcome %q", rec.Outcome)}, now)
	}
}

// syncBranches reads the project's own land target and default branch
// for the record (§7.1): the grant named them, and the engine's record is
// the project's configured target and default, whatever the runner
// reported.
func syncBranches(tx *sql.Tx, project int64) (target, defaultBranch string, err error) {
	wanted, target, defaultBranch, err := syncTargetTx(tx, project)
	if err != nil {
		return "", "", err
	}
	if !wanted {
		return "", "", fmt.Errorf("sync of project %d: no land target off its default branch", project)
	}
	return target, defaultBranch, nil
}

// syncCurrentTx records the target current (§7.1): nothing merged, or
// the merge pushed under auto. The gate opens — the leases queued behind
// the sync go on the next poll — and the behind count is zero.
func (e *Engine) syncCurrentTx(tx *sql.Tx, job *jobRow, rec syncRecord, now time.Time) error {
	target, defaultBranch, err := syncBranches(tx, job.ProjectID)
	if err != nil {
		return err
	}
	if err := writeLandSyncTx(tx, &landSyncRow{
		ProjectID: job.ProjectID, State: syncStateCurrent, Behind: 0,
		DefaultBranch: defaultBranch, TargetBranch: target,
		DefaultHead: rec.DefaultHead, TargetHead: rec.TargetHead,
		MergeCommit: rec.MergeCommit, Pushed: rec.Pushed,
	}, now); err != nil {
		return err
	}
	_, err = appendEventTx(tx, evSyncCurrent, job.ProjectID, 0, job.ID,
		eventPayload{Branch: target, Commit: rec.MergeCommit, Count: 0}, now)
	return err
}

// syncMergedTx records a merge the sync made (§7.1): pushed under auto,
// the target is current; waiting at the ask gate or local under never,
// the merge waits on the waiting ref and the sync re-queues itself on
// the backoff, the gate held until the push settles.
func (e *Engine) syncMergedTx(tx *sql.Tx, job *jobRow, rec syncRecord, now time.Time) error {
	target, defaultBranch, err := syncBranches(tx, job.ProjectID)
	if err != nil {
		return err
	}
	if rec.MergeCommit == "" {
		return e.syncErrorTx(tx, job, Completion{Error: "the sync record names no merge commit"}, now)
	}
	switch rec.Pushed {
	case "pushed":
		return e.syncCurrentTx(tx, job, rec, now)
	case "waiting", "local":
		if err := writeLandSyncTx(tx, &landSyncRow{
			ProjectID: job.ProjectID, State: syncStateWaiting, Behind: rec.Behind,
			DefaultBranch: defaultBranch, TargetBranch: target,
			DefaultHead: rec.DefaultHead, TargetHead: rec.TargetHead,
			MergeCommit: rec.MergeCommit, Pushed: rec.Pushed, PushCmd: rec.PushCmd,
		}, now); err != nil {
			return err
		}
		if _, err := appendEventTx(tx, evSyncLanded, job.ProjectID, 0, job.ID,
			eventPayload{Branch: target, Commit: rec.MergeCommit, Pushed: rec.Pushed, PushCmd: rec.PushCmd, Count: int64(rec.Behind)}, now); err != nil {
			return err
		}
		return e.requeueSyncTx(tx, job, now)
	default:
		return e.syncErrorTx(tx, job, Completion{Error: fmt.Sprintf("the sync merge's push state %q is not pushed, waiting or local", rec.Pushed)}, now)
	}
}

// syncConflictTx records a conflicting merge (§7.1): the attempt is
// discarded, nothing pushed, and the gate holds until the user resolves
// the conflict by hand and pushes. The sync re-queues itself on the
// backoff; the first attempt after the user's push reads the target
// current and opens the gate. One needs-you line names the project.
func (e *Engine) syncConflictTx(tx *sql.Tx, job *jobRow, rec syncRecord, now time.Time) error {
	target, defaultBranch, err := syncBranches(tx, job.ProjectID)
	if err != nil {
		return err
	}
	if err := writeLandSyncTx(tx, &landSyncRow{
		ProjectID: job.ProjectID, State: syncStateConflict, Behind: rec.Behind,
		DefaultBranch: defaultBranch, TargetBranch: target,
		DefaultHead: rec.DefaultHead, TargetHead: rec.TargetHead, ConflictFiles: rec.ConflictFiles,
	}, now); err != nil {
		return err
	}
	if _, err := appendEventTx(tx, evSyncConflict, job.ProjectID, 0, job.ID,
		eventPayload{Branch: target, Count: int64(rec.Behind), Reason: boundNote(strings.Join(rec.ConflictFiles, ", "))}, now); err != nil {
		return err
	}
	return e.requeueSyncTx(tx, job, now)
}

// syncBehindStackTx records a sync queued behind a waiting push stack
// (§7.1): it merges nothing while the stack stands, and re-queues itself
// on the backoff — and at once when the last waiting landing settles
// (wakeBehindStackSyncTx). Landings and waves lease on the standing
// record while the stack waits.
func (e *Engine) syncBehindStackTx(tx *sql.Tx, job *jobRow, rec syncRecord, now time.Time) error {
	target, defaultBranch, err := syncBranches(tx, job.ProjectID)
	if err != nil {
		return err
	}
	if err := writeLandSyncTx(tx, &landSyncRow{
		ProjectID: job.ProjectID, State: syncStateBehindStack, Behind: rec.Behind,
		DefaultBranch: defaultBranch, TargetBranch: target,
		DefaultHead: rec.DefaultHead, TargetHead: rec.TargetHead,
	}, now); err != nil {
		return err
	}
	if _, err := appendEventTx(tx, evSyncBehindStack, job.ProjectID, 0, job.ID,
		eventPayload{Branch: target, Count: int64(rec.Behind)}, now); err != nil {
		return err
	}
	return e.requeueSyncTx(tx, job, now)
}

// syncErrorTx answers a sync that could not run (§7.1): a refused push
// (the target moved past the recorded merge) forgets the waiting merge,
// and any failure re-queues the sync on the backoff. No task parks, for
// the sync has none; the gate stays held until a later attempt settles
// the target.
func (e *Engine) syncErrorTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	if pushRejected(c.Error) {
		if err := forgetSyncMergeTx(tx, job.ProjectID); err != nil {
			return err
		}
	}
	if _, err := appendEventTx(tx, evSyncConflict, job.ProjectID, 0, job.ID,
		eventPayload{Reason: boundNote(c.Error)}, now); err != nil {
		return err
	}
	return e.requeueSyncTx(tx, job, now)
}

// requeueSyncTx queues the sync's next attempt (§7.1) on the not-before
// backoff, grown by the run of consecutive attempts that ended waiting,
// conflicting or behind the stack. The completing job freed the key, so
// the submit creates — or coalesces behind a sync another path already
// queued (a stale-ref drop re-ran the land rule), which settles the
// target just the same.
func (e *Engine) requeueSyncTx(tx *sql.Tx, job *jobRow, now time.Time) error {
	streak, err := syncRequeueStreakTx(tx, job.Key)
	if err != nil {
		return err
	}
	sr, err := e.submitTx(tx, SubmitParams{
		Key:       job.Key,
		ProjectID: job.ProjectID,
		Kind:      KindSync,
		Mode:      ModeFresh,
		Prompt:    "", // no worker
		Labels:    []string{"git"},
		NotBefore: now.Add(retryDelayFor(streak)),
	}, now)
	if err != nil {
		return err
	}
	if sr.Answer != AnsCreated && sr.Answer != AnsPending {
		return fmt.Errorf("re-queue the sync of project %d: submit %s answered %s", job.ProjectID, job.Key, sr.Answer)
	}
	return nil
}

// syncRequeueStreakTx measures a key's current run of consecutive sync
// attempts that ended waiting, conflicting or behind the stack (§7.1),
// newest first — the completing job included, its result already
// written. Zero when the latest attempt ended current or pushed.
func syncRequeueStreakTx(tx *sql.Tx, key string) (int, error) {
	rows, err := tx.Query(`SELECT COALESCE(result, '') FROM jobs WHERE key = ? AND kind = 'sync' ORDER BY id DESC`, key)
	if err != nil {
		return 0, fmt.Errorf("read the sync jobs of key %s: %w", key, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return 0, fmt.Errorf("read the sync jobs of key %s: %w", key, err)
		}
		var rec syncRecord
		if json.Unmarshal([]byte(jobResultText(stored)), &rec) != nil || !syncRequeues(rec) {
			break
		}
		n++
	}
	return n, rows.Err()
}

// syncRequeues reports whether a sync outcome re-queues the sync on the
// backoff (§7.1): a merge waiting at the gate, a conflict, or a queue
// behind the stack. Current and a pushed merge open the gate instead.
func syncRequeues(rec syncRecord) bool {
	switch rec.Outcome {
	case syncOutcomeConflict, syncOutcomeBehindStack:
		return true
	case syncOutcomeMerged:
		return rec.Pushed == "waiting" || rec.Pushed == "local"
	}
	return false
}

// wakeBehindStackSyncTx re-queues a behind-stack sync at once when the
// last waiting landing of its target settles (§7.1) — pushed, or
// forgotten with a stale ref: the stack no longer blocks the merge, so
// the queued sync's not-before clears and the next poll leases it.
func (e *Engine) wakeBehindStackSyncTx(tx *sql.Tx, project int64, now time.Time) error {
	rec, err := landSyncTx(tx, project)
	if err != nil || rec == nil || rec.State != syncStateBehindStack {
		return err
	}
	stack, err := waitingStackTx(tx, project, rec.TargetBranch)
	if err != nil || stack {
		return err
	}
	live, err := liveJobTx(tx, syncKey(project))
	if err != nil || live == nil || live.Status != statusQueued {
		return err
	}
	if _, err := tx.Exec("UPDATE jobs SET not_before = NULL, updated_at = ? WHERE id = ?", fmtTime(now), live.ID); err != nil {
		return fmt.Errorf("wake the behind-stack sync of project %d: %w", project, err)
	}
	return nil
}

// syncStaleDroppedTx applies a sync attempt's drop of the target's stale
// waiting ref (§7.1, the landing record's drop): the landings that waited
// on it are forgotten and land again, and the waiting sync's merge is
// dropped the same way — the sync runs again.
func (e *Engine) syncStaleDroppedTx(tx *sql.Tx, job *jobRow, target string, now time.Time) error {
	if err := e.clearStaleLandRefTx(tx, job.ProjectID, target); err != nil {
		return err
	}
	if err := e.forgetWaitingLandingsTx(tx, job.ProjectID, target, 0, now); err != nil {
		return err
	}
	return forgetSyncMergeTx(tx, job.ProjectID)
}

// releaseSyncPushTx releases a waiting sync merge at the push gate
// (§7.1), the sync's share of `brigada push approve`: the pending
// backoff attempt is cancelled and a push-phase sync job queues, pushing
// the recorded merge commit with no new merge. A local merge under never
// is the user's hand push alone, as a local landing is. ok reports
// whether a merge was released.
func (e *Engine) releaseSyncPushTx(tx *sql.Tx, project int64, now time.Time) (bool, error) {
	rec, err := landSyncTx(tx, project)
	if err != nil || rec == nil || rec.State != syncStateWaiting || rec.Pushed != "waiting" || rec.MergeCommit == "" {
		return false, err
	}
	if live, err := liveJobTx(tx, syncKey(project)); err != nil {
		return false, err
	} else if live != nil {
		if _, err := e.cancelTx(tx, live, "the user approved the sync push", now); err != nil {
			return false, err
		}
	}
	payload, err := json.Marshal(syncPayload{Push: true})
	if err != nil {
		return false, fmt.Errorf("encode the sync push of project %d: %w", project, err)
	}
	if _, err := e.submitTx(tx, SubmitParams{
		Key:       syncKey(project),
		ProjectID: project,
		Kind:      KindSync,
		Mode:      ModeFresh,
		Prompt:    string(payload),
		Labels:    []string{"git"},
	}, now); err != nil {
		return false, err
	}
	return true, nil
}

// statusSyncTx is the needs-you entries of a project's sync (§7.1, §10):
// one line for the project when brigada cannot clear the target alone —
// a merge waiting at the push gate (ask names both branches and the
// waiting commit; never carries the exact hand push), or a conflicting
// merge naming the files and the count behind. A current target, or one
// behind a waiting stack the sync will settle, names nothing.
func statusSyncTx(tx *sql.Tx, project int64) ([]NeedsYouItem, error) {
	query := `SELECT s.project_id, s.state, s.behind, s.default_branch, s.target_branch, s.merge_commit, s.pushed, s.push_cmd, s.conflict_files, p.repo_url
		FROM land_syncs s JOIN projects p ON p.id = s.project_id
		WHERE p.archived_at IS NULL AND s.state IN ('waiting', 'conflict')`
	args := []any{}
	if project != 0 {
		query += " AND s.project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY s.project_id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the sync waits: %w", err)
	}
	defer rows.Close()
	var items []NeedsYouItem
	for rows.Next() {
		var it NeedsYouItem
		var state, def, target, merge, pushed, cmd, files string
		var behind int
		if err := rows.Scan(&it.ID, &state, &behind, &def, &target, &merge, &pushed, &cmd, &files, &it.Title); err != nil {
			return nil, fmt.Errorf("list the sync waits: %w", err)
		}
		it.Kind = "project"
		it.State = state
		it.Title = RedactRepoURL(it.Title)
		switch state {
		case syncStateConflict:
			var paths []string
			_ = json.Unmarshal([]byte(files), &paths)
			it.Reason = fmt.Sprintf("land target is %d commits behind %s and the merge conflicts in %s: merge %s into %s by hand and push, or clear land_target",
				behind, def, strings.Join(paths, ", "), def, target)
		case syncStateWaiting:
			if pushed == "local" {
				it.Reason = "push by hand: " + cmd
			} else {
				it.Reason = fmt.Sprintf("waits to push: merge %s into %s at %s; release: brigada push approve", def, target, merge)
			}
		}
		it.Reason = oneLine(it.Reason, needsYouReasonLimit)
		items = append(items, it)
	}
	return items, rows.Err()
}
