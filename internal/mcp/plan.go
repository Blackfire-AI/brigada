package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// planCreateArgs is the plan_create tool's arguments (§6.2, §12.2): the
// plan's title and its tasks, each title, name, spec, check, class
// and the names of the tasks it depends on.
type planCreateArgs struct {
	Title string        `json:"title"`
	Tasks []planTaskArg `json:"tasks"`
}

type planTaskArg struct {
	Tier   string         `json:"tier,omitempty"`
	Effort string         `json:"effort,omitempty"`
	Caps   map[string]int `json:"caps,omitempty"`
	Title  string         `json:"title"`
	Name   string         `json:"name"`
	Deps   []string       `json:"deps"`
	Base   string         `json:"base,omitempty"`
	Spec   string         `json:"spec"`
	Check  string         `json:"check"`
	Class  string         `json:"class"`
	// ExtraRepos (§5.8): absent inherits the project default, a list (even
	// []) replaces it. No omitempty, so [] and absent hash differently.
	ExtraRepos []extraRepoArg `json:"extra_repos"`
}

// extraRepoArg is one read-only extra repository of a task (§5.8).
type extraRepoArg struct {
	Name string `json:"name"`
	Repo string `json:"repo"`
	Ref  string `json:"ref,omitempty"`
}

// extraRepos maps the tool's argument to the API's shape, keeping the
// difference between absent (nil) and empty.
func extraRepos(in []extraRepoArg) []engine.ExtraRepo {
	if in == nil {
		return nil
	}
	out := make([]engine.ExtraRepo, len(in))
	for i, x := range in {
		out[i] = engine.ExtraRepo{Name: x.Name, Repo: x.Repo, Ref: x.Ref}
	}
	return out
}

// callPlanCreate runs the plan_create tool (§6.2, §12.2): the plan and
// its tasks through /v1/plans, rendered as text naming the plan, its
// tasks and their queued jobs (none for a task waiting on dependencies). The bridge mints the idempotency key per
// logical call (§12.1) — the model never supplies it — so a repeated
// call replays the first answer instead of planning twice.
// canonical returns the call's arguments with their defaults applied
// (§12.1): the shape one logical call hashes as, so an explicit default
// and an omitted one mint the same key.
func (a planCreateArgs) canonical() planCreateArgs {
	for i := range a.Tasks {
		if a.Tasks[i].Tier == "" {
			a.Tasks[i].Tier = "standard"
		}
		if a.Tasks[i].Class == "" {
			a.Tasks[i].Class = "implement"
		}
	}
	return a
}

func callPlanCreate(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a planCreateArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	a = a.canonical()
	p := api.PlanCreate{Title: a.Title, Tasks: make([]api.PlanTask, len(a.Tasks))}
	for i, t := range a.Tasks {
		p.Tasks[i] = api.PlanTask{Title: t.Title, Name: t.Name, Deps: t.Deps, Base: t.Base, Spec: t.Spec, Check: t.Check, Class: t.Class, Tier: t.Tier, Effort: t.Effort, Caps: t.Caps, ExtraRepos: extraRepos(t.ExtraRepos)}
	}
	res, err := c.CreatePlan(ctx, p, idemKey("plan_create", a))
	if err != nil {
		return "", err
	}
	if res.State == "pending_approval" {
		// Over the approval limit (§6.3): nothing queued, and only the
		// user's `brigada plan approve` starts it — the foreman has no
		// tool for that. Tell the user why it waits.
		text := fmt.Sprintf("plan %d %q: awaiting approval (state pending_approval), %d tasks, no work job queued. Why: %s. Tell the user: `brigada plan approve %d` starts it, `brigada plan reject %d` cancels it.",
			res.PlanID, a.Title, len(res.Tasks), strings.Join(res.Reasons, "; "), res.PlanID, res.PlanID)
		for i, t := range res.Tasks {
			if suffix := taskTierEffort(a.Tasks[i].Tier, a.Tasks[i].Effort); suffix != "" {
				text += fmt.Sprintf("\ntask #%d %s%s", t.TaskID, t.Name, suffix)
			}
		}
		return text, nil
	}
	if len(res.Tasks) == 1 && taskTierEffort(a.Tasks[0].Tier, a.Tasks[0].Effort) != "" {
		t := res.Tasks[0]
		label := fmt.Sprintf("task %d", t.TaskID)
		if t.Name != "" {
			label += " " + t.Name
		}
		return fmt.Sprintf("plan %d %q: %s%s, work job %d %s", res.PlanID, a.Title, label, taskTierEffort(a.Tasks[0].Tier, a.Tasks[0].Effort), res.JobID, res.Answer), nil
	}
	if len(res.Tasks) <= 1 {
		return fmt.Sprintf("plan %d %q: task %d, work job %d %s",
			res.PlanID, a.Title, res.TaskID, res.JobID, res.Answer), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "plan %d %q: %d tasks", res.PlanID, a.Title, len(res.Tasks))
	for i, t := range res.Tasks {
		label := fmt.Sprintf("task %d", t.TaskID)
		if t.Name != "" {
			label += " " + t.Name
		}
		label += taskTierEffort(a.Tasks[i].Tier, a.Tasks[i].Effort)
		if t.JobID == 0 {
			fmt.Fprintf(&b, "\n%s: waiting on dependencies", label)
		} else {
			fmt.Fprintf(&b, "\n%s: work job %d", label, t.JobID)
		}
	}
	return b.String(), nil
}

// taskRef names a task of a plan — by name or by id — as the model writes
// it: a string, or a bare number for an id.
type taskRef string

func (r *taskRef) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*r = taskRef(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("a task is named by a string or an id, got %s", b)
	}
	*r = taskRef(n.String())
	return nil
}

func refStrings(refs []taskRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = string(r)
	}
	return out
}

// planAmendArgs is the plan_amend tool's arguments (§12.2).
type planAmendArgs struct {
	Plan     int64             `json:"plan"`
	Add      []planAddArg      `json:"add"`
	Cancel   []taskRef         `json:"cancel"`
	Retarget []planRetargetArg `json:"retarget"`
	Retier   []planRetierArg   `json:"retier"`
}

type planAddArg struct {
	Tier   string         `json:"tier,omitempty"`
	Effort string         `json:"effort,omitempty"`
	Caps   map[string]int `json:"caps,omitempty"`
	Title  string         `json:"title"`
	Name   string         `json:"name"`
	Deps   []taskRef      `json:"deps"`
	Base   taskRef        `json:"base,omitempty"`
	Spec   string         `json:"spec"`
	Check  string         `json:"check"`
	Class  string         `json:"class"`
	// ExtraRepos as planTaskArg's.
	ExtraRepos []extraRepoArg `json:"extra_repos"`
}

type planRetierArg struct {
	Task   taskRef        `json:"task"`
	Tier   string         `json:"tier,omitempty"`
	Effort string         `json:"effort,omitempty"`
	Caps   map[string]int `json:"caps,omitempty"`
}

type planRetargetArg struct {
	Task taskRef   `json:"task"`
	Deps []taskRef `json:"deps"`
	Base taskRef   `json:"base,omitempty"`
}

// callPlanAmend runs the plan_amend tool (§12.2): the amendment through
// /v1/plans/{id}/amend, rendered as text naming what changed, what
// started and the plan's state. The idempotency key is minted from the
// arguments, as plan_create's is.
func callPlanAmend(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a planAmendArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	if a.Plan == 0 {
		return "", fmt.Errorf("plan is required: the id of the plan to amend")
	}
	for i := range a.Add {
		if a.Add[i].Tier == "" {
			a.Add[i].Tier = "standard"
		}
		if a.Add[i].Class == "" {
			a.Add[i].Class = "implement"
		}
	}
	am := api.PlanAmend{Cancel: refStrings(a.Cancel)}
	for _, t := range a.Add {
		am.Add = append(am.Add, api.PlanTask{Title: t.Title, Name: t.Name, Deps: refStrings(t.Deps), Base: string(t.Base), Spec: t.Spec, Check: t.Check, Class: t.Class, Tier: t.Tier, Effort: t.Effort, Caps: t.Caps, ExtraRepos: extraRepos(t.ExtraRepos)})
	}
	for _, rt := range a.Retarget {
		am.Retarget = append(am.Retarget, api.PlanRetarget{Task: string(rt.Task), Deps: refStrings(rt.Deps), Base: string(rt.Base)})
	}
	for _, rt := range a.Retier {
		am.Retier = append(am.Retier, api.PlanRetier{Task: string(rt.Task), Tier: rt.Tier, Effort: rt.Effort, Caps: rt.Caps})
	}
	res, err := c.AmendPlan(ctx, a.Plan, am, idemKey("plan_amend", a))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "plan %d amended: state %s", res.PlanID, res.State)
	label := func(t api.PlanCreatedTask) string {
		s := fmt.Sprintf("task %d", t.TaskID)
		if t.Name != "" {
			s += " " + t.Name
		}
		return s
	}
	for i, t := range res.Added {
		fmt.Fprintf(&b, "\nadded %s%s", label(t), taskTierEffort(a.Add[i].Tier, a.Add[i].Effort))
	}
	for _, id := range res.Cancelled {
		fmt.Fprintf(&b, "\ncancelled task %d", id)
	}
	for _, id := range res.Retargeted {
		fmt.Fprintf(&b, "\nre-pointed task %d", id)
	}
	for _, id := range res.Retiered {
		fmt.Fprintf(&b, "\nretiered task %d", id)
	}
	for _, t := range res.Started {
		fmt.Fprintf(&b, "\n%s: work job %d queued", label(t), t.JobID)
	}
	switch {
	case res.State == "pending_approval":
		fmt.Fprintf(&b, "\nawaiting approval, no new work queued. Why: %s. Tell the user: `brigada plan approve %d` starts it, `brigada plan reject %d` cancels it.",
			strings.Join(res.Reasons, "; "), res.PlanID, res.PlanID)
	case res.State == "done":
		b.WriteString("\nthe amendment closed the plan; its done report follows in your events")
	}
	return b.String(), nil
}

// idemKey mints one logical call's idempotency key (§12.1): the tool's
// name plus a hash of its arguments, canonicalized through the parsed
// shape so two serializations of one call — the same fields in another
// order — hash alike. The key is scoped to the caller by the API, so
// the bridge's mint never crosses callers.
func idemKey(tool string, args any) string {
	canonical, err := json.Marshal(args)
	if err != nil {
		canonical = []byte(fmt.Sprintf("%v", args))
	}
	sum := sha256.Sum256(canonical)
	return tool + "-" + hex.EncodeToString(sum[:])
}

// taskTierEffort describes a task when either setting differs from defaults.
func taskTierEffort(tier, effort string) string {
	if tier == "" {
		tier = "standard"
	}
	if tier == "standard" && effort == "" {
		return ""
	}
	parts := []string{tier}
	if effort != "" {
		parts = append(parts, "effort "+effort)
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
