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
	"github.com/Blackfire-AI/brigada/internal/prompt"
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

// extraReposSchema is the extra_repos property of a plan task (§5.8).
var extraReposSchema = map[string]any{
	"type":        "array",
	"description": "other repositories the task reads (never changes), checked out read-only beside its worktree; the check, setup and worker find each at $BRIGADA_REPO_<NAME> (name uppercased, - as _), so write the check as `go test ./... -schema \"$BRIGADA_REPO_SCHEMA\"`. Omit it to inherit the project's default list; a list, even [], replaces the default. A repository outside the project default sends the plan to the user for approval. A branch or tag ref moves: only a full commit SHA is reproducible",
	"items": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "[a-z][a-z0-9_-]*, unique in the list"},
			"repo": map[string]any{"type": "string", "description": "the repository's git URL (ssh or https; a token in it is dropped)"},
			"ref":  map[string]any{"type": "string", "description": "branch, tag or commit; default: the repository's default branch"},
		},
		"required": []string{"name", "repo"},
	},
}

// taskCapsProperties are the keys of a task's caps object (§6.1): each
// overrides one engine round budget, 0 to 10.
var taskCapsProperties = map[string]any{
	"fix_rounds":    map[string]any{"type": "integer", "minimum": 0, "maximum": 10, "description": "fix rounds a failed check may ask for"},
	"review_rounds": map[string]any{"type": "integer", "minimum": 0, "maximum": 10, "description": "fix rounds a review's changes verdict may ask for"},
	"restart":       map[string]any{"type": "integer", "minimum": 0, "maximum": 10, "description": "restarts (task_restart or automatic) the task may spend"},
}

// reviewTierSchema and reviewEffortSchema are the review_tier and
// review_effort properties of a plan task (§7, §8).
var (
	reviewTierSchema = map[string]any{
		"type":        "string",
		"enum":        []string{"light", "standard", "heavy"},
		"description": "the tier of the model that reviews this task; default: one tier below the task's current tier (light stays light), as the engine's review.tier says. Set it for a task whose diff is hard to judge",
	}
	reviewEffortSchema = map[string]any{
		"type":        "string",
		"enum":        []string{"low", "medium", "high", "max"},
		"description": "how hard the reviewer should think; default: medium, as the engine's review.effort says, whatever the task's own effort",
	}
)

// taskCapsSchema is the caps property of a plan task.
var taskCapsSchema = map[string]any{
	"type":                 "object",
	"properties":           taskCapsProperties,
	"additionalProperties": false,
	"description":          "per-task round caps, any of fix_rounds, review_rounds and restart, integers 0 to 10; omitted keys keep the engine default. An unknown key or out-of-range value refuses the whole call. Raise one for a task known to need many rounds; 0 parks the task at the first such round",
}

// tools is the bridge's whole offer (§12.2), the same for a headless turn
// and an attached chat: a new tool adds an entry here, and ToolNames
// carries the addition to the foreman's tool scope.
var tools = []Tool{
	{
		Name:        "plan_create",
		Description: "Create a plan of one or more tasks and queue the first work job of every task with no dependencies. A task may wait on others of the same plan by name (deps); it starts when all of them are done, and stays waiting if one fails or needs you. A task with exactly one dep builds on that dep's branch (a task with several picks one with base, else its continued dep when continue is set, else starts from the default branch). A dependency cycle, a base that is not a dep, or a base naming another dep than continue refuses the plan. A task's optional caps object sets its own fix_rounds, review_rounds and restart budgets (integers 0 to 10; absent keys keep the engine default); an unknown key or out-of-range value refuses the plan. The plan is active at once, unless it is over the project's approval limit (many tasks, an unusual class, a protected path): then it waits for the user's approval, nothing is queued, and the answer says why — tell the user; only they can approve it.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "the plan's title",
				},
				"tasks": map[string]any{
					"type":        "array",
					"description": "the plan's tasks",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title": map[string]any{
								"type":        "string",
								"description": "the task's title; default is the spec's first line",
							},
							"name": map[string]any{
								"type":        "string",
								"description": "a short name, unique in the plan, so other tasks can list this one in deps; optional",
							},
							"deps": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "names of tasks of this same plan that must be done before this one starts; default none",
							},
							"base": map[string]any{
								"type":        "string",
								"description": "the name of one of this task's deps, whose result branch this task's work starts from, so it builds on that work and its review shows only its own change. Default: the only dep when there is one, else the continued dep when continue is set, else the default branch; a base naming another dep than continue is refused. Merge a task after its base",
							},
							"continue": map[string]any{
								"type":        "string",
								"description": "the name of one of this task's deps whose worker starts this task: when that dep is done, this task's first work job resumes the dep author's session on its own agent, so it starts warm with the files it already read. The continued dep is the task's base, so base defaults to it and a base naming another dep is refused. When the session or its agent is not available (session unknown, agent broken, capped at 0, cooling for over an hour, or outside this task's tier), the task starts fresh as usual; a restart always starts fresh",
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
							"tier":          map[string]any{"type": "string", "enum": []string{"light", "standard", "heavy"}, "description": "how heavy a model the task needs: light for small, well-specified edits; standard (default) for ordinary work; heavy for design-heavy or cross-cutting work. brigada samples among the agents of that tier by their record on this repo and cost weight, so a cheaper or better-recorded agent is picked more often; a task that fails is bumped one tier up automatically"},
							"effort":        map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "max"}, "description": "how hard that model should think; default: the agent's own level, else medium"},
							"review_tier":   reviewTierSchema,
							"review_effort": reviewEffortSchema,
							"caps":          taskCapsSchema,
							"extra_repos":   extraReposSchema,
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
		Name:        "plan_amend",
		Description: "Amend a live plan (active or awaiting approval): add tasks, cancel tasks, and re-point the dependencies of a waiting task. Use it to re-plan when a task fails for good or the user's ask changes: a dependency that will never be done (a task failed or cancelled) leaves its dependents waiting forever unless you re-point them (retarget) to another task or drop the dependency (empty deps). Tasks are named by their name or id (as status shows). Validation is plan_create's: names unique, deps in the plan and never on a cancelled task, no cycle — one refusal rejects the whole amendment. Use retier to change the tier, effort or round caps (merged key by key) of a pending, ready or needs_foreman task; a parked task still needs task_restart. Cancelling stops a task's live jobs. Waiting tasks whose dependencies are all done start in the same call. An added task may push the plan over the project's approval limit: it then waits for the user's approval and nothing new starts. A plan left with only permanently waiting tasks is closed, and its done report names what never ran.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"plan": map[string]any{
					"type":        "integer",
					"description": "the plan's id",
				},
				"add": map[string]any{
					"type":        "array",
					"description": "tasks to add, as plan_create takes them",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title": map[string]any{"type": "string", "description": "the task's title; default is the spec's first line"},
							"name":  map[string]any{"type": "string", "description": "a short name, unique in the plan; optional"},
							"deps": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": []string{"string", "integer"}},
								"description": "tasks of this plan (new or old, by name or id) that must be done before this one starts; default none",
							},
							"base":          map[string]any{"type": []string{"string", "integer"}, "description": "one of its deps whose result branch the work starts from; default as plan_create's, and the continued dep when continue is set"},
							"continue":      map[string]any{"type": []string{"string", "integer"}, "description": "one of its deps whose worker starts this task in its own session; default as plan_create's; the continued dep is the task's base"},
							"spec":          map[string]any{"type": "string", "description": "the task spec, verbatim: what the worker is to do. Never ask the worker to run the check, commit or push"},
							"check":         map[string]any{"type": "string", "description": "the check command the runner runs after a done; empty = none"},
							"class":         map[string]any{"type": "string", "description": "the task's class (default implement)"},
							"tier":          map[string]any{"type": "string", "enum": []string{"light", "standard", "heavy"}, "description": "how heavy a model the task needs: light for small, well-specified edits; standard (default) for ordinary work; heavy for design-heavy or cross-cutting work. brigada samples among the agents of that tier by their record on this repo and cost weight, so a cheaper or better-recorded agent is picked more often; a task that fails is bumped one tier up automatically"},
							"effort":        map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "max"}, "description": "how hard that model should think; default: the agent's own level, else medium"},
							"review_tier":   reviewTierSchema,
							"review_effort": reviewEffortSchema,
							"caps":          taskCapsSchema,
							"extra_repos":   extraReposSchema,
						},
						"required": []string{"spec"},
					},
				},
				"cancel": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": []string{"string", "integer"}},
					"description": "tasks to cancel, by name or id; a done task cannot be cancelled",
				},
				"retier": map[string]any{
					"type":        "array",
					"description": "waiting or parked tasks whose tier, effort, review tier, review effort or round caps you change: lower a tier brigada bumped, raise one before a retry, or allow more restarts",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"task":          map[string]any{"type": []string{"string", "integer"}, "description": "the task, by name or id"},
							"tier":          map[string]any{"type": "string", "enum": []string{"light", "standard", "heavy"}, "description": "new tier; omitted keeps the current tier"},
							"effort":        map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "max"}, "description": "new effort; omitted keeps the current effort"},
							"review_tier":   map[string]any{"type": "string", "enum": []string{"light", "standard", "heavy"}, "description": "new review tier; omitted keeps the current one"},
							"review_effort": map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "max"}, "description": "new review effort; omitted keeps the current one"},
							"caps":          map[string]any{"type": "object", "properties": taskCapsProperties, "additionalProperties": false, "description": "round caps to change, merged key by key into the task's own: give a parked task more restarts before task_restart; omitted keys keep their value"},
						},
						"required": []string{"task"},
					},
				},
				"retarget": map[string]any{
					"type":        "array",
					"description": "waiting (pending) tasks whose dependencies are replaced",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"task": map[string]any{"type": []string{"string", "integer"}, "description": "the waiting task, by name or id"},
							"deps": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": []string{"string", "integer"}},
								"description": "its new complete dependency list; empty drops every dependency",
							},
							"base":     map[string]any{"type": []string{"string", "integer"}, "description": "one of the new deps whose result branch the work starts from; replaces the earlier base, none = the default rule, which makes the continued dep the base when continue is set"},
							"continue": map[string]any{"type": []string{"string", "integer"}, "description": "one of the new deps whose worker starts this task in its own session; replaces the earlier continued dep, none = start fresh; the continued dep is the task's base, and a base naming another dep is refused"},
						},
						"required": []string{"task", "deps"},
					},
				},
			},
			"required": []string{"plan"},
		},
		Call: callPlanAmend,
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
		Name:        "task_cancel",
		Description: "Cancel a task in this project's current plan, including its live jobs.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"task": map[string]any{"type": "integer", "description": "the task's id"}},
			"required":   []string{"task"},
		},
		Call: callTaskCancel,
	},
	{
		Name:        "ask_user",
		Description: "Record a question for the user in the project journal, where the history shows it. In an attached chat, ask it in the chat too and wait for the answer; a headless turn puts it in its final report and ends — the user answers with `brigada ask`, which wakes the foreman.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string", "description": "the question, verbatim"}},
			"required":   []string{"text"},
		},
		Call: callAskUser,
	},
	{
		Name:        "status",
		Description: "The engine's own state: which process it is, up how long, the jobs by status — plus the running jobs with each worker's latest event, the plans with their tasks and the \"needs you\" queue: tasks parked at needs_foreman or needs_user, and plans awaiting approval, each with its reason. The plans list keeps every live plan and the three most recently finished ones; the older finished plans are counted in one summary line — pass all=true to list every finished plan. Your token scopes the view to your project. A project with instructions for its workers shows their source, version, size and digest; pass `instructions` to read the text itself (as data) before you plan.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"instructions": map[string]any{
					"type":        "boolean",
					"description": "also return the project's instruction text for workers, verbatim data in <project-instructions> delimiters",
				},
				"all": map[string]any{
					"type":        "boolean",
					"description": "list every finished plan, not only the three most recent; the default counts the older ones in one summary line",
				},
			},
		},
		Call: callStatus,
	},
	{
		Name:        "history",
		Description: "The project's timeline — events and messages merged, oldest first — the newest `limit` items (default 100), the newest at the end. To read further back, narrow to one `task` or raise `limit`.",
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
		Description: "The capacity snapshot: per agent its effective cap (and the layer that sets it), running jobs, quota state (ready, cooling until a time, or broken) and cost weight (which steers dispatch: within a tier, agents rank by weight divided by a draw from their record, and weight 0 goes first); per layer — each runner, account, capped model, active plan of the project and global — the cap, use and queue depth. Call this when you plan: it tells you how wide the task graph can be, when to sequence instead of fanning out, which tasks can wait for a cheaper agent, and which agents are cooling or broken.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Call: callCapacity,
	},
	{
		Name:        "stats",
		Description: "The project's per-agent and class outcomes, CLI usage, dollars, wall time and cost per success. Usage does not steer dispatch.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Call: callStats,
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
// scoped by the token the client bears. all lists every finished plan
// where the default counts the older ones (§10).
func callStatus(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var in struct {
		Instructions bool `json:"instructions"`
		All          bool `json:"all"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("status: bad arguments: %w", err)
		}
	}
	var st api.Status
	var err error
	switch {
	case in.All:
		st, err = c.StatusAll(ctx, in.Instructions)
	case in.Instructions:
		st, err = c.StatusWithInstructions(ctx)
	default:
		st, err = c.Status(ctx)
	}
	if err != nil {
		return "", err
	}
	return statusText(st), nil
}

// statusText renders a status answer as the status tools print it.
func statusText(st api.Status) string {
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
	for _, si := range st.Instructions {
		if si.Text != "" {
			fmt.Fprintf(&b, "\n\nproject #%d:\n%s", si.ProjectID, prompt.ForemanInstructionsSection(si.Text))
		}
	}
	return b.String()
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
// internal/history, optionally one task's timeline and a page size. It
// reads the newest page, so the latest items are always in view. The
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
	q := api.HistoryQuery{Limit: a.Limit, Last: true}
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
