package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Batch progress (§7.1 "A lost runner resumes by remote evidence"). The
// runner reports each member's parking and skip outcome as it is reached,
// and — before the push — each surviving member's input, local check
// result and publication intent. The engine keeps them per batch job and
// member (land_batch_progress), so a lost runner's next lease does not
// squash a parked member again or forget a skipped dependent, and a
// terminal lost-runner failure parks only the members that have no
// outcome of their own: a member parked keeps its own failure, a skipped
// dependent waits, and a member whose push may have published is
// requeued for the next batch to reconcile from the remote's trailers
// rather than parked over a landing that may exist. A local commit with an
// intent is never a landing record.

// ErrLandProgressRefused marks a progress report the engine refuses as
// the caller's mistake: a member the batch does not own, an unreadable
// entry, an unknown outcome or intent.
var ErrLandProgressRefused = errors.New("land progress refused")

// landProgressEntry is one member's progress as the runner reports it:
// a settled outcome — parked or dropped, with its member result — or the
// pre-push intent.
type landProgressEntry struct {
	landMemberResult
	ApprovedHead string `json:"approved_head,omitempty"`
	BaseRef      string `json:"base_ref,omitempty"`
	Commit       string `json:"commit,omitempty"`
	CheckPassed  bool   `json:"check_passed,omitempty"`
	Intent       string `json:"intent,omitempty"` // 'target' or 'waiting'
	Position     int    `json:"position,omitempty"`
}

// landProgress is one member's stored progress.
type landProgress struct {
	outcome string           // 'parked', 'dropped' or '' while unsettled
	result  landMemberResult // the settled outcome as reported
	intent  string           // 'target', 'waiting' or '' before the push
	// approvedHead is the head the member's pre-push attempt squashed —
	// its recorded input, which its published trailers name.
	approvedHead string
}

// LandProgress records a batch land job's per-member progress (§7.1),
// each entry a member's settled outcome or its pre-push intent. The
// lease is checked as a heartbeat checks it: a job that is gone, ended,
// cancelled or leased to another answers so and records nothing. An
// entry naming a task the batch does not own, or an unknown outcome or
// intent, refuses the whole report, naming it. A settled outcome stands
// once written, so a repeated report changes nothing.
func (e *Engine) LandProgress(ctx context.Context, owner, jobID int64, leaseID string, entries []json.RawMessage) (Answer, error) {
	var ans Answer
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		var err error
		ans, err = e.landProgressTx(tx, owner, jobID, leaseID, entries, e.cfg.Now())
		return err
	})
	if err != nil {
		return "", fmt.Errorf("record the progress of land batch %d: %w", jobID, err)
	}
	return ans, nil
}

func (e *Engine) landProgressTx(tx *sql.Tx, owner, jobID int64, leaseID string, entries []json.RawMessage, now time.Time) (Answer, error) {
	job, err := loadJobTx(tx, jobID)
	if err != nil {
		return "", err
	}
	switch {
	case job == nil:
		return AnsGone, nil
	case job.Status == statusCancelled:
		return AnsCancelled, nil
	case isTerminal(job.Status):
		return AnsGone, nil
	case job.LeaseID == "" || job.LeaseID != leaseID || job.LeaseOwner != owner:
		return AnsLost, nil
	case !isLandBatchKey(job.Key):
		return "", fmt.Errorf("%w: job %d is no land batch", ErrLandProgressRefused, jobID)
	}
	members, _, err := landBatchMembersTx(tx, job.ID)
	if err != nil {
		return "", err
	}
	owned := map[int64]bool{}
	for _, id := range members {
		owned[id] = true
	}
	for _, raw := range entries {
		var p landProgressEntry
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", fmt.Errorf("%w: an unreadable progress entry: %v", ErrLandProgressRefused, err)
		}
		if !owned[p.TaskID] {
			return "", fmt.Errorf("%w: land batch %d reports progress for task %d, which it does not own", ErrLandProgressRefused, job.ID, p.TaskID)
		}
		switch {
		case p.Outcome == landMemberParked || p.Outcome == landMemberDropped:
			b, err := json.Marshal(p.landMemberResult)
			if err != nil {
				return "", fmt.Errorf("encode the progress of task %d: %w", p.TaskID, err)
			}
			if _, err := tx.Exec(`INSERT INTO land_batch_progress (job_id, task_id, outcome, result, updated_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (job_id, task_id) DO UPDATE SET outcome = excluded.outcome, result = excluded.result,
					updated_at = excluded.updated_at
				WHERE land_batch_progress.outcome = ''`, job.ID, p.TaskID, p.Outcome, string(b), fmtTime(now)); err != nil {
				return "", fmt.Errorf("record the outcome of task %d in land batch %d: %w", p.TaskID, job.ID, err)
			}
		case p.Outcome == "" && (p.Intent == "target" || p.Intent == "waiting"):
			if _, err := tx.Exec(`INSERT INTO land_batch_progress (job_id, task_id, approved_head, base_ref, commit_sha,
					check_passed, intent, position, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (job_id, task_id) DO UPDATE SET approved_head = excluded.approved_head,
					base_ref = excluded.base_ref, commit_sha = excluded.commit_sha, check_passed = excluded.check_passed,
					intent = excluded.intent, position = excluded.position, updated_at = excluded.updated_at
				WHERE land_batch_progress.outcome = ''`,
				job.ID, p.TaskID, p.ApprovedHead, p.BaseRef, p.Commit, boolInt(p.CheckPassed), p.Intent, p.Position,
				fmtTime(now)); err != nil {
				return "", fmt.Errorf("record the publication intent of task %d in land batch %d: %w", p.TaskID, job.ID, err)
			}
		default:
			return "", fmt.Errorf("%w: land batch %d reports task %d with outcome %q and intent %q: want parked, dropped or a publication intent",
				ErrLandProgressRefused, job.ID, p.TaskID, p.Outcome, p.Intent)
		}
	}
	return AnsOK, nil
}

// landBatchProgressTx reads the batch job's stored progress by member.
func landBatchProgressTx(tx *sql.Tx, job int64) (map[int64]landProgress, error) {
	rows, err := tx.Query("SELECT task_id, outcome, result, intent, approved_head FROM land_batch_progress WHERE job_id = ?", job)
	if err != nil {
		return nil, fmt.Errorf("read the progress of land batch %d: %w", job, err)
	}
	defer rows.Close()
	out := map[int64]landProgress{}
	for rows.Next() {
		var id int64
		var p landProgress
		var raw string
		if err := rows.Scan(&id, &p.outcome, &raw, &p.intent, &p.approvedHead); err != nil {
			return nil, fmt.Errorf("read the progress of land batch %d: %w", job, err)
		}
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &p.result)
		}
		p.result.TaskID = id
		out[id] = p
	}
	return out, rows.Err()
}

// settleUnreportedTx settles a member a terminal job error leaves without
// an outcome of its own in the completion (§7.1): its stored parking
// stands as its own failure, a stored skip leaves it waiting, and a stored
// publication intent requeues it — released for the next batch, whose
// runner reconciles it from the remote, since its push may have published
// — so only a member with no progress takes the error. requeued reports
// that the member keeps its released gate for that next batch.
func (e *Engine) settleUnreportedTx(tx *sql.Tx, mj *jobRow, prog map[int64]landProgress, c Completion, now time.Time) (requeued bool, err error) {
	p, ok := prog[mj.TaskID]
	switch {
	case ok && p.outcome == landMemberParked:
		return false, e.landMemberOutcomeTx(tx, mj, p.result, nil, now)
	case ok && p.outcome == landMemberDropped:
		return true, nil // waits, done, for its dependency's landing (§6.3)
	case ok && p.intent != "":
		return true, nil
	}
	return false, e.landErrorTx(tx, mj, c, now)
}
