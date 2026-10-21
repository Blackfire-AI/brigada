package engine

import (
	"context"
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
// failed — the conflicting files or the check log. For gate_push never
// it also carries the exact push command, printed for the user; for a
// run whose after_land failed it carries that failure, reported and
// never undone (§6.3).
type landRecord struct {
	TargetBranch  string   `json:"target_branch"`
	Commit        string   `json:"commit"`
	CheckPassed   bool     `json:"check_passed"`
	Pushed        string   `json:"pushed"` // 'pushed', 'waiting' or 'local'
	PushCmd       string   `json:"push_cmd,omitempty"`
	AfterLandErr  string   `json:"after_land_error,omitempty"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
	CheckLog      string   `json:"check_log,omitempty"`
}

// pushRecord is a push phase's completion (§7.1): the land job, reused
// for the push after `brigada push approve`, reports the push's outcome
// on the landing record that already exists — the commit was squashed
// once and is not re-made. AfterLandErr carries the after_land failure
// that follows the push, reported and never undone (§6.3).
type pushRecord struct {
	TargetBranch string `json:"target_branch"`
	Commit       string `json:"commit"`
	Pushed       string `json:"pushed"` // 'pushed' on success
	AfterLandErr string `json:"after_land_error,omitempty"`
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
	// A land_holds row of any state — held at the gate, or rejected — means
	// the landing is already decided for this task: the rule never re-holds
	// or re-queues it. The user's release deletes the row and queues the
	// land job directly (§6.3), so a released task does not re-trip the
	// gate here.
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
// class's dependency has nothing to wait for. A landing that waits at
// the push gate counts: the runner squashes the dependent onto the
// remote's waiting ref, on top of the dependency's commit, so the stack
// keeps its order when the gate releases it.
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
	// The protected-path rule matches the paths the task changed. The
	// engine runs no git (product §5 rule 1), so the runner records the
	// task's real changed paths (`git diff --name-only base...branch`)
	// with the work job's result and they are what the rule matches;
	// the spec and check text stand in only when no paths are recorded
	// — an older runner, or a task whose work predates the recording —
	// the same proxy the plan gate uses (§6.3).
	var spec, check, tier, changed string
	if err := tx.QueryRow("SELECT spec, COALESCE(check_cmd, ''), tier, COALESCE(changed_paths, '') FROM tasks WHERE id = ?", taskID).
		Scan(&spec, &check, &tier, &changed); err != nil {
		return nil, fmt.Errorf("read task %d: %w", taskID, err)
	}
	if slices.Contains(s.ApprovalTiers, tier) {
		reasons = append(reasons, tier)
	}
	paths := spec + "\n" + check
	if changed != "" {
		var real []string
		if json.Unmarshal([]byte(changed), &real) == nil {
			paths = strings.Join(real, "\n")
		}
	}
	seen := map[string]bool{}
	for _, p := range matchProtected(s.ProtectedPaths, paths) {
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
		WHERE h.state = 'held' AND p.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)`
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
// record the runner reported, or — when the task already has a landing
// waiting at the push gate — the push phase's completion, which updates
// that same record in place. A conflict or a failed check on the landed
// result parks the task at needs_foreman with the files or the check log,
// nothing pushed, and the foreman restarts it with a note. Success keeps
// the task done and marks it landed.
func (e *Engine) landCompletedTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	if c.Error != "" {
		// The runner could not attempt the landing at all (git failed):
		// the task waits for the foreman like any failure.
		return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{Reason: boundNote(c.Error)}, now)
	}
	// The push phase reuses the land job (§7.1): the landing record
	// already exists — pushed = waiting — and this completion is the
	// push's, decoded and applied to that record, never re-squashed.
	if existing, err := landingTx(tx, job.TaskID); err != nil {
		return err
	} else if existing != nil {
		var pr pushRecord
		if err := json.Unmarshal([]byte(c.Result), &pr); err != nil {
			return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{Reason: "unreadable push record"}, now)
		}
		return e.pushCompletedTx(tx, job, existing, pr, now)
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

// landingRow is one landings row as the engine reads it (§7.1): the
// landing record the land job wrote, and the push phase updates.
type landingRow struct {
	TaskID         int64
	TargetBranch   string
	Commit         string
	Pushed         string
	PushCmd        string
	AfterLandError string
}

// landingTx reads the task's landing record; nil, nil when it has none.
func landingTx(tx *sql.Tx, taskID int64) (*landingRow, error) {
	l := &landingRow{TaskID: taskID}
	err := tx.QueryRow(`SELECT target_branch, commit_sha, pushed, push_cmd, after_land_error
		FROM landings WHERE task_id = ?`, taskID).
		Scan(&l.TargetBranch, &l.Commit, &l.Pushed, &l.PushCmd, &l.AfterLandError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the landing of task %d: %w", taskID, err)
	}
	return l, nil
}

// landSucceededTx marks the task landed (§7.1): the landing record is
// written — the engine's "landed" mark, carrying the push state the
// gate made (pushed, waiting at the push gate, or local with the exact
// push command) and any after_land failure — and the dependents'
// landings are re-evaluated. The task stays done.
func (e *Engine) landSucceededTx(tx *sql.Tx, job *jobRow, rec landRecord, now time.Time) error {
	pushed := rec.Pushed
	if pushed == "" {
		pushed = "waiting"
	}
	if _, err := tx.Exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, push_cmd, after_land_error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		job.TaskID, job.ProjectID, rec.TargetBranch, rec.Commit, pushed, rec.PushCmd, rec.AfterLandErr, fmtTime(now)); err != nil {
		return fmt.Errorf("record the landing of task %d: %w", job.TaskID, err)
	}
	if _, err := appendEventTx(tx, evTaskLanded, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: taskDone, Branch: rec.TargetBranch, Commit: rec.Commit, Pushed: pushed,
			PushCmd: rec.PushCmd, AfterLandError: rec.AfterLandErr}, now); err != nil {
		return err
	}
	if err := e.releaseDependentLandsTx(tx, job.TaskID, now); err != nil {
		return err
	}
	return e.finishLandPlanTx(tx, job, now)
}

// liveLandJobTx reports whether the task has a land job queued, leased
// or running (§7.1): a landing, or its push, still under way.
func liveLandJobTx(tx *sql.Tx, taskID int64) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'
		AND status IN ('queued', 'leased', 'running')`, taskID).Scan(&n); err != nil {
		return false, fmt.Errorf("read the land jobs of task %d: %w", taskID, err)
	}
	return n > 0, nil
}

// finishLandPlanTx closes the task's plan once its land job has an
// outcome (§6.2, §7.1): a live land job kept the plan open, so the
// landing's completion is where the plan's done report is written.
func (e *Engine) finishLandPlanTx(tx *sql.Tx, job *jobRow, now time.Time) error {
	var plan int64
	if err := tx.QueryRow("SELECT plan_id FROM tasks WHERE id = ?", job.TaskID).Scan(&plan); err != nil {
		return fmt.Errorf("read the plan of task %d: %w", job.TaskID, err)
	}
	_, err := e.finishPlanIfClosedTx(tx, job.ProjectID, plan, job.TaskID, job.ID, now)
	return err
}

// pushCompletedTx applies a push phase's completion (§7.1): the landing
// record's push state moves from waiting to pushed — the squash is never
// re-made — and the after_land failure that followed the push is reported
// in the record and the history, never undone, never changing the task's
// state. A malformed or failed push leaves the record waiting.
func (e *Engine) pushCompletedTx(tx *sql.Tx, job *jobRow, existing *landingRow, pr pushRecord, now time.Time) error {
	if pr.TargetBranch == "" {
		pr.TargetBranch = existing.TargetBranch
	}
	if pr.Commit == "" {
		pr.Commit = existing.Commit
	}
	if pr.TargetBranch != existing.TargetBranch || pr.Commit != existing.Commit {
		return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{Reason: "the push names a landing other than the recorded one"}, now)
	}
	if pr.Pushed != "pushed" {
		return e.needsForemanTx(tx, job, evTaskLandFailed, eventPayload{Reason: "the push did not push"}, now)
	}
	if _, err := tx.Exec(`UPDATE landings SET pushed = 'pushed', after_land_error = ? WHERE task_id = ?`,
		pr.AfterLandErr, job.TaskID); err != nil {
		return fmt.Errorf("record the push of task %d: %w", job.TaskID, err)
	}
	if _, err := appendEventTx(tx, evTaskPushed, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: taskDone, Branch: pr.TargetBranch, Commit: pr.Commit, Pushed: "pushed",
			AfterLandError: pr.AfterLandErr}, now); err != nil {
		return err
	}
	return e.finishLandPlanTx(tx, job, now)
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

// LandMessage is the task data a land job's squash commit message is
// built from (§7.1): the fields of internal/landing's Task the engine
// knows without running git — what the hand-driven land reads through
// the API, read here from the task, its last author job and that job's
// agent. The runner adds the branch and its fetched head.
type LandMessage struct {
	Title   string `json:"title"`
	Spec    string `json:"spec"`
	Summary string `json:"summary"`
	Family  string `json:"family"`
	Model   string `json:"model"`
}

// landMessageTx reads a task's squash-message data (§7.1): the title
// and spec from the task, the worker's summary from its last done
// author job, and that job's agent family and model. A task with no
// done author job keeps the empty fields; none can reach a land job.
func landMessageTx(tx *sql.Tx, taskID int64) (*LandMessage, error) {
	m := &LandMessage{}
	if err := tx.QueryRow("SELECT title, spec FROM tasks WHERE id = ?", taskID).
		Scan(&m.Title, &m.Spec); err != nil {
		return nil, fmt.Errorf("read task %d: %w", taskID, err)
	}
	var agent int64
	var authorResult string
	err := tx.QueryRow(`SELECT COALESCE(agent_id, 0), COALESCE(result, '') FROM jobs
		WHERE task_id = ? AND kind IN ('work', 'fix') AND status = 'done' ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&agent, &authorResult)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the last author job of task %d: %w", taskID, err)
	}
	m.Summary = jobResultSummary(authorResult)
	if agent != 0 {
		err := tx.QueryRow("SELECT family, model FROM agents WHERE id = ?", agent).Scan(&m.Family, &m.Model)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read agent %d: %w", agent, err)
		}
	}
	return m, nil
}

// landLeaseGrant builds the grant a land job's lease hands the runner
// (§7.1): the repo, the task's result branch to squash and its base for
// the diff, the target branch, the project's default check to rerun on
// the result, the push gate the runner obeys, and the after_land command
// it runs after the push (or after the landing when the gate is never).
// No worker, no agent, no prompt. A land job whose task already has a
// landing record is the push phase (§7.1): the grant carries the target
// and the recorded commit alone, and the runner pushes — no new squash.
func (e *Engine) landLeaseGrantTx(tx *sql.Tx, j *jobRow, owner int64, leaseID string, expires time.Time) (*LeaseGrant, error) {
	s, err := projectApprovalTx(tx, j.ProjectID)
	if err != nil {
		return nil, err
	}
	var defaultBranch string
	if err := tx.QueryRow("SELECT default_branch FROM projects WHERE id = ?", j.ProjectID).Scan(&defaultBranch); err != nil {
		return nil, fmt.Errorf("read the default branch of project %d: %w", j.ProjectID, err)
	}
	target := s.LandTarget
	if target == "" {
		target = defaultBranch
	}
	// The push phase reuses the land job: the landing record exists, so
	// the grant is the push's alone — the repo to push to, the target
	// and the recorded commit. No runner affinity: the commit waits on
	// the remote's waiting ref, where any runner fetches it.
	if existing, err := landingTx(tx, j.TaskID); err != nil {
		return nil, err
	} else if existing != nil {
		repo, _, _, _, err := taskInfoTx(tx, j)
		if err != nil {
			return nil, err
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
			LandTarget:     existing.TargetBranch,
			DefaultBranch:  defaultBranch,
			LandCommit:     existing.Commit,
			Push:           true,
			AfterLand:      s.AfterLand,
			Attempt:        j.Attempts + 1,
		}, nil
	}
	repo, baseRef, _, branch, err := taskInfoTx(tx, j)
	if err != nil {
		return nil, err
	}
	msg, err := landMessageTx(tx, j.TaskID)
	if err != nil {
		return nil, err
	}
	// The landing check runs as a work's does (§5.7, §5.8): after the
	// task's recorded setup, with its extra checkouts named.
	setup, err := taskSetupTx(tx, j.TaskID)
	if err != nil {
		return nil, err
	}
	extraRepos, err := taskExtraReposTx(tx, j.TaskID)
	if err != nil {
		return nil, err
	}
	gatePush := s.GatePush
	if gatePush == "" {
		gatePush = GateAsk // the design's default (§6.3), which projectApprovalTx leaves unapplied
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
		Setup:          setup,
		ExtraRepos:     extraRepos,
		Branch:         branch,
		LandTarget:     target,
		DefaultBranch:  defaultBranch,
		LandMessage:    msg,
		GatePush:       gatePush,
		AfterLand:      s.AfterLand,
		Attempt:        j.Attempts + 1,
	}, nil
}

// LandDecision is what releasing or cancelling a land-gate hold did
// (§6.3): the task, and whether the landing now runs (Released) or is
// cancelled — the branch left alone, the hold row kept as rejected so the
// rule never re-holds a landing the user refused.
type LandDecision struct {
	TaskID   int64 `json:"task_id"`
	Released bool  `json:"released"` // true for an approval, false for a rejection
}

// LandApprove releases a task held at the land gate (§6.3): the hold's
// row is deleted, the event names the release, and the land job is queued
// directly — the user's yes overrides the gate, which would otherwise
// re-hold the task. The foreman has no tool for it: the gate is the
// user's.
func (e *Engine) LandApprove(ctx context.Context, project, taskID int64) (LandDecision, error) {
	return e.landDecision(ctx, project, taskID, true)
}

// LandReject cancels the landing of a task held at the land gate (§6.3):
// the hold's row is kept, marked rejected, and the event records the
// cancellation. The task stays done and its branch is left alone —
// nothing is squashed, nothing is pushed, and the landing is never
// recorded.
func (e *Engine) LandReject(ctx context.Context, project, taskID int64) (LandDecision, error) {
	return e.landDecision(ctx, project, taskID, false)
}

func (e *Engine) landDecision(ctx context.Context, project, taskID int64, approve bool) (LandDecision, error) {
	var res LandDecision
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.landDecisionTx(tx, project, taskID, approve, now)
		if err != nil {
			return fmt.Errorf("decide the landing of task %d: %w", taskID, err)
		}
		res = r
		return nil
	})
	return res, err
}

// landDecisionTx applies the user's decision on a land-gate hold (§6.3).
func (e *Engine) landDecisionTx(tx *sql.Tx, project, taskID int64, approve bool, now time.Time) (LandDecision, error) {
	reason, err := landHoldTaskTx(tx, project, taskID)
	if err != nil {
		return LandDecision{}, err
	}
	if approve {
		if _, err := tx.Exec(`DELETE FROM land_holds WHERE task_id = ?`, taskID); err != nil {
			return LandDecision{}, fmt.Errorf("release the land hold of task %d: %w", taskID, err)
		}
		if _, err := appendEventTx(tx, evTaskLandReleased, project, taskID, 0,
			eventPayload{Status: taskDone, Reason: boundNote(reason)}, now); err != nil {
			return LandDecision{}, err
		}
		// The user passed the gate: queue the land job directly. Running
		// the land rule again would re-hold the same task, since the gate
		// still trips — the user's yes is what overrides it (§6.3). The
		// rule checked the dependencies before it held, so they are still
		// landed.
		if err := e.queueLandJobTx(tx, project, taskID, now); err != nil {
			return LandDecision{}, err
		}
		return LandDecision{TaskID: taskID, Released: true}, nil
	}
	// Reject keeps the row, marked rejected: the landing is cancelled,
	// the branch stays, and the rule never re-holds what the user refused.
	if _, err := tx.Exec(`UPDATE land_holds SET state = 'rejected' WHERE task_id = ?`, taskID); err != nil {
		return LandDecision{}, fmt.Errorf("reject the landing of task %d: %w", taskID, err)
	}
	if _, err := appendEventTx(tx, evTaskLandRejected, project, taskID, 0,
		eventPayload{Status: taskDone, Reason: boundNote(reason)}, now); err != nil {
		return LandDecision{}, err
	}
	return LandDecision{TaskID: taskID, Released: false}, nil
}

// landHoldTaskTx validates the task the land decision acts on (§6.3): a
// task of project with a live land-gate hold. It returns the hold's
// reason.
func landHoldTaskTx(tx *sql.Tx, project, taskID int64) (string, error) {
	var reason string
	var proj int64
	err := tx.QueryRow(`SELECT COALESCE(h.reason, ''), p.project_id
		FROM tasks t JOIN plans p ON p.id = t.plan_id
		LEFT JOIN land_holds h ON h.task_id = t.id AND h.state = 'held'
		WHERE t.id = ?`, taskID).Scan(&reason, &proj)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && project != 0 && proj != project) {
		return "", fmt.Errorf("no such task %d", taskID)
	}
	if err != nil {
		return "", fmt.Errorf("read the land hold of task %d: %w", taskID, err)
	}
	if err := activeProjectTx(tx, proj); err != nil {
		return "", err
	}
	if reason == "" {
		return "", fmt.Errorf("task %d is not held at the land gate", taskID)
	}
	return reason, nil
}

// PushApproveResult is what releasing the push gate did (§6.3): how many
// waiting landings now have a push job queued.
type PushApproveResult struct {
	// Released counts the landings moved from waiting to a queued push.
	Released int `json:"released"`
}

// PushApprove releases the push gate for the project's waiting landings
// (§6.3): each landing recorded as waiting gets a push job — the land job
// reused, its push phase — which a runner leases and completes by
// pushing the recorded commit, no new squash. The foreman has no tool
// for it: the gate is the user's. The waiting landings stack on the
// remote's waiting ref, each on the one before, so their pushes are
// fast-forwards in any order and on any runner: a push whose commit a
// later landing already carried is pushed already. repoURL names the
// project, as every client command does.
func (e *Engine) PushApprove(ctx context.Context, repoURL string) (PushApproveResult, error) {
	if repoURL == "" {
		return PushApproveResult{}, fmt.Errorf("repo URL is required (product §5 rule 6: repos are addressed by URL)")
	}
	var res PushApproveResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no project for repo %s", RedactRepoURL(repoURL))
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		if err := activeProjectTx(tx, project); err != nil {
			return err
		}
		r, err := e.pushApproveTx(tx, project, now)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return PushApproveResult{}, fmt.Errorf("approve the push: %w", err)
	}
	return res, nil
}

func (e *Engine) pushApproveTx(tx *sql.Tx, project int64, now time.Time) (PushApproveResult, error) {
	query := `SELECT l.task_id, l.project_id
		FROM landings l JOIN tasks t ON t.id = l.task_id JOIN plans p ON p.id = t.plan_id
		WHERE l.pushed = 'waiting' AND p.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.task_id = l.task_id AND j.kind = 'land' AND j.status IN ('queued', 'leased', 'running'))`
	args := []any{}
	if project != 0 {
		query += " AND p.project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY l.task_id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return PushApproveResult{}, fmt.Errorf("list the waiting landings: %w", err)
	}
	defer rows.Close()
	type wait struct {
		task, project int64
	}
	var waits []wait
	for rows.Next() {
		var w wait
		if err := rows.Scan(&w.task, &w.project); err != nil {
			return PushApproveResult{}, fmt.Errorf("list the waiting landings: %w", err)
		}
		waits = append(waits, w)
	}
	if err := rows.Err(); err != nil {
		return PushApproveResult{}, fmt.Errorf("list the waiting landings: %w", err)
	}
	rows.Close()
	for _, w := range waits {
		// The push job reuses the land job's key, kind and git label, so
		// a repeat approve coalesces behind a live push (§7.1).
		if _, err := e.submitTx(tx, SubmitParams{
			Key:       fmt.Sprintf("task:%d", w.task),
			ProjectID: w.project,
			TaskID:    w.task,
			Kind:      KindLand,
			Mode:      ModeFresh,
			Prompt:    "", // no worker
			Labels:    []string{"git"},
		}, now); err != nil {
			return PushApproveResult{}, err
		}
	}
	if len(waits) > 0 {
		if _, err := appendEventTx(tx, evPushApproved, project, 0, 0,
			eventPayload{Count: int64(len(waits))}, now); err != nil {
			return PushApproveResult{}, err
		}
	}
	return PushApproveResult{Released: len(waits)}, nil
}

// statusPushWaitsTx is the needs-you entries of the landings waiting at
// the push gate (§6.3, §10): a landing recorded as waiting, with no live
// push job on the task's key — a queued or running push is on its way,
// not waiting for the user. The task stays done; only its push waits.
func statusPushWaitsTx(tx *sql.Tx, project int64) ([]NeedsYouItem, error) {
	query := `SELECT l.task_id, t.title, t.plan_id, t.state, l.target_branch, l.commit_sha
		FROM landings l JOIN tasks t ON t.id = l.task_id JOIN plans p ON p.id = t.plan_id
		WHERE l.pushed = 'waiting' AND p.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.task_id = l.task_id AND j.kind = 'land' AND j.status IN ('queued', 'leased', 'running'))`
	args := []any{}
	if project != 0 {
		query += " AND p.project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY l.task_id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the push-gate waits: %w", err)
	}
	defer rows.Close()
	var items []NeedsYouItem
	for rows.Next() {
		var it NeedsYouItem
		var state, branch, sha string
		if err := rows.Scan(&it.ID, &it.Title, &it.PlanID, &state, &branch, &sha); err != nil {
			return nil, fmt.Errorf("list the push-gate waits: %w", err)
		}
		it.Kind = "task"
		it.State = state
		it.Reason = "waits to push: " + branch + "@" + sha
		items = append(items, it)
	}
	return items, rows.Err()
}

// statusLocalLandsTx is the needs-you entries of the landings gate_push
// never recorded as local (§6.3, §10): the push is the user's alone, so
// the entry carries the exact recorded command — fetch the durable ref,
// push it to the target — for the user to run by hand. The task stays
// done; nothing of brigada's waits.
func statusLocalLandsTx(tx *sql.Tx, project int64) ([]NeedsYouItem, error) {
	query := `SELECT l.task_id, t.title, t.plan_id, t.state, l.push_cmd
		FROM landings l JOIN tasks t ON t.id = l.task_id JOIN plans p ON p.id = t.plan_id
		WHERE l.pushed = 'local' AND l.push_cmd != '' AND p.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)`
	args := []any{}
	if project != 0 {
		query += " AND p.project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY l.task_id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the local landings: %w", err)
	}
	defer rows.Close()
	var items []NeedsYouItem
	for rows.Next() {
		var it NeedsYouItem
		var state, cmd string
		if err := rows.Scan(&it.ID, &it.Title, &it.PlanID, &state, &cmd); err != nil {
			return nil, fmt.Errorf("list the local landings: %w", err)
		}
		it.Kind = "task"
		it.State = state
		it.Reason = "push by hand: " + cmd
		items = append(items, it)
	}
	return items, rows.Err()
}
