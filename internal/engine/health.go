package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The reasons an agent is broken (§8 "Health"). The runner-stale one is
// the only one the runner's return clears; the other two wait for the
// user's reset.
const (
	brokenAuth     = "auth"
	brokenFailures = "repeated failures"
	brokenStale    = "runner stale"
)

// Shipped defaults of the quota and health settings (§8).
const (
	// defaultLimitCooling is how long a limit with no known reset cools
	// the account: a known reset may name hours, an unknown one is
	// retried sooner.
	defaultLimitCooling = 30 * time.Minute
	// defaultFailureStreak is how many consecutive failed completions
	// by one agent mark it broken.
	defaultFailureStreak = 2
	// defaultRunnerStale is how long a runner may go unseen before its
	// agents are marked broken.
	defaultRunnerStale = 15 * time.Minute
)

// ErrNoAgent is the reset's refusal when no registered agent matches
// the harness and model named.
var ErrNoAgent = errors.New("no such agent")

// agentOutcomeTx keeps the quota and health states (§8) from what a
// task or foreman completion shows: a limit cools the account, an auth
// error or a streak of failures marks the agent broken. A job that ran
// on no recorded agent, or a completion without an error, changes
// nothing.
func (e *Engine) agentOutcomeTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	if job.AgentID == 0 || c.Error == "" {
		return nil
	}
	switch c.ErrorKind {
	case "limit":
		// The foreman's own key already handles its limit (§4.4); whether
		// the dispatcher should also cool the account a foreman's limit
		// names is left to a later decision (§4.4), so it is skipped here
		// on purpose. A limit is quota, not health either way: it never
		// counts toward the failure streak.
		if job.Kind == KindForeman {
			return nil
		}
		return e.coolAccountTx(tx, job, c, now)
	case "auth":
		return e.markBrokenTx(tx, job, job.AgentID, brokenAuth, now)
	}
	// The agent's last N completions all failed, the one just written
	// included. The jobs table is the count: terminal done, failed and
	// stalled jobs on the agent, newest first.
	n := e.cfg.FailureStreak
	rows, err := tx.Query(`SELECT status FROM jobs
		WHERE agent_id = ? AND status IN ('done', 'failed', 'stalled')
		ORDER BY updated_at DESC, id DESC LIMIT ?`, job.AgentID, n)
	if err != nil {
		return fmt.Errorf("count the failures of agent %d: %w", job.AgentID, err)
	}
	defer rows.Close()
	seen, failed := 0, 0
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return fmt.Errorf("count the failures of agent %d: %w", job.AgentID, err)
		}
		seen++
		if status == statusFailed {
			failed++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("count the failures of agent %d: %w", job.AgentID, err)
	}
	rows.Close()
	if seen < n || failed < n {
		return nil
	}
	return e.markBrokenTx(tx, job, job.AgentID, brokenFailures, now)
}

// coolAccountTx cools every agent sharing the failed job's agent's
// account label (§8 "Quota"): until the completion's reset time when it
// lies ahead, else now plus the configured default. An account already
// cooling past that keeps its later deadline. An agent with no account
// label shares its quota with no one and cools alone. One plain event
// names the account and the deadline; no inbox, no wake.
func (e *Engine) coolAccountTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	var account string
	if err := tx.QueryRow("SELECT account FROM agents WHERE id = ?", job.AgentID).Scan(&account); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read the account of agent %d: %w", job.AgentID, err)
	}
	until := now.Add(e.cfg.LimitCooling)
	if t, err := time.Parse(time.RFC3339, c.LimitUntil); err == nil && t.After(now) {
		until = t
	}
	untilStr := fmtTime(until)
	query := `UPDATE agents SET cooling_until = ?, updated_at = ?
		WHERE (cooling_until IS NULL OR cooling_until < ?) AND `
	args := []any{untilStr, fmtTime(now), untilStr}
	if account == "" {
		query += "id = ?"
		args = append(args, job.AgentID)
	} else {
		query += "account = ?"
		args = append(args, account)
	}
	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf("cool account %q: %w", account, err)
	}
	_, err := appendEventTx(tx, evAgentCooling, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Account: account, Until: untilStr}, now)
	return err
}

// markBrokenTx marks the agent broken with reason (§8 "Health") and
// appends the plain event. An agent already broken for a lasting reason
// stays as it is; one broken only for a stale runner takes a lasting
// reason over, so the runner's return does not clear what the agent's
// own failures earned.
func (e *Engine) markBrokenTx(tx *sql.Tx, job *jobRow, agent int64, reason string, now time.Time) error {
	var harness, model, health, why string
	if err := tx.QueryRow("SELECT harness, model, health, health_reason FROM agents WHERE id = ?", agent).
		Scan(&harness, &model, &health, &why); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read agent %d: %w", agent, err)
	}
	if health == quotaBroken && (why != brokenStale || reason == brokenStale) {
		return nil
	}
	if _, err := tx.Exec("UPDATE agents SET health = 'broken', health_reason = ?, updated_at = ? WHERE id = ?",
		reason, fmtTime(now), agent); err != nil {
		return fmt.Errorf("mark agent %d broken: %w", agent, err)
	}
	var project, task, jobID int64
	if job != nil {
		project, task, jobID = job.ProjectID, job.TaskID, job.ID
	}
	_, err := appendEventTx(tx, evAgentBroken, project, task, jobID,
		eventPayload{Agent: agent, Harness: harness, Model: model, Reason: reason}, now)
	return err
}

// sweepStaleRunnersTx marks the agents of every runner unseen past the
// stale window broken("runner stale") (§8 "Health"). A runner's
// last_seen_at is its register, lease poll or heartbeat; one that never
// recorded a sighting is left alone.
func (e *Engine) sweepStaleRunnersTx(tx *sql.Tx, now time.Time) error {
	rows, err := tx.Query(`SELECT a.id FROM agents a JOIN runners r ON r.id = a.runner_id
		WHERE a.health = 'ready' AND r.last_seen_at IS NOT NULL AND r.last_seen_at <= ?
		ORDER BY a.id`, fmtTime(now.Add(-e.cfg.RunnerStale)))
	if err != nil {
		return fmt.Errorf("sweep stale runners: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("sweep stale runners: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("sweep stale runners: %w", err)
	}
	rows.Close()
	for _, id := range ids {
		if err := e.markBrokenTx(tx, nil, id, brokenStale, now); err != nil {
			return err
		}
	}
	return nil
}

// runnerSeenTx records that the runner is here (§8 "Health"): its
// last_seen_at moves to now, and its agents broken only for a stale
// runner come back — the runner is back. Auth and repeated-failure
// broken stay, and so does cooling: quota is real until it resets.
// Callers run it before the sweep, so a runner that is polling is never
// swept as stale by its own call.
func (e *Engine) runnerSeenTx(tx *sql.Tx, runner int64, now time.Time) error {
	if _, err := tx.Exec("UPDATE runners SET last_seen_at = ? WHERE id = ?", fmtTime(now), runner); err != nil {
		return fmt.Errorf("touch runner %d: %w", runner, err)
	}
	rows, err := tx.Query(`SELECT id, harness, model FROM agents
		WHERE runner_id = ? AND health = 'broken' AND health_reason = ? ORDER BY id`, runner, brokenStale)
	if err != nil {
		return fmt.Errorf("list the stale agents of runner %d: %w", runner, err)
	}
	type back struct {
		id             int64
		harness, model string
	}
	var agents []back
	for rows.Next() {
		var b back
		if err := rows.Scan(&b.id, &b.harness, &b.model); err != nil {
			rows.Close()
			return fmt.Errorf("list the stale agents of runner %d: %w", runner, err)
		}
		agents = append(agents, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list the stale agents of runner %d: %w", runner, err)
	}
	rows.Close()
	for _, b := range agents {
		if _, err := tx.Exec("UPDATE agents SET health = 'ready', health_reason = '', updated_at = ? WHERE id = ?",
			fmtTime(now), b.id); err != nil {
			return fmt.Errorf("clear the stale mark of agent %d: %w", b.id, err)
		}
		if _, err := appendEventTx(tx, evAgentRecovered, 0, 0, 0,
			eventPayload{Agent: b.id, Harness: b.harness, Model: b.model, Reason: brokenStale}, now); err != nil {
			return err
		}
	}
	return nil
}

// ResetAgents clears broken and cooling (§8: "or the user resets it")
// for the agents named by harness and model. The user's handle is the
// pair `brigada status` and `brigada stats` print, not an id: a model
// name shared by several runners resets all of them, which is what a
// user fixing one login or waiting out one quota means. Cooling is
// cleared on the matching agents only — their account siblings keep
// theirs until they reset or lapse. One plain event; ErrNoAgent when
// nothing matches. It returns how many agents matched.
func (e *Engine) ResetAgents(ctx context.Context, harness, model string) (int, error) {
	if harness == "" || model == "" {
		return 0, fmt.Errorf("reset agents: %w: harness and model are required", ErrNoAgent)
	}
	var n int
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE agents SET health = 'ready', health_reason = '', cooling_until = NULL, updated_at = ?
			WHERE harness = ? AND model = ?`, fmtTime(now), harness, model)
		if err != nil {
			return fmt.Errorf("reset agents: %w", err)
		}
		cnt, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("reset agents: rows: %w", err)
		}
		if cnt == 0 {
			return fmt.Errorf("reset agents: %w: %s/%s", ErrNoAgent, harness, model)
		}
		n = int(cnt)
		_, err = appendEventTx(tx, evAgentReset, 0, 0, 0,
			eventPayload{Harness: harness, Model: model, Count: cnt}, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// CoolingAgent is one agent the dispatcher is holding back for quota
// (§8, §10): it shows in status next to the running jobs, but not in
// the needs-you queue — cooling waits for a clock, not a person.
type CoolingAgent struct {
	ID      int64  `json:"id"`
	Harness string `json:"harness"`
	Model   string `json:"model"`
	Account string `json:"account"`
	Until   string `json:"until"`
}

// statusCoolingTx lists the agents cooling at now that are not broken
// (a broken agent is in the needs-you queue instead).
func statusCoolingTx(tx *sql.Tx, now time.Time) ([]CoolingAgent, error) {
	rows, err := tx.Query(`SELECT id, harness, model, account, cooling_until FROM agents
		WHERE health = 'ready' AND cooling_until > ? ORDER BY id`, fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("list the cooling agents: %w", err)
	}
	defer rows.Close()
	out := []CoolingAgent{}
	for rows.Next() {
		var a CoolingAgent
		if err := rows.Scan(&a.ID, &a.Harness, &a.Model, &a.Account, &a.Until); err != nil {
			return nil, fmt.Errorf("list the cooling agents: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
