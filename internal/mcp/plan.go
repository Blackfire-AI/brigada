package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// planCreateArgs is the plan_create tool's arguments (§6.2, §12.2): the
// plan's title and its tasks, each title, spec, check and class.
type planCreateArgs struct {
	Title string        `json:"title"`
	Tasks []planTaskArg `json:"tasks"`
}

type planTaskArg struct {
	Title string `json:"title"`
	Spec  string `json:"spec"`
	Check string `json:"check"`
	Class string `json:"class"`
}

// callPlanCreate runs the plan_create tool (§6.2, §12.2): the plan and
// its one task through /v1/plans, rendered as text naming the plan, the
// task and the queued job. The bridge mints the idempotency key per
// logical call (§12.1) — the model never supplies it — so a repeated
// call replays the first answer instead of planning twice.
// canonical returns the call's arguments with their defaults applied
// (§12.1): the shape one logical call hashes as, so an explicit default
// and an omitted one mint the same key.
func (a planCreateArgs) canonical() planCreateArgs {
	for i := range a.Tasks {
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
		p.Tasks[i] = api.PlanTask{Title: t.Title, Spec: t.Spec, Check: t.Check, Class: t.Class}
	}
	res, err := c.CreatePlan(ctx, p, idemKey("plan_create", a))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("plan %d %q: task %d, work job %d %s",
		res.PlanID, a.Title, res.TaskID, res.JobID, res.Answer), nil
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
