package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// CoordinatorTools is the coordinator bridge's offer (§12.2, §4.6 mode
// B): `brigada mcp --coordinator`, a session over several projects. It
// bears the client token, never a foreman credential. It talks to each
// project's foreman and reads across projects, and it carries the user's
// decision on a plan — and nothing that plans or edits: no plan_create,
// no plan_amend, no task mutation and no hold tool.
var CoordinatorTools = []Tool{
	{
		Name:        "list_projects",
		Description: "Every project the engine knows, by canonical repo URL: its foreman, its active plans and how many things need the user. Name a project in the other tools by this URL.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Call: callListProjects,
	},
	{
		Name:        "ask_foreman",
		Description: "Send one project's foreman a message as the user (a request: plan this, fix that, what is blocking task 3?). One request per project: a cross-project plan does not exist, so send each project its own message. With nobody attached to the foreman, it wakes up and acts on it; otherwise the message waits for the attached chat. Follow the work with status and history. The same text again within a day counts as a retry of the first call; reword it to send it anew.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo_url": map[string]any{
					"type":        "string",
					"description": "the project's canonical repo URL, as list_projects shows it",
				},
				"text": map[string]any{
					"type":        "string",
					"description": "the message, as the user would write it to the foreman",
				},
			},
			"required": []string{"repo_url", "text"},
		},
		Call: callCoordinatorAsk,
	},
	{
		Name:        "status",
		Description: "The engine's state: the running jobs with each worker's latest event, the plans with their tasks, messages still waiting for a foreman and the \"needs you\" queue, with each plan awaiting approval in full (its id, version, tasks and why it waits). Across every project, or for one with repo_url.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo_url": map[string]any{
					"type":        "string",
					"description": "one project's canonical repo URL; omit for every project",
				},
			},
		},
		Call: callCoordinatorStatus,
	},
	{
		Name:        "history",
		Description: "The timeline — events and messages merged, oldest first — across every project, or one project's with repo_url, optionally one task's, at most `limit` items.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo_url": map[string]any{
					"type":        "string",
					"description": "one project's canonical repo URL; omit for every project",
				},
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
		Call: callCoordinatorHistory,
	},
	{
		Name:        "plan_approve",
		Description: "Approve a plan awaiting approval: its first wave of work starts. This is the USER's decision, never yours: call it only after you showed the user the plan (status shows it) and they said an explicit yes in this chat. \"Awaiting approval\" is a result to present, not a permission. Name the plan's id and the version you showed; a plan amended since is refused, then read status again and show it anew.",
		InputSchema: planDecisionSchema,
		Call:        callPlanApprove,
	},
	{
		Name:        "plan_reject",
		Description: "Reject a plan awaiting approval: its tasks are cancelled and its foreman hears of it. This is the USER's decision, never yours: call it only after you showed the user the plan and they said an explicit no (or asked to drop it) in this chat. Name the plan's id and the version you showed; a plan amended since is refused, then read status again and show it anew.",
		InputSchema: planDecisionSchema,
		Call:        callPlanReject,
	},
}

// planDecisionSchema is plan_approve's and plan_reject's arguments: the
// project, the plan and the plan version the user was shown (§4.6).
var planDecisionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"repo_url": map[string]any{
			"type":        "string",
			"description": "the plan's project, by canonical repo URL",
		},
		"plan": map[string]any{
			"type":        "integer",
			"description": "the plan's id, as status shows it",
		},
		"version": map[string]any{
			"type":        "integer",
			"description": "the plan version the user was shown, as status shows it",
		},
	},
	"required": []string{"repo_url", "plan", "version"},
}

// CoordinatorToolNames lists the coordinator's tools by name.
func CoordinatorToolNames() []string {
	out := make([]string, 0, len(CoordinatorTools))
	for _, t := range CoordinatorTools {
		out = append(out, t.Name)
	}
	return out
}

func callListProjects(ctx context.Context, c *api.Client, _ json.RawMessage) (string, error) {
	projects, err := c.Projects(ctx)
	if err != nil {
		return "", err
	}
	if len(projects) == 0 {
		return "(no projects: run `brigada init` in a repository or in the folder holding several)", nil
	}
	var b strings.Builder
	for i, p := range projects {
		if i > 0 {
			b.WriteByte('\n')
		}
		foreman := "no foreman"
		if p.Harness != "" {
			foreman = p.Harness + "/" + p.Model
		}
		fmt.Fprintf(&b, "%s: foreman %s, %d active plans, %d need you", p.RepoURL, foreman, p.ActivePlans, p.NeedsYou)
	}
	return b.String(), nil
}

// scopedClient narrows c's reads to the project repo names, resolved
// among the engine's projects by canonical URL. An empty repo is every
// project; a repo no project carries is an error naming list_projects.
func scopedClient(ctx context.Context, c *api.Client, repo string) (*api.Client, error) {
	if strings.TrimSpace(repo) == "" {
		return c, nil
	}
	projects, err := c.Projects(ctx)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(repo)
	want := raw
	if canon, err := engine.Canonicalize(raw); err == nil {
		want = canon
	}
	for _, p := range projects {
		have := p.RepoURL
		if canon, err := engine.Canonicalize(have); err == nil {
			have = canon
		}
		if p.RepoURL == raw || have == want {
			return c.ProjectReader(p.ProjectID), nil
		}
	}
	return nil, fmt.Errorf("no project for repo %s: call list_projects for the URLs", engine.RedactRepoURL(repo))
}

func callCoordinatorAsk(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a struct {
		RepoURL string `json:"repo_url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("arguments: %w", err)
	}
	if strings.TrimSpace(a.RepoURL) == "" {
		return "", fmt.Errorf("arguments: repo_url is required: name the project (list_projects shows them)")
	}
	return callAskForeman(ctx, c, args)
}

func callCoordinatorStatus(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a struct {
		RepoURL string `json:"repo_url"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	sc, err := scopedClient(ctx, c, a.RepoURL)
	if err != nil {
		return "", err
	}
	st, err := sc.Status(ctx)
	if err != nil {
		return "", err
	}
	text := statusText(st)
	var b strings.Builder
	for _, n := range st.NeedsYou {
		if n.Kind != "plan" || n.State != "pending_approval" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\n\nplans awaiting approval (the user decides; show each one in full and wait for their explicit yes or no):")
		}
		fmt.Fprintf(&b, "\nplan %d version %d of %s: %q", n.ID, n.Version, n.RepoURL, n.Title)
		if n.Reason != "" {
			fmt.Fprintf(&b, "\n  why it waits: %s", n.Reason)
		}
		for _, t := range n.Tasks {
			fmt.Fprintf(&b, "\n  task %d [%s] %s", t.ID, t.Class, t.Title)
			if len(t.WaitingOn) > 0 {
				fmt.Fprintf(&b, " (waits on %s)", strings.Join(t.WaitingOn, ", "))
			}
		}
	}
	return text + b.String(), nil
}

func callCoordinatorHistory(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a struct {
		RepoURL string `json:"repo_url"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	sc, err := scopedClient(ctx, c, a.RepoURL)
	if err != nil {
		return "", err
	}
	return callHistory(ctx, sc, args)
}

// planDecisionArgs is plan_approve's and plan_reject's arguments.
type planDecisionArgs struct {
	RepoURL string `json:"repo_url"`
	Plan    int64  `json:"plan"`
	Version int64  `json:"version"`
}

func readPlanDecision(args json.RawMessage) (planDecisionArgs, error) {
	var a planDecisionArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("arguments: %w", err)
	}
	switch {
	case strings.TrimSpace(a.RepoURL) == "":
		return a, fmt.Errorf("arguments: repo_url is required: the plan's project")
	case a.Plan <= 0:
		return a, fmt.Errorf("arguments: plan is required: the plan's id")
	case a.Version <= 0:
		return a, fmt.Errorf("arguments: version is required: the plan version the user was shown, as status shows it")
	}
	return a, nil
}

func callPlanApprove(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	a, err := readPlanDecision(args)
	if err != nil {
		return "", err
	}
	res, err := c.ApprovePlanVersion(ctx, a.RepoURL, a.Plan, a.Version)
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("plan %d approved (state %s): %d tasks queued", res.PlanID, res.State, len(res.Tasks))
	for _, t := range res.Tasks {
		text += fmt.Sprintf("\n  task %d: work job %d", t.TaskID, t.JobID)
	}
	return text, nil
}

func callPlanReject(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	a, err := readPlanDecision(args)
	if err != nil {
		return "", err
	}
	res, err := c.RejectPlanVersion(ctx, a.RepoURL, a.Plan, a.Version)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("plan %d rejected (state %s): its tasks are cancelled", res.PlanID, res.State), nil
}
