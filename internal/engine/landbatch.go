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

// Batch formation and reservation (§7.1 "Batch formation", §12.3
// "Readers of live task ownership"). The land rule stays the one creator
// of land jobs: run for a ready landing it gathers every landing ready
// for the project's one target, across the project's plans, and queues
// one job of kind land keyed land-batch:<project> for the batch. The
// one-live-job-per-key rule (§12.1) bounds the formations — a second
// finds the key held and queues nothing — and the members are reserved
// in the submit's own transaction: each member's tasks.land_batch_job
// names the job, and while that job is queued, leased or running the
// reservation holds the member's task:<id> key the way a live job holds
// it. A reservation naming a job that is no longer live is no ownership.
//
// The batch run's own grant and completion — every member squashed in
// batch order, one push (§12.3 "Land batch grants and completion") — are
// not built yet: the batch job's task is its first member, the lead, and
// its lease and completion are the one-task landing's. Every member is
// released when the job ends, and the job's outcome re-runs the rule, so
// the members after the lead form the next batch in the same order.

// landBatchKey is the key of a project's batch land job (§7.1): a project
// has one land target, so one key covers it, even with a single member.
func landBatchKey(project int64) string { return fmt.Sprintf("land-batch:%d", project) }

// isLandBatchKey reports whether a job key is a batch land job's.
func isLandBatchKey(key string) bool { return strings.HasPrefix(key, "land-batch:") }

// reservedLiveBatch is the SQL predicate "the task named by the column is
// reserved by a live batch job" (§12.3): its land_batch_job joined to a
// queued, leased or running job. Readers of live task ownership add it
// beside their live-job-on-task:<id> lookup.
func reservedLiveBatch(col string) string {
	return `EXISTS (SELECT 1 FROM tasks rt JOIN jobs rj ON rj.id = rt.land_batch_job
		WHERE rt.id = ` + col + ` AND rj.status IN ('queued', 'leased', 'running'))`
}

// reservedBatchTx reads the live batch job that reserves the task (§12.3),
// nil when none does: no reservation, or one naming a job that ended.
func reservedBatchTx(tx *sql.Tx, taskID int64) (*jobRow, error) {
	j, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM jobs
		WHERE id = (SELECT land_batch_job FROM tasks WHERE id = ?)
			AND status IN ('queued', 'leased', 'running')`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the land batch of task %d: %w", taskID, err)
	}
	return j, nil
}

// liveTaskOwnerTx is the task-key lookup every reader of live task
// ownership makes (§12.3): the live job on task:<id>, else the live batch
// job that reserves the task, nil when neither holds it.
func liveTaskOwnerTx(tx *sql.Tx, taskID int64) (*jobRow, error) {
	live, err := liveJobTx(tx, fmt.Sprintf("task:%d", taskID))
	if err != nil || live != nil {
		return live, err
	}
	return reservedBatchTx(tx, taskID)
}

// releaseLandBatchTx ends every reservation the batch job holds (§7.1):
// each member is released once per batch, when the job ends.
func releaseLandBatchTx(tx *sql.Tx, job int64) error {
	if _, err := tx.Exec(`UPDATE tasks SET land_batch_job = NULL WHERE land_batch_job = ?`, job); err != nil {
		return fmt.Errorf("release the members of land batch %d: %w", job, err)
	}
	return nil
}

// releaseLandMemberTx ends one member's reservation once its landing
// settled outside the batch's completion (§7.1): a trailer on the branch
// recorded it.
func releaseLandMemberTx(tx *sql.Tx, taskID int64) error {
	if _, err := tx.Exec(`UPDATE tasks SET land_batch_job = NULL WHERE id = ?`, taskID); err != nil {
		return fmt.Errorf("release the land batch reservation of task %d: %w", taskID, err)
	}
	return nil
}

// landBatchMaxTx reads the project's land_batch_max (§6.3), the default
// when unset.
func landBatchMaxTx(tx *sql.Tx, project int64) (int, error) {
	var raw string
	if err := tx.QueryRow("SELECT settings FROM projects WHERE id = ?", project).Scan(&raw); err != nil {
		return 0, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	var s struct {
		Max *int `json:"land_batch_max"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return 0, fmt.Errorf("parse the settings of project %d: %w", project, err)
		}
	}
	if s.Max == nil || *s.Max < 1 {
		return defaultLandBatchMax, nil
	}
	return min(*s.Max, maxLandBatchMax), nil
}

// landBatchCandidate is one landing formation weighs: its task and the
// dependencies its landing waits on.
type landBatchCandidate struct {
	id   int64
	deps []int64
}

// landBatchCandidatesTx lists the project's landings that may join a
// batch now (§7.1), lowest task id first: the task done, of class
// implement, approved by its last review, with no landing record, no
// land-gate hold — held or rejected — no retry waiting on the not-before
// backoff, no live job on its task:<id> key and no reservation in a live
// batch. A candidate whose recorded base is a superseded round of its
// stacked dependency is refused here as the rule refuses it (issue #124):
// it parks with task_base_stale and joins nothing. The land-gate rules are
// not re-judged: each candidate's own run of the rule judged them, or the
// user released the hold they took. trigger, when not 0, is the task whose
// landing the caller already vetted — its rule ran, or a release queued
// it — and it is a candidate whatever the filters read.
func (e *Engine) landBatchCandidatesTx(tx *sql.Tx, project, trigger int64, now time.Time) ([]landBatchCandidate, error) {
	rows, err := tx.Query(`SELECT t.id FROM tasks t JOIN plans p ON p.id = t.plan_id
		WHERE p.project_id = ? AND t.state = ? AND t.class = ?
			AND NOT EXISTS (SELECT 1 FROM landings l WHERE l.task_id = t.id)
			AND NOT EXISTS (SELECT 1 FROM land_holds h WHERE h.task_id = t.id)
			AND (t.land_retry_at IS NULL OR t.land_retry_at <= ?)
			AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.key = 'task:' || t.id
				AND j.status IN ('queued', 'leased', 'running'))
			AND NOT `+reservedLiveBatch("t.id")+`
		ORDER BY t.id`, project, taskDone, classImplement, fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("list the ready landings of project %d: %w", project, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the ready landings of project %d: %w", project, err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("list the ready landings of project %d: %w", project, err)
	}
	if trigger != 0 && !slices.Contains(ids, trigger) {
		ids = append(ids, trigger)
		slices.Sort(ids)
	}
	var out []landBatchCandidate
	for _, id := range ids {
		if id == trigger {
			deps, err := taskDepsTx(tx, id)
			if err != nil {
				return nil, err
			}
			out = append(out, landBatchCandidate{id: id, deps: deps})
			continue
		}
		if ok, err := taskApprovedTx(tx, id); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		if dep, baseRef, depBranch, stale, err := staleBaseTx(tx, id); err != nil {
			return nil, err
		} else if stale {
			if err := e.parkStaleBaseTx(tx, project, id, dep, baseRef, depBranch, "the landing is refused", true, now); err != nil {
				return nil, err
			}
			continue
		}
		deps, err := taskDepsTx(tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, landBatchCandidate{id: id, deps: deps})
	}
	return out, nil
}

// taskDepsTx lists a task's dependencies, lowest id first.
func taskDepsTx(tx *sql.Tx, taskID int64) ([]int64, error) {
	rows, err := tx.Query("SELECT depends_on_task_id FROM task_deps WHERE task_id = ? ORDER BY depends_on_task_id", taskID)
	if err != nil {
		return nil, fmt.Errorf("read the dependencies of task %d: %w", taskID, err)
	}
	defer rows.Close()
	var deps []int64
	for rows.Next() {
		var dep int64
		if err := rows.Scan(&dep); err != nil {
			return nil, fmt.Errorf("read the dependencies of task %d: %w", taskID, err)
		}
		deps = append(deps, dep)
	}
	return deps, rows.Err()
}

// orderLandBatchTx picks the batch's members from the candidates (§7.1):
// a candidate joins when every dependency is landed for it
// (depLandedForTx) or sits earlier in the same batch; members land in
// dependency order, the stack's, and tasks with no order between them go
// by task id, lowest first; at most max members, the landings past the
// bound waiting for the next batch.
func orderLandBatchTx(tx *sql.Tx, cands []landBatchCandidate, max int) ([]int64, error) {
	picked := map[int64]bool{}
	type edge struct{ task, dep int64 }
	landed := map[edge]bool{}
	var members []int64
	for len(members) < max {
		next := int64(0)
		for _, c := range cands {
			if picked[c.id] {
				continue
			}
			ok := true
			for _, d := range c.deps {
				if picked[d] {
					continue
				}
				k := edge{c.id, d}
				v, seen := landed[k]
				if !seen {
					var err error
					if v, err = depLandedForTx(tx, c.id, d); err != nil {
						return nil, err
					}
					landed[k] = v
				}
				if !v {
					ok = false
					break
				}
			}
			if ok {
				next = c.id
				break // the lowest id whose order allows it
			}
		}
		if next == 0 {
			break
		}
		picked[next] = true
		members = append(members, next)
	}
	return members, nil
}

// formLandBatchTx forms the project's next land batch (§7.1): with the
// land-batch key free it gathers the ready landings, orders them, queues
// the one batch job and reserves every member in this same transaction.
// trigger, when not 0, is the task whose rule asked: the answer is
// created with the job when the trigger is a member, pending naming the
// live batch when the key is held or the trigger fell past the bound —
// its landing waits for the next batch — and the zero answer when no
// batch formed. A landing that turns ready while a batch runs never joins
// it: the running batch's outcome re-runs this formation.
func (e *Engine) formLandBatchTx(tx *sql.Tx, project, trigger int64, now time.Time) (SubmitResult, error) {
	if live, err := liveJobTx(tx, landBatchKey(project)); err != nil {
		return SubmitResult{}, err
	} else if live != nil {
		return SubmitResult{Answer: AnsPending, JobID: live.ID}, nil
	}
	max, err := landBatchMaxTx(tx, project)
	if err != nil {
		return SubmitResult{}, err
	}
	cands, err := e.landBatchCandidatesTx(tx, project, trigger, now)
	if err != nil {
		return SubmitResult{}, err
	}
	members, err := orderLandBatchTx(tx, cands, max)
	if err != nil {
		return SubmitResult{}, err
	}
	if len(members) == 0 {
		return SubmitResult{}, nil
	}
	// The landing's squash consumes the target (§7.1): queue the sync
	// ahead of it, so the batch leases only once the target is current.
	if err := e.maybeQueueSyncTx(tx, project, now); err != nil {
		return SubmitResult{}, err
	}
	sr, err := e.submitTx(tx, SubmitParams{
		Key:       landBatchKey(project),
		ProjectID: project,
		TaskID:    members[0], // the lead: the one-task grant lands it
		Kind:      KindLand,
		Mode:      ModeFresh,
		Prompt:    "", // no worker: nothing is composed (§1.4)
		Labels:    []string{"git"},
	}, now)
	if err != nil || sr.Answer != AnsCreated {
		return sr, err
	}
	for _, id := range members {
		if _, err := tx.Exec(`UPDATE tasks SET land_batch_job = ?, updated_at = ? WHERE id = ?`,
			sr.JobID, fmtTime(now), id); err != nil {
			return SubmitResult{}, fmt.Errorf("reserve task %d in land batch %d: %w", id, sr.JobID, err)
		}
	}
	if trigger != 0 && !slices.Contains(members, trigger) {
		return SubmitResult{Answer: AnsPending, JobID: sr.JobID}, nil
	}
	return sr, nil
}

// endLandBatchTx re-runs formation after a batch job ended (§7.1): its
// members are released, and the landings still ready form the next
// batch.
func (e *Engine) endLandBatchTx(tx *sql.Tx, job *jobRow, now time.Time) error {
	if err := releaseLandBatchTx(tx, job.ID); err != nil {
		return err
	}
	if !isLandBatchKey(job.Key) {
		return nil
	}
	if err := activeProjectTx(tx, job.ProjectID); err != nil {
		return nil // an archived project forms nothing
	}
	_, err := e.formLandBatchTx(tx, job.ProjectID, 0, now)
	return err
}

// sweepLandBatchesTx ends the reservations whose batch job ended without
// a completion — cancelled, failed by the queue TTL — and re-forms each
// active project's next batch (§7.1): a job cancelled releases every
// member still reserved, and the land rule re-forms the batch.
func (e *Engine) sweepLandBatchesTx(tx *sql.Tx, now time.Time) error {
	rows, err := tx.Query(`SELECT DISTINCT j.project_id FROM tasks t JOIN jobs j ON j.id = t.land_batch_job
		WHERE j.status NOT IN ('queued', 'leased', 'running')
			AND j.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)
		ORDER BY j.project_id`)
	if err != nil {
		return fmt.Errorf("sweep the ended land batches: %w", err)
	}
	var projects []int64
	for rows.Next() {
		var p int64
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return fmt.Errorf("sweep the ended land batches: %w", err)
		}
		projects = append(projects, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("sweep the ended land batches: %w", err)
	}
	if _, err := tx.Exec(`UPDATE tasks SET land_batch_job = NULL WHERE land_batch_job IS NOT NULL
		AND land_batch_job NOT IN (SELECT id FROM jobs WHERE status IN ('queued', 'leased', 'running'))`); err != nil {
		return fmt.Errorf("release the members of ended land batches: %w", err)
	}
	for _, p := range projects {
		if _, err := e.formLandBatchTx(tx, p, 0, now); err != nil {
			return err
		}
	}
	return nil
}
