package engine

import (
	"database/sql"
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
// Tier fallback is resolved before this hold (decided 2026-10-20): a served
// tier whose agents all cool for no more than longCooling (an hour) waits
// until its reset: only a reset within longCooling is waited out. An agent
// cooling for longer, such as a multi-day limit, counts like a broken one, and a tier
// with only broken or long-cooling agents is skipped in favor of an available
// tier when one exists (climbing, then stepping down from heavy). The hold
// reads agents of the served tier alone, so a job whose tier has moved to an
// available agent is not held, and a hold lifts when it does. Only when no
// tier has an available agent does the job hold, for the tier whose agent
// resets first. A pinned job never moves: it waits for its agent.
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
		agents, err := qualifyingAgentsTx(tx, e.cfg.Caps, nil, 0, j, kindClass(j.Kind), nowStr, true)
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
				if _, err := tx.Exec(`UPDATE jobs SET cap_held_at = ?, cap_held_layer = NULL, cap_held_key = NULL, slot_hold = 0, updated_at = ?
					WHERE id = ?`, nowStr, nowStr, j.ID); err != nil {
					return fmt.Errorf("lift the cooling hold of job %d: %w", j.ID, err)
				}
			}
			continue
		}
		if err := holdCoolingJobTx(tx, j, soonest.id, now); err != nil {
			return err
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
