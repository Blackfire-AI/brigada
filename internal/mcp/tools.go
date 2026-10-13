package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
)

// Tool is one of the foreman's MCP tools (§12.2): its name and
// description as tools/list shows them, the JSON schema of its
// arguments, and its call — the engine reached through the API client
// the server bears, the answer rendered as text. A tool renders; it
// never judges (§1).
type Tool struct {
	Name        string
	Description string
	// InputSchema is the tool's JSON Schema object: an object type whose
	// properties are the tool's arguments.
	InputSchema any
	// Call runs the tool. An error is the tool's own answer — isError
	// with the message (§12.2) — never the bridge's death.
	Call func(ctx context.Context, c *api.Client, args json.RawMessage) (string, error)
}

// tools is the bridge's whole offer (§12.2), the one list a later item
// extends: plan_create, events and capacity each add an entry here, and
// ToolNames carries the addition to the foreman's tool scope.
var tools = []Tool{
	{
		Name:        "status",
		Description: "The engine's own state: which process it is, up how long, and the jobs by status.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Call: callStatus,
	},
	{
		Name:        "history",
		Description: "The project's timeline — events and messages merged, oldest first — optionally one task's, at most `limit` items.",
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

// toolInfo is one tool as tools/list shows it (§12.2).
type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// toolsListResult is the tools/list answer: the offer, in order.
type toolsListResult struct {
	Tools []toolInfo `json:"tools"`
}

// toolInfos renders the offer for tools/list.
func toolInfos() []toolInfo {
	out := make([]toolInfo, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// ToolNames lists the bridge's tools by name, in the offer's order. It
// is what a harness's foreman tool scope names (§4) — in Claude Code's
// spelling, mcp__brigada__<name> — so the tool table stays the one
// place a tool is added.
func ToolNames() []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// toolByName finds the tool a tools/call names.
func toolByName(name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// callStatus renders /v1/status as text (§12.2): the engine's process
// and uptime, then the jobs by status in the statuses' fixed order.
func callStatus(ctx context.Context, c *api.Client, _ json.RawMessage) (string, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "engine pid %d", st.Pid)
	if started := parseStartedAt(st.StartedAt); !started.IsZero() {
		fmt.Fprintf(&b, " up %s", time.Since(started).Round(time.Second))
	}
	fmt.Fprintln(&b)
	counts := make([]string, 0, len(engine.JobStatuses))
	for _, s := range engine.JobStatuses {
		counts = append(counts, fmt.Sprintf("%s %d", s, st.Jobs[s]))
	}
	fmt.Fprintf(&b, "jobs: %s", strings.Join(counts, " "))
	return b.String(), nil
}

// parseStartedAt reads the engine's started_at into a time; a zero time
// means "not recorded" and is left out of the rendering.
func parseStartedAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// callHistory renders /v1/history as text (§9, §12.2) with
// internal/history, optionally one task's timeline and a page size. The
// token the client bears scopes the project (§12.1): a foreman's token
// sees its own project whatever it asks.
func callHistory(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a struct {
		Task  *int64 `json:"task"`
		Limit int    `json:"limit"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	q := api.HistoryQuery{Limit: a.Limit}
	if a.Task != nil {
		q.TaskID = *a.Task
	}
	page, err := c.History(ctx, q)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := history.Render(&b, page.Items); err != nil {
		return "", err
	}
	switch {
	case len(page.Items) == 0:
		return "(no history items)", nil
	case page.NextCursor != "":
		fmt.Fprintln(&b)
		fmt.Fprint(&b, "… older items precede this page")
	}
	return b.String(), nil
}
