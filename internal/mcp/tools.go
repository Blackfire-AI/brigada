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
// extends: plan_amend, task_restart, answer, ask_user, events and
// capacity each add an entry here, and ToolNames carries the addition
// to the foreman's tool scope.
var tools = []Tool{
	{
		Name:        "plan_create",
		Description: "Create a plan with exactly one task and queue its first work job (one task per plan until plans with dependencies). The plan is active at once.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "the plan's title",
				},
				"tasks": map[string]any{
					"type":        "array",
					"description": "the plan's tasks; exactly one",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title": map[string]any{
								"type":        "string",
								"description": "the task's title; default is the spec's first line",
							},
							"spec": map[string]any{
								"type":        "string",
								"description": "the task spec, verbatim: what the worker is to do. Never ask the worker to run the check, commit or push: the runner does all three after the worker exits, and a worker may have no shell at all",
							},
							"check": map[string]any{
								"type":        "string",
								"description": "the check command the runner runs after a done, in the task's worktree; empty = none. The worker never runs it",
							},
							"class": map[string]any{
								"type":        "string",
								"description": "the task's class (default implement)",
							},
						},
						"required": []string{"spec"},
					},
				},
			},
			"required": []string{"title", "tasks"},
		},
		Call: callPlanCreate,
	},
	{
		Name:        "answer",
		Description: "Answer a task worker's question (the task must be parked at needs_foreman by its question, as status shows). The answer reaches the worker verbatim on its own session and agent, and spends none of the task's rounds.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "integer",
					"description": "the asking task's id",
				},
				"text": map[string]any{
					"type":        "string",
					"description": "the answer, verbatim: what the worker must know to proceed. A worker asking permission for git, a build or the check needs none of them: tell it to leave its edits in the worktree and report done, since the runner commits, pushes and checks. Never ask the user to widen a worker's permissions",
				},
			},
			"required": []string{"task", "text"},
		},
		Call: callAnswer,
	},
	{
		Name:        "task_restart",
		Description: "Restart a task parked at needs_foreman — failed, out of rounds, or one whose review keeps asking for changes — as a fresh attempt from the task's base on a new branch, with your note handed to the worker verbatim as feedback (what went wrong, what to avoid). Spends one of the task's restarts; the fresh attempt gets its rounds back.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "integer",
					"description": "the parked task's id",
				},
				"note": map[string]any{
					"type":        "string",
					"description": "the note for the fresh attempt, verbatim; empty = none",
				},
			},
			"required": []string{"task"},
		},
		Call: callTaskRestart,
	},
	{
		Name:        "status",
		Description: "The engine's own state: which process it is, up how long, the jobs by status — plus the running jobs with each worker's latest event, the plans with their tasks and the \"needs you\" queue: tasks parked at needs_foreman or needs_user, and plans awaiting approval, each with its reason. Your token scopes the view to your project.",
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
	{
		Name:        "events",
		Description: "Wait for judgment events addressed to the foreman and return them as they arrive, rendered exactly as a wake prompt carries them. Call this between replies while you are attached: events a worker parks during the chat reach you here, and reading them is what consumes them. An empty answer says there is nothing new.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"wait_seconds": map[string]any{
					"type":        "number",
					"description": "how long to wait for events in seconds, 0..25, fractions allowed (default 0: report what is pending now)",
				},
			},
		},
		Call: callEvents,
	},
	{
		Name:        "capacity",
		Description: "The capacity snapshot: per agent its effective cap (and the layer that sets it), running jobs, quota state (ready, cooling until a time, or broken) and cost weight; per layer — each runner, account, capped model, active plan of the project and global — the cap, use and queue depth. Call this when you plan: it tells you how wide the task graph can be, when to sequence instead of fanning out, which tasks can wait for a cheaper agent, and which agents are cooling or broken.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Call: callCapacity,
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

// toolInfos renders an offer for tools/list.
func toolInfos(offer []Tool) []toolInfo {
	out := make([]toolInfo, 0, len(offer))
	for _, t := range offer {
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

// toolByName finds the tool a tools/call names in an offer.
func toolByName(offer []Tool, name string) (Tool, bool) {
	for _, t := range offer {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// callStatus renders /v1/status as text (§12.2): the engine's process
// and uptime, the jobs by status in the statuses' fixed order, then the
// plans with their tasks and the "needs you" queue (§10) — the same
// sections api.StatusSections renders for the client's status command,
// scoped by the token the client bears.
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
	fmt.Fprintln(&b)
	fmt.Fprint(&b, api.StatusSections(st))
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
