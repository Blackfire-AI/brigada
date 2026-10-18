package engine

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

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
func capsJobTx(tx *sql.Tx, owner, agent int64, advertised int, j *jobRow, limits caps.Limits) (caps.Job, error) {
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
	// Foreman headroom (§8.2): a task job — work, fix or review — on one
	// of the project's foreman accounts sees that account's cap lowered.
	if !limits.NoHeadroom && !job.Foreman && job.Account != "" {
		accounts, err := foremanAccountsTx(tx, j.ProjectID)
		if err != nil {
			return caps.Job{}, err
		}
		job.Headroom = slices.Contains(accounts, job.Account)
	}
	return job, nil
}

// foremanAccountsTx is the project's reserved accounts (§8.2): the
// account labels of the agents advertising the project's foreman
// harness and model — the pair its turns are leased to — sorted. A
// project with no foreman, or a foreman pair no agent advertises, has
// none.
func foremanAccountsTx(tx *sql.Tx, project int64) ([]string, error) {
	rows, err := tx.Query(`SELECT DISTINCT a.account FROM agents a
		JOIN projects p ON p.foreman_harness = a.harness AND p.foreman_model = a.model
		WHERE p.id = ? AND p.archived_at IS NULL AND p.foreman_harness <> '' ORDER BY a.account`, project)
	if err != nil {
		return nil, fmt.Errorf("list foreman accounts of project %d: %w", project, err)
	}
	defer rows.Close()
	var accounts []string
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			return nil, fmt.Errorf("list foreman accounts of project %d: %w", project, err)
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}
