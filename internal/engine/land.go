package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
// release (LandApprove and LandReject below), and the
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

// landHeldRecord is the payload a land job's held outcome carries
// (issue #87): the land_ready command's own hold — exit 1 — with its
// first output line as the reason.
type landHeldRecord struct {
	Outcome string `json:"outcome"` // 'held'
	Line    string `json:"line"`    // the command's first output line
}

// landNotReadyRecord is the payload a land job's not-ready outcome
// carries (issue #87): the command's last exit and a short reason — a
// timeout, a spawn failure — empty when the exit alone speaks.
type landNotReadyRecord struct {
	Outcome string `json:"outcome"` // 'not ready'
	Exit    int    `json:"exit"`    // the command's last exit
	Reason  string `json:"reason,omitempty"`
}

// maybeLandTaskTx runs the land rule for one task (§7.1). A task that is
// done, implement, approved and whose dependencies are all landed gets
// its land job queued; a land-gate rule that trips holds it instead. The
// rule is idempotent: a task already landed, already held, or with a live
// job on its key is left alone, so the callers that re-evaluate it — a
// dependency landing, a hold release — never queue a second job.
func (e *Engine) maybeLandTaskTx(tx *sql.Tx, taskID int64, now time.Time) error {
	var plan int64
	if err := tx.QueryRow("SELECT plan_id FROM tasks WHERE id = ?", taskID).Scan(&plan); err != nil {
		return fmt.Errorf("read task %d: %w", taskID, err)
	}
	landable, err := landableDoneTx(tx, taskID)
	if err != nil {
		return err
	}
	if !landable {
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
	reason, err := landHoldReasonTx(tx, project, taskID)
	if err != nil {
		return err
	}
	if reason != "" {
		return e.holdLandTx(tx, project, taskID, reason, now)
	}
	_, err = e.queueLandJobTx(tx, project, taskID, false, now)
	return err
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

// landableDoneTx reports whether the task is one the land rule queues a
// landing for (§7.1): done, of the implement class — the one class that
// lands — and approved by its last review. The done report's
// not-yet-landed list and the blocked-plan rule's stranded tasks (§6.2)
// read the same eligibility here, so the three can never disagree: a
// done task of a class that never lands is never "not yet landed" and
// never holds its plan open.
func landableDoneTx(tx *sql.Tx, taskID int64) (bool, error) {
	var state, class string
	if err := tx.QueryRow("SELECT state, class FROM tasks WHERE id = ?", taskID).Scan(&state, &class); err != nil {
		return false, fmt.Errorf("read task %d: %w", taskID, err)
	}
	if state != taskDone || class != classImplement {
		return false, nil
	}
	return taskApprovedTx(tx, taskID)
}

// depsLandedTx reports whether every dependency of taskID is landed
// (§7.1): a dependency with a landing record, or a dependency of a class
// that never lands — only implement tasks are landable, so another
// class's dependency has nothing to wait for. A landing whose push
// reached the target counts, and so does one whose push was released —
// the push job the user's approval queued is on its way. A landing
// still waiting at the push gate counts only where the dependent's own
// landing stacks onto the remote's waiting ref, on top of the
// dependency's commit, so the stack keeps its order (§7.1): the ask and
// never gates. Under auto the dependent would squash onto the target
// without the dependency's work, so it waits for the dependency's push.
func depsLandedTx(tx *sql.Tx, taskID int64) (bool, error) {
	rows, err := tx.Query("SELECT depends_on_task_id FROM task_deps WHERE task_id = ?", taskID)
	if err != nil {
		return false, fmt.Errorf("read the dependencies of task %d: %w", taskID, err)
	}
	defer rows.Close()
	var deps []int64
	for rows.Next() {
		var dep int64
		if err := rows.Scan(&dep); err != nil {
			return false, fmt.Errorf("read the dependencies of task %d: %w", taskID, err)
		}
		deps = append(deps, dep)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the dependencies of task %d: %w", taskID, err)
	}
	for _, dep := range deps {
		landed, err := depLandedForTx(tx, taskID, dep)
		if err != nil {
			return false, err
		}
		if !landed {
			return false, nil
		}
	}
	return true, nil
}

// depLandedForTx reports whether the dependency dep of the task taskID
// is settled for that task's landing (§7.1), the one predicate
// depsLandedTx walks: a dependency of a class other than implement
// never lands, so it never blocks; an implement dependency counts with
// a landing pushed, released, or — waiting or local — while the
// dependent's own landing stacks onto the waiting ref. The blocked-plan
// rule (§6.2) reads it too, so a dependency the landing rule skips can
// never strand a dependent either.
func depLandedForTx(tx *sql.Tx, taskID, dep int64) (bool, error) {
	var class string
	if err := tx.QueryRow("SELECT class FROM tasks WHERE id = ?", dep).Scan(&class); err != nil {
		return false, fmt.Errorf("read the class of dependency %d: %w", dep, err)
	}
	if class != classImplement {
		return true, nil // never lands, so nothing waits on it
	}
	var pushed string
	err := tx.QueryRow("SELECT pushed FROM landings WHERE task_id = ?", dep).Scan(&pushed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the landing of dependency %d: %w", dep, err)
	}
	switch pushed {
	case "pushed":
		// The dependency's work is on the target.
		return true, nil
	case "waiting", "local":
		released, err := depPushReleasedTx(tx, dep)
		if err != nil {
			return false, err
		}
		if released {
			return true, nil
		}
		return dependentStacksTx(tx, taskID)
	default:
		return false, nil
	}
}

// crossPlanDepTx checks a dependency ref that names no task of the plan
// (§6.1): the id of a task of another plan counts when its work is on
// the project's land target — the task done, with a landing pushed to
// the branch land_target names, or the default branch when none is set —
// because a task of this plan builds on that branch and so starts from
// the dep's work. Such a dep is already satisfied: no edge row is
// written for it, so the plan file and recovery stay within the plan
// (§6.4). ok says it counts; why, with ok false, says why a ref that
// names a task still does not; ok false with an empty why says the ref
// is no id of any task, and the caller refuses it as it refuses a name.
func crossPlanDepTx(tx *sql.Tx, project int64, ref string) (ok bool, why string, err error) {
	id, perr := strconv.ParseInt(ref, 10, 64)
	if perr != nil || id <= 0 {
		return false, "", nil
	}
	var owner int64
	var state string
	var target, pushed sql.NullString
	qerr := tx.QueryRow(`SELECT p.project_id, t.state, l.target_branch, l.pushed
		FROM tasks t JOIN plans p ON p.id = t.plan_id
		LEFT JOIN landings l ON l.task_id = t.id
		WHERE t.id = ?`, id).Scan(&owner, &state, &target, &pushed)
	if errors.Is(qerr, sql.ErrNoRows) {
		return false, "", nil
	}
	if qerr != nil {
		return false, "", fmt.Errorf("read task %d as a dependency: %w", id, qerr)
	}
	if owner != project {
		return false, fmt.Sprintf("task %d belongs to another project", id), nil
	}
	if state != taskDone {
		return false, fmt.Sprintf("task %d is %s, not landed", id, state), nil
	}
	if !pushed.Valid {
		return false, fmt.Sprintf("task %d has not landed", id), nil
	}
	if pushed.String != "pushed" {
		return false, fmt.Sprintf("task %d has not pushed to the target yet", id), nil
	}
	s, err := projectApprovalTx(tx, project)
	if err != nil {
		return false, "", err
	}
	want := s.LandTarget
	if want == "" {
		if err := tx.QueryRow("SELECT default_branch FROM projects WHERE id = ?", project).Scan(&want); err != nil {
			return false, "", fmt.Errorf("read the default branch of project %d: %w", project, err)
		}
	}
	if target.String != want {
		return false, fmt.Sprintf("task %d landed on %s, not the project's land target %s", id, target.String, want), nil
	}
	return true, "", nil
}

// depPushReleasedTx reports whether a dependency's push was released
// (§6.3): a live land job on its key, which a task with a landing
// record holds only as the push phase brigada push approve queued.
func depPushReleasedTx(tx *sql.Tx, dep int64) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'land'
		AND status IN ('queued', 'leased', 'running')`, dep).Scan(&n); err != nil {
		return false, fmt.Errorf("read the land jobs of dependency %d: %w", dep, err)
	}
	return n > 0, nil
}

// dependentStacksTx reports whether the task's own landing would stack
// onto the waiting ref (§7.1): its project's push gate is ask or never,
// the gates whose landings wait on the ref — the dependent's squash
// then lands on top of a waiting dependency's commit.
func dependentStacksTx(tx *sql.Tx, taskID int64) (bool, error) {
	var project int64
	if err := tx.QueryRow(`SELECT p.project_id FROM tasks t JOIN plans p ON p.id = t.plan_id
		WHERE t.id = ?`, taskID).Scan(&project); err != nil {
		return false, fmt.Errorf("read the project of task %d: %w", taskID, err)
	}
	s, err := projectApprovalTx(tx, project)
	if err != nil {
		return false, err
	}
	gate := s.GatePush
	if gate == "" {
		gate = GateAuto // the design's default (§6.3)
	}
	return gate == GateAsk || gate == GateNever, nil
}

// landHoldAsksTx names the rules that hold a task at the land gate
// (§6.3) as hold asks (approval_stop.go), empty when the landing runs
// on: gate_land is ask; the task's tier among approval_tiers; a
// protected path the task changed; or approval_restarted with a task
// that spent a restart or a tier bump.
func landHoldAsksTx(tx *sql.Tx, project, taskID int64) ([]holdAsk, error) {
	s, err := projectApprovalTx(tx, project)
	if err != nil {
		return nil, err
	}
	var asks []holdAsk
	if s.GateLand == GateAsk {
		asks = append(asks, holdAsk{kind: holdGateLand})
	}
	// The protected-path rule matches the paths the task changed. The
	// engine runs no git (product §5 rule 1), so the runner records the
	// task's real changed paths (`git diff --name-only base...branch`)
	// with the work job's result and they are what the rule matches;
	// the spec and check text stand in only when no paths are recorded
	// — an older runner, or a task whose work predates the recording —
	// the same proxy the plan gate uses (§6.3).
	var spec, check, tier, changed string
	if err := tx.QueryRow("SELECT spec, COALESCE(check_cmd, ''), effective_tier, COALESCE(changed_paths, '') FROM tasks WHERE id = ?", taskID).
		Scan(&spec, &check, &tier, &changed); err != nil {
		return nil, fmt.Errorf("read task %d: %w", taskID, err)
	}
	// The effective tier counts: the escalation ladder may have moved the
	// task off the tier it was planned at.
	if slices.Contains(s.ApprovalTiers, tier) {
		asks = append(asks, holdAsk{kind: holdTier, item: tier})
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
			asks = append(asks, holdAsk{kind: holdPath, item: p})
		}
	}
	if s.ApprovalRestarted {
		restarted, err := taskSpentRestartTx(tx, taskID)
		if err != nil {
			return nil, err
		}
		if restarted {
			asks = append(asks, holdAsk{kind: holdRestart})
		}
	}
	return asks, nil
}

// landHoldReasonTx words the land-gate rules that hold a task (§6.3),
// empty when none does: the settings that hold it, then the reasons in
// the needs-you line's order, tier before paths — "held by rule
// approval_tiers, approval_protected_paths: heavy; touches infra/**".
func landHoldReasonTx(tx *sql.Tx, project, taskID int64) (string, error) {
	asks, err := landHoldAsksTx(tx, project, taskID)
	if err != nil || len(asks) == 0 {
		return "", err
	}
	var keys, words []string
	for _, a := range asks {
		if k := holdSettingOf[a.kind]; !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
		words = append(words, a.word())
	}
	return "held by rule " + strings.Join(keys, ", ") + ": " + strings.Join(words, "; "), nil
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
func (e *Engine) holdLandTx(tx *sql.Tx, project, taskID int64, reason string, now time.Time) error {
	if _, err := tx.Exec(`INSERT INTO land_holds (task_id, reason, created_at) VALUES (?, ?, ?)`,
		taskID, reason, fmtTime(now)); err != nil {
		return fmt.Errorf("hold task %d at the land gate: %w", taskID, err)
	}
	_, err := appendEventTx(tx, evTaskLandHeld, project, taskID, 0,
		eventPayload{Status: taskDone, Reason: boundNote(reason)}, now)
	return err
}

// clearLandHoldTx drops a held land-gate hold of the task once its landing
// is recorded, by trailer or by the land job (§6.3): the landing is done,
// so the needs-you line that waited on it goes too. A rejected row stays,
// as LandReject leaves it, so the rule never re-holds the task.
func clearLandHoldTx(tx *sql.Tx, taskID int64) error {
	if _, err := tx.Exec(`DELETE FROM land_holds WHERE task_id = ? AND state = 'held'`, taskID); err != nil {
		return fmt.Errorf("clear the land hold of task %d: %w", taskID, err)
	}
	return nil
}

// landReadyHold reports whether a land-gate hold's reason is the
// land_ready rule's (issue #87): the gate's exit 1, whose release by an
// approval re-queues the landing with the gate skipped rather than
// re-running the command. Every other hold releases as it always has.
func landReadyHold(reason string) bool {
	return strings.HasPrefix(reason, "held by rule land_ready")
}

// queueLandJobTx creates the task's land job (§7.1): kind land on the
// task's own key, so the one-live-job-per-key rule makes a repeat
// coalesce. No worker and no agent; the git label is what a runner
// offers to take it. A task that already has a landing record — its own
// land job landed it, or a detected trailer did — has nothing left to
// squash, so no land job is queued for it. skipLandReady marks the job
// an approval of the land_ready gate's hold queues (issue #87): its
// grant carries the gate as skipped, so the runner lands without
// re-running the command. The submit's answer says what happened:
// created with the new job, pending behind the live job that holds the
// key, or the zero answer for a task already landed.
func (e *Engine) queueLandJobTx(tx *sql.Tx, project, taskID int64, skipLandReady bool, now time.Time) (SubmitResult, error) {
	var have int
	if err := tx.QueryRow("SELECT COUNT(*) FROM landings WHERE task_id = ?", taskID).Scan(&have); err != nil {
		return SubmitResult{}, fmt.Errorf("read the landing of task %d: %w", taskID, err)
	}
	if have > 0 {
		return SubmitResult{}, nil
	}
	return e.submitTx(tx, SubmitParams{
		Key:           fmt.Sprintf("task:%d", taskID),
		ProjectID:     project,
		TaskID:        taskID,
		Kind:          KindLand,
		Mode:          ModeFresh,
		Prompt:        "", // no worker: nothing is composed (§1.4)
		Labels:        []string{"git"},
		SkipLandReady: skipLandReady,
	}, now)
}

// queuePushJobTx creates the push phase's land job (§7.1): the land job
// reused for a landing already recorded — the squash happened once, so
// the grant carries the recorded commit and pushes, no new squash.
// skipLandReady marks the job an approval of the push-phase land_ready
// hold queues (issue #87): the runner runs no command and pushes.
func (e *Engine) queuePushJobTx(tx *sql.Tx, project, taskID int64, skipLandReady bool, now time.Time) (SubmitResult, error) {
	return e.submitTx(tx, SubmitParams{
		Key:           fmt.Sprintf("task:%d", taskID),
		ProjectID:     project,
		TaskID:        taskID,
		Kind:          KindLand,
		Mode:          ModeFresh,
		Prompt:        "", // no worker
		Labels:        []string{"git"},
		SkipLandReady: skipLandReady,
	}, now)
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
		WHERE h.state = 'held' AND NOT EXISTS (SELECT 1 FROM landings l WHERE l.task_id = h.task_id AND l.pushed IN ('pushed', 'local'))
			AND p.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)`
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
		it.Reason = fmt.Sprintf("%s; release: brigada land approve %d; change the rule: brigada land approve --and-stop-asking %d",
			reason, it.ID, it.ID)
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
	switch c.Outcome {
	case OutcomeLandHeld:
		return e.landHeldTx(tx, job, c, now)
	case OutcomeLandNotReady:
		return e.landNotReadyTx(tx, job, c, now)
	}
	if c.Error != "" {
		// A push rejected because the target moved, for a task whose
		// landing is already recorded as pushed — by its trailer on the
		// default branch — leaves nothing for the foreman.
		if existing, err := landingTx(tx, job.TaskID); err != nil {
			return err
		} else if existing != nil && existing.Pushed == "pushed" && pushRejected(c.Error) {
			// The trailer scan may have settled the task while this job
			// was live, leaving the plan open; the job's end closes it.
			return e.finishLandPlanTx(tx, job, now)
		}
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

// relandStaleWaitingTx forgets the other landings of the target that
// sat on a waiting ref the runner has just deleted as stale (§7.1) —
// waiting at the push gate, or left to the user under gate_push never
// with a push command that fetches that ref: their commits are gone
// from the remote, so they stop showing "waits to push" or "push by
// hand" and the land rule queues each task again. The landing that
// follows never pushes without the user's yes: its grant keeps the push
// gate at ask while the gate is auto (heldPushTx). A landing with a live
// land job — its push already released — is left for that job, as the
// push approval leaves it.
func (e *Engine) relandStaleWaitingTx(tx *sql.Tx, job *jobRow, target string, now time.Time) error {
	rows, err := tx.Query(`SELECT l.task_id FROM landings l WHERE l.project_id = ? AND l.target_branch = ?
		AND l.pushed IN ('waiting', 'local') AND l.task_id <> ?
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.task_id = l.task_id AND j.kind = 'land'
			AND j.status IN ('queued', 'leased', 'running'))
		ORDER BY l.task_id`, job.ProjectID, target, job.TaskID)
	if err != nil {
		return fmt.Errorf("list the landings on the stale waiting ref: %w", err)
	}
	var tasks []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("list the landings on the stale waiting ref: %w", err)
		}
		tasks = append(tasks, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("list the landings on the stale waiting ref: %w", err)
	}
	for _, id := range tasks {
		if _, err := tx.Exec("DELETE FROM landings WHERE task_id = ?", id); err != nil {
			return fmt.Errorf("forget the stale landing of task %d: %w", id, err)
		}
		if err := e.maybeLandTaskTx(tx, id, now); err != nil {
			return err
		}
	}
	return nil
}

// pushRejected reports whether a land job's error is git rejecting a push
// because the target moved.
func pushRejected(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "rejected") || strings.Contains(msg, "non-fast-forward")
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
	if err := clearLandHoldTx(tx, job.TaskID); err != nil {
		return err
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

// landHeldTx answers the land_ready gate's hold (issue #87): the command
// refused the landing with its own words — exit 1 — so nothing is
// squashed, pushed or recorded, and the task holds at the land gate with
// the command's first output line as the reason, exactly as a land-gate
// rule holds it (§6.3). The hold is the user's to release — `brigada
// land approve` — which re-queues the landing with the gate skipped. In
// the push phase the landing is already recorded waiting; the same hold
// then sits on the push, and the approval re-queues the push instead.
// The hold has no job, so the plan closes here, named under "waits at a
// gate" (§7.1).
func (e *Engine) landHeldTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	reason := "held by rule land_ready"
	var rec landHeldRecord
	if json.Unmarshal([]byte(c.Result), &rec) == nil && rec.Line != "" {
		reason += ": " + boundNote(rec.Line)
	}
	if err := e.holdLandTx(tx, job.ProjectID, job.TaskID, reason, now); err != nil {
		return err
	}
	return e.finishLandPlanTx(tx, job, now)
}

// landNotReadyTx answers the land_ready gate's not ready (issue #87):
// the command could not say yes — exit 2, another exit, a spawn failure,
// a timeout — so the landing did not happen and tries again. The job
// completes without failure, no restart is spent, and a fresh land job
// is queued under the not-before backoff (retryDelayFor, §3), its delay
// grown by the run of consecutive not-ready answers. The next try time
// and the last exit land on the task for the status to read.
func (e *Engine) landNotReadyTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	var rec landNotReadyRecord
	if err := json.Unmarshal([]byte(c.Result), &rec); err != nil {
		return e.needsForemanTx(tx, job, evTaskLandFailed,
			eventPayload{Reason: "unreadable not-ready record"}, now)
	}
	streak, err := landNotReadyStreakTx(tx, job.Key)
	if err != nil {
		return err
	}
	notBefore := now.Add(retryDelayFor(streak))
	// The completing job freed the key earlier in this transaction, so
	// the submit cannot sit behind a live job; an answer but a created
	// job is an error, not a silent loss of the retry.
	sr, err := e.submitTx(tx, SubmitParams{
		Key:       job.Key,
		ProjectID: job.ProjectID,
		TaskID:    job.TaskID,
		Kind:      KindLand,
		Mode:      ModeFresh,
		Prompt:    "", // no worker: nothing is composed (§1.4)
		Labels:    []string{"git"},
		NotBefore: notBefore,
	}, now)
	if err != nil {
		return err
	}
	if sr.Answer != AnsCreated {
		return fmt.Errorf("queue the land retry of task %d: submit %s answered %s", job.TaskID, job.Key, sr.Answer)
	}
	if _, err := tx.Exec(`UPDATE tasks SET land_retry_at = ?, land_ready_exit = ?, updated_at = ? WHERE id = ?`,
		fmtTime(notBefore), rec.Exit, fmtTime(now), job.TaskID); err != nil {
		return fmt.Errorf("record the land retry of task %d: %w", job.TaskID, err)
	}
	_, err = appendEventTx(tx, evTaskLandNotReady, job.ProjectID, job.TaskID, sr.JobID,
		eventPayload{ExitCode: rec.Exit, Reason: boundNote(rec.Reason), Until: fmtTime(notBefore)}, now)
	return err
}

// landNotReadyStreakTx measures a key's current run of consecutive
// not-ready land jobs (issue #87), newest first — the completing job
// included, for its result is already written — so the backoff grows
// with each retry the gate refuses again. Zero when the key's latest
// land job ended any other way.
func landNotReadyStreakTx(tx *sql.Tx, key string) (int, error) {
	rows, err := tx.Query(`SELECT COALESCE(result, '') FROM jobs
		WHERE key = ? AND kind = 'land' ORDER BY id DESC`, key)
	if err != nil {
		return 0, fmt.Errorf("read the land jobs of key %s: %w", key, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return 0, fmt.Errorf("read the land jobs of key %s: %w", key, err)
		}
		var rec landNotReadyRecord
		if json.Unmarshal([]byte(jobResultText(stored)), &rec) != nil || rec.Outcome != OutcomeLandNotReady {
			break
		}
		n++
	}
	return n, rows.Err()
}

// LandMessage is the task data a land job's squash commit message is
// built from (§7.1): the fields of internal/landing's Task the engine
// knows without running git — what the hand-driven land reads through
// the API, read here from the task, its last author job and that job's
// agent. The runner adds the branch and its fetched head. CommitMessage
// carries the worker's own commit message (§5.4) when it wrote one;
// empty means the runner builds today's format.
type LandMessage struct {
	Title         string `json:"title"`
	Spec          string `json:"spec"`
	Summary       string `json:"summary"`
	CommitMessage string `json:"commit_message,omitempty"`
	Family        string `json:"family"`
	Model         string `json:"model"`
}

// landMessageTx reads a task's squash-message data (§7.1): the title
// and spec from the task, the worker's summary and commit message from
// its last done author job, and that job's agent family and model. A
// task with no done author job keeps the empty fields; none can reach
// a land job.
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
	if wr, ok := parseWorkerResult(jobResultText(authorResult)); ok {
		m.Summary = wr.Summary
		m.CommitMessage = wr.CommitMessage
	}
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
			LandReady:      s.LandReady,
			SkipLandReady:  j.SkipLandReady,
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
		gatePush = GateAuto // the design's default (§6.3), which projectApprovalTx leaves unapplied
	}
	if gatePush == GateAuto {
		// A landing re-made after its waiting ref went stale was waiting
		// for the user's yes, or left to the user under never: a switch
		// of the gate to auto since never pushes it unasked (§6.3).
		held, err := heldPushTx(tx, j.TaskID)
		if err != nil {
			return nil, err
		}
		if held {
			gatePush = GateAsk
		}
	}
	approvedHead, err := approvedHeadTx(tx, j.TaskID)
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
		BaseRef:        baseRef,
		CheckCmd:       s.DefaultCheck,
		Setup:          setup,
		ExtraRepos:     extraRepos,
		Branch:         branch,
		ApprovedHead:   approvedHead,
		LandTarget:     target,
		DefaultBranch:  defaultBranch,
		LandMessage:    msg,
		GatePush:       gatePush,
		AfterLand:      s.AfterLand,
		LandReady:      s.LandReady,
		SkipLandReady:  j.SkipLandReady,
		Attempt:        j.Attempts + 1,
	}, nil
}

// heldPushTx reports whether the task's last landing, since forgotten,
// was never pushed by brigada with the user's yes (§6.3, §7.1): its
// latest task_landed event recorded it waiting at the push gate, or
// local under gate_push never, and no task_pushed followed. Such a
// landing is re-made only when its waiting ref went stale
// (relandStaleWaitingTx), and the re-made landing waits at ask again.
func heldPushTx(tx *sql.Tx, taskID int64) (bool, error) {
	var kind, payload string
	err := tx.QueryRow(`SELECT kind, payload FROM events WHERE task_id = ? AND kind IN (?, ?)
		ORDER BY id DESC LIMIT 1`, taskID, evTaskLanded, evTaskPushed).Scan(&kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the last landing of task %d: %w", taskID, err)
	}
	if kind != evTaskLanded {
		return false, nil
	}
	var p eventPayload
	if json.Unmarshal([]byte(payload), &p) != nil {
		return false, nil
	}
	return p.Pushed == "waiting" || p.Pushed == "local", nil
}

// approvedHeadTx reads the head the task's approving review judged
// (§7.1): the head its last done author job pushed, as that job's
// result recorded it. "" when the result names none.
func approvedHeadTx(tx *sql.Tx, taskID int64) (string, error) {
	var authored string
	err := tx.QueryRow(`SELECT COALESCE(result, '') FROM jobs
		WHERE task_id = ? AND kind IN ('work', 'fix') AND status = 'done' ORDER BY id DESC LIMIT 1`, taskID).Scan(&authored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the last author job of task %d: %w", taskID, err)
	}
	var head headField
	_ = json.Unmarshal([]byte(jobResultText(authored)), &head)
	return strings.TrimSpace(head.Head), nil
}

// LandDecision is what releasing or cancelling a land-gate hold did
// (§6.3): the task, whether the landing now runs (Released) or is
// cancelled — the branch left alone, the hold row kept as rejected so the
// rule never re-holds a landing the user refused — and the key=value
// pairs an approval that stops asking changed.
//
// For an approval, Queued says whether the land job was queued, with
// its id in JobID; a live job already on the task's key queues nothing,
// and JobID then names that job.
type LandDecision struct {
	TaskID   int64    `json:"task_id"`
	Released bool     `json:"released"` // true for an approval, false for a rejection
	Queued   bool     `json:"queued,omitempty"`
	JobID    int64    `json:"job_id,omitempty"`
	Settings []string `json:"settings_changed,omitempty"`
}

// LandApprove releases a task held at the land gate (§6.3): the hold's
// row is deleted, the event names the release, and the land job is queued
// directly — the user's yes overrides the gate, which would otherwise
// re-hold the task. stopAsking also changes, through the
// `brigada project set` path, the one setting each hold reason maps to
// (§6.3), so the same question is not asked twice. The foreman has no
// tool for it: the gate is the user's.
func (e *Engine) LandApprove(ctx context.Context, project, taskID int64, stopAsking bool) (LandDecision, error) {
	return e.landDecision(ctx, project, taskID, true, stopAsking)
}

// LandReject cancels the landing of a task held at the land gate (§6.3):
// the hold's row is kept, marked rejected, and the event records the
// cancellation. The task stays done and its branch is left alone —
// nothing is squashed, nothing is pushed, and the landing is never
// recorded.
func (e *Engine) LandReject(ctx context.Context, project, taskID int64) (LandDecision, error) {
	return e.landDecision(ctx, project, taskID, false, false)
}

func (e *Engine) landDecision(ctx context.Context, project, taskID int64, approve, stopAsking bool) (LandDecision, error) {
	var res LandDecision
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.landDecisionTx(tx, project, taskID, approve, stopAsking, now)
		if err != nil {
			return fmt.Errorf("decide the landing of task %d: %w", taskID, err)
		}
		res = r
		return nil
	})
	return res, err
}

// landDecisionTx applies the user's decision on a land-gate hold (§6.3).
func (e *Engine) landDecisionTx(tx *sql.Tx, project, taskID int64, approve, stopAsking bool, now time.Time) (LandDecision, error) {
	reason, err := landHoldTaskTx(tx, project, taskID)
	if err != nil {
		return LandDecision{}, err
	}
	// The land_ready gate's hold (issue #87) releases with the gate
	// skipped: the user's yes replaces the command, which already spoke,
	// so the re-queued landing runs no command and lands.
	skip := landReadyHold(reason)
	if approve {
		var changed []string
		if stopAsking {
			s, err := projectApprovalTx(tx, project)
			if err != nil {
				return LandDecision{}, err
			}
			asks, err := landHoldAsksTx(tx, project, taskID)
			if err != nil {
				return LandDecision{}, err
			}
			changed, err = e.stopAskingTx(tx, project, s, asks, now)
			if err != nil {
				return LandDecision{}, fmt.Errorf("stop the land gate asking: %w", err)
			}
		}
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
		// landed. A live job on the task's key queues nothing: the
		// decision names that job instead of a land job it did not queue.
		res := LandDecision{TaskID: taskID, Released: true, Settings: changed}
		live, err := liveJobTx(tx, fmt.Sprintf("task:%d", taskID))
		if err != nil {
			return LandDecision{}, err
		}
		if live != nil {
			res.JobID = live.ID
			return res, nil
		}
		// A hold beside a landing record is the push phase's (issue #87):
		// the gate refused the push of a commit already squashed and
		// waiting, so the release re-queues the push — not a new squash —
		// with the gate skipped. A hold with no landing is the squash
		// phase's, and the landing is re-queued whole.
		existing, err := landingTx(tx, taskID)
		if err != nil {
			return LandDecision{}, err
		}
		var sr SubmitResult
		if existing != nil {
			sr, err = e.queuePushJobTx(tx, project, taskID, skip, now)
		} else {
			sr, err = e.queueLandJobTx(tx, project, taskID, skip, now)
		}
		if err != nil {
			return LandDecision{}, err
		}
		res.Queued = sr.Answer == AnsCreated
		res.JobID = sr.JobID
		return res, nil
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
		// The release unblocks the landing's dependents under auto
		// (depsLandedTx): their rule runs again here, as a landing runs
		// it for its own dependents (§7.1).
		if err := e.releaseDependentLandsTx(tx, w.task, now); err != nil {
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
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.task_id = l.task_id AND j.kind = 'land' AND j.status IN ('queued', 'leased', 'running'))
		  AND NOT EXISTS (SELECT 1 FROM land_holds h WHERE h.task_id = l.task_id AND h.state = 'held')`
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
