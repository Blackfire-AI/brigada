package engine

import (
	"database/sql"
	"fmt"
	"time"
)

// reviewUnservableTx reports whether a queued review job could never be
// taken by any registered agent, even one ready and out of cooling: no
// agent of a family other than the job's ExcludeFamily, other than its
// ExcludeAgent (the retry case), offers the review class — the test
// eligibleAgentsTx applies minus health and cooling. A cooling or broken
// agent of the right family makes the job merely wait, never
// unservable: health is temporary, a missing structural reviewer is not.
func reviewUnservableTx(tx *sql.Tx, j *jobRow) (bool, error) {
	// A review that excludes nothing (an agent-less author) is taken by
	// any runner, agent-less ones included: never structurally stuck.
	if j.Kind != KindReview || (j.ExcludeFamily == "" && j.ExcludeAgent == 0) {
		return false, nil
	}
	return noReviewAgentTx(tx, j.ExcludeFamily, j.ExcludeAgent)
}

// noReviewerWhy is the one sentence for why a review job can never be
// taken, the words the queue-TTL park gives.
func noReviewerWhy(family string) string {
	if family == "" {
		return "no reviewer: no agent offers review"
	}
	return "no reviewer: no agent of a family other than " + family + " offers review"
}

// noReviewerEntry is the needs-you line, and the notification's, for an
// unservable review: what is missing and what the user can do about it.
func noReviewerEntry(family string) string {
	if family == "" {
		return "no reviewer: no agent offers review; offer an agent that does to the runner (--agent harness=…) or review by hand"
	}
	return "no reviewer: every agent offering review is of the author's family (" + family +
		"); offer an agent of another family to the runner (--agent harness=…) or review by hand"
}

// queuedReview is a queued review job of a task waiting for its review.
type queuedReview struct {
	job, project, task, plan int64
	title, family            string
	excludeAgent             int64
	flagged                  bool // its review_unservable event is already logged
}

// queuedReviewsTx lists the queued review jobs of tasks in reviewing, in
// task order; project scopes them, 0 for all. Standalone reviews (no
// task) are not listed: they have no task to name.
func queuedReviewsTx(tx *sql.Tx, project int64) ([]queuedReview, error) {
	query := `SELECT j.id, j.project_id, j.task_id, t.plan_id, t.title, j.exclude_family, COALESCE(j.exclude_agent, 0),
		EXISTS (SELECT 1 FROM events e WHERE e.kind = ? AND e.job_id = j.id)
		FROM jobs j JOIN tasks t ON t.id = j.task_id
		WHERE j.status = 'queued' AND j.kind = 'review' AND t.state = ?`
	args := []any{evReviewUnservable, taskReviewing}
	if project != 0 {
		query += " AND j.project_id = ?"
		args = append(args, project)
	}
	rows, err := tx.Query(query+" ORDER BY j.task_id, j.id", args...)
	if err != nil {
		return nil, fmt.Errorf("list the queued reviews: %w", err)
	}
	defer rows.Close()
	var out []queuedReview
	for rows.Next() {
		var q queuedReview
		if err := rows.Scan(&q.job, &q.project, &q.task, &q.plan, &q.title, &q.family, &q.excludeAgent, &q.flagged); err != nil {
			return nil, fmt.Errorf("list the queued reviews: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// unservableReviewsTx is the queued task reviews no agent could ever
// take (see reviewUnservableTx).
func unservableReviewsTx(tx *sql.Tx, project int64) ([]queuedReview, error) {
	all, err := queuedReviewsTx(tx, project)
	if err != nil {
		return nil, err
	}
	var out []queuedReview
	for _, q := range all {
		bad, err := reviewUnservableTx(tx, &jobRow{Kind: KindReview, ExcludeFamily: q.family, ExcludeAgent: q.excludeAgent})
		if err != nil {
			return nil, err
		}
		if bad {
			out = append(out, q)
		}
	}
	return out, nil
}

// flagUnservableReviewsTx logs one review_unservable event the first
// time a queued review job is found unservable; the delivery loop turns
// it into the user's notification (§10). The event is the dedup mark:
// the sweep runs on every tick and status call, the event once per job.
func flagUnservableReviewsTx(tx *sql.Tx, now time.Time) error {
	bad, err := unservableReviewsTx(tx, 0)
	if err != nil {
		return err
	}
	for _, q := range bad {
		if q.flagged {
			continue
		}
		if _, err := appendEventTx(tx, evReviewUnservable, q.project, q.task, q.job,
			eventPayload{Reason: noReviewerEntry(q.family)}, now); err != nil {
			return err
		}
	}
	return nil
}

// statusNoReviewerTx is the needs-you entries of the unservable task
// reviews (§10): the task waits at reviewing until an agent of another
// family registers.
func statusNoReviewerTx(tx *sql.Tx, project int64) ([]NeedsYouItem, error) {
	bad, err := unservableReviewsTx(tx, project)
	if err != nil {
		return nil, err
	}
	var items []NeedsYouItem
	for _, q := range bad {
		items = append(items, NeedsYouItem{
			Kind: "task", ID: q.task, State: taskReviewing, Title: q.title, PlanID: q.plan,
			Reason: noReviewerEntry(q.family),
		})
	}
	return items, nil
}
