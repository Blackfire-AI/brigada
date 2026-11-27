package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// layerCooling is the hold layer of a job waiting for its agents'
// account quota to reset (§8, §8.1). It is not a cap layer — the caps
// model never names it — but it holds like one: cap_held_at keeps the
// queue TTL clock from running, and the status lists the job as held.
const layerCooling = "cooling"

// defaultCoolingWait is how long a queued task job waits for its cooling
// agents to reset before the sweep parks it (§8.1): a quota reset a
// job waits on is usually a window of a few hours, and a wait past this
// bound is a limit the user should see, never a silent multi-day hold.
const defaultCoolingWait = 6 * time.Hour

// The cooling rule of the queue TTL (§8.1). A queued job whose every
// qualifying agent is cooling — a fix round pinned to its author's
// agent (§8 session affinity) whose account hit a usage limit, or any
// job whose whole candidate set shares that fate — is not unserved: no
// runner can lease it until the quota resets, and it never will be
// moved to another agent. Polls cannot stamp the hold (no agent is
// eligible, so the lease never reaches the cap gate); the sweep does,
// with layer "cooling", and renews it on every sweep while the cooling
// lasts. When the cooling ends the hold lifts and the TTL clock
// restarts at that moment, like the runner layer's.
//
// Tier fallback is resolved before this hold: an agent that is cooling is
// unavailable whatever the distance to its reset, so the served tier is the
// closest tier at or above the asked floor with an available agent — the
// climb never steps down (issue #263). The hold reads agents of the served
// tier alone, so a job whose tier has moved to an available agent is not
// held, and a hold lifts when it does. Only when no tier at or above the
// floor has an available agent does the job hold, for the tier whose agent
// resets first among those tiers. A pinned job never moves: it waits for its
// agent.
//
// Precedence between holds. Cooling is a precondition of the others:
// while every qualifying agent is cooling, no poll can reach the cap gate
// and no runner has a candidate agent, so a stale poll-stamped layer
// hold or a sweep-stamped runner hold can no longer be renewed and
// would only mislead. The cooling hold therefore replaces them (the
// runner rule, which runs first, has already lifted its own). When the
// cooling ends the hold is lifted outright and the next poll or sweep
// stamps whichever layer applies then. A job with at least one
// qualifying agent that is not cooling is never cooling-held.
//
// What is "cooling". Only an agent that is ready with a cooling_until
// ahead of now. A limit with no reset time is not an open-ended wait in
// this engine: coolAccountTx cools the account for the configured
// default (LimitCooling), so it has a deadline and falls on the
// "cooling until" side. A broken agent is the other side: it has no end
// in sight, is not held, and the TTL parks the job with a reason that
// names the pin and its state (see the queue TTL sweep).

// holdCoolingTx stamps the cooling hold on every queued job whose
// qualifying agents are all cooling, renews it while they stay so, and
// lifts it — restarting the TTL clock — when one is available again or
// no agent qualifies at all.
func (e *Engine) holdCoolingTx(tx *sql.Tx, now time.Time) error {
	jobRows, err := tx.Query(`SELECT ` + jobColumns + ` FROM jobs WHERE status = 'queued' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list queued jobs: %w", err)
	}
	var queued []*jobRow
	for jobRows.Next() {
		j, err := scanJob(jobRows)
		if err != nil {
			jobRows.Close()
			return fmt.Errorf("list queued jobs: %w", err)
		}
		queued = append(queued, j)
	}
	if err := jobRows.Err(); err != nil {
		jobRows.Close()
		return fmt.Errorf("list queued jobs: %w", err)
	}
	jobRows.Close()

	nowStr := fmtTime(now)
	for _, j := range queued {
		// The lookup leaves out an agent capped at exactly 0 for the
		// job's kind (zeroCapLayer, issue #195): it never frees up, so a
		// job whose only agent is one is not held but parked by the TTL.
		agents, _, err := qualifyingAgentsTx(tx, &e.cfg, nil, 0, j, kindClass(j.Kind), nowStr, true)
		if err != nil {
			return err
		}
		var soonest *agentCand
		free := false
		for i := range agents {
			a := &agents[i]
			switch {
			case a.health != "ready":
				// broken: no end in sight, neither free nor cooling
			case a.cooling == "" || a.cooling <= nowStr:
				free = true
			case soonest == nil || a.cooling < soonest.cooling:
				soonest = a
			}
		}
		if free || soonest == nil {
			if j.CapHeldLayer == layerCooling {
				if _, err := tx.Exec(`UPDATE jobs SET cap_held_at = ?, cap_held_layer = NULL, cap_held_key = NULL, slot_hold = 0, cooling_since = NULL, updated_at = ?
					WHERE id = ?`, nowStr, nowStr, j.ID); err != nil {
					return fmt.Errorf("lift the cooling hold of job %d: %w", j.ID, err)
				}
			}
			continue
		}
		fresh := j.CapHeldLayer != layerCooling
		if err := holdCoolingJobTx(tx, j, soonest.id, now); err != nil {
			return err
		}
		if fresh {
			// The bound's clock starts when the hold begins, not on every
			// sweep; a later renewal keeps the first stamp.
			if _, err := tx.Exec(`UPDATE jobs SET cooling_since = ? WHERE id = ?`, nowStr, j.ID); err != nil {
				return fmt.Errorf("stamp the cooling hold's start on job %d: %w", j.ID, err)
			}
		}
	}
	return nil
}

// holdCoolingJobTx stamps one cooling hold. The agent whose cooling
// ends first rides in cap_held_key, so the status can name it and the
// time.
func holdCoolingJobTx(tx *sql.Tx, j *jobRow, agent int64, now time.Time) error {
	if err := holdAtCapTx(tx, j, caps.Job{}, layerCooling, now); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE jobs SET cap_held_key = ? WHERE id = ?", strconv.FormatInt(agent, 10), j.ID); err != nil {
		return fmt.Errorf("stamp the cooling agent of job %d: %w", j.ID, err)
	}
	return nil
}

// parkCoolingBoundTx fails a queued job that has waited on its cooling
// agents past CoolingWait (§8.1). The bound makes a quota reset days
// away a visible park — the reason names the soonest-cooling agent, its
// reset and the bound — instead of a silent multi-day hold. The sweep
// runs it after holdCoolingTx, so only a job still cooling-held (its
// cooling_since stamp is live) is read. A job of a paused scope is
// never read (§8.3); the lift that frees it shifts the clock.
func (e *Engine) parkCoolingBoundTx(tx *sql.Tx, now time.Time) error {
	if e.cfg.CoolingWait <= 0 {
		return nil
	}
	rows, err := tx.Query(`SELECT id, project_id, COALESCE(task_id, 0), COALESCE(cap_held_key, '')
		FROM jobs
		WHERE status = 'queued' AND cap_held_layer = '`+layerCooling+`' AND cooling_since IS NOT NULL AND cooling_since <= ?
		AND `+notPausedSQL("jobs"), fmtTime(now.Add(-e.cfg.CoolingWait)))
	if err != nil {
		return fmt.Errorf("sweep cooling wait: %w", err)
	}
	type coolingTimedOut struct {
		id            int64
		project, task int64
		agent         string
	}
	var timedOut []coolingTimedOut
	for rows.Next() {
		var x coolingTimedOut
		if err := rows.Scan(&x.id, &x.project, &x.task, &x.agent); err != nil {
			rows.Close()
			return fmt.Errorf("sweep cooling wait: %w", err)
		}
		timedOut = append(timedOut, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("sweep cooling wait: %w", err)
	}
	rows.Close()

	for _, x := range timedOut {
		reason := fmt.Sprintf("cooling wait exceeded: no runner leased the job within %s", capText(e.cfg.CoolingWait))
		var harness, model, until string
		err := tx.QueryRow("SELECT harness, model, COALESCE(cooling_until, '') FROM agents WHERE id = ?", x.agent).
			Scan(&harness, &model, &until)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read the cooling agent %s of job %d: %w", x.agent, x.id, err)
		}
		if err == nil {
			reason = fmt.Sprintf("cooling wait exceeded: %s/%s is cooling until %s; no runner leased the job within %s",
				harness, model, until, capText(e.cfg.CoolingWait))
		}
		if err := failJobTx(tx, x.id, reason, 0, now); err != nil {
			return err
		}
		if _, err := appendEventTx(tx, evJobQueueTimedOut, x.project, x.task, x.id,
			eventPayload{Status: statusFailed, Reason: reason}, now); err != nil {
			return err
		}
		if err := e.sweptForemanFinishedTx(tx, x.id, x.project, now); err != nil {
			return err
		}
		if x.task != 0 {
			if err := e.needsForemanTx(tx, &jobRow{ID: x.id, ProjectID: x.project, TaskID: x.task},
				evTaskFailed, eventPayload{Reason: boundNote(reason)}, now); err != nil {
				return err
			}
		}
	}
	return nil
}
