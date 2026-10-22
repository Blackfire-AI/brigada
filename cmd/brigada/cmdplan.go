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
	ApprovePlan(ctx context.Context, plan int64, andStopAsking bool) (api.PlanDecisionResult, error)
	RejectPlan(ctx context.Context, plan int64) (api.PlanDecisionResult, error)
}

const planUsage = "usage: brigada plan approve [--and-stop-asking] <id> | brigada plan reject <id>"

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "plan", planUsage, "approve or reject", planCommands)
}

// runPlanDecision is the user's yes or no on a plan over the approval
// limit (§6.3). Only the client token can say it: the foreman has no
// way to confirm its own plans. --and-stop-asking (approve only) also
// changes the one setting each hold reason maps to, the way
// `brigada project set` does, so the same question is not asked twice.
func runPlanDecision(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) int {
	description := "Approve a plan waiting over the approval limit and queue its tasks."
	if verb == "reject" {
		description = "Reject a plan waiting over the approval limit and cancel its tasks."
	}
	fs := newFlagSet("plan "+verb, "brigada plan "+verb+" [--no-start] <id>", description)
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	stopAsking := fs.Bool("and-stop-asking", false, "also change the rule that held the plan, the way `brigada project set` does, so the same question is not asked twice")
	if code, stop := common.parse(stdout, stderr, args); stop {
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
	return printPlanDecision(ctx, client, verb, id, *stopAsking, stdout, stderr)
}

func printPlanDecision(ctx context.Context, client planDecider, verb string, id int64, stopAsking bool, stdout, stderr io.Writer) int {
	var res api.PlanDecisionResult
	var err error
	if verb == "approve" {
		res, err = client.ApprovePlan(ctx, id, stopAsking)
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
		for _, s := range res.Settings {
			fmt.Fprintf(stdout, "rule changed: %s\n", s)
		}
		return 0
	}
	fmt.Fprintf(stdout, "plan #%d rejected: its tasks are cancelled\n", res.PlanID)
	return 0
}
