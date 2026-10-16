package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// The busy-runner rule of the queue TTL (§8.1). The TTL is for jobs no
// runner can serve; a job whose every candidate runner is at its own
// parallel limit is not unserved, it is waiting for a slot, and it
// waits as long as it takes — like a job held by any other full cap
// layer. A runner at capacity does not poll for work, so no lease poll
// stamps the hold: the sweep does, for every queued job whose candidate
// runners are all full, with layer "runner". The stamp renews on every
// sweep while the runners stay full, so the TTL clock (the later of
// queued_at and cap_held_at) restarts at the moment a slot frees, and
// the job is parked only after it has spent a whole TTL with a free
// slot nobody took.

// runnerSlot is one registered runner as the busy-runner rule reads it.
type runnerSlot struct {
	id     int64
	limit  int      // parallel jobs the runner runs at once
	labels []string // what it offered on its latest poll
	known  bool     // whether its labels were recorded by a poll

	harnesses      []string // the harnesses it offered on its latest poll
	harnessesKnown bool
}

// holdBusyRunnersTx stamps the runner-layer hold on every queued job
// that has at least one candidate runner, all of them at capacity, and
// lifts a runner-layer hold whose candidates have a free slot again —
// restarting the TTL clock at that moment. A job with no candidate
// runner at all is left alone: today's TTL and park apply to it. polling
// is the runner whose lease poll triggered the sweep, 0 for any other
// call: it is asking for work, so it has a free slot.
func (e *Engine) holdBusyRunnersTx(tx *sql.Tx, now time.Time, polling int64) error {
	rows, err := tx.Query("SELECT id, max_parallel, labels, harnesses FROM runners ORDER BY id")
	if err != nil {
		return fmt.Errorf("list runners: %w", err)
	}
	var runners []runnerSlot
	for rows.Next() {
		var r runnerSlot
		var advertised int
		var labels, harnesses string
		if err := rows.Scan(&r.id, &advertised, &labels, &harnesses); err != nil {
			rows.Close()
			return fmt.Errorf("list runners: %w", err)
		}
		// A runner that advertised no cap runs one job at a time, which
		// is what a sequential runner is; the config's runner cap can
		// only lower that.
		r.limit = advertised
		if r.limit <= 0 {
			r.limit = 1
		}
		if e.cfg.Caps.Runner != nil && *e.cfg.Caps.Runner < r.limit {
			r.limit = *e.cfg.Caps.Runner
		}
		if labels != "" && json.Unmarshal([]byte(labels), &r.labels) == nil {
			r.known = true
		}
		if harnesses != "" && json.Unmarshal([]byte(harnesses), &r.harnesses) == nil {
			r.harnessesKnown = true
		}
		runners = append(runners, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list runners: %w", err)
	}
	rows.Close()
	if len(runners) == 0 {
		return nil
	}

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
	if len(queued) == 0 {
		return nil
	}

	usage, err := capsUsageTx(tx)
	if err != nil {
		return err
	}
	swept := map[int64]bool{} // the jobs whose runner hold the sweep itself stamped
	sweptRows, err := tx.Query("SELECT id FROM jobs WHERE status = 'queued' AND slot_hold = 1")
	if err != nil {
		return fmt.Errorf("list the slot holds: %w", err)
	}
	for sweptRows.Next() {
		var id int64
		if err := sweptRows.Scan(&id); err != nil {
			sweptRows.Close()
			return fmt.Errorf("list the slot holds: %w", err)
		}
		swept[id] = true
	}
	if err := sweptRows.Err(); err != nil {
		sweptRows.Close()
		return fmt.Errorf("list the slot holds: %w", err)
	}
	sweptRows.Close()

	nowStr := fmtTime(now)
	for _, j := range queued {
		required := jobLabels(j)
		var first int64
		candidates, busy := 0, 0
		for _, r := range runners {
			if len(required) > 0 && !(r.known && labelsCover(r.labels, required)) {
				continue
			}
			// The lease's own rule: a job pinned to a harness goes only
			// to a runner whose poll offers it, whatever it registered.
			// A runner whose offer is not recorded yet is unknown, so it
			// is no candidate for a pinned job.
			if j.Harness != "" && !(r.harnessesKnown && slices.Contains(r.harnesses, j.Harness)) {
				continue
			}
			if j.ReleasedBy == r.id && j.ReleasedUntil != "" && j.ReleasedUntil > nowStr {
				continue
			}
			if j.Mode == ModeSession {
				free, err := affinityFreeTx(tx, j.Key, r.id)
				if err != nil {
					return err
				}
				if !free {
					continue
				}
			}
			agents, err := eligibleAgentsTx(tx, r.id, j, kindClass(j.Kind), nowStr)
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				continue
			}
			if candidates == 0 {
				first = r.id
			}
			candidates++
			// A runner polling right now has a free slot, whatever the
			// count says: a full runner does not poll.
			if r.id != polling && usage.Runner[r.id] >= r.limit {
				busy++
			}
		}
		waitingForSlot := candidates > 0 && busy == candidates
		switch {
		case waitingForSlot && j.CapHeldLayer != "" && j.CapHeldLayer != caps.LayerRunner:
			// Another layer's hold keeps its layer and its event; the
			// clock still restarts, since the runners that would poll
			// for it are full.
			if _, err := tx.Exec("UPDATE jobs SET cap_held_at = ?, updated_at = ? WHERE id = ?", nowStr, nowStr, j.ID); err != nil {
				return fmt.Errorf("renew the hold of job %d: %w", j.ID, err)
			}
		case waitingForSlot:
			if err := holdAtCapTx(tx, j, caps.Job{Runner: first}, caps.LayerRunner, now); err != nil {
				return err
			}
			// A runner hold a poll stamped stays the poll's; one the
			// sweep put on an unheld job is the sweep's to lift.
			if j.CapHeldLayer == "" || swept[j.ID] {
				if _, err := tx.Exec("UPDATE jobs SET slot_hold = 1 WHERE id = ?", j.ID); err != nil {
					return fmt.Errorf("mark the slot hold of job %d: %w", j.ID, err)
				}
			}
		case swept[j.ID]:
			// A slot freed (or the candidates went away): the hold the
			// sweep put lifts and the TTL clock restarts here.
			if _, err := tx.Exec(`UPDATE jobs SET cap_held_at = ?, cap_held_layer = NULL, cap_held_key = NULL, slot_hold = 0, updated_at = ?
				WHERE id = ?`, nowStr, nowStr, j.ID); err != nil {
				return fmt.Errorf("lift the runner hold of job %d: %w", j.ID, err)
			}
		}
	}
	return nil
}

// HeldJob is a queued job a full cap layer holds, as the status shows
// it (§10): the job, its task and kind, and the layer that has no room
// — "runner" is a job waiting for a runner slot.
type HeldJob struct {
	ID     int64  `json:"id"`
	TaskID int64  `json:"task_id,omitempty"`
	Kind   string `json:"kind"`
	Layer  string `json:"layer"`
	Since  string `json:"since,omitempty"`
}

// statusHeldTx lists the queued jobs held by a full cap layer, oldest
// first.
func statusHeldTx(tx *sql.Tx, project int64) ([]HeldJob, error) {
	query := `SELECT id, COALESCE(task_id, 0), kind, cap_held_layer, COALESCE(cap_held_at, '')
		FROM jobs WHERE status = 'queued' AND cap_held_layer IS NOT NULL`
	var args []any
	if project != 0 {
		query += " AND project_id = ?"
		args = append(args, project)
	}
	rows, err := tx.Query(query+" ORDER BY id", args...)
	if err != nil {
		return nil, fmt.Errorf("list the held jobs: %w", err)
	}
	defer rows.Close()
	out := []HeldJob{}
	for rows.Next() {
		var h HeldJob
		if err := rows.Scan(&h.ID, &h.TaskID, &h.Kind, &h.Layer, &h.Since); err != nil {
			return nil, fmt.Errorf("list the held jobs: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
