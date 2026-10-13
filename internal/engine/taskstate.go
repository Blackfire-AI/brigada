package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Task states (§6.1): the road a task walks, and the places it can
// wait. checking is the runner's own step between a done worker and the
// completion that carries the check's result; the engine sees the two as
// one completion, so it never sets checking itself.
const (
	taskPending      = "pending"
	taskReady        = "ready"
	taskRunning      = "running"
	taskChecking     = "checking"
	taskReviewing    = "reviewing"
	taskDone         = "done"
	taskNeedsForeman = "needs_foreman"
	taskNeedsUser    = "needs_user"
	taskFailed       = "failed"
	taskCancelled    = "cancelled"
)

// Plan states (§6.2) the engine sets: active from its creation, done
// when every task of it is done.
const planDone = "done"

// classImplement is the one class the review policy applies to (§7):
// every other class ends its work the same way — done when the run is
// done, no review. The class set stays open (§6.3).
const classImplement = "implement"

// The result block's statuses the state machine acts on (§5.4): the
// worker's three, plus the runner's check_failed for a done run whose
// check the runner ran and failed (§5.5) — the runner rewrites the
// block's status, so the engine reads one word either way.
const (
	workerDone        = "done"
	workerNeedsHelp   = "needs_help"
	workerCheckFailed = "check_failed"
)

// The review's verdict block (§7): approve or changes, plus findings.
const (
	verdictApprove = "approve"
	verdictChanges = "changes"
)

// minSubstantiveFindings is how much findings text an approve must
// carry to count (§7): an approve below it is a lazy approval, which
// must not inflate the outcome record, so it reads as no verdict and
// waits for the foreman like any other unusable review.
const minSubstantiveFindings = 80

// taskRunningTx moves a task off ready when one of its jobs is leased
// (§6.1): running while the work happens. A task already past ready —
// reviewing under its review job, parked at needs_foreman by an earlier
// completion — keeps its state; only the road's first step is the
// lease's to take.
func taskRunningTx(tx *sql.Tx, j *jobRow, now time.Time) error {
	res, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		taskRunning, fmtTime(now), j.TaskID, taskReady)
	if err != nil {
		return fmt.Errorf("mark task %d running: %w", j.TaskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark task %d running: %w", j.TaskID, err)
	}
	if n == 0 {
		return nil
	}
	_, err = appendEventTx(tx, evTaskRunning, j.ProjectID, j.TaskID, j.ID,
		eventPayload{Status: taskRunning}, now)
	return err
}

// taskAfterCompletionTx moves a task's state after one of its jobs
// completed (§3, §6.1, §7), inside the completion's own transaction:
//
//   - a work job of an implement task that ended done with its check
//     passed moves the task to reviewing and queues its review job;
//   - a work job of any other class that ended done finishes the task;
//   - a failed check, a worker's needs_help, a failed or unusable result
//     parks the task at needs_foreman — the rules cannot carry those
//     forward alone, so they wait for the foreman's judgment (§3; the
//     fix rounds that would follow arrive with F1d3);
//   - a review job's verdict finishes the task, asks for changes, or
//     names itself unusable (§7).
func (e *Engine) taskAfterCompletionTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	switch job.Kind {
	case KindWork:
		var class string
		if err := tx.QueryRow("SELECT class FROM tasks WHERE id = ?", job.TaskID).Scan(&class); err != nil {
			return fmt.Errorf("read task %d: %w", job.TaskID, err)
		}
		return e.workCompletedTx(tx, job, c, class, now)
	case KindReview:
		return reviewCompletedTx(tx, job, c, now)
	default:
		return nil // fix rounds answer to F1d3's rules, not yet these
	}
}

// workCompletedTx applies a work job's completion to its task.
func (e *Engine) workCompletedTx(tx *sql.Tx, job *jobRow, c Completion, class string, now time.Time) error {
	wr, haveResult := parseWorkerResult(c.Result)
	switch {
	case !haveResult || (wr.Status != workerDone && wr.Status != workerCheckFailed && wr.Status != workerNeedsHelp):
		// A failed run, a stopped one, a block that never parsed: all
		// the foreman's to judge (§3, §5.6), each with what the run
		// said for itself.
		reason := wr.Summary
		if reason == "" {
			reason = wr.Reason
		}
		if reason == "" {
			reason = c.Error
		}
		if reason == "" {
			reason = "no result block"
		}
		return needsForemanTx(tx, job, evTaskFailed, eventPayload{Reason: boundNote(reason)}, now)
	case wr.Status == workerNeedsHelp:
		// §3: the question is the foreman's to answer, verbatim.
		return needsForemanTx(tx, job, evTaskNeedsHelp, eventPayload{Question: boundNote(wr.Question)}, now)
	case wr.Status == workerCheckFailed || !checkPassed(c.CheckResult):
		// The check the runner ran failed (§5.5) — the runner says so
		// itself, or the done carries a failed check result. The tail
		// names the failure; a check that left no usable report is not
		// a pass either.
		tail := checkTail(c.CheckResult)
		if tail == "" {
			tail = wr.Summary
		}
		return needsForemanTx(tx, job, evTaskCheckFailed, eventPayload{Reason: boundNote(tail)}, now)
	case class != classImplement:
		// No review behind any other class (§7): done is done.
		return taskDoneTx(tx, job, now)
	default:
		return e.queueReviewTx(tx, job, wr, c.CheckResult, now)
	}
}

// queueReviewTx moves an implement task into reviewing and queues its
// review job (§7): same key, fresh session, placed on another family
// than the author's. The review job's prompt is the runner item's to
// compose (F1d2); until then the job carries the reviewer's inputs —
// the spec, the check result and the branch. The implementer's summary
// is deliberately none of them (§7's anti-steering rule).
func (e *Engine) queueReviewTx(tx *sql.Tx, job *jobRow, wr workerResultFields, checkJSON string, now time.Time) error {
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, branch = COALESCE(NULLIF(?, ''), branch), updated_at = ?
		WHERE id = ?`, taskReviewing, wr.Branch, fmtTime(now), job.TaskID); err != nil {
		return fmt.Errorf("move task %d to reviewing: %w", job.TaskID, err)
	}
	if _, err := appendEventTx(tx, evTaskReviewing, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: taskReviewing, Branch: wr.Branch}, now); err != nil {
		return err
	}
	exclude, err := agentFamilyTx(tx, job.AgentID)
	if err != nil {
		return err
	}
	spec, err := taskSpecTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	inputs, err := json.Marshal(reviewInputs{Spec: spec, CheckResult: json.RawMessage(checkJSON), Branch: wr.Branch})
	if err != nil {
		return fmt.Errorf("encode the review inputs of task %d: %w", job.TaskID, err)
	}
	// The one-live-job-per-key rule is free here: the work job went
	// terminal earlier in this completion's transaction. A task whose
	// own caps close its reviews (a review cap of 0) answers capped and
	// sits in reviewing with the job_capped event naming why — the
	// rounds rules that would carry it on are F1d3's.
	_, err = e.submitTx(tx, SubmitParams{
		Key:           job.Key,
		ProjectID:     job.ProjectID,
		TaskID:        job.TaskID,
		Kind:          KindReview,
		Mode:          ModeFresh,
		Prompt:        string(inputs),
		ExcludeFamily: exclude,
	}, now)
	return err
}

// reviewInputs is what a review job carries until the runner item (F1d2)
// composes the reviewer's prompt from it (§7).
type reviewInputs struct {
	Spec        string          `json:"spec"`
	CheckResult json.RawMessage `json:"check_result,omitempty"` // the runner's check JSON, verbatim; none when the task has no check
	Branch      string          `json:"branch,omitempty"`
}

// agentFamilyTx reads the family a review must differ from (§7): the
// family of the agent the author job ran on. No agent — an agent-less
// runner — or an agent a re-register has since dropped excludes
// nothing: the job record is the family's only witness.
func agentFamilyTx(tx *sql.Tx, agent int64) (string, error) {
	if agent == 0 {
		return "", nil
	}
	var family string
	err := tx.QueryRow("SELECT family FROM agents WHERE id = ?", agent).Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read agent %d: %w", agent, err)
	}
	return family, nil
}

// reviewCompletedTx applies a review job's verdict block (§7):
// a substantive approve finishes the task, changes asks the foreman for
// a fix round (F1d3), and anything else — no block, a malformed one, an
// approve without substantive findings — is no verdict and waits for
// the foreman with another reviewer (F1d3).
func reviewCompletedTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	var v verdictBlock
	haveVerdict := json.Unmarshal([]byte(c.Result), &v) == nil &&
		(v.Verdict == verdictApprove || v.Verdict == verdictChanges)
	switch {
	case haveVerdict && v.Verdict == verdictApprove && len(v.Findings) >= minSubstantiveFindings:
		if err := recordOutcomeTx(tx, job, now); err != nil {
			return err
		}
		return taskDoneTx(tx, job, now)
	case haveVerdict && v.Verdict == verdictChanges:
		return needsForemanTx(tx, job, evReviewChanges, eventPayload{Findings: boundNote(v.Findings)}, now)
	default:
		reason := "no verdict in the review result"
		if haveVerdict {
			reason = "approve without substantive findings"
		}
		return needsForemanTx(tx, job, evReviewNoVerdict, eventPayload{Reason: reason}, now)
	}
}

// verdictBlock is the review's answer (§7): what the reviewer's final
// message ends with, as the runner delivers it.
type verdictBlock struct {
	Verdict  string `json:"verdict"`
	Findings string `json:"findings"`
}

// recordOutcomeTx counts a substantive approve on the outcome record
// (§7, §11): the reviewer's agent, on the review class, one attempt and
// one success — success is what a review is, which is why the lazy
// approval next to it reads as no verdict instead. A review that ran
// with no agent — an agent-less runner — has nobody to score and
// records nothing.
func recordOutcomeTx(tx *sql.Tx, job *jobRow, now time.Time) error {
	if job.AgentID == 0 {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
		VALUES (?, ?, ?, 1, 1, ?)
		ON CONFLICT(project_id, agent_id, class) DO UPDATE SET
			attempts = attempts + 1, successes = successes + 1, updated_at = excluded.updated_at`,
		job.ProjectID, job.AgentID, string(KindReview), fmtTime(now))
	if err != nil {
		return fmt.Errorf("record the outcome of job %d: %w", job.ID, err)
	}
	return nil
}

// needsForemanTx parks a task at needs_foreman (§3): the completion was
// nothing the rules can carry forward on their own, so the task waits
// for the foreman's judgment. The event kind names what waited and the
// payload what the foreman will read.
func needsForemanTx(tx *sql.Tx, job *jobRow, kind string, payload eventPayload, now time.Time) error {
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
		taskNeedsForeman, fmtTime(now), job.TaskID); err != nil {
		return fmt.Errorf("park task %d at needs_foreman: %w", job.TaskID, err)
	}
	payload.Status = taskNeedsForeman
	_, err := appendEventTx(tx, kind, job.ProjectID, job.TaskID, job.ID, payload, now)
	return err
}

// taskDoneTx finishes a task and, when that closed the plan's last open
// task, the plan with it (§6.1, §6.2). The plan_done event names the
// task and the job that ended it; the user's notification (§3, §10)
// rides on that event later.
func taskDoneTx(tx *sql.Tx, job *jobRow, now time.Time) error {
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
		taskDone, fmtTime(now), job.TaskID); err != nil {
		return fmt.Errorf("finish task %d: %w", job.TaskID, err)
	}
	if _, err := appendEventTx(tx, evTaskDone, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: taskDone}, now); err != nil {
		return err
	}
	var plan, open int64
	if err := tx.QueryRow(`SELECT t.plan_id,
		(SELECT COUNT(*) FROM tasks o WHERE o.plan_id = t.plan_id AND o.state <> ?)
		FROM tasks t WHERE t.id = ?`, taskDone, job.TaskID).Scan(&plan, &open); err != nil {
		return fmt.Errorf("read the plan of task %d: %w", job.TaskID, err)
	}
	if open > 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`,
		planDone, fmtTime(now), plan); err != nil {
		return fmt.Errorf("finish plan %d: %w", plan, err)
	}
	_, err := appendEventTx(tx, evPlanDone, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: planDone}, now)
	return err
}

// parseWorkerResult decodes the worker's result block (§5.4). ok is
// false when the completion carried nothing the engine can act on: no
// block, a malformed one, or one with no status word.
func parseWorkerResult(result string) (workerResultFields, bool) {
	var wr workerResultFields
	if json.Unmarshal([]byte(result), &wr) != nil || wr.Status == "" {
		return workerResultFields{}, false
	}
	return wr, true
}

// checkOutcome is the runner's check result (§5.5) as the state machine
// reads it: the exit code, whether the timeout cut it short, and the
// tail of its output.
type checkOutcome struct {
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
	Output   string `json:"output_tail"`
}

// checkPassed reports whether a done run's check allows the review
// (§5.5): no check ran, or it exited 0 within its time. A check result
// that will not parse is a failure, not an absence — the check ran, and
// a broken report is not a pass.
func checkPassed(check string) bool {
	co, ran := parseCheckResult(check)
	return !ran || (co.ExitCode == 0 && !co.TimedOut)
}

// checkTail returns the output tail a failed check's event carries, or
// "" when there is nothing usable to name it.
func checkTail(check string) string {
	co, _ := parseCheckResult(check)
	return co.Output
}

func parseCheckResult(check string) (checkOutcome, bool) {
	if check == "" {
		return checkOutcome{}, false
	}
	var co checkOutcome
	if json.Unmarshal([]byte(check), &co) != nil {
		return checkOutcome{ExitCode: -1, Output: "unparsable check result: " + tailOf(check, 200)}, true
	}
	return co, true
}
