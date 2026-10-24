package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrSlotHeld marks a foreman turn the engine will not submit because an
// attached chat holds the slot (§4.2 item 1, §4.6): a headless turn only
// ever starts from a free slot.
var ErrSlotHeld = errors.New("the foreman slot is held by an attached chat")

// SubmitParams describes the job to create for a key (§12.1).
type SubmitParams struct {
	// Key is the queue: 'task:<id>' for worker jobs, 'foreman:<project>'
	// for the foreman's turns. At most one live job exists per key.
	Key       string
	ProjectID int64
	TaskID    int64 // 0 for a foreman turn
	Kind      Kind
	Mode      Mode
	Prompt    string
	// Harness and Labels are placement constraints: the harness that
	// must run the job ('' = any) and the labels a runner must offer in
	// full (§8).
	Harness string
	Labels  []string
	// Model is the model a foreman turn must run on, beside its harness:
	// only an agent offering exactly that pair may lease the turn. Empty on
	// every other job, and on a turn submitted without one, which follows
	// the project's own choice (foremanPairTx).
	Model string
	// ExcludeFamily is the family rule's exclusion (§7, §8): when set,
	// only an agent of another family that offers the review class may
	// lease the job. The engine sets it on a task's review job, from
	// the family of the author job's agent.
	ExcludeFamily string
	// ExcludeAgent is the retry rule's exclusion (§3, §7): when set,
	// that one agent may not lease the job. Work jobs have no family
	// rule; review retries also apply the review family and class rules.
	ExcludeAgent int64
	// RequireAgent is a placement pin (§3): when set, the one agent
	// the job must run on — the author's own, read from the latest work
	// or fix job — because the harness alone does not name it: another
	// runner may offer the same harness, and the author's runner may
	// hold a second agent of it. An automatic restart pins a different
	// eligible agent instead. A runner not offering the agent cannot
	// lease the job at all.
	RequireAgent int64
	// Round names the task's rounds budget this job spends (§7, §6.1):
	// roundFix for a fix round a failed check asked for, roundReview
	// for one a review's changes asked for — both counted as the task's
	// fix jobs — and roundRestart for a restart. Empty spends nothing
	// (a work job, a review job: §7 caps the review loop's fix rounds,
	// not the reviews themselves) and lets a plain fix submit spend a
	// fix round. RoundNone spends nothing even for a fix job: the
	// foreman's answer resumes the author's session without a round
	// (§3), so its job must clear the caps and the counts both.
	Round string
	// Restart marks the job as a restart round (§5.6): any live job on
	// the key is cancelled first, and the job counts toward the task's
	// restart cap.
	Restart bool
	// Branch is the branch a standalone review (no task) reads: the one
	// job whose branch the submit names, since no task row holds it. The
	// engine names the branch of every other job at its first lease.
	Branch string
	// NotBefore is the time before which the queued job may not be
	// leased (§3, §4.2): the engine's retry and re-wake delays write it
	// so a job does not fire again while the network is still down. The
	// zero value means the job is leasable at once.
	NotBefore time.Time
	// SkipLandReady marks the land job an approval queues to release the
	// land_ready gate's hold (issue #87): the runner runs no command and
	// lands. False on every other job.
	SkipLandReady bool
}

// branch is the branch the submit names, for the job_created event: only
// a task-less review carries one.
func (p SubmitParams) branch() string {
	if p.TaskID == 0 {
		return p.Branch
	}
	return ""
}

// SubmitResult is what Submit did.
type SubmitResult struct {
	Answer Answer
	// JobID is the created job, or the live job a pending submit
	// coalesced behind.
	JobID int64
	// Run is the key's run number when a job was created: how many jobs
	// the key has had, this one included.
	Run int
}

// Submit creates a queued job for the key, or coalesces behind the live
// one (§12.1): while a live job holds the key, nothing is created,
// the live job's follow-up flag is set and the caller learns which job
// to wait for. A submit past the task's round cap creates nothing and
// answers capped. Losing the one-live-job-per-key race to a concurrent
// submit returns the same pending answer, never an error.
func (e *Engine) Submit(ctx context.Context, p SubmitParams) (SubmitResult, error) {
	var res SubmitResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.submitTx(tx, p, now)
		if err != nil {
			return fmt.Errorf("submit %s: %w", p.Key, err)
		}
		res = r
		return nil
	})
	return res, err
}

func (e *Engine) submitTx(tx *sql.Tx, p SubmitParams, now time.Time) (SubmitResult, error) {
	if err := activeProjectTx(tx, p.ProjectID); err != nil {
		return SubmitResult{}, err
	}
	switch {
	case p.Key == "":
		return SubmitResult{}, fmt.Errorf("key is required")
	case p.Kind == KindForeman && p.TaskID != 0:
		return SubmitResult{}, fmt.Errorf("a foreman turn belongs to no task")
	case p.Kind != KindForeman && p.Kind != KindReview && p.Kind != KindPlanReview && p.TaskID == 0:
		return SubmitResult{}, fmt.Errorf("kind %s needs a task", p.Kind)
	case p.Kind == KindReview && p.TaskID == 0 && p.Branch == "":
		return SubmitResult{}, fmt.Errorf("a review without a task needs a branch")
	}
	if p.Kind == KindForeman {
		// The common foreman submission boundary (§4.6): a headless turn
		// starts only from a free slot, whoever asks — the wake, a manual
		// submit or the admin path — so never beside an attached chat.
		var held int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM attach_holds WHERE project_id = ? AND state = 'live' AND expires_at > ?`,
			p.ProjectID, fmtTime(now)).Scan(&held); err != nil {
			return SubmitResult{}, fmt.Errorf("read the attach holds of project %d: %w", p.ProjectID, err)
		}
		if held > 0 {
			return SubmitResult{}, ErrSlotHeld
		}
	}
	round, capped, err := e.roundCapReached(tx, p)
	if err != nil {
		return SubmitResult{}, err
	}
	if capped {
		if _, err := appendEventTx(tx, evJobCapped, p.ProjectID, p.TaskID, 0,
			eventPayload{Answer: string(AnsCapped), Reason: "round cap reached: " + round}, now); err != nil {
			return SubmitResult{}, err
		}
		return SubmitResult{Answer: AnsCapped}, nil
	}
	if p.Restart {
		// §5.6: a restart replaces whatever is live on the key.
		live, err := liveJobTx(tx, p.Key)
		if err != nil {
			return SubmitResult{}, err
		}
		if live != nil {
			if _, err := e.cancelTx(tx, live, "replaced by a restart", now); err != nil {
				return SubmitResult{}, err
			}
		}
	}
	labels := p.Labels
	if labels == nil {
		labels = []string{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("encode labels: %w", err)
	}
	var run int
	if err := tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE key = ?", p.Key).Scan(&run); err != nil {
		return SubmitResult{}, fmt.Errorf("count jobs for the run number: %w", err)
	}
	run++

	nowStr := fmtTime(now)
	var branch any
	if p.TaskID == 0 && p.Branch != "" {
		branch = p.Branch
	}
	var notBefore any
	if !p.NotBefore.IsZero() {
		notBefore = fmtTime(p.NotBefore)
	}
	res, err := tx.Exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status,
		harness, model, labels, restart, round, exclude_family, exclude_agent, require_agent, branch, queued_at, not_before, skip_land_ready, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ProjectID, nullIfZero(p.TaskID), string(p.Kind), p.Key, string(p.Mode), p.Prompt,
		p.Harness, p.Model, string(labelsJSON), boolInt(p.Restart), round, p.ExcludeFamily, nullIfZero(p.ExcludeAgent),
		nullIfZero(p.RequireAgent), branch, nowStr, notBefore, boolInt(p.SkipLandReady), nowStr, nowStr)
	if err != nil {
		if !isUniqueViolation(err) {
			return SubmitResult{}, fmt.Errorf("create job: %w", err)
		}
		// The one-live-job-per-key race is lost: the winner's live job is
		// this submit's answer. The insert doubles as the live-job check,
		// so there is no window between the two for anyone to slip into.
		live, lerr := liveJobTx(tx, p.Key)
		if lerr != nil {
			return SubmitResult{}, lerr
		}
		if live == nil {
			return SubmitResult{}, fmt.Errorf("create job: %w", err)
		}
		if _, uerr := tx.Exec("UPDATE jobs SET followup = 1, updated_at = ? WHERE id = ?", nowStr, live.ID); uerr != nil {
			return SubmitResult{}, fmt.Errorf("set the follow-up flag on job %d: %w", live.ID, uerr)
		}
		if _, err := appendEventTx(tx, evJobPending, p.ProjectID, p.TaskID, live.ID,
			eventPayload{Answer: string(AnsPending)}, now); err != nil {
			return SubmitResult{}, err
		}
		return SubmitResult{Answer: AnsPending, JobID: live.ID}, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return SubmitResult{}, fmt.Errorf("create job: last insert id: %w", err)
	}
	eventID, err := appendEventTx(tx, evJobCreated, p.ProjectID, p.TaskID, id,
		eventPayload{Answer: string(AnsCreated), Run: int64(run), Branch: p.branch()}, now)
	if err != nil {
		return SubmitResult{}, err
	}
	// A worker job's submit is the conversation's first word about the
	// task: the spec the worker will be told, kept as a message (§9).
	if p.TaskID != 0 {
		spec, err := taskSpecTx(tx, p.TaskID)
		if err != nil {
			return SubmitResult{}, err
		}
		if err := appendMessageTx(tx, p.ProjectID, eventID, msgKindTaskSpec, authorEngine, spec, now); err != nil {
			return SubmitResult{}, err
		}
	}
	return SubmitResult{Answer: AnsCreated, JobID: id, Run: run}, nil
}

// taskSpecTx reads the task's spec (§6.1): the verbatim text the worker
// prompt wraps (§1.4) and the task_spec message keeps (§9).
func taskSpecTx(tx *sql.Tx, taskID int64) (string, error) {
	spec, _, err := taskSpecAndBaseTx(tx, taskID)
	return spec, err
}

// taskCheckTx reads the task's check command (§5.5), "" when it has
// none: the worker prompt names it so the worker knows what decides its
// task, and the check-failure feedback and events quote it.
func taskCheckTx(tx *sql.Tx, taskID int64) (string, error) {
	var check string
	if err := tx.QueryRow("SELECT COALESCE(check_cmd, '') FROM tasks WHERE id = ?", taskID).Scan(&check); err != nil {
		return "", fmt.Errorf("read the check of task %d: %w", taskID, err)
	}
	return check, nil
}

// taskSpecAndBaseTx reads the task's spec and base ref together: the
// two verbatim inputs a review prompt wraps (§7) beside the check
// result — the base names the diff's other end, and it is read here so
// the composition stays one query inside the completion's transaction.
func taskSpecAndBaseTx(tx *sql.Tx, taskID int64) (spec, base string, err error) {
	if err := tx.QueryRow("SELECT spec, base_ref FROM tasks WHERE id = ?", taskID).Scan(&spec, &base); err != nil {
		return "", "", fmt.Errorf("read task %d: %w", taskID, err)
	}
	return spec, base, nil
}

// The rounds budgets a task carries (§7, §6.1), named as the keys of the
// task's caps JSON: a fix round after a failed check spends a fix_rounds
// budget, a fix round after a review's changes spends a review_rounds
// budget — both are the task's fix jobs, one count against two caps —
// and a restart spends the restart budget. roundNone is the answer's
// word for "a fix job that is no round" (§3): the job that resumes the
// author's session with the foreman's answer spends nothing, so Submit
// names it explicitly instead of letting the kind imply a fix round.
const (
	roundFix     = "fix_rounds"
	roundReview  = "review_rounds"
	roundRestart = "restart"
	roundNone    = "none"
)

// spentRound names the rounds budget p spends (§7, §6.1): the restart
// budget when the submit is a restart — whatever Round said, a restart
// is its own budget — Round when the engine names one, a fix round for
// a plain fix submit, and nothing for the rest. Rounds belong to tasks,
// so a foreman turn spends none however it is submitted. The answer's
// roundNone is nothing spent even for its fix job.
func spentRound(p SubmitParams) string {
	switch {
	case p.TaskID == 0:
		return ""
	case p.Restart:
		return roundRestart
	case p.Round == roundNone:
		return ""
	case p.Round != "":
		return p.Round
	case p.Kind == KindFix:
		return roundFix
	}
	return ""
}

// roundCapReached counts the rounds the task has already spent against
// the cap this submit would consume (§7, §6.1), and names the budget it
// checked: the spent round, which is also what the created job's row
// carries. A work or review job, a foreman turn and the answer's
// no-round resume spend nothing and are never capped here. A task's
// caps JSON — {"fix_rounds": 3, "review_rounds": 2, "restart": 2} —
// overrides the configured defaults key by key.
func (e *Engine) roundCapReached(tx *sql.Tx, p SubmitParams) (string, bool, error) {
	round := spentRound(p)
	if round == "" {
		return "", false, nil
	}
	limit, err := e.roundLimitTx(tx, p.TaskID, round)
	if err != nil {
		return round, false, err
	}
	// Both fix-round budgets count the same jobs — the task's fix jobs
	// that spent one of them, restarts excluded (§7) — and the restart
	// budget counts restarts.
	var count int
	if round == roundRestart {
		if err := tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1",
			p.TaskID).Scan(&count); err != nil {
			return round, false, fmt.Errorf("count %s rounds of task %d: %w", round, p.TaskID, err)
		}
		return round, count >= limit, nil
	}
	// The count is scoped to the task's latest attempt (§5.6): a
	// restart begins a new attempt from the base, and the new attempt
	// carries its rounds unspent — so the fix jobs before the task's
	// latest restart never count against the rounds after it. Without a
	// restart the latest attempt is the task's whole life, and the count
	// is every fix round it ever spent.
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs
		WHERE task_id = ? AND kind = ? AND round IN (?, ?)
		AND id > COALESCE((SELECT MAX(id) FROM jobs WHERE task_id = ? AND restart = 1), 0)`,
		p.TaskID, KindFix, roundFix, roundReview, p.TaskID).Scan(&count); err != nil {
		return round, false, fmt.Errorf("count %s rounds of task %d: %w", round, p.TaskID, err)
	}
	return round, count >= limit, nil
}

// roundLimitTx reads one rounds budget of the task: the configured
// default, overridden by the task's caps JSON under the budget's own
// key. round is one of the round… constants, which are those keys.
func (e *Engine) roundLimitTx(tx *sql.Tx, taskID int64, round string) (int, error) {
	limit := e.cfg.RoundCaps.Restart
	switch round {
	case roundFix:
		limit = e.cfg.RoundCaps.Fix
	case roundReview:
		limit = e.cfg.RoundCaps.Review
	}
	var capsJSON string
	err := tx.QueryRow("SELECT caps FROM tasks WHERE id = ?", taskID).Scan(&capsJSON)
	switch {
	case err == nil:
		var caps map[string]int
		if json.Unmarshal([]byte(capsJSON), &caps) == nil {
			if v, ok := caps[round]; ok {
				limit = v
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		// the insert's foreign key will say this plainly
	default:
		return 0, fmt.Errorf("read caps of task %d: %w", taskID, err)
	}
	return limit, nil
}
