package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// ClientTools is the client bridge's offer (§12.2, proposal 2026-10-14):
// `brigada mcp --client`, for any harness acting for the user — a coding
// session, a coordinator — so it can talk to the foreman and watch the
// work without a person relaying between the two. It bears the client
// token, never a foreman token: it can send the user's message, read the
// status and read the history, and nothing a foreman does.
var ClientTools = []Tool{
	{
		Name:        "ask_foreman",
		Description: "Send the project's foreman a message as the user (a request: plan this, fix that, what is blocking task 3?). With nobody attached to the foreman, it wakes up and acts on it; otherwise the message waits for the attached chat. The answer says which happened and gives the message's id. Follow the work with status and history. The same text again within a day counts as a retry of the first call; reword it to send it anew.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{
					"type":        "string",
					"description": "the message, as the user would write it to the foreman",
				},
				"repo_url": map[string]any{
					"type":        "string",
					"description": "the project's repo URL; omit when the engine has one project",
				},
			},
			"required": []string{"text"},
		},
		Call: callAskForeman,
	},
	{
		Name:        "status",
		Description: "The engine's state across every project: the running jobs with each worker's latest event, the plans with their tasks and branches, the foreman's latest report, messages still waiting for the foreman, and the \"needs you\" queue.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Call: callStatus,
	},
	{
		Name:        "history",
		Description: "The timeline — events and messages merged, oldest first — optionally one task's, at most `limit` items: what the foreman planned, what each worker did and said, reviews, reports.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "integer",
					"description": "show one task's timeline only",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "how many items to show, 1..500 (default 100)",
				},
			},
		},
		Call: callHistory,
	},
}

// callAskForeman runs ask_foreman (§4.2 item 3): the user's message
// through /v1/foreman/messages, answered with what it did.
func callAskForeman(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a struct {
		Text    string `json:"text"`
		RepoURL string `json:"repo_url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("arguments: %w", err)
	}
	if strings.TrimSpace(a.Text) == "" {
		return "", fmt.Errorf("arguments: text is required")
	}
	// The key is the call's content (§12.1): a retried call sends one
	// message; the same words again within the key's day replay the
	// first answer, so a deliberate repeat is reworded.
	res, err := c.AskForeman(ctx, a.RepoURL, a.Text, idemKey("ask_foreman", a))
	if err != nil {
		return "", err
	}
	return AskOutcomeLine(res), nil
}

// AskOutcomeLine says in one line what a user message did (§4.2 item
// 3): the foreman woke for it, or why it waits — never a silent queue.
// The CLI's ask prints the same line.
func AskOutcomeLine(res engine.AskResult) string {
	return fmt.Sprintf("message #%d %s", res.EventID, askOutcome(res))
}

// askOutcome is the outcome half of AskOutcomeLine.
func askOutcome(res engine.AskResult) string {
	p := res.ProjectID
	switch res.Outcome {
	case engine.WakeWoken:
		return fmt.Sprintf("sent to the foreman of project %d: it is waking up to act on it", p)
	case engine.WakeAttached:
		return fmt.Sprintf("queued for the foreman of project %d: a chat is attached (hold %d), and the foreman reads it on its next events call", p, res.HoldID)
	case engine.WakeDuplicate:
		return fmt.Sprintf("is the same message sent to the foreman of project %d moments ago; it was not queued again", p)
	case engine.WakeNothing:
		return fmt.Sprintf("queued for the foreman of project %d: nothing was pending to wake it for", p)
	case engine.WakeTurnRunning:
		return fmt.Sprintf("queued for the foreman of project %d: a foreman turn is running, and the next turn carries it", p)
	case engine.WakeNoForeman:
		return fmt.Sprintf("queued for project %d, which has no foreman yet: set one with `brigada project foreman`", p)
	case engine.WakeStalled:
		return fmt.Sprintf("queued for the foreman of project %d: its wake stalled after failing turns; see `brigada status`", p)
	case engine.WakeBudgetSpent:
		return fmt.Sprintf("queued for the foreman of project %d: today's wake budget is spent", p)
	case engine.WakeLimited:
		if res.LimitedUntil == "" {
			return fmt.Sprintf("queued for the foreman of project %d: the foreman is limited, reset time unknown; wait, or switch it with `brigada project foreman`", p)
		}
		return fmt.Sprintf("queued for the foreman of project %d: the foreman is limited until %s; wait, or switch it with `brigada project foreman`", p, res.LimitedUntil)
	default:
		return fmt.Sprintf("queued for the foreman of project %d (%s)", p, res.Outcome)
	}
}
