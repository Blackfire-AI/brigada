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

// pairRegisteredTx reports whether any agent of the foreman's pair is
// registered, on any runner and in any health. The pair is the constraint:
// foreman turns are exempt from the advertised-class filter. An agent that
// is busy, cooling or broken is a different wait from no agent at all.
func pairRegisteredTx(tx *sql.Tx, harness, model string) (bool, error) {
	var registered bool
	if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM agents WHERE harness = ? AND model = ?)", harness, model).Scan(&registered); err != nil {
		return false, fmt.Errorf("look up the agents of %s/%s: %w", harness, model, err)
	}
	return registered, nil
}

// foremanTurnMove is a queued turn and the pair it resolved to before the
// project's foreman changed.
type foremanTurnMove struct {
	job  *jobRow
	from foremanPair
}

// queuedForemanTurnMovesTx lists the turns a foreman switch carries. A
// queued turn without a recorded model keeps its own resolution: notably,
// one on another harness stays unresolved.
func queuedForemanTurnMovesTx(tx *sql.Tx, project int64) ([]foremanTurnMove, error) {
	rows, err := tx.Query(`SELECT `+jobColumns+` FROM jobs
		WHERE project_id = ? AND status = 'queued' AND kind = ? AND model <> '' ORDER BY id`, project, string(KindForeman))
	if err != nil {
		return nil, fmt.Errorf("list queued foreman turns to move: %w", err)
	}
	var moves []foremanTurnMove
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("list queued foreman turns to move: %w", err)
		}
		from, err := foremanPairTx(tx, j)
		if err != nil {
			rows.Close()
			return nil, err
		}
		moves = append(moves, foremanTurnMove{job: j, from: from})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list queued foreman turns to move: %w", err)
	}
	rows.Close()
	return moves, nil
}

// moveForemanTurnTx puts a turn on its new pair without touching its prompt,
// events or queue position, and records both pairs when they differ.
func moveForemanTurnTx(tx *sql.Tx, job *jobRow, from, to foremanPair, now time.Time) (bool, error) {
	if from == to {
		return false, nil
	}
	if _, err := tx.Exec("UPDATE jobs SET harness = ?, model = ?, updated_at = ? WHERE id = ?",
		to.harness, to.model, fmtTime(now), job.ID); err != nil {
		return false, fmt.Errorf("move foreman turn %d to %s: %w", job.ID, to, err)
	}
	if _, err := appendEventTx(tx, evForemanTurnMoved, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Harness: to.harness, Model: to.model, FromHarness: from.harness, FromModel: from.model}, now); err != nil {
		return false, err
	}
	return true, nil
}

// requeueForemanTurnTx gives a returning leased turn the pair the project
// holds now. Its old pair is the one recorded on the job, or the leased
// agent's for an older turn that did not record a complete pair.
func requeueForemanTurnTx(tx *sql.Tx, job *jobRow, now time.Time) error {
	if job.Kind != KindForeman {
		return nil
	}
	from := foremanPair{harness: job.Harness, model: job.Model}
	if !from.ok() && job.AgentID != 0 {
		err := tx.QueryRow("SELECT harness, model FROM agents WHERE id = ?", job.AgentID).Scan(&from.harness, &from.model)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read the pair foreman turn %d leased: %w", job.ID, err)
		}
	}
	if !from.ok() {
		var err error
		if from, err = foremanPairTx(tx, job); err != nil {
			return err
		}
	}
	var to foremanPair
	if err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", job.ProjectID).
		Scan(&to.harness, &to.model); err != nil {
		return fmt.Errorf("read the current foreman of project %d: %w", job.ProjectID, err)
	}
	_, err := moveForemanTurnTx(tx, job, from, to, now)
	return err
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
