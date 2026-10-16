package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// layerNoPair is the hold layer of a foreman turn waiting for a runner
// agent that offers its harness/model pair (§4.1, §8.1). Like the cooling
// layer it is not a cap layer, but it holds like one: cap_held_at keeps
// the queue TTL from failing the turn, and the status lists it as held.
const layerNoPair = "no_pair"

// foremanPair is the harness/model a foreman turn must run on.
type foremanPair struct {
	harness, model string
}

// ok reports whether the pair is whole. A turn whose pair is not is
// unrecoverable (a legacy turn queued on a harness the project has since
// left, with no model recorded): no agent matches it, so it is held, never
// treated as a wildcard model.
func (p foremanPair) ok() bool { return p.harness != "" && p.model != "" }

// String is the pair as the status and the warnings name it.
func (p foremanPair) String() string {
	harness, model := p.harness, p.model
	if harness == "" {
		harness = "?"
	}
	if model == "" {
		model = "?"
	}
	return harness + "/" + model
}

// foremanPairTx resolves the harness/model pair a job must run on. It is
// the one resolution every placement decision reads — the agent filter,
// the poll's harness check, the busy-runner rule, the no-pair hold — so
// they cannot disagree. For a foreman turn the pair is the one recorded on
// the job; a turn submitted without a harness (the admin job submit) takes
// the project's chosen harness, and one without a model takes the
// project's model whenever the harness is the project's own. A turn queued
// on another harness with no model recorded stays unresolved. Any other
// kind of job has no pair: its harness constraint alone stands.
func foremanPairTx(tx *sql.Tx, j *jobRow) (foremanPair, error) {
	pair := foremanPair{harness: j.Harness, model: j.Model}
	if j.Kind != KindForeman {
		return pair, nil
	}
	var projectHarness, projectModel string
	err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", j.ProjectID).
		Scan(&projectHarness, &projectModel)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return foremanPair{}, fmt.Errorf("read the foreman of project %d: %w", j.ProjectID, err)
	}
	switch {
	case pair.harness == "":
		pair.harness = projectHarness
		if pair.model == "" {
			pair.model = projectModel
		}
	case pair.model == "" && pair.harness == projectHarness:
		pair.model = projectModel
	}
	return pair, nil
}

// pairRegisteredTx reports whether any registered agent, on any runner and
// in any health, offers the foreman's class for the pair. An agent that is
// busy, cooling or broken is registered: it is a different wait from no
// agent at all.
func pairRegisteredTx(tx *sql.Tx, harness, model string) (bool, error) {
	rows, err := tx.Query("SELECT classes FROM agents WHERE harness = ? AND model = ?", harness, model)
	if err != nil {
		return false, fmt.Errorf("list the agents of %s/%s: %w", harness, model, err)
	}
	defer rows.Close()
	for rows.Next() {
		var classes string
		if err := rows.Scan(&classes); err != nil {
			return false, fmt.Errorf("list the agents of %s/%s: %w", harness, model, err)
		}
		if agentOffersClass(classes, kindClass(KindForeman)) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// holdNoPairTx stamps the no-pair hold on every queued foreman turn whose
// harness/model pair no registered agent offers, renews it on every sweep
// while that stays so, and lifts it — restarting the TTL clock — once the
// pair is registered. It runs from the sweep alone: no poll can stamp it
// (no agent is eligible, so the lease never reaches the cap gate), and no
// poll of an unrelated runner can lift it.
func (e *Engine) holdNoPairTx(tx *sql.Tx, now time.Time) error {
	rows, err := tx.Query(`SELECT `+jobColumns+` FROM jobs WHERE status = 'queued' AND kind = ? ORDER BY id`, string(KindForeman))
	if err != nil {
		return fmt.Errorf("list queued foreman turns: %w", err)
	}
	var queued []*jobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("list queued foreman turns: %w", err)
		}
		queued = append(queued, j)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list queued foreman turns: %w", err)
	}
	rows.Close()

	nowStr := fmtTime(now)
	for _, j := range queued {
		pair, err := foremanPairTx(tx, j)
		if err != nil {
			return err
		}
		registered := false
		if pair.ok() {
			if registered, err = pairRegisteredTx(tx, pair.harness, pair.model); err != nil {
				return err
			}
		}
		if registered {
			if j.CapHeldLayer == layerNoPair {
				if _, err := tx.Exec(`UPDATE jobs SET cap_held_at = ?, cap_held_layer = NULL, cap_held_key = NULL, slot_hold = 0, updated_at = ?
					WHERE id = ?`, nowStr, nowStr, j.ID); err != nil {
					return fmt.Errorf("lift the no-pair hold of job %d: %w", j.ID, err)
				}
			}
			continue
		}
		// Stamped on every sweep so the TTL clock never runs. No
		// job_cap_held event is appended: the status names the wait, and
		// the event is for cap layers, which this is not.
		if _, err := tx.Exec(`UPDATE jobs SET cap_held_at = ?, cap_held_layer = ?, cap_held_key = ?, slot_hold = 0, updated_at = ?
			WHERE id = ?`, nowStr, layerNoPair, pair.String(), nowStr, j.ID); err != nil {
			return fmt.Errorf("hold job %d for the pair %s: %w", j.ID, pair, err)
		}
	}
	return nil
}

// foremanSessionStaleTx reports whether a foreman turn's session id must
// not become the key's resume target: the project has since switched to
// another harness than the one the turn's lease ran on, and a session id
// belongs to one harness's CLI. The lease is bound to the session
// generation it was stamped with — every harness change moves the
// project's on, so a turn that outlived a switch away and back is stale
// too — and to the harness of the agent it ran on, the lease's own
// record, not whatever the project says now.
func foremanSessionStaleTx(tx *sql.Tx, job *jobRow) (bool, error) {
	if job.Kind != KindForeman {
		return false, nil
	}
	var leasedGen, currentGen int64
	if err := tx.QueryRow("SELECT session_gen FROM jobs WHERE id = ?", job.ID).Scan(&leasedGen); err != nil {
		return false, fmt.Errorf("read the session generation of job %d: %w", job.ID, err)
	}
	if err := tx.QueryRow("SELECT foreman_session_gen FROM projects WHERE id = ?", job.ProjectID).Scan(&currentGen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read the session generation of project %d: %w", job.ProjectID, err)
	}
	if leasedGen != currentGen {
		return true, nil
	}
	ranOn := ""
	if job.AgentID != 0 {
		err := tx.QueryRow("SELECT harness FROM agents WHERE id = ?", job.AgentID).Scan(&ranOn)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("read the agent of job %d: %w", job.ID, err)
		}
	}
	if ranOn == "" {
		pair, err := foremanPairTx(tx, job)
		if err != nil {
			return false, err
		}
		ranOn = pair.harness
	}
	var current string
	if err := tx.QueryRow("SELECT foreman_harness FROM projects WHERE id = ?", job.ProjectID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read the foreman of project %d: %w", job.ProjectID, err)
	}
	return ranOn != current, nil
}
