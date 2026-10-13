package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// The event kinds the engine writes to the append-only log (design §9):
// one per state change it makes, so the log is the backbone the history
// hangs off. The set is open — new kinds arrive with engine features —
// and the payload's vocabulary stays small: what happened, never the
// prompts or secrets that made it happen.
const (
	evPlanCreated         = "plan_created"          // CreatePlan: a plan now exists (§6.2)
	evPlanDone            = "plan_done"             // the plan's last open task finished (§6.2)
	evJobCreated          = "job_created"           // submit: a queued job now exists
	evJobPending          = "job_pending"           // submit: coalesced behind the key's live job
	evJobCapped           = "job_capped"            // submit: the task's round cap is spent
	evJobLeased           = "job_leased"            // a runner took the job
	evJobCapHeld          = "job_cap_held"          // a lease poll skipped the job: a cap layer has no room (§8.1)
	evLeaseRevived        = "lease_revived"         // a heartbeat revived a lapsed lease
	evJobReleased         = "job_released"          // the holder gave the job back
	evJobCompleted        = "job_completed"         // done, failed, stalled, time_cap, check_failed…
	evJobCancelled        = "job_cancelled"         // a live job was stopped
	evLeaseRequeued       = "lease_requeued"        // the sweep requeued an expired lease
	evJobHardCapped       = "job_hard_capped"       // the sweep failed a job past its kind's hard cap
	evJobQueueTimedOut    = "job_queue_timed_out"   // the sweep failed a job past the queue TTL
	evJobAttemptsExceeded = "job_attempts_exceeded" // the job lost MaxAttempts runners
	evFenceEpochBumped    = "fence_epoch_bumped"    // recover fenced older-epoch leases out (§6.4)
	evTaskRunning         = "task_running"          // a lease took the task from ready (§6.1)
	evTaskReviewing       = "task_reviewing"        // the work passed its check; the review job is queued (§7)
	evTaskDone            = "task_done"             // the task is finished (§6.1)
	evTaskCheckFailed     = "task_check_failed"     // the check failed; the task waits for the foreman (§3)
	evTaskNeedsHelp       = "task_needs_help"       // the worker asked its question; the foreman answers (§3)
	evTaskFailed          = "task_failed"           // the run failed or said nothing usable; the foreman decides (§3)
	evReviewChanges       = "review_changes"        // the reviewer wants changes; the foreman decides (§7)
	evReviewNoVerdict     = "review_no_verdict"     // the review ended without a usable verdict (§7)
	evProjectForemanSet   = "project_foreman_set"   // setup: the project's foreman harness and model were chosen (§4.1)
)

// eventPayload is the small JSON an event row carries: the fields the
// kind's readers need, each left out when empty. The lease appears as
// its id's prefix — enough to tie the log entry to the lease, never the
// whole capability.
type eventPayload struct {
	Answer   string `json:"answer,omitempty"`
	Status   string `json:"status,omitempty"` // the completion's, or the state a task or plan reached
	Reason   string `json:"reason,omitempty"` // a check's output tail, a failure's words (§3, §5.5)
	Lease    string `json:"lease,omitempty"`
	Owner    int64  `json:"owner,omitempty"`
	Run      int64  `json:"run,omitempty"`
	Attempt  int64  `json:"attempt,omitempty"`
	Epoch    int64  `json:"epoch,omitempty"`
	Layer    string `json:"layer,omitempty"`    // the cap layer that held the job (§8.1)
	Branch   string `json:"branch,omitempty"`   // the task's result branch (§6.4)
	Question string `json:"question,omitempty"` // the worker's question (§5.4)
	Findings string `json:"findings,omitempty"` // the review's findings (§7)
	Harness  string `json:"harness,omitempty"`
	Model    string `json:"model,omitempty"`
}

// appendEventTx adds one row to the event log inside the caller's
// transaction (§9): what the engine does never lands without the log
// entry naming it, and a change that fails takes its event with it. The
// row's id comes back for the message that references the event.
func appendEventTx(tx *sql.Tx, kind string, project, task, job int64, payload eventPayload, now time.Time) (int64, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("encode the %s event: %w", kind, err)
	}
	res, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		kind, nullIfZero(project), nullIfZero(task), nullIfZero(job), string(b), fmtTime(now))
	if err != nil {
		return 0, fmt.Errorf("append the %s event: %w", kind, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("append the %s event: last insert id: %w", kind, err)
	}
	return id, nil
}

// appendMessageTx adds one conversation item to the project's history
// (§9): the message the event produced — a task's spec, a worker's
// result, and the items the foreman's later turns add. Kind stays an
// open set; eventID, when not 0, is the event the message answers to.
func appendMessageTx(tx *sql.Tx, project, eventID int64, kind, author, content string, now time.Time) error {
	_, err := tx.Exec(`INSERT INTO messages (project_id, event_id, kind, author, content, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		project, nullIfZero(eventID), kind, author, content, fmtTime(now))
	if err != nil {
		return fmt.Errorf("append the %s message: %w", kind, err)
	}
	return nil
}

// leasePrefix is how an event names a lease: the id's first bytes,
// enough to recognize, never the whole capability.
func leasePrefix(leaseID string) string {
	const keep = 8
	if len(leaseID) <= keep {
		return leaseID
	}
	return leaseID[:keep]
}

// The message kinds BP8a produces (§9). Like event kinds, an open set:
// the foreman's turns will add their own.
const (
	msgKindTaskSpec     = "task_spec"
	msgKindWorkerResult = "worker_result"
)

// authorWorker is the messages' author for what a run said. The engine
// speaks as itself in task_spec; the result block is the worker's.
const (
	authorEngine = "engine"
	authorWorker = "worker"
)

// workerResultFields is the result block's conversation shape (§5.4):
// what the worker_result message shows of a completed run. Reason is
// the runner's word for its own outcomes — no-result, a stopped run
// (§5.5, §5.6) — which a worker's own block never carries.
type workerResultFields struct {
	Status   string `json:"status"`
	Summary  string `json:"summary,omitempty"`
	Question string `json:"question,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// workerResultMessage renders a completion's result as the message the
// history keeps: the block's own fields when it parsed, else the block
// verbatim under an unknown status, so a malformed contract still shows
// up as the worker's answer rather than vanishing.
func workerResultMessage(result string) string {
	var wr workerResultFields
	if err := json.Unmarshal([]byte(result), &wr); err == nil && wr.Status != "" {
		if b, err := json.Marshal(wr); err == nil {
			return string(b)
		}
	}
	return `{"status":"unknown","summary":` + mustJSON(boundNote(tailOf(result, 400))) + `}`
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// tailOf returns the last max bytes of s, cut to a complete rune.
func tailOf(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut++
	}
	return s[cut:]
}

// completionStatus is how the completed event names the run's outcome:
// the stop outcome the runner stopped it with (§5.6), the error that
// failed it, else the status the worker's result block spoke — done,
// needs_help, check_failed, failed (§5.4, §5.5).
func completionStatus(c Completion) string {
	switch {
	case c.Outcome != "":
		return c.Outcome
	case c.Error != "":
		return statusFailed
	}
	var wr workerResultFields
	if err := json.Unmarshal([]byte(c.Result), &wr); err == nil && wr.Status != "" {
		return wr.Status
	}
	return statusDone
}
