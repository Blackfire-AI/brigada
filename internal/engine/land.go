package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The land rule (§7.1, decided 2026-10-20): when a task is done — class
// implement, an approve verdict on its last review — and every dependency
// is landed, the engine creates one job of kind land for it, which a
// runner with git for the project's repo leases and completes with the
// landing record. A land-gate rule (§6.3) holds the task in the needs-you
// queue instead, naming the rule; only its dependents' landings wait on
// it. The foreman has no tool for any of this: the hold is the user's to
// release (the release endpoint ships next to this, not here), and the
// job is the engine's own.

// landRecord is the landing record a land job's completion carries
// (§7.1): the target branch, the resulting commit, whether the check on
// the landed result passed, the push state, and — when the landing
// failed — the conflicting files or the check log.
type landRecord struct {
	TargetBranch  string   `json:"target_branch"`
	Commit        string   `json:"commit"`
	CheckPassed   bool     `json:"check_passed"`
	Pushed        string   `json:"pushed"` // 'pushed', 'waiting' or 'local'
	ConflictFiles []string `json:"conflict_files,omitempty"`
	CheckLog      string   `json:"check_log,omitempty"`
}

// maybeLandTaskTx runs the land rule for one task (§7.1). A task that is
// done, implement, approved and whose dependencies are all landed gets
// its land job queued; a land-gate rule that trips holds it instead. The
// rule is idempotent: a task already landed, already held, or with a live
// job on its key is left alone, so the callers that re-evaluate it — a
// dependency landing, a hold release — never queue a second job.
func (e *Engine) maybeLandTaskTx(tx *sql.Tx, taskID int64, now time.Time) error {
	var state, class string
	var plan int64
	if err := tx.QueryRow("SELECT state, class, plan_id FROM tasks WHERE id = ?", taskID).
		Scan(&state, &class, &plan); err != nil {
		return fmt.Errorf("read task %d: %w", taskID, err)
	}
	if state != taskDone || class != classImplement {
		return nil
	}
	approved, err := taskApprovedTx(tx, taskID)
	if err != nil {
		return err
	}
	if !approved {
		return nil
	}
	landed, err := depsLandedTx(tx, taskID)
	if err != nil {
		return err
	}
	if !landed {
		return nil
	}
	var project int64
	if err := tx.QueryRow("SELECT project_id FROM plans WHERE id = ?", plan).Scan(&project); err != nil {
		return fmt.Errorf("read the project of task %d: %w", taskID, err)
	}
	var have int
	if err := tx.QueryRow("SELECT COUNT(*) FROM landings WHERE task_id = ?", taskID).Scan(&have); err != nil {
		return fmt.Errorf("read the landing of task %d: %w", taskID, err)
	}
	if have > 0 {
		return nil
	}
	var held int
	if err := tx.QueryRow("SELECT COUNT(*) FROM land_holds WHERE task_id = ?", taskID).Scan(&held); err != nil {
		return fmt.Errorf("read the land hold of task %d: %w", taskID, err)
	}
	if held > 0 {
		return nil
	}
	if live, err := liveJobTx(tx, fmt.Sprintf("task:%d", taskID)); err != nil {
		return err
	} else if live != nil {
		return nil // the land job (or another of the task's jobs) already holds the key
	}
	reasons, err := landHoldReasonsTx(tx, project, taskID)
	if err != nil {
		return err
	}
	if len(reasons) > 0 {
		return e.holdLandTx(tx, project, taskID, reasons, now)
	}
	return e.queueLandJobTx(tx, project, taskID, now)
}

// taskApprovedTx reports whether the task's last review, after its last
// author job, was a substantive approve (§7) — the verdict that lets an
// implement task land.
func taskApprovedTx(tx *sql.Tx, taskID int64) (bool, error) {
	var authorID int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM jobs
		WHERE task_id = ? AND kind IN ('work', 'fix') AND status = 'done'`, taskID).Scan(&authorID); err != nil {
		return false, fmt.Errorf("read the last author job of task %d: %w", taskID, err)
	}
	if authorID == 0 {
		return false, nil
	}
	var reviewResult string
	err := tx.QueryRow(`SELECT COALESCE(result, '') FROM jobs
		WHERE task_id = ? AND kind = 'review' AND status = 'done' AND id > ?
		ORDER BY id DESC LIMIT 1`, taskID, authorID).Scan(&reviewResult)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the last review of task %d: %w", taskID, err)
	}
	var v verdictBlock
	if json.Unmarshal([]byte(jobResultText(reviewResult)), &v) != nil {
		return false, nil
	}
	return v.Verdict == verdictApprove && len(v.Findings) >= minSubstantiveFindings, nil
}

// depsLandedTx reports whether every dependency of taskID is landed
// (§7.1): a dependency with a landing record, or a dependency of a class
// that never lands — only implement tasks are landable, so another
// class's dependency has nothing to wait for.
func depsLandedTx(tx *sql.Tx, taskID int64) (bool, error) {
	rows, err := tx.Query("SELECT depends_on_task_id FROM task_deps WHERE task_id = ?", taskID)
	if err != nil {
		return false, fmt.Errorf("read the dependencies of task %d: %w", taskID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var dep int64
		if err := rows.Scan(&dep); err != nil {
			return false, fmt.Errorf("read the dependencies of task %d: %w", taskID, err)
		}
		var class string
		if err := tx.QueryRow("SELECT class FROM tasks WHERE id = ?", dep).Scan(&class); err != nil {
			return false, fmt.Errorf("read the class of dependency %d: %w", dep, err)
		}
		if class != classImplement {
			continue
		}
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM landings WHERE task_id = ?", dep).Scan(&n); err != nil {
			return false, fmt.Errorf("read the landing of dependency %d: %w", dep, err)
		}
		if n == 0 {
			return false, nil
		}
	}
	return true, rows.Err()
}

// landHoldReasonsTx names the rules that hold a task at the land gate
// (§6.3), empty when the landing runs on: gate_land is ask; the task's
// tier among approval_tiers; a protected path the task changed; or
// approval_restarted with a task that spent a restart or a tier bump.
// The reasons order matches the needs-you line's example — "heavy;
// touches infra/**" — tier before paths.
func landHoldReasonsTx(tx *sql.Tx, project, taskID int64) ([]string, error) {
	s, err := projectApprovalTx(tx, project)
	if err != nil {
		return nil, err
	}
	var reasons []string
	if s.GateLand == GateAsk {
		reasons = append(reasons, "gate_land is "+GateAsk)
	}
	var spec, check, tier string
	if err := tx.QueryRow("SELECT spec, COALESCE(check_cmd, ''), tier FROM tasks WHERE id = ?", taskID).
		Scan(&spec, &check, &tier); err != nil {
		return nil, fmt.Errorf("read task %d: %w", taskID, err)
	}
	if slices.Contains(s.ApprovalTiers, tier) {
		reasons = append(reasons, tier)
	}
	// The protected-path rule matches the paths the task changed. The
	// engine runs no git (product §5 rule 1), and the work job's result
	// block does not yet carry the changed paths (§7.1's approved-head
	// gap, §16 item 10), so until the runner records them the spec and
	// check text stand in — the same proxy the plan gate uses (§6.3). A
	// task that changes infra/** without mentioning it therefore lands
	// without a hold for now; the runner that reports the paths closes
	// this gap.
	seen := map[string]bool{}
	for _, p := range matchProtected(s.ProtectedPaths, spec+"\n"+check) {
		if !seen[p] {
			seen[p] = true
			reasons = append(reasons, "touches "+p)
		}
	}
	if s.ApprovalRestarted {
		restarted, err := taskSpentRestartTx(tx, taskID)
		if err != nil {
			return nil, err
		}
		if restarted {
			reasons = append(reasons, "restarted")
		}
	}
	return reasons, nil
}

// taskSpentRestartTx reports whether the task spent a restart or a tier
// bump (§6.3): any restart job — an explicit restart, an escalation's
// effort step or tier step all queue one (§5.6, §8) — or a
// task_tier_bumped event.
func taskSpentRestartTx(tx *sql.Tx, taskID int64) (bool, error) {
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", taskID).Scan(&n); err != nil {
		return false, fmt.Errorf("count the restarts of task %d: %w", taskID, err)
	}
	if n > 0 {
		return true, nil
	}
	if err := tx.QueryRow("SELECT COUNT(*) FROM events WHERE task_id = ? AND kind = ?", taskID, evTaskTierBumped).Scan(&n); err != nil {
		return false, fmt.Errorf("count the tier bumps of task %d: %w", taskID, err)
	}
	return n > 0, nil
}

// holdLandTx holds a task at the land gate (§6.3): the land_holds row is
// the hold — the task stays done, since its work is finished and only its
// dependents' landings wait on it — and the event names the rule for the
// history and the needs-you queue. The foreman has no tool to release or
// pass the gate; the user's release deletes the row (next to this).
func (e *Engine) holdLandTx(tx *sql.Tx, project, taskID int64, reasons []string, now time.Time) error {
	reason := "waits to land: " + strings.Join(reasons, "; ")
	if _, err := tx.Exec(`INSERT INTO land_holds (task_id, reason, created_at) VALUES (?, ?, ?)`,
		taskID, reason, fmtTime(now)); err != nil {
		return fmt.Errorf("hold task %d at the land gate: %w", taskID, err)
	}
	_, err := appendEventTx(tx, evTaskLandHeld, project, taskID, 0,
		eventPayload{Status: taskDone, Reason: boundNote(reason)}, now)
	return err
}

// queueLandJobTx creates the task's land job (§7.1): kind land on the
// task's own key, so the one-live-job-per-key rule makes a repeat
// coalesce. No worker and no agent; the git label is what a runner
// offers to take it.
func (e *Engine) queueLandJobTx(tx *sql.Tx, project, taskID int64, now time.Time) error {
	sr, err := e.submitTx(tx, SubmitParams{
		Key:       fmt.Sprintf("task:%d", taskID),
		ProjectID: project,
		TaskID:    taskID,
		Kind:      KindLand,
		Mode:      ModeFresh,
		Prompt:    "", // no worker: nothing is composed (§1.4)
		Labels:    []string{"git"},
	}, now)
	if err != nil {
		return err
	}
	if sr.Answer != AnsCreated {
		// A live job already holds the key: the land job exists. Nothing
		// to do — idempotent.
		return nil
	}
	return nil
}

// releaseDependentLandsTx re-runs the land rule for the tasks that depend
// on a task that just landed (§7.1): a dependent that waited on this
// dependency now has it landed, so its own landing may proceed.
func (e *Engine) releaseDependentLandsTx(tx *sql.Tx, landed int64, now time.Time) error {
	rows, err := tx.Query(`SELECT d.task_id FROM task_deps d JOIN tasks t ON t.id = d.task_id
		WHERE d.depends_on_task_id = ? AND t.state = ? ORDER BY d.task_id`, landed, taskDone)
	if err != nil {
		return fmt.Errorf("read the dependents of task %d: %w", landed, err)
	}
	defer rows.Close()
	var deps []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("read the dependents of task %d: %w", landed, err)
		}
		deps = append(deps, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the dependents of task %d: %w", landed, err)
	}
	rows.Close()
	for _, id := range deps {
		if err := e.maybeLandTaskTx(tx, id, now); err != nil {
			return err
		}
	}
	return nil
}

// statusLandHoldsTx is the needs-you entries of the tasks held at the
// land gate (§6.3, §10): the task stays done — only its landing waits —
// so the entry names the hold's rule beside the task's done state.
func statusLandHoldsTx(tx *sql.Tx, project int64) ([]NeedsYouItem, error) {
	query := `SELECT h.task_id, t.title, t.plan_id, t.state, h.reason
		FROM land_holds h JOIN tasks t ON t.id = h.task_id JOIN plans p ON p.id = t.plan_id
		WHERE p.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)`
	args := []any{}
	if project != 0 {
		query += " AND p.project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY h.task_id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the land-gate holds: %w", err)
	}
	defer rows.Close()
	var items []NeedsYouItem
	for rows.Next() {
		var it NeedsYouItem
		var state, reason string
		if err := rows.Scan(&it.ID, &it.Title, &it.PlanID, &state, &reason); err != nil {
			return nil, fmt.Errorf("list the land-gate holds: %w", err)
		}
		it.Kind = "task"
		it.State = state
		it.Reason = reason
		items = append(items, it)
	}
	return items, rows.Err()
}

// validate reports a landing record the engine will not store as a
// success: an empty target branch or commit, or a push state outside the
// landings table's vocabulary. A malformed record parks the task instead
// of failing the completion's transaction and leaving the runner to
// retry forever.
func (r landRecord) validate() error {
	switch {
	case r.TargetBranch == "":
		return errors.New("the landing record names no target branch")
	case r.Commit == "":
		return errors.New("the landing record names no commit")
	case r.Pushed != "" && r.Pushed != "pushed" && r.Pushed != "waiting" && r.Pushed != "local":
		return fmt.Errorf("the landing record's push state %q is not pushed, waiting or local", r.Pushed)
	}
	return nil
}

// landCompletedTx applies a land job's completion (§7.1): the landing
// record the runner reported. A conflict or a failed check on the landed
// result parks the task at needs_foreman with the files or the check log,
// nothing pushed, and the foreman restarts it with a note. Success keeps
// the task done and marks it landed.
func (e *Engine) landCompletedTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	if c.Error != "" {
		// The runner could not attempt the landing at all (git failed):
		// the task waits for the foreman like any failure.
		return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{Reason: boundNote(c.Error)}, now)
	}
	var rec landRecord
	if err := json.Unmarshal([]byte(c.Result), &rec); err != nil {
		return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{Reason: "unreadable landing record"}, now)
	}
	switch {
	case len(rec.ConflictFiles) > 0:
		return e.landConflictTx(tx, job, rec, now)
	case !rec.CheckPassed:
		return e.landCheckFailedTx(tx, job, rec, now)
	default:
		if err := rec.validate(); err != nil {
			return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{Reason: boundNote(err.Error())}, now)
		}
		return e.landSucceededTx(tx, job, rec, now)
	}
}

// landSucceededTx marks the task landed (§7.1): the landing record is
// written — the engine's "landed" mark — and the dependents' landings are
// re-evaluated. The task stays done.
func (e *Engine) landSucceededTx(tx *sql.Tx, job *jobRow, rec landRecord, now time.Time) error {
	pushed := rec.Pushed
	if pushed == "" {
		pushed = "waiting"
	}
	if _, err := tx.Exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		job.TaskID, job.ProjectID, rec.TargetBranch, rec.Commit, pushed, fmtTime(now)); err != nil {
		return fmt.Errorf("record the landing of task %d: %w", job.TaskID, err)
	}
	if _, err := appendEventTx(tx, evTaskLanded, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: taskDone, Branch: rec.TargetBranch, Commit: rec.Commit, Pushed: pushed}, now); err != nil {
		return err
	}
	return e.releaseDependentLandsTx(tx, job.TaskID, now)
}

// landConflictTx parks a task whose diff no longer applies to the target
// (§7.1): nothing is pushed, and the conflicting files name the reason
// the foreman reads.
func (e *Engine) landConflictTx(tx *sql.Tx, job *jobRow, rec landRecord, now time.Time) error {
	files := strings.Join(rec.ConflictFiles, ", ")
	if files == "" {
		files = "the task's diff no longer applies"
	}
	reason := "land conflict: " + files
	return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{Reason: boundNote(reason)}, now)
}

// landCheckFailedTx parks a task whose check failed on the landed result
// (§7.1): nothing is pushed, and the check log names the failure.
func (e *Engine) landCheckFailedTx(tx *sql.Tx, job *jobRow, rec landRecord, now time.Time) error {
	reason := "the check failed on the landed result"
	if rec.CheckLog != "" {
		reason += ": " + boundNote(rec.CheckLog)
	}
	return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{
		Reason:      boundNote(reason),
		CheckOutput: boundNote(rec.CheckLog),
	}, now)
}

// landLeaseGrant builds the grant a land job's lease hands the runner
// (§7.1): the repo, the task's result branch to squash and its base for
// the diff, the target branch, and the project's default check to rerun
// on the result. No worker, no agent, no prompt.
func (e *Engine) landLeaseGrantTx(tx *sql.Tx, j *jobRow, owner int64, leaseID string, expires time.Time) (*LeaseGrant, error) {
	repo, baseRef, _, branch, err := taskInfoTx(tx, j)
	if err != nil {
		return nil, err
	}
	s, err := projectApprovalTx(tx, j.ProjectID)
	if err != nil {
		return nil, err
	}
	target := s.LandTarget
	if target == "" {
		if err := tx.QueryRow("SELECT default_branch FROM projects WHERE id = ?", j.ProjectID).Scan(&target); err != nil {
			return nil, fmt.Errorf("read the default branch of project %d: %w", j.ProjectID, err)
		}
	}
	return &LeaseGrant{
		JobID:          j.ID,
		LeaseOwner:     owner,
		ProjectID:      j.ProjectID,
		TaskID:         j.TaskID,
		Kind:           j.Kind,
		Mode:           j.Mode,
		Key:            j.Key,
		LeaseID:        leaseID,
		LeaseExpiresAt: expires,
		RepoURL:        repo,
		BaseRef:        baseRef,
		CheckCmd:       s.DefaultCheck,
		Branch:         branch,
		LandTarget:     target,
		Attempt:        j.Attempts + 1,
	}, nil
}
