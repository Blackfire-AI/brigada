package engine

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// CapsUsage builds the caps model's usage view (§8.1) in one read-only
// transaction: the live jobs — leased or running, not queued, which
// hold no runner, no agent and no slot — counted per runner (the lease's
// holder), per account and per model (the agent the lease recorded),
// per plan (the task's) and globally. The lease gate (8b) reads this
// beside the configured limits; nothing here changes the store.
func (e *Engine) CapsUsage(ctx context.Context) (caps.Usage, error) {
	var usage caps.Usage
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT COALESCE(j.lease_owner, 0), COALESCE(a.account, ''), COALESCE(a.model, ''), COALESCE(t.plan_id, 0)
			FROM jobs j
			LEFT JOIN agents a ON a.id = j.agent_id
			LEFT JOIN tasks t ON t.id = j.task_id
			WHERE j.status IN ('leased', 'running')
			ORDER BY j.id`)
		if err != nil {
			return fmt.Errorf("list live jobs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var runner, plan int64
			var account, model string
			if err := rows.Scan(&runner, &account, &model, &plan); err != nil {
				return fmt.Errorf("list live jobs: %w", err)
			}
			usage.Add(runner, account, model, plan)
		}
		return rows.Err()
	})
	if err != nil {
		return caps.Usage{}, fmt.Errorf("caps usage: %w", err)
	}
	return usage, nil
}
