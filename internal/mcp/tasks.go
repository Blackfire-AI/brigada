package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/api"
)

func callStats(ctx context.Context, c *api.Client, _ json.RawMessage) (string, error) {
	stats, err := c.Stats(ctx, 0)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(stats)
	return string(data), err
}

// callTaskCancel uses the same atomic graph amendment as plan_amend.
func callTaskCancel(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a struct {
		Task int64 `json:"task"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("arguments: %w", err)
	}
	status, err := c.Status(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range status.Plans {
		for _, t := range p.Tasks {
			if t.ID == a.Task {
				res, err := c.AmendPlan(ctx, p.ID, api.PlanAmend{Cancel: []string{fmt.Sprint(a.Task)}}, idemKey("task_cancel", a))
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("task %d cancelled: %s", a.Task, res.State), nil
			}
		}
	}
	return "", fmt.Errorf("task %d is not in this project's current plans", a.Task)
}

// callAskUser records the foreman's question for the user: an attached
// chat asks it there too, a headless turn reports it.
func callAskUser(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("arguments: %w", err)
	}
	id, err := c.AskUser(ctx, a.Text, idemKey("ask_user", a))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("question %d recorded: %s", id, a.Text), nil
}

// answerArgs is the answer tool's arguments (§3, §12.2): the asking
// task and the answer's text.
type answerArgs struct {
	Task int64  `json:"task"`
	Text string `json:"text"`
}

// taskRestartArgs is the task_restart tool's arguments (§3, §5.6,
// §12.2): the parked task and the note for its fresh attempt, optional.
type taskRestartArgs struct {
	Task int64  `json:"task"`
	Note string `json:"note"`
}

// callAnswer runs the answer tool (§3, §12.2): the foreman's answer to
// a worker's question through /v1/tasks/<id>/answer, rendered as text
// naming the question the answer filled and the job that carries it.
// The bridge mints the idempotency key per logical call (§12.1) — the
// model never supplies it — so a repeated call replays the first answer
// instead of answering twice.
func callAnswer(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a answerArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	res, err := c.Answer(ctx, a.Task, a.Text, idemKey("answer", a))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("task %d answered: question %d, fix job %d %s",
		a.Task, res.QuestionID, res.JobID, res.Answer), nil
}

// callTaskRestart runs the task_restart tool (§3, §5.6, §12.2): a fresh
// work attempt for a parked task through /v1/tasks/<id>/restart, the
// note verbatim when there is one, rendered as text naming the new
// work job. The idempotency key is minted as in answer.
func callTaskRestart(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a taskRestartArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	res, err := c.RestartTask(ctx, a.Task, a.Note, idemKey("task_restart", a))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("task %d restarted: work job %d %s", a.Task, res.JobID, res.Answer), nil
}
