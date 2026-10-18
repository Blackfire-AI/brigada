package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
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
// (§6.1): running while the work happens. A fix round's lease also takes
// the task back out of reviewing (§3): the review asked for changes, the
// fix job on the author's session is the answer, and the task runs
// again. Every other task already past ready — reviewing under its
// review job, parked at needs_foreman by an earlier completion — keeps
// its state; only the road's first step is the lease's to take.
func taskRunningTx(tx *sql.Tx, j *jobRow, now time.Time) error {
	query := `UPDATE tasks SET state = ?, updated_at = ? WHERE id = ? AND state = ?`
	args := []any{taskRunning, fmtTime(now), j.TaskID, taskReady}
	if j.Kind == KindFix {
		query = `UPDATE tasks SET state = ?, updated_at = ? WHERE id = ? AND state IN (?, ?)`
		args = append(args[:3], taskReady, taskReviewing)
	}
	res, err := tx.Exec(query, args...)
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
//   - a work or fix job of an implement task that ended done with its
//     check passed moves the task to reviewing and queues its review
//     job;
//   - a work or fix job of any other class that ended done finishes the
//     task;
//   - a failed check of an implement task with rounds left queues a fix
//     round on the author's session (§3); without rounds, a worker's
//     needs_help, a failed or unusable result, the task parks at
//     needs_foreman — the rules cannot carry those forward alone, so
//     they wait for the foreman's judgment (§3);
//   - a review job's verdict finishes the task, asks for a fix round,
//     or names itself unusable and is retried once (§7).
//
// A classified transient work failure can instead spend the first restart
// on another eligible agent, before any failure judgment is emitted (§3).
func (e *Engine) taskAfterCompletionTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	switch job.Kind {
	case KindWork, KindFix:
		var class string
		if err := tx.QueryRow("SELECT class FROM tasks WHERE id = ?", job.TaskID).Scan(&class); err != nil {
			return fmt.Errorf("read task %d: %w", job.TaskID, err)
		}
		return e.workCompletedTx(tx, job, c, class, now)
	case KindReview:
		return e.reviewCompletedTx(tx, job, c, now)
	default:
		return nil // a foreman turn belongs to no task (§4)
	}
}

// workCompletedTx applies a work or fix job's completion to its task.
func (e *Engine) workCompletedTx(tx *sql.Tx, job *jobRow, c Completion, class string, now time.Time) error {
	// Retry only classified CLI errors or terminal runner loss. A result
	// contract failure, stopped run or failed check belongs to its own path.
	if c.Error != "" {
		switch c.ErrorKind {
		case ErrorKindSetup:
			if retried, err := e.retrySetupFailureTx(tx, job, c, now); err != nil || retried {
				return err
			}
		case "limit", "auth", "other", "runner_lost":
			if retried, err := e.autoRestartTaskTx(tx, job, c.ErrorKind, c.Error, now); err != nil || retried {
				return err
			}
		}
	}
	wr, haveResult := parseWorkerResult(c.Result)
	// Discoveries (§5.4) reach the foreman's inbox whatever the task does
	// next, so a park in this same completion carries them in its wake.
	// They never wake the foreman alone: a later wake delivers them with
	// the rest of the inbox (§4.2).
	if haveResult && wr.Discoveries != "" {
		if err := inboxEventTx(tx, evTaskDiscoveries, job.ProjectID, job.TaskID, job.ID,
			eventPayload{Discoveries: boundNote(wr.Discoveries)}, now); err != nil {
			return err
		}
	}
	// The task's branch follows the work that certainly shipped (§6.4):
	// a done run's, a check-failed run's too — the push precedes the
	// check (§5.5) — so those completions repoint it, each to the branch
	// the engine named the job (a restart's branch above all, so the
	// review behind it reads the new attempt's work). Nothing else
	// moves it: a questioning or failed run pushed nothing, and a
	// stopped one's push is a rescue of half-made work, not a result —
	// its branch, a restart's especially, may hold no work at all. The
	// value is the job's, not the result block's: the branch is the
	// engine's to name (§5.1), and the runner fills the block with this
	// same name when the worker left it empty.
	if job.Branch != "" && haveResult &&
		(wr.Status == workerDone || wr.Status == workerCheckFailed || !checkPassed(c.CheckResult)) {
		if _, err := tx.Exec(`UPDATE tasks SET branch = ?, updated_at = ? WHERE id = ?`,
			job.Branch, fmtTime(now), job.TaskID); err != nil {
			return fmt.Errorf("record the branch of task %d: %w", job.TaskID, err)
		}
	}
	switch {
	case !haveResult || (wr.Status != workerDone && wr.Status != workerCheckFailed && wr.Status != workerNeedsHelp):
		// A failure the retry policy could not carry, a stopped run, or
		// a block that never parsed: all
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
		return e.needsForemanTx(tx, job, evTaskFailed, eventPayload{Reason: boundNote(reason)}, now)
	case wr.Status == workerNeedsHelp:
		// §3: the question is the foreman's to answer, verbatim.
		return e.needsForemanTx(tx, job, evTaskNeedsHelp, eventPayload{Question: boundNote(wr.Question)}, now)
	case wr.Status == workerCheckFailed || !checkPassed(c.CheckResult):
		// The check the runner ran failed (§5.5) — the runner says so
		// itself, or the done carries a failed check result. The tail
		// names the failure; a check that left no usable report is not
		// a pass either.
		tail := checkTail(c.CheckResult)
		if tail == "" {
			tail = wr.Summary
		}
		cmd, err := taskCheckTx(tx, job.TaskID)
		if err != nil {
			return err
		}
		co, ran := parseCheckResult(c.CheckResult)
		failure := &checkFailure{Cmd: cmd, ExitCode: co.ExitCode, TimedOut: co.TimedOut, Output: co.Output}
		if !ran {
			// The runner said the check failed but sent no report: no
			// exit code to quote.
			failure.ExitCode = -1
		}
		feedback := tail
		if ran {
			feedback = failure.feedback()
		}
		if class != classImplement {
			// No fix rounds behind any other class (§7: a review-class
			// task has none unless asked): the failure is the foreman's.
			return e.needsForemanTx(tx, job, evTaskCheckFailed, failure.payload(boundNote(tail)), now)
		}
		// §3: the failed check is a fix round's to answer, with the
		// check's own command, exit code and output as the feedback; the
		// event keeps them for the history whatever follows.
		if _, err := appendEventTx(tx, evTaskCheckFailed, job.ProjectID, job.TaskID, job.ID,
			failure.payload(boundNote(tail)), now); err != nil {
			return err
		}
		note := wr.Summary
		if note == "" {
			note = tail
		}
		return e.queueFixRoundTx(tx, job, roundFix, feedback, note, failure, now)
	case class != classImplement:
		// No review behind any other class (§7): done is done.
		return e.taskDoneTx(tx, job, now)
	default:
		return e.queueReviewTx(tx, job, wr, c.CheckResult, now)
	}
}

// retrySetupFailureTx answers a failed setup (§5.7) like a transient error
// (§3): the work job is queued again, up to setupRetries times, on any
// eligible agent — the environment failed, not the agent, and no fix round
// or agent health is charged. A fix job, or a task whose retries are spent,
// falls through to the foreman's park with the failure as its reason.
func (e *Engine) retrySetupFailureTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) (bool, error) {
	if job.Kind != KindWork {
		return false, nil
	}
	failed, err := setupRetryCountTx(tx, job.TaskID)
	if err != nil {
		return false, err
	}
	if failed > setupRetries {
		return false, nil
	}
	// A spent restart budget cannot take the retry's submit: the task goes
	// to the foreman instead, as for any retry that cannot be queued.
	if _, capped, err := e.roundCapReached(tx, SubmitParams{TaskID: job.TaskID, Restart: true}); err != nil || capped {
		return false, err
	}
	reason := boundNote(c.Error)
	note := reason + "\nThe environment failed before any work was judged; nothing of the previous attempt is lost or to be fixed."
	r, err := e.queueTaskRestartTx(tx, job.ProjectID, job.TaskID, note, 0, now)
	if err != nil {
		return false, err
	}
	_, err = appendEventTx(tx, evTaskRestarted, job.ProjectID, job.TaskID, r.JobID,
		eventPayload{Status: taskReady, Run: int64(r.Run), Reason: reason}, now)
	return err == nil, err
}

// autoRestartTaskTx spends the first restart, at most once, on a different
// ready agent (§3, §8). Fix jobs and reviews keep their existing policies.
// Decide before emitting task_failed: the restart is the inbox's single
// judgment event for this failure, with its kind and text in the reason.
func (e *Engine) autoRestartTaskTx(tx *sql.Tx, job *jobRow, kind, text string, now time.Time) (bool, error) {
	if !*e.cfg.TaskAutoRetry || job.Kind != KindWork || job.AgentID == 0 {
		return false, nil
	}
	var restarts int
	if err := tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", job.TaskID).Scan(&restarts); err != nil {
		return false, fmt.Errorf("count the restarts of task %d: %w", job.TaskID, err)
	}
	if restarts != 0 {
		return false, nil
	}
	if _, capped, err := e.roundCapReached(tx, SubmitParams{TaskID: job.TaskID, Restart: true}); err != nil || capped {
		return false, err
	}
	// Retry candidates must offer the task's own class. Keep this filter
	// local to retry selection: ordinary work/fix dispatch uses implement.
	var class string
	if err := tx.QueryRow("SELECT class FROM tasks WHERE id = ?", job.TaskID).Scan(&class); err != nil {
		return false, fmt.Errorf("read the class of task %d: %w", job.TaskID, err)
	}
	// Use the dispatcher's eligibility and ranking, across runners, without
	// a family exclusion: a work retry may use the same family.
	cands, err := eligibleAgentsTx(tx, 0, &jobRow{
		ProjectID: job.ProjectID, TaskID: job.TaskID, Kind: KindWork, ExcludeAgent: job.AgentID,
	}, class, fmtTime(now))
	if err != nil || len(cands) == 0 {
		return false, err
	}
	agent := cands[0].id
	reason := boundNote(fmt.Sprintf("%s: %s", kind, text))
	// A CLI error has no usable result summary. Carry the last progress
	// the runner persisted, when available, into the fresh session (§5.6).
	var lastEvent string
	if err := tx.QueryRow("SELECT COALESCE(last_event, '') FROM jobs WHERE id = ?", job.ID).Scan(&lastEvent); err != nil {
		return false, fmt.Errorf("read the last progress of job %d: %w", job.ID, err)
	}
	note := reason
	if lastEvent != "" {
		note += "\nLast progress: " + boundNote(lastEvent)
	}
	r, err := e.queueTaskRestartTx(tx, job.ProjectID, job.TaskID, note, agent, now)
	if err != nil {
		return false, err
	}
	if err := inboxEventTx(tx, evTaskRestarted, job.ProjectID, job.TaskID, r.JobID,
		eventPayload{Status: taskReady, Run: int64(r.Run), Reason: reason, Agent: agent}, now); err != nil {
		return false, err
	}
	return true, e.wakeForemanTx(tx, job.ProjectID, now)
}

// queueFixRoundTx queues the task's next fix round (§3): a job of kind
// fix on the task's own key, in session mode, so it resumes the author's
// session on the author's agent — the required agent is the pin, the
// harness beside it, and the session's affinity the runner (§8). The
// prompt is engine-composed (§1.4, §5.3): the worker template, the spec
// verbatim, and the feedback verbatim — the check's output after a
// failed check, the review's findings after a changes verdict. The
// round names the budget it spends (§7): fix rounds after a failed
// check, review rounds after changes. When the budget is spent the task
// parks at needs_foreman with the event naming which cap ran out and
// note beside it — the check's tail or the findings, the shorter line
// of the same matter — for the foreman's judgment (§3, §10).
func (e *Engine) queueFixRoundTx(tx *sql.Tx, job *jobRow, round, feedback, note string, failure *checkFailure, now time.Time) error {
	_, capped, err := e.roundCapReached(tx, SubmitParams{TaskID: job.TaskID, Round: round})
	if err != nil {
		return err
	}
	if capped {
		return e.outOfRoundsTx(tx, job, round, note, failure, now)
	}
	spec, err := taskSpecTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	check, err := taskCheckTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	instructions, err := taskInstructionsTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	harness, agent, err := authorPlacementTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	sr, err := e.submitTx(tx, SubmitParams{
		Key:          job.Key,
		ProjectID:    job.ProjectID,
		TaskID:       job.TaskID,
		Kind:         KindFix,
		Mode:         ModeSession,
		Prompt:       prompt.ComposeWorkerPrompt(spec, check, feedback, prompt.WithInstructions(instructions)),
		Harness:      harness,
		RequireAgent: agent,
		Round:        round,
	}, now)
	if err != nil {
		return err
	}
	switch sr.Answer {
	case AnsCreated:
		_, err = appendEventTx(tx, evTaskFixRound, job.ProjectID, job.TaskID, job.ID,
			eventPayload{Run: int64(sr.Run), Reason: round}, now)
		return err
	case AnsCapped:
		return e.outOfRoundsTx(tx, job, round, note, failure, now)
	default:
		// The completing job freed the key earlier in this transaction,
		// so no live job can hold this submit. An event without its job
		// would log a round that was never queued, so the impossible
		// answer is an error instead.
		return fmt.Errorf("queue the fix round of task %d: submit %s answered %s", job.TaskID, job.Key, sr.Answer)
	}
}

// outOfRoundsTx parks a task whose rounds budget is spent (§3: "task
// out of rounds"): the rules have carried it as far as its caps allow,
// and what follows — re-plan, switch agent, split, ask the user — is
// the foreman's judgment, so the event names which cap ran out and
// carries what asked for the round — the check's tail or the review's
// findings, bounded — beside it: that note is the needs-you queue's
// reason line, the foreman's first word of the matter (§10).
func (e *Engine) outOfRoundsTx(tx *sql.Tx, job *jobRow, round, note string, failure *checkFailure, now time.Time) error {
	reason := "the task's " + round + " are spent"
	if failure != nil {
		// The check's command and exit code render first (needsYouReason),
		// so the reason keeps the round's name and the worker's words.
		reason = round + " spent"
	}
	if note != "" {
		reason += ": " + boundNote(note)
	}
	if err := recordTaskOutcomeTx(tx, job, false, now); err != nil {
		return err
	}
	payload := eventPayload{Reason: reason}
	if failure != nil {
		payload = failure.payload(reason)
	}
	return e.needsForemanTx(tx, job, evTaskOutOfRounds, payload, now)
}

// checkFailure is a failed check as the events and the fix round's
// feedback quote it: the command the task carries, the exit code, whether
// the timeout cut it short, and the tail of its output.
type checkFailure struct {
	Cmd      string
	ExitCode int
	TimedOut bool
	Output   string
}

// payload is the event payload of a failed check: the reason beside the
// bounded command, exit code and output tail.
func (f *checkFailure) payload(reason string) eventPayload {
	return eventPayload{
		Reason: reason, Check: boundNote(f.Cmd), ExitCode: f.ExitCode,
		TimedOut: f.TimedOut, CheckOutput: boundNote(f.Output),
	}
}

// feedback renders the failed check as the fix round's feedback: which
// command, how it ended, then its output tail verbatim.
func (f *checkFailure) feedback() string {
	out := f.Output
	if out == "" {
		out = "(the check printed nothing)"
	}
	return checkHeadline("The check", f.Cmd, f.ExitCode, f.TimedOut) + ".\n" + out
}

// checkEventText renders a check-failure event's payload for a reader of
// the event — the needs-you queue and the foreman's wake: the command,
// its exit code and its output (or "no output") first, then, for the
// spent rounds, the reason beside them. ok is false for any other event
// and for one that names no command.
func checkEventText(kind string, p eventPayload) (text string, ok bool) {
	if p.Check == "" || (kind != evTaskCheckFailed && kind != evTaskOutOfRounds) {
		return "", false
	}
	text = checkHeadline("check", p.Check, p.ExitCode, p.TimedOut)
	if p.CheckOutput == "" {
		text += " (no output)"
	} else {
		text += ": " + p.CheckOutput
	}
	if kind == evTaskOutOfRounds && p.Reason != "" {
		text += "; " + p.Reason
	}
	return text, true
}

// checkHeadline words a failed check: `<subject> "<command>" exited <code>`,
// plus " after the timeout" when the timeout cut it short.
func checkHeadline(subject, cmd string, exitCode int, timedOut bool) string {
	s := subject
	if cmd != "" {
		s += ` "` + cmd + `"`
	}
	s += " exited " + strconv.Itoa(exitCode)
	if timedOut {
		s += " after the timeout"
	}
	return s
}

// authorPlacementTx reads the placement the task's fix rounds pin to
// (§3, §8): the agent the latest work or fix job was leased to — the
// job a fix round resumes, whose session sits on its runner — and that
// agent's harness. The agent is the pin: the harness alone does not
// name it, for another runner may offer the same harness and the
// author's own may hold a second agent of it. An author with no agent
// — an agent-less runner, or one a re-register has since dropped —
// leaves the agent open and falls back to the job's own harness
// constraint, the record's only word on where the work ran.
func authorPlacementTx(tx *sql.Tx, taskID int64) (harness string, agent int64, err error) {
	var agentHarness, jobHarness string
	err = tx.QueryRow(`SELECT CASE WHEN a.id IS NULL THEN 0 ELSE j.agent_id END, COALESCE(a.harness, ''), COALESCE(j.harness, '')
		FROM jobs j LEFT JOIN agents a ON a.id = j.agent_id
		WHERE j.task_id = ? AND j.kind IN ('work', 'fix')
		ORDER BY j.id DESC LIMIT 1`, taskID).Scan(&agent, &agentHarness, &jobHarness)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil // no author job: the fix round's placement stays open
	}
	if err != nil {
		return "", 0, fmt.Errorf("read the author job of task %d: %w", taskID, err)
	}
	if agentHarness != "" {
		return agentHarness, agent, nil
	}
	return jobHarness, agent, nil
}

// queueReviewTx moves an implement task into reviewing and queues its
// review job (§7): same key, fresh session, placed on another family
// than the author's. The engine composes the reviewer's prompt itself
// (§1.4: only the engine composes prompts) — the review template, the
// spec and the check result verbatim, the feedback the author was given
// for the round (the completing job's prompt is its record: a fix
// round's check output or findings, a restart's note), and the diff
// command naming the task's base and the result branch. The
// implementer's summary is
// deliberately no input of the composition (§7's anti-steering rule);
// the branch and base stay on the task's own row for the runner's
// grant to read at lease time. The branch itself is the engine's, from
// the job it was named for at lease time — the runner pushed exactly
// it (§5.1) — and a worker's block naming another branch cannot
// redirect the review: the block's name stands in only for a job no
// engine named a branch for.
func (e *Engine) queueReviewTx(tx *sql.Tx, job *jobRow, wr workerResultFields, checkJSON string, now time.Time) error {
	branch := job.Branch
	if branch == "" {
		branch = wr.Branch
	}
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, branch = COALESCE(NULLIF(?, ''), branch), updated_at = ?
		WHERE id = ?`, taskReviewing, branch, fmtTime(now), job.TaskID); err != nil {
		return fmt.Errorf("move task %d to reviewing: %w", job.TaskID, err)
	}
	if _, err := appendEventTx(tx, evTaskReviewing, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: taskReviewing, Branch: branch}, now); err != nil {
		return err
	}
	// An unknown author family leaves ExcludeFamily empty; only an explicit agent exclusion applies.
	exclude, err := agentFamilyTx(tx, job.AgentID)
	if err != nil {
		return err
	}
	spec, base, err := taskSpecAndBaseTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	instructions, err := taskInstructionsTx(tx, job.TaskID)
	if err != nil {
		return err
	}
	// The one-live-job-per-key rule is free here: the work job went
	// terminal earlier in this completion's transaction. A review spends
	// none of the task's rounds (§7 caps the review loop's fix rounds,
	// not the reviews), so nothing here answers capped.
	_, err = e.submitTx(tx, SubmitParams{
		Key:           job.Key,
		ProjectID:     job.ProjectID,
		TaskID:        job.TaskID,
		Kind:          KindReview,
		Mode:          ModeFresh,
		Prompt:        prompt.ComposeReviewPrompt(spec, prompt.WorkerFeedback(job.Prompt), checkJSON, base, branch, prompt.WithInstructions(instructions)),
		ExcludeFamily: exclude,
	}, now)
	if err != nil {
		return err
	}
	// A review nobody could ever take is flagged at once, not at the
	// next sweep: the user hears of it when the task starts waiting.
	return flagUnservableReviewsTx(tx, now)
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
// a substantive approve finishes the task; changes asks for a fix round
// on the author's session with the findings as the feedback (§3), or
// parks the task when the review rounds are spent; anything else — no
// block, a malformed one, an approve without substantive findings, a run
// that ended in an error — is no verdict and goes to another reviewer
// once, before the task parks waiting for the foreman.
func (e *Engine) reviewCompletedTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	var v verdictBlock
	haveVerdict := json.Unmarshal([]byte(c.Result), &v) == nil &&
		(v.Verdict == verdictApprove || v.Verdict == verdictChanges)
	switch {
	case haveVerdict && v.Verdict == verdictApprove && len(v.Findings) >= minSubstantiveFindings:
		if err := recordOutcomeTx(tx, job, now); err != nil {
			return err
		}
		return e.taskDoneTx(tx, job, now)
	case haveVerdict && v.Verdict == verdictChanges:
		// The findings are the fix round's feedback (§3) and the event's
		// own payload (§9): the review's words are kept whatever follows.
		if _, err := appendEventTx(tx, evReviewChanges, job.ProjectID, job.TaskID, job.ID,
			eventPayload{Findings: boundNote(v.Findings)}, now); err != nil {
			return err
		}
		return e.queueFixRoundTx(tx, job, roundReview, v.Findings, v.Findings, nil, now)
	default:
		reason := "no verdict in the review result"
		switch {
		case c.Error != "":
			// The run itself failed — the reviewer's CLI crashed, its
			// account hit a limit — and the error is the retry's to
			// carry, not lost beside a generic no-verdict.
			reason = c.Error
		case haveVerdict:
			reason = "approve without substantive findings"
		}
		return e.queueReviewRetryTx(tx, job, reason, now)
	}
}

// queueReviewRetryTx answers an unusable review with another reviewer
// (§7): a new review job, once per review cycle — a cycle being the
// review of the change the task's latest author job produced — placed
// on another agent of a non-author family, so the same reviewer never
// reads its own output again. The retry asks the same question: its
// prompt is the unusable review's own, which the engine composed from
// the same template and the same verbatim data. When the retry is spent
// or no agent can take it, the task parks at needs_foreman with the
// no-verdict event, for the foreman's judgment.
func (e *Engine) queueReviewRetryTx(tx *sql.Tx, job *jobRow, reason string, now time.Time) error {
	spent, err := reviewRetrySpentTx(tx, job)
	if err != nil {
		return err
	}
	noAgent, err := noReviewAgentTx(tx, job.ExcludeFamily, job.AgentID)
	if err != nil {
		return err
	}
	if spent || noAgent {
		if noAgent && !spent {
			reason += "; no other agent of another family can take the retry"
		}
		return e.needsForemanTx(tx, job, evReviewNoVerdict, eventPayload{Reason: boundNote(reason)}, now)
	}
	sr, err := e.submitTx(tx, SubmitParams{
		Key:           job.Key,
		ProjectID:     job.ProjectID,
		TaskID:        job.TaskID,
		Kind:          KindReview,
		Mode:          ModeFresh,
		Prompt:        job.Prompt,
		ExcludeFamily: job.ExcludeFamily,
		ExcludeAgent:  job.AgentID,
	}, now)
	if err != nil {
		return err
	}
	if sr.Answer != AnsCreated {
		// The unusable review freed the key earlier in this transaction,
		// and a retry spends none of the task's rounds (§7), so no answer
		// but a created job is expected here; an event without its job
		// would log a retry that was never queued.
		return fmt.Errorf("queue the review retry of task %d: submit %s answered %s", job.TaskID, job.Key, sr.Answer)
	}
	_, err = appendEventTx(tx, evReviewRetry, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Run: int64(sr.Run), Reason: boundNote(reason)}, now)
	return err
}

// reviewRetrySpentTx reports whether this review is itself the retry of
// its cycle (§7: up to one retry): the reviews of one cycle are the
// review jobs queued after the task's latest author job, so a second
// one means the cycle's retry already ran. A restarted review — one a
// restart round replaced, §5.6 — is the same review asked again, not
// another reviewer's word, so it does not count. Author jobs — work or
// fix, including restarts — begin a new cycle with a fresh first
// review.
func reviewRetrySpentTx(tx *sql.Tx, job *jobRow) (bool, error) {
	var reviews int64
	err := tx.QueryRow(`SELECT COUNT(*) FROM jobs
		WHERE task_id = ? AND kind = 'review' AND restart = 0
		AND id > COALESCE((SELECT MAX(id) FROM jobs WHERE task_id = ? AND kind IN ('work', 'fix')), 0)`,
		job.TaskID, job.TaskID).Scan(&reviews)
	if err != nil {
		return false, fmt.Errorf("count the reviews of task %d: %w", job.TaskID, err)
	}
	return reviews > 1, nil
}

// noReviewAgentTx reports whether no registered agent could take the
// retry (§7): none passes the exclusions the job would carry — another
// family than the author's, not the first reviewer's agent, offering
// the review class — under the same test the lease-time pick applies
// (§8). An agent whose runner never polls again waits out the queue TTL
// like any queued job; this is the test for no candidate at all.
func noReviewAgentTx(tx *sql.Tx, excludeFamily string, excludeAgent int64) (bool, error) {
	rows, err := tx.Query("SELECT id, family, classes FROM agents")
	if err != nil {
		return false, fmt.Errorf("list the agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var family, classes string
		if err := rows.Scan(&id, &family, &classes); err != nil {
			return false, fmt.Errorf("list the agents: %w", err)
		}
		if reviewAgentQualifies(id, family, classes, excludeFamily, excludeAgent) {
			return false, nil
		}
	}
	return true, rows.Err()
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

// recordTaskOutcomeTx counts a task's end on the outcome record (§8,
// §11): one attempt on the task's class for the agent of the task's last
// work or fix job — the author of the final result, which a fix round may
// have moved off the first agent — and one success when the task ended
// approved: its check passed and its review approved within the rounds.
// A task that ran out of rounds counts the attempt alone. An author with
// no agent — an agent-less runner, a task cancelled before it ran — has
// nobody to score and records nothing. The task's state guards the
// count: a task already past its end (done, failed, cancelled, or parked
// at needs_foreman by the same end) was counted once, so a second
// processing of the same end adds nothing. It runs before the transition
// that moves the state, in that transition's transaction.
func recordTaskOutcomeTx(tx *sql.Tx, job *jobRow, success bool, now time.Time) error {
	var state, class string
	if err := tx.QueryRow("SELECT state, class FROM tasks WHERE id = ?", job.TaskID).Scan(&state, &class); err != nil {
		return fmt.Errorf("read task %d: %w", job.TaskID, err)
	}
	switch state {
	case taskDone, taskFailed, taskCancelled, taskNeedsForeman:
		return nil
	}
	var agent int64
	err := tx.QueryRow(`SELECT COALESCE(agent_id, 0) FROM jobs
		WHERE task_id = ? AND kind IN ('work', 'fix')
		ORDER BY id DESC LIMIT 1`, job.TaskID).Scan(&agent)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && agent == 0) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the author job of task %d: %w", job.TaskID, err)
	}
	successes := 0
	if success {
		successes = 1
	}
	_, err = tx.Exec(`INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_id, agent_id, class) DO UPDATE SET
			attempts = attempts + 1, successes = successes + excluded.successes, updated_at = excluded.updated_at`,
		job.ProjectID, agent, class, successes, fmtTime(now))
	if err != nil {
		return fmt.Errorf("record the outcome of task %d: %w", job.TaskID, err)
	}
	return nil
}

// needsForemanTx parks a task at needs_foreman (§3): the completion was
// nothing the rules can carry forward on their own, so the task waits
// for the foreman's judgment. The event kind names what waited and the
// payload what the foreman will read. The event lands in the foreman's
// inbox (§4.2) and a wake is evaluated, both in this same transaction:
// the park itself is what makes the foreman's turn happen.
func (e *Engine) needsForemanTx(tx *sql.Tx, job *jobRow, kind string, payload eventPayload, now time.Time) error {
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
		taskNeedsForeman, fmtTime(now), job.TaskID); err != nil {
		return fmt.Errorf("park task %d at needs_foreman: %w", job.TaskID, err)
	}
	payload.Status = taskNeedsForeman
	if err := inboxEventTx(tx, kind, job.ProjectID, job.TaskID, job.ID, payload, now); err != nil {
		return err
	}
	return e.wakeForemanTx(tx, job.ProjectID, now)
}

// taskDoneTx finishes a task and, when that closed the plan's last open
// task, the plan with it (§6.1, §6.2). The plan_done event names the
// task and the job that ended it; the user's notification (§3, §10)
// rides on that event later. The plan_done event is a judgment event too
// (§4.2) — the foreman reports the finished plan — so it lands in the
// inbox and wakes a turn, in this same transaction.
func (e *Engine) taskDoneTx(tx *sql.Tx, job *jobRow, now time.Time) error {
	if err := recordTaskOutcomeTx(tx, job, true, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
		taskDone, fmtTime(now), job.TaskID); err != nil {
		return fmt.Errorf("finish task %d: %w", job.TaskID, err)
	}
	if _, err := appendEventTx(tx, evTaskDone, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Status: taskDone}, now); err != nil {
		return err
	}
	if err := e.releaseDependentsTx(tx, job.ProjectID, job.TaskID, now); err != nil {
		return err
	}
	var plan int64
	if err := tx.QueryRow("SELECT plan_id FROM tasks WHERE id = ?", job.TaskID).Scan(&plan); err != nil {
		return fmt.Errorf("read the plan of task %d: %w", job.TaskID, err)
	}
	_, err := e.finishPlanIfClosedTx(tx, job.ProjectID, plan, job.TaskID, job.ID, now)
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
