package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// InteractiveTools extends the existing project foreman offer. Lifecycle calls
// are intercepted by Interactive, never run with the bootstrap client.
func InteractiveTools() []Tool {
	offer := append([]Tool(nil), tools...)
	for _, t := range []struct{ name, description string }{
		{"hold_acquire", "Acquire the project's foreman hold and read the current state digest before deciding. Required explicitly after hold_lost or stale_hold."},
		{"hold_yield", "Release this bridge's hold after recording decisions. The next turn must catch up from the engine."},
	} {
		offer = append(offer, Tool{Name: t.name, Description: t.description, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}})
	}
	offer = append(offer, Tool{Name: "task_cancel", Description: "Cancel a task in this project's current plan, including its live jobs.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"task": map[string]any{"type": "integer"}}, "required": []string{"task"}}, Call: callTaskCancel})
	offer = append(offer, Tool{Name: "ask_user", Description: "Record a question for the user in the project journal. Present the question in chat and wait for the user's answer.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}, Call: callAskUser})
	offer = append(offer, Tool{Name: "stats", Description: "The project's agent outcome statistics.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Call: callStats})
	return offer
}

func callStats(ctx context.Context, c *api.Client, _ json.RawMessage) (string, error) {
	stats, err := c.Stats(ctx, 0)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(stats)
	return string(data), err
}

// callTaskCancel uses the same fenced, atomic graph amendment as plan_amend.
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
	return fmt.Sprintf("question %d recorded; ask the user and wait: %s", id, a.Text), nil
}
