package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// The start-up reopen pass (issue #137): plans the engine closed `done`
// before #133's live-work rule shipped still hold tasks parked at
// needs_foreman — a rejected push, a land conflict, a failed land check —
// and every guard that reads a plan's state refuses them: `land_retry`
// the plan, `task_cancel` the live-plan lists, `plan_amend` the liveness
// itself, and a plan that is not active never closes again. The fix
// brings the data to the invariant, not the guards: one pass at engine
// start-up returns every such plan to active, and the work then runs
// through the existing paths — a retried landing, an amendment's cancel,
// the plan's own re-close — with the land_retry and task_cancel guards
// exactly as they are.

// ReopenStrandedPlans is the start-of-engine pass (§13, issue #137): in
// one store transaction, after the sweep as every recovery call does, it
// finds every plan in state done whose nodes still hold live work by the
// plan-close rule's own test (planLiveWorkTx, #133) and returns each to
// active. Every done plan is read, not only the ones known when the pass
// was written. For each reopened plan it bumps updated_at — a plan write
// the plan-file mirror's trigger (§6.4) marks owed, so plan-file recovery
// cannot restore `done` — and appends one plan_reopened event naming the
// plan and the task ids that kept it open. It writes no inbox row and
// wakes no foreman: the foreman's tools, not the foreman, were what the
// closed plan refused. It leaves cancelled plans and every other state
// untouched, and it does not touch the outcome record: a reopened plan's
// re-close counts once more on the plan class (§8), which is measurement
// only. A second run changes nothing — a reopened plan is active, and no
// done plan that settles stays behind. It returns the reopened plan ids.
func (e *Engine) ReopenStrandedPlans(ctx context.Context) ([]int64, error) {
	var reopened []int64
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT id, project_id FROM plans WHERE state = ? ORDER BY id`, planDone)
		if err != nil {
			return fmt.Errorf("list the done plans: %w", err)
		}
		type donePlan struct {
			id, project int64
		}
		var plans []donePlan
		for rows.Next() {
			var p donePlan
			if err := rows.Scan(&p.id, &p.project); err != nil {
				rows.Close()
				return fmt.Errorf("list the done plans: %w", err)
			}
			plans = append(plans, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list the done plans: %w", err)
		}
		rows.Close()
		for _, p := range plans {
			nodes, err := loadAmendNodesTx(tx, p.id)
			if err != nil {
				return err
			}
			live, err := planLiveWorkTx(tx, nodes)
			if err != nil {
				return err
			}
			if len(live) == 0 {
				continue
			}
			// The state write is a plan write like any other: the
			// mirror's trigger marks the plan's file owed, so recovery
			// reads the plan active and cannot restore `done`.
			if _, err := tx.Exec(`UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`,
				"active", fmtTime(now), p.id); err != nil {
				return fmt.Errorf("reopen plan %d: %w", p.id, err)
			}
			if _, err := appendEventTx(tx, evPlanReopened, p.project, 0, 0,
				eventPayload{Status: "active", Plan: p.id, Tasks: live}, now); err != nil {
				return err
			}
			reopened = append(reopened, p.id)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reopen stranded plans: %w", err)
	}
	return reopened, nil
}
