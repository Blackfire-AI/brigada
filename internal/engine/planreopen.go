package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The stranded-plan reopen (issue #137): plans closed `done` before
// #133's plan-close rule shipped (§6.2) can hold live work still — tasks
// a failed landing parked at needs_foreman, done tasks whose landing
// never happened — and every tool that could act on them refuses a plan
// that is not live: land_retry, plan_amend's cancel, task_cancel. A plan
// that is not active never closes again. ReopenStrandedPlans brings the
// existing state up to the invariant once, at engine start-up: every
// done plan whose nodes hold live work by the plan-close rule's own test
// (livePlanWorkTx) returns to active, and the dependents run and the
// plan closes again through the existing paths. The guards themselves
// stay as they are; only the data moves.

// ReopenStrandedPlans is the start-of-engine pass (§13, issue #137): in
// one store transaction, after the sweep, every plan in state done whose
// nodes hold live work (§6.2, §7.1 — the same test finishPlanIfClosedTx
// closes on) returns to active, one plan_reopened event naming the task
// ids that kept it open. The plan's file is mirrored again through the
// plan-state change itself — the plan_mirror_dirty trigger the store's
// UPDATE fires (§6.4) — so a later plan-file recovery cannot restore
// done. Cancelled plans and every other state stay untouched, no foreman
// is woken, and the outcome record is not read or written: a reopened
// plan's re-close counts once more on the plan class (§8), measurement
// only. A second run changes nothing: the plans it reopened are active.
// It returns the reopened plan ids.
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
		type donePlan struct{ id, project int64 }
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
			live, err := livePlanWorkTx(tx, nodes)
			if err != nil {
				return err
			}
			if len(live) == 0 {
				continue
			}
			if _, err := tx.Exec(`UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`,
				"active", fmtTime(now), p.id); err != nil {
				return fmt.Errorf("reopen plan %d: %w", p.id, err)
			}
			why := make([]string, len(live))
			for k, i := range live {
				why[k] = fmt.Sprintf("%s (%s)", nodes[i].label(), nodes[i].state)
			}
			if _, err := appendEventTx(tx, evPlanReopened, p.project, 0, 0,
				eventPayload{Plan: p.id, Status: "active", Reason: strings.Join(why, ", ")}, now); err != nil {
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
