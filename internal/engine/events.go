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
	evProjectArchived        = "project_archived"          // removal retains history but closes the work gate
	evProjectSettingSet      = "project_setting_set"       // a project setting changed — `brigada project set` or an approve that stops asking (§6.3); Reason lists key=value
	evPlanCancelled          = "plan_cancelled"            // removal cancelled an unfinished plan
	evPlanCreated            = "plan_created"              // CreatePlan: a plan now exists (§6.2)
	evPlanDone               = "plan_done"                 // the plan's last open task finished (§6.2)
	evPlanAwaitingApproval   = "plan_awaiting_approval"    // CreatePlan: the plan is over the approval limit and waits for the user's yes (§6.3)
	evPlanReviewQueued       = "plan_review_queued"        // a plan_review job is queued: the plan waits in reviewing for the verdict before any work is queued (§6.2)
	evPlanReviewed           = "plan_reviewed"             // a plan review round ended: approve, changes (with findings), no verdict, or accepted by the foreman (plan_accept); the plan proceeded, parks at needs_foreman for the foreman's answer, or waits for the user's yes (§6.2)
	evPlanAmended            = "plan_amended"              // the foreman amended a live plan: tasks added, cancelled or re-pointed (§12.2)
	evPlanApproved           = "plan_approved"             // the user approved the plan; its first wave is queued (§6.3)
	evPlanRejected           = "plan_rejected"             // the user rejected the plan; its tasks are cancelled (§6.3)
	evJobCreated             = "job_created"               // submit: a queued job now exists
	evJobPending             = "job_pending"               // submit: coalesced behind the key's live job
	evJobCapped              = "job_capped"                // submit: the task's round cap is spent
	evJobLeased              = "job_leased"                // a runner took the job
	evJobCapHeld             = "job_cap_held"              // a lease poll skipped the job: a cap layer has no room (§8.1)
	evJobAgentPicked         = "job_agent_picked"          // why a task job's lease took its agent: score, weight and draws of the sampled ranking, passed over for cap room (§8)
	evLeaseRevived           = "lease_revived"             // a heartbeat revived a lapsed lease
	evJobReleased            = "job_released"              // the holder gave the job back
	evJobCompleted           = "job_completed"             // done, failed, stalled, time_cap, check_failed…
	evJobCancelled           = "job_cancelled"             // a live job was stopped
	evLeaseRequeued          = "lease_requeued"            // the sweep requeued an expired lease
	evJobHardCapped          = "job_hard_capped"           // the sweep failed a job past its kind's hard cap
	evJobQueueTimedOut       = "job_queue_timed_out"       // the sweep failed a job past the queue TTL
	evJobAttemptsExceeded    = "job_attempts_exceeded"     // the job lost MaxAttempts runners
	evFenceEpochBumped       = "fence_epoch_bumped"        // recover fenced older-epoch leases out (§6.4)
	evStoreRecovered         = "store_recovered"           // recover rebuilt the store; dispatch is held one lease TTL (§6.4)
	evPlanRecovered          = "plan_recovered"            // recover rebuilt or updated a plan from its plan file (§6.4)
	evSnapshotTaken          = "snapshot_taken"            // the engine snapshotted its store (§6.4)
	evTaskRunning            = "task_running"              // a lease took the task from ready (§6.1)
	evTaskReviewing          = "task_reviewing"            // the work passed its check; the review job is queued (§7)
	evTaskDone               = "task_done"                 // the task is finished (§6.1)
	evTaskCheckFailed        = "task_check_failed"         // the check failed; a fix round answers it, or the task waits for the foreman (§3)
	evTaskFixRound           = "task_fix_round"            // a fix round is queued on the author's session (§3)
	evTaskFixRetry           = "task_fix_retry"            // a fix round's transient CLI failure was retried, on the author's session or another agent (§3)
	evTaskInfraRetry         = "task_infra_retry"          // a work or fix job's infrastructure failure was requeued on the infra backoff; no round, restart or wake is spent (§5.1)
	evTaskOutOfRounds        = "task_out_of_rounds"        // the task's rounds are spent and escalation declined; the foreman decides (§3)
	evTaskNeedsHelp          = "task_needs_help"           // the worker asked its question; the foreman answers (§3)
	evTaskDiscoveries        = "task_discoveries"          // a worker's result carried discoveries; they reach the foreman, no wake of their own (§4.2)
	evTaskAnswered           = "task_answered"             // the foreman answered; the author's session resumes (§3)
	evTaskTierBumped         = "task_tier_bumped"          // rule escalation; TierAsked is the old tier, TierServed the new tier, Reason the trigger (§8)
	evTaskEffortBumped       = "task_effort_bumped"        // rule escalation on the same tier; EffortFrom is the old effort, EffortTo the new, Reason the trigger (§8)
	evTaskRestarted          = "task_restarted"            // a foreman or automatic restart queued a fresh work attempt (§3, §5.6)
	evTaskFailed             = "task_failed"               // the run failed or said nothing usable; the foreman decides (§3)
	evTaskLoop               = "task_loop"                 // the worker repeats itself: the same check failure across fix rounds, or the same tool call in a window; the foreman decides (§5.6)
	evReviewChanges          = "review_changes"            // the reviewer wants changes; a fix round answers them (§3, §7)
	evReviewNoVerdict        = "review_no_verdict"         // the review ended without a usable verdict; the retry is spent or impossible (§7)
	evReviewVerdict          = "review_verdict"            // a standalone review (no task) ended: approve, changes or no verdict, with the findings and the branch (§7)
	evReviewUnservable       = "review_unservable"         // a task's queued review can never be taken: no agent of another family offers review (§7, §10)
	evReviewRetry            = "review_retry"              // an unusable verdict goes to another reviewer (§7)
	evTaskLandHeld           = "task_land_held"            // the task waits at the land gate (§6.3); Reason names the rule
	evTaskPlanHeld           = "task_plan_held"            // a plan rule held a task that had not started — an amendment's added or respecified task, or the plan's spend passing approval_budget — and it waits at the plan gate (§6.3); Reason names the rule
	evTaskLandReleased       = "task_land_released"        // the user released the land gate; the landing re-evaluates (§6.3)
	evTaskLandRejected       = "task_land_rejected"        // the user rejected the landing; the branch stays, nothing recorded (§6.3)
	evTaskLanded             = "task_landed"               // the task's landing succeeded (§7.1); Branch, Commit and Pushed name it
	evTaskPushed             = "task_pushed"               // the push phase pushed a waiting landing (§7.1)
	evPushApproved           = "push_approved"             // the user released the push gate for waiting landings (§6.3)
	evTaskLandFailed         = "task_land_failed"          // a conflict or a failed check on the landed result; the task parks (§7.1)
	evTaskLandNotReady       = "task_land_not_ready"       // the land_ready gate answered not ready (issue #87); the landing is requeued after the backoff, Until names the next try
	evProjectForemanSet      = "project_foreman_set"       // setup: the project's foreman harness and model were chosen (§4.1)
	evForemanReport          = "foreman_report"            // a done foreman turn's final message, reported to the user (§3, §10)
	evUserMessage            = "user_message"              // the user's message to the foreman, sent without attaching (§4.2 item 3)
	evForemanWoken           = "foreman_woken"             // a foreman turn was queued with the project's pending events (§4.2)
	evForemanWakeBudgetSpent = "foreman_wake_budget_spent" // the day's foreman wake budget is spent (Count: the budget, Until: the window's end); the events stay pending (§4.3)
	evForemanWakeStalled     = "foreman_wake_stalled"      // foreman turns failed consecutively on the same events; waking stops (§4.2)
	evForemanOutageSpent     = "foreman_outage_spent"      // the foreman's turns failed on the infrastructure until the retry window was spent; waking stops and the needs-you queue names the outage (§5.1)
	evForemanLimited         = "foreman_limited"           // the foreman's CLI hit its subscription limit; waking stops until it resets or the user switches (§4.4)
	evForemanAttached        = "foreman_attached"          // a hold took the foreman for an attached chat (§4.2 item 1)
	evForemanDetached        = "foreman_detached"          // the attached chat ended; the hold released (§4.2 item 1)
	evForemanAttachExpired   = "foreman_attach_expired"    // the hold expired unheartbeated; its rows returned pending (§4.2 item 1)
	evForemanSessionRotated  = "foreman_session_rotated"   // the foreman session outgrew foreman_rotate_tokens; the next turn starts fresh (Tokens, Threshold, Gen) (§4.2)
	evAgentCooling           = "agent_cooling"             // a task job's limit cooled its account until a time (§8 "Quota")
	evAgentBroken            = "agent_broken"              // an agent was marked broken, with the reason (§8 "Health")
	evAgentRecovered         = "agent_recovered"           // a broken agent is ready again: a runner's return (runner stale) or a passing probe (repeated failures) (§8 "Health")
	evAgentProbeFailed       = "agent_probe_failed"        // a broken agent's probe failed; the next probe is Until (§8 "Health")
	evAgentReset             = "agent_reset"               // the user cleared broken and cooling on the agents of a harness and model (§8)
	evEngineSuspended        = "engine_suspended"          // the engine's host slept; the sweep restarted every wall-clock deadline (§13)
)

// eventPayload is the small JSON an event row carries: the fields the
// kind's readers need, each left out when empty. The lease appears as
// its id's prefix — enough to tie the log entry to the lease, never the
// whole capability.
type eventPayload struct {
	Answer  string `json:"answer,omitempty"`
	Status  string `json:"status,omitempty"` // the completion's, or the state a task or plan reached
	Reason  string `json:"reason,omitempty"` // a check's output tail, a failure's words (§3, §5.5)
	Lease   string `json:"lease,omitempty"`
	Owner   int64  `json:"owner,omitempty"`
	Run     int64  `json:"run,omitempty"`
	Attempt int64  `json:"attempt,omitempty"`
	Epoch   int64  `json:"epoch,omitempty"`
	Layer   string `json:"layer,omitempty"` // the cap layer that held the job (§8.1)
	// The failed check a task_check_failed or task_out_of_rounds names:
	// its command, exit code, whether the timeout cut it short, and the
	// tail of its output — each bounded.
	Check       string `json:"check,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
	TimedOut    bool   `json:"timed_out,omitempty"`
	CheckOutput string `json:"check_output,omitempty"`
	Branch      string `json:"branch,omitempty"`   // the task's result branch (§6.4)
	Commit      string `json:"commit,omitempty"`   // a landing's resulting commit (§7.1)
	Pushed      string `json:"pushed,omitempty"`   // a landing's push state: pushed, waiting or local (§7.1)
	PushCmd     string `json:"push_cmd,omitempty"` // the exact push command for gate_push never (§7.1)
	// AfterLandError is an after_land command's failure, reported in the
	// record and the history, never undone (§6.3).
	AfterLandError string `json:"after_land_error,omitempty"`
	Question       string `json:"question,omitempty"` // the worker's question (§5.4)
	// Discoveries is what a worker learned beyond its own task (§4.2).
	Discoveries string `json:"discoveries,omitempty"`
	// QuestionID names the questions row an answer filled (§3, §11):
	// the task_answered event's own reference to the exchange it made.
	QuestionID int64  `json:"question_id,omitempty"`
	Findings   string `json:"findings,omitempty"` // the review's findings (§7)
	Report     string `json:"report,omitempty"`   // a foreman turn's final message, its report to the user, bounded to about 4 KB (boundReport) (§3, §10)
	Text       string `json:"text,omitempty"`     // a user message's words (§4.2 item 3)
	Source     string `json:"source,omitempty"`   // the client token label a user message came from (§12.2)
	Harness    string `json:"harness,omitempty"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`  // a foreman setting's reasoning-effort level
	Account    string `json:"account,omitempty"` // the quota account a cooling names (§8)
	Until      string `json:"until,omitempty"`   // a deadline, RFC3339: a foreman limit's reset, a probe's next attempt; absent when unknown (§4.4)
	Job        int64  `json:"job,omitempty"`     // a job the event names besides its own job_id column
	Hold       int64  `json:"hold,omitempty"`    // the attach hold the event names (§4.2 item 1)
	Count      int64  `json:"count,omitempty"`   // how many of something the event counts (§4.2, §4.3)
	Path       string `json:"path,omitempty"`    // a snapshot's file (§6.4)
	Bytes      int64  `json:"bytes,omitempty"`   // a snapshot's size (§6.4)
	// The agent pick's why (§8): the agent, its score, how the rank
	// chose it (pickSampled), its cost weight and draw, every ranked
	// candidate's weight and draw in rank order, and the better-ranked
	// agents passed over because their cap path had no room.
	// The approval limit's why (§6.3): the plan, and the classes and
	// protected paths that put it over (Count holds the task count,
	// Reason the reasons in words).
	Plan int64 `json:"plan,omitempty"`
	// Prompt is what a leased job's prompt was composed from (§5.9
	// 'Versioning', §9): the template version and the digests of the
	// instruction and skill texts it carried.
	Prompt  *promptMeta `json:"prompt,omitempty"`
	Classes []string    `json:"classes,omitempty"`
	Paths   []string    `json:"paths,omitempty"`
	Agent   int64       `json:"agent,omitempty"`
	Score   float64     `json:"score,omitempty"`
	Pick    string      `json:"pick,omitempty"`
	// ExclusionLifted marks an agent pick of the very agent a review
	// retry's exclusion named (§7, decided 2026-10-20): no other
	// qualifying reviewer was available, so the preference was lifted
	// and the excluded agent took the job again.
	ExclusionLifted bool     `json:"exclusion_lifted,omitempty"`
	TierAsked       string   `json:"tier_asked,omitempty"`
	TierServed      string   `json:"tier_served,omitempty"`
	EffortFrom      string   `json:"effort_from,omitempty"` // an effort step's old effective effort (§8)
	EffortTo        string   `json:"effort_to,omitempty"`   // an effort step's new effective effort (§8)
	Retiered        []int64  `json:"retiered,omitempty"`    // tasks whose tier or effort plan_amend changed
	PassedOver      []int64  `json:"passed_over,omitempty"`
	Weight          *int     `json:"weight,omitempty"`
	Draw            float64  `json:"draw,omitempty"`
	Candidates      []Ranked `json:"candidates,omitempty"`
	// A foreman session rotation's why (§4.2): the input tokens the last
	// turn read, the threshold it crossed, and the generation it opened.
	Tokens    int64 `json:"tokens,omitempty"`
	Threshold int64 `json:"threshold,omitempty"`
	Gen       int64 `json:"gen,omitempty"`
	// SuspendedFor is the wall-clock gap an engine_suspended event names
	// (§13): how long the host slept, in whole seconds.
	SuspendedFor int64 `json:"suspended_for,omitempty"`
}

// Ranked is one candidate of an agent pick (§8): its cost weight and the
// success-rate draw the ranking sorted it by.
type Ranked struct {
	Agent  int64   `json:"agent"`
	Weight int     `json:"weight"`
	Draw   float64 `json:"draw"`
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
	msgKindUserMessage  = "user_message"
)

// authorWorker is the messages' author for what a run said. The engine
// speaks as itself in task_spec; the result block is the worker's.
const (
	authorEngine = "engine"
	authorWorker = "worker"
	authorUser   = "user"
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
	// Discoveries is what the worker learned beyond its task (§5.4, §9).
	Discoveries string `json:"discoveries,omitempty"`
	// CommitMessage is the commit message the worker wrote for the land
	// job (§5.4), subject and body in the repo's own style; it stays with
	// the result message like the rest of the block.
	CommitMessage string `json:"commit_message,omitempty"`
}

// workerResultMessage renders a completion's result as the message the
// history keeps: the block's own fields when it parsed — a worker's
// result block (§5.4), or a reviewer's verdict block (§7), decoded as
// reviewCompletedTx decodes it — else the block verbatim under an
// unknown status, so a malformed contract still shows up as the
// worker's answer rather than vanishing.
func workerResultMessage(result string) string {
	var wr workerResultFields
	if err := json.Unmarshal([]byte(result), &wr); err == nil && wr.Status != "" {
		if b, err := json.Marshal(wr); err == nil {
			return string(b)
		}
	}
	var v verdictBlock
	if err := json.Unmarshal([]byte(result), &v); err == nil && v.Verdict != "" {
		if b, err := json.Marshal(v); err == nil {
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
