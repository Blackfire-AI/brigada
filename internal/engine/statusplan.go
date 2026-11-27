package engine

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/history"
)

// planRecentTransitions is how many of a plan's newest timeline items
// its scoped status carries.
const planRecentTransitions = 8

// PlanScope is what a plan-scoped status adds to the plan's own entry
// (§10): the plan's spend, the commit its landing head stands at and the
// newest items of its timeline.
type PlanScope struct {
	PlanID int64 `json:"plan_id"`
	// Tokens and CostUSD sum the usage the plan's jobs reported: input
	// plus output tokens, and dollars. A counter no harness reported
	// stays out of the sums.
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"cost_usd"`
	// LandHead is the commit the plan's most recently landed task landed
	// as; empty while none has landed.
	LandHead string `json:"land_head,omitempty"`
	// Recent is the plan's newest timeline items, oldest first.
	Recent []history.Item `json:"recent,omitempty"`
}

// StatusForPlan is the status narrowed to one plan (§10), so an operator
// watches it without the global output: the plan with its tasks, and
// only the running jobs, held jobs, retries and needs-you entries that
// belong to them, with the plan's PlanScope. The global job counts, the
// other plans, pauses, slots, the foreman report and the skill and
// instruction inventories are left out. A plan that does not exist, or
// that project's scope does not reach, is ErrNoPlan.
func (e *Engine) StatusForPlan(ctx context.Context, project, plan int64) (StatusInfo, error) {
	full, err := e.status(ctx, project, false, true)
	if err != nil {
		return StatusInfo{}, err
	}
	out := StatusInfo{
		Pid:       full.Pid,
		StartedAt: full.StartedAt,
		Jobs:      map[string]int{},
		Running:   []StatusJob{},
		Slots:     []Slot{},
		Plans:     []StatusPlan{},
		NeedsYou:  []NeedsYouItem{},
	}
	tasks := map[int64]bool{}
	for _, p := range full.Plans {
		if p.ID != plan {
			continue
		}
		out.Plans = append(out.Plans, p)
		for _, t := range p.Tasks {
			tasks[t.ID] = true
		}
	}
	if len(out.Plans) == 0 {
		return StatusInfo{}, fmt.Errorf("%w: %d", ErrNoPlan, plan)
	}
	mine := func(it NeedsYouItem) bool {
		return (it.Kind == "plan" && it.ID == plan) || (it.Kind == "task" && it.PlanID == plan)
	}
	for _, j := range full.Running {
		if tasks[j.TaskID] {
			out.Running = append(out.Running, j)
		}
	}
	for _, h := range full.Held {
		if tasks[h.TaskID] {
			out.Held = append(out.Held, h)
		}
	}
	for _, r := range full.Retries {
		if tasks[r.TaskID] {
			out.Retries = append(out.Retries, r)
		}
	}
	for _, it := range full.NeedsYou {
		if mine(it) {
			out.NeedsYou = append(out.NeedsYou, it)
		}
	}
	for _, it := range full.ForemanHolds {
		if mine(it) {
			out.ForemanHolds = append(out.ForemanHolds, it)
		}
	}

	scope := &PlanScope{PlanID: plan}
	for _, t := range out.Plans[0].Tasks {
		if t.Landed && t.LandedCommit != "" {
			scope.LandHead = t.LandedCommit
		}
	}
	err = e.store.View(ctx, func(tx *sql.Tx) error {
		var err error
		scope.Tokens, scope.CostUSD, err = planSpendTx(tx, plan)
		return err
	})
	if err != nil {
		return StatusInfo{}, err
	}
	page, err := e.History(ctx, HistoryQuery{ProjectID: project, PlanID: plan, Limit: planRecentTransitions, Last: true})
	if err != nil {
		return StatusInfo{}, err
	}
	scope.Recent = page.Items
	out.PlanScope = scope
	return out, nil
}
