package main

import (
	"context"
	"fmt"
	"io"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// planDecider is the two calls the plan commands need from the API
// client; tests stub it.
type planDecider interface {
	ApprovePlan(ctx context.Context, plan int64) (api.PlanDecisionResult, error)
	RejectPlan(ctx context.Context, plan int64) (api.PlanDecisionResult, error)
}

const planUsage = "usage: brigada plan approve <id> | brigada plan reject <id>"

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usagef(stderr, planUsage)
	}
	switch args[0] {
	case "approve", "reject":
		return runPlanDecision(ctx, args[0], args[1:], stdout, stderr)
	default:
		return usagef(stderr, "unknown plan subcommand %q; want approve or reject", args[0])
	}
}

// runPlanDecision is the user's yes or no on a plan over the approval
// limit (§6.3). Only the client token can say it: the foreman has no
// way to confirm its own plans.
func runPlanDecision(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plan " + verb)
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usagef(stderr, "usage: brigada plan %s <id>", verb)
	}
	id, ok := parseID(stderr, "plan id", rest[0])
	if !ok {
		return 2
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	return printPlanDecision(ctx, client, verb, id, stdout, stderr)
}

func printPlanDecision(ctx context.Context, client planDecider, verb string, id int64, stdout, stderr io.Writer) int {
	var res api.PlanDecisionResult
	var err error
	if verb == "approve" {
		res, err = client.ApprovePlan(ctx, id)
	} else {
		res, err = client.RejectPlan(ctx, id)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if verb == "approve" {
		fmt.Fprintf(stdout, "plan #%d approved: %d tasks queued\n", res.PlanID, len(res.Tasks))
		for _, t := range res.Tasks {
			fmt.Fprintf(stdout, "task #%d: work job %d\n", t.TaskID, t.JobID)
		}
		return 0
	}
	fmt.Fprintf(stdout, "plan #%d rejected: its tasks are cancelled\n", res.PlanID)
	return 0
}
