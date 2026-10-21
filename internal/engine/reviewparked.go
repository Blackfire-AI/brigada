package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Review outcomes as the parked task's event words them, one per review.
const (
	reviewLineNoVerdict   = "no verdict"
	reviewLineThinApprove = "thin approve"
)

// reviewParkedText words the review_no_verdict event of a task parked
// because its review retry is spent or impossible: the engine's own
// sentences say the task is parked and waits for the foreman, one line
// per review of the cycle names the agent and what it gave, and the
// options close it. outcome is the unusable review being judged (it is
// not yet stored); why, when set, is the engine-written cause that
// replaces "its review retry is spent", e.g. that no agent can take a
// retry. Only the error text of a failed review is worker-produced, and
// the whole text travels bounded like every event's words.
func reviewParkedText(tx *sql.Tx, job *jobRow, outcome, why string) (string, error) {
	lines, err := reviewCycleLinesTx(tx, job, outcome)
	if err != nil {
		return "", err
	}
	var branch string
	err = tx.QueryRow(`SELECT COALESCE(branch, '') FROM jobs
		WHERE task_id = ? AND kind IN ('work', 'fix') ORDER BY id DESC LIMIT 1`, job.TaskID).Scan(&branch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read the branch of task %d: %w", job.TaskID, err)
	}
	if why == "" {
		why = "its review retry is spent"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "task %d is parked (needs_foreman): %s", job.TaskID, why)
	for _, l := range lines {
		b.WriteString("\n" + l)
	}
	review := "brigada review --branch <branch>"
	if branch != "" {
		review = "brigada review --branch " + branch
	}
	b.WriteString("\noptions: task_restart with a note, or review it by hand (" + review + ") and land it")
	return boundNote(b.String()), nil
}

// reviewCycleLinesTx lists the reviews of the task's current cycle, the
// ones reviewRetrySpentTx counts, oldest first, then the review being
// judged: "<harness>/<model>: no verdict | thin approve | <error>".
func reviewCycleLinesTx(tx *sql.Tx, job *jobRow, outcome string) ([]string, error) {
	rows, err := tx.Query(`SELECT j.result, j.error_kind, COALESCE(a.harness, j.harness), COALESCE(a.model, j.model)
		FROM jobs j LEFT JOIN agents a ON a.id = j.agent_id
		WHERE j.task_id = ? AND j.kind = 'review' AND j.restart = 0 AND j.id <> ?
		AND j.error_kind NOT IN ('limit', 'auth', ?, ?)
		AND j.id > COALESCE((SELECT MAX(id) FROM jobs WHERE task_id = ? AND kind IN ('work', 'fix')), 0)
		ORDER BY j.id`, job.TaskID, job.ID, ErrorKindSetup, ErrorKindInfra, job.TaskID)
	if err != nil {
		return nil, fmt.Errorf("read the reviews of task %d: %w", job.TaskID, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var result, kind, harness, model sql.NullString
		if err := rows.Scan(&result, &kind, &harness, &model); err != nil {
			return nil, fmt.Errorf("read the reviews of task %d: %w", job.TaskID, err)
		}
		// The stored result wraps the completion's: {"result": …} for a
		// run that answered, {"error": …} for one that failed.
		var stored struct {
			Result string `json:"result"`
			Error  string `json:"error"`
		}
		_ = json.Unmarshal([]byte(result.String), &stored)
		what := reviewLineNoVerdict
		var v verdictBlock
		switch {
		case stored.Error != "":
			what = stored.Error
		case kind.String != "":
			what = "error (" + kind.String + ")"
		case json.Unmarshal([]byte(stored.Result), &v) == nil && v.Verdict == verdictApprove:
			what = reviewLineThinApprove
		}
		lines = append(lines, reviewLine(harness.String, model.String, what))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the reviews of task %d: %w", job.TaskID, err)
	}
	var harness, model string
	if job.AgentID != 0 {
		err := tx.QueryRow("SELECT harness, model FROM agents WHERE id = ?", job.AgentID).Scan(&harness, &model)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read agent %d: %w", job.AgentID, err)
		}
	}
	return append(lines, reviewLine(harness, model, outcome)), nil
}

func reviewLine(harness, model, what string) string {
	agent := harness + "/" + model
	if harness == "" && model == "" {
		agent = "unknown agent"
	}
	// An error's words are cut short so a long one cannot push the
	// options line out of the event's bound.
	return agent + ": " + oneLine(cutRune(what, 120), 120)
}

// reviewOutcomeWord turns the reason queueReviewRetryTx judged into the
// word a review line shows.
func reviewOutcomeWord(reason string) string {
	switch reason {
	case "no verdict in the review result":
		return reviewLineNoVerdict
	case "approve without substantive findings":
		return reviewLineThinApprove
	}
	return reason
}
