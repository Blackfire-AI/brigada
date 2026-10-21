package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// TaskLandInfo is what `brigada land` needs of a task beyond TaskInfo
// (§7.1): the data the commit message is built from, the task's
// dependencies, and whether its last review approved it. The client
// builds the message and runs every git command; the engine runs none.
type TaskLandInfo struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Class string `json:"class"`
	State string `json:"state"`
	Spec  string `json:"spec"`
	// Deps are the ids of the tasks this one depends on (§6.1).
	Deps []int64 `json:"deps,omitempty"`
	// Approved: the task is done and its last review, after its last
	// author job, was a substantive approve (§7).
	Approved bool `json:"approved"`
	// ApprovedHead is the pushed head the approving review saw. The
	// runner does not record it yet (§16 item 10), so it is empty and
	// land warns instead of refusing (§7.1).
	ApprovedHead string `json:"approved_head,omitempty"`
	// Summary is the worker's summary (§5.4) from the task's last author
	// job: model output, data only.
	Summary string `json:"summary,omitempty"`
	// CommitMessage is the commit message (§5.4) the task's last author
	// job's worker wrote for the land job (§7.1): subject and body in the
	// repo's own style, model output, data only. Empty when the worker
	// gave none, and land then builds the message itself.
	CommitMessage string `json:"commit_message,omitempty"`
	// Family and Model are the authoring agent's, from the job record
	// (§7); empty when the job has no agent or the agent is gone.
	Family string `json:"family,omitempty"`
	Model  string `json:"model,omitempty"`
}

// TaskLand reads a task's landing data; nil when there is no such task.
func (e *Engine) TaskLand(ctx context.Context, id int64) (*TaskLandInfo, error) {
	var info *TaskLandInfo
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		t := &TaskLandInfo{ID: id}
		err := tx.QueryRow(`SELECT title, class, state, spec FROM tasks WHERE id = ?`, id).
			Scan(&t.Title, &t.Class, &t.State, &t.Spec)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT depends_on_task_id FROM task_deps WHERE task_id = ? ORDER BY depends_on_task_id`, id)
		if err != nil {
			return fmt.Errorf("read the dependencies of task %d: %w", id, err)
		}
		defer rows.Close()
		for rows.Next() {
			var d int64
			if err := rows.Scan(&d); err != nil {
				return err
			}
			t.Deps = append(t.Deps, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		var authorID, agent int64
		var authorResult string
		err = tx.QueryRow(`SELECT id, COALESCE(agent_id, 0), COALESCE(result, '') FROM jobs
			WHERE task_id = ? AND kind IN ('work', 'fix') AND status = 'done' ORDER BY id DESC LIMIT 1`, id).
			Scan(&authorID, &agent, &authorResult)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read the last author job of task %d: %w", id, err)
		default:
			wr, ok := parseWorkerResult(jobResultText(authorResult))
			if ok {
				t.Summary = wr.Summary
				t.CommitMessage = wr.CommitMessage
			}
			if agent != 0 {
				err := tx.QueryRow(`SELECT family, model FROM agents WHERE id = ?`, agent).Scan(&t.Family, &t.Model)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("read agent %d: %w", agent, err)
				}
			}
		}
		var reviewResult string
		err = tx.QueryRow(`SELECT COALESCE(result, '') FROM jobs
			WHERE task_id = ? AND kind = 'review' AND status = 'done' AND id > ? ORDER BY id DESC LIMIT 1`,
			id, authorID).Scan(&reviewResult)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read the last review of task %d: %w", id, err)
		default:
			var v verdictBlock
			if json.Unmarshal([]byte(jobResultText(reviewResult)), &v) == nil &&
				v.Verdict == verdictApprove && len(v.Findings) >= minSubstantiveFindings {
				t.Approved = t.State == "done"
			}
		}
		info = t
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the landing data of task %d: %w", id, err)
	}
	return info, nil
}

// jobResultText unwraps the jobs.result column ({"result": "<block>"})
// to the block the runner delivered; "" when it is not that shape.
func jobResultText(stored string) string {
	var w struct {
		Result string `json:"result"`
	}
	if json.Unmarshal([]byte(stored), &w) != nil {
		return ""
	}
	return w.Result
}
