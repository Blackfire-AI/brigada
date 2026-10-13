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
// per plan (the task's) and globally. The lease gate reads the same
// view beside the configured limits; nothing here changes the store.
func (e *Engine) CapsUsage(ctx context.Context) (caps.Usage, error) {
	var usage caps.Usage
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		var err error
		usage, err = capsUsageTx(tx)
		return err
	})
	if err != nil {
		return caps.Usage{}, fmt.Errorf("caps usage: %w", err)
	}
	return usage, nil
}

// capsUsageTx counts the live jobs into the caps model's usage view
// (§8.1) inside the caller's transaction: the lease gate builds it once
// per call and adds what it leases, so every decision in that call sees
// the slots already spoken for.
func capsUsageTx(tx *sql.Tx) (caps.Usage, error) {
	var usage caps.Usage
	rows, err := tx.Query(`SELECT COALESCE(j.lease_owner, 0), COALESCE(a.account, ''), COALESCE(a.model, ''), COALESCE(t.plan_id, 0)
		FROM jobs j
		LEFT JOIN agents a ON a.id = j.agent_id
		LEFT JOIN tasks t ON t.id = j.task_id
		WHERE j.status IN ('leased', 'running')
		ORDER BY j.id`)
	if err != nil {
		return usage, fmt.Errorf("list live jobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runner, plan int64
		var account, model string
		if err := rows.Scan(&runner, &account, &model, &plan); err != nil {
			return usage, fmt.Errorf("list live jobs: %w", err)
		}
		usage.Add(runner, account, model, plan)
	}
	return usage, rows.Err()
}

// capsJobTx builds the caps model's Job for a queued job owner would
// take (§8.1): the agent the lease would record (leaseAgentTx's pick)
// with its account and model, the plan through the task, the foreman
// flag, and the cap the runner advertised for the runner layer to fold
// with the config's. Agent 0 — the runner advertises none that fits —
// leaves the account and model layers nothing to count by.
func capsJobTx(tx *sql.Tx, owner, agent int64, advertised int, j *jobRow) (caps.Job, error) {
	job := caps.Job{Runner: owner, RunnerCap: advertised, Foreman: j.Kind == KindForeman}
	if agent != 0 {
		if err := tx.QueryRow("SELECT account, model FROM agents WHERE id = ?", agent).Scan(&job.Account, &job.Model); err != nil {
			return caps.Job{}, fmt.Errorf("read agent %d: %w", agent, err)
		}
	}
	if j.TaskID != 0 {
		if err := tx.QueryRow("SELECT plan_id FROM tasks WHERE id = ?", j.TaskID).Scan(&job.Plan); err != nil {
			return caps.Job{}, fmt.Errorf("read the plan of task %d: %w", j.TaskID, err)
		}
	}
	return job, nil
}
