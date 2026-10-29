package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// reviewUnservableTx reports whether a queued review job could never be
// taken by any registered agent, even one ready and out of cooling.
// Under review.family=require (issue #126) that is today's rule: no
// agent other than the author passes ExcludeFamily and offers the
// review class — and, on a retry whose exclusion lifts nowhere (the
// excluded agent gone or of the author's family), ExcludeAgent too.
// Under prefer a review is unservable only when no review-class agent
// at all could take it: the family ladder always has a step for a
// known family, and an unknown one keeps any agent but the author. A
// cooling or broken agent of the winning step makes the job merely
// wait, never unservable: health is temporary, a missing structural
// reviewer is not. The test qualifyingAgentRowsTx applies.
func reviewUnservableTx(tx *sql.Tx, reviewFamily string, j *jobRow) (bool, error) {
	// A review that excludes nothing (an agent-less author) is taken by
	// any runner, agent-less ones included: never structurally stuck.
	if j.Kind != KindReview {
		return false, nil
	}
	author, err := reviewAuthorTx(tx, j)
	if err != nil {
		return false, err
	}
	if j.ExcludeFamily == "" && j.ExcludeAgent == 0 && author == 0 {
		return false, nil
	}
	return noReviewAgentTx(tx, reviewFamily, j)
}

// reviewAuthorTx identifies whose work a review reads, independently of
// the family and retry exclusions. Task reviews follow the latest work or
// fix job; standalone reviews follow the branch's last work job. No job
// (or an agent-less author) leaves the author exclusion open.
func reviewAuthorTx(tx *sql.Tx, j *jobRow) (int64, error) {
	if j.Kind != KindReview {
		return 0, nil
	}
	var row *sql.Row
	switch {
	case j.TaskID != 0:
		row = tx.QueryRow(`SELECT COALESCE(agent_id, 0) FROM jobs
			WHERE task_id = ? AND kind IN ('work', 'fix') ORDER BY id DESC LIMIT 1`, j.TaskID)
	case j.Branch != "":
		row = tx.QueryRow(`SELECT COALESCE(agent_id, 0) FROM jobs
			WHERE project_id = ? AND branch = ? AND kind = 'work' ORDER BY id DESC LIMIT 1`, j.ProjectID, j.Branch)
	default:
		return 0, nil
	}
	var author int64
	if err := row.Scan(&author); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read the author of review job %d: %w", j.ID, err)
	}
	return author, nil
}

// noReviewAgentTx applies lease eligibility without temporary health and
// cooling limits. In particular, the author remains excluded on retries
// whose family is unknown, alongside the family rule require keeps. A
// retry's agent exclusion lifts when no other reviewer exists
// structurally (decided 2026-10-20): the excluded reviewer then takes
// the retry, so it alone is never a missing reviewer.
func noReviewAgentTx(tx *sql.Tx, reviewFamily string, j *jobRow) (bool, error) {
	agents, err := qualifyingAgentRowsTx(tx, reviewFamily, 0, j, string(KindReview), "")
	return len(agents) == 0, err
}

const noReviewerAuthor = "no reviewer: the only agent offering review is the author; offer another agent to the runner (--agent harness=…) or review by hand"

// onlyAuthorReviewerTx distinguishes a missing second agent from a missing
// family. Ignore the family rule for this explanation, but keep the harness
// and retry exclusions: another agent offering review may still need a
// different family, while the author can never review its own work.
func onlyAuthorReviewerTx(tx *sql.Tx, j *jobRow) (bool, error) {
	author, err := reviewAuthorTx(tx, j)
	if err != nil || author == 0 {
		return false, err
	}
	rows, err := tx.Query("SELECT id, family, classes FROM agents WHERE (? = '' OR harness = ?)", j.Harness, j.Harness)
	if err != nil {
		return false, fmt.Errorf("list the reviewers: %w", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id int64
		var family, classes string
		if err := rows.Scan(&id, &family, &classes); err != nil {
			return false, fmt.Errorf("list the reviewers: %w", err)
		}
		if !reviewAgentQualifies(id, family, classes, "", j.ExcludeAgent, 0) {
			continue
		}
		if id != author {
			return false, nil
		}
		found = true
	}
	return found, rows.Err()
}

func noReviewerReasonTx(tx *sql.Tx, reviewFamily string, j *jobRow, entry bool) (string, error) {
	onlyAuthor, err := onlyAuthorReviewerTx(tx, j)
	if err != nil {
		return "", err
	}
	if onlyAuthor {
		return noReviewerAuthor, nil
	}
	family := j.ExcludeFamily
	if familyPrefer(reviewFamily) {
		// The family ladder never leaves a review unservable (issue
		// #126): whatever is missing, the family rule is not the cause.
		family = ""
	}
	if entry {
		return noReviewerEntry(family), nil
	}
	return noReviewerWhy(family, reviewFamily), nil
}

// noReviewerWhy is the one sentence for why a review job can never be
// taken, the words the queue-TTL park gives. Where the family rule is
// the cause it is named with its setting (issue #126).
func noReviewerWhy(family, reviewFamily string) string {
	if family == "" {
		return "no reviewer: no agent offers review"
	}
	why := "no reviewer: no agent of a family other than " + family + " offers review"
	if familyPrefer(reviewFamily) {
		return why
	}
	return why + ", review.family=require"
}

// noReviewerEntry is the needs-you line, and the notification's, for an
// unservable review: what is missing and what the user can do about it.
func noReviewerEntry(family string) string {
	if family == "" {
		return "no reviewer: no agent offers review; offer an agent that does to the runner (--agent harness=…) or review by hand"
	}
	return "no reviewer: needs a reviewer of another family than the author (" + family +
		"); offer an agent of another family to the runner (--agent harness=…) or review by hand"
}

// queuedReview is a queued review job of a task waiting for its review.
type queuedReview struct {
	job, project, task, plan int64
	title                    string
	reason                   string
	flagged                  bool // its review_unservable event is already logged
}

// queuedReviewsTx lists the queued review jobs of tasks in reviewing, in
// task order; project scopes them, 0 for all. Standalone reviews (no
// task) are not listed: they have no task to name.
func queuedReviewsTx(tx *sql.Tx, project int64) ([]queuedReview, error) {
	query := `SELECT j.id, j.project_id, j.task_id, t.plan_id, t.title,
		EXISTS (SELECT 1 FROM events e WHERE e.kind = ? AND e.job_id = j.id)
		FROM jobs j JOIN tasks t ON t.id = j.task_id
		WHERE j.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL) AND j.status = 'queued' AND j.kind = 'review' AND t.state = ?`
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
		if err := rows.Scan(&q.job, &q.project, &q.task, &q.plan, &q.title, &q.flagged); err != nil {
			return nil, fmt.Errorf("list the queued reviews: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// unservableReviewsTx is the queued task reviews no agent could ever
// take (see reviewUnservableTx).
func unservableReviewsTx(tx *sql.Tx, reviewFamily string, project int64) ([]queuedReview, error) {
	all, err := queuedReviewsTx(tx, project)
	if err != nil {
		return nil, err
	}
	var out []queuedReview
	for _, q := range all {
		j, err := loadJobTx(tx, q.job)
		if err != nil {
			return nil, err
		}
		bad, err := reviewUnservableTx(tx, reviewFamily, j)
		if err != nil {
			return nil, err
		}
		if bad {
			q.reason, err = noReviewerReasonTx(tx, reviewFamily, j, true)
			if err != nil {
				return nil, err
			}
			out = append(out, q)
		}
	}
	return out, nil
}

// flagUnservableReviewsTx logs one review_unservable event the first
// time a queued review job is found unservable; the delivery loop turns
// it into the user's notification (§10). The event is the dedup mark:
// the sweep runs on every tick and status call, the event once per job.
func flagUnservableReviewsTx(tx *sql.Tx, reviewFamily string, now time.Time) error {
	bad, err := unservableReviewsTx(tx, reviewFamily, 0)
	if err != nil {
		return err
	}
	for _, q := range bad {
		if q.flagged {
			continue
		}
		if _, err := appendEventTx(tx, evReviewUnservable, q.project, q.task, q.job,
			eventPayload{Reason: q.reason}, now); err != nil {
			return err
		}
	}
	return nil
}

// statusNoReviewerTx is the needs-you entries of the unservable task
// reviews (§10): the task waits at reviewing until an eligible reviewer
// registers.
func statusNoReviewerTx(tx *sql.Tx, reviewFamily string, project int64) ([]NeedsYouItem, error) {
	bad, err := unservableReviewsTx(tx, reviewFamily, project)
	if err != nil {
		return nil, err
	}
	var items []NeedsYouItem
	for _, q := range bad {
		items = append(items, NeedsYouItem{
			Kind: "task", ID: q.task, State: taskReviewing, Title: q.title, PlanID: q.plan,
			Reason: q.reason,
		})
	}
	return items, nil
}
