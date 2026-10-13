package engine

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// StatusInfo is the engine's own state as the API shows it (§13): which
// process is the engine, since when, and the jobs by status. The pid and
// start are what the lifecycle recorded (RecordInstance); a store no
// engine of this binary has recorded shows zero values.
type StatusInfo struct {
	Pid       int64          `json:"pid"`
	StartedAt string         `json:"started_at"`
	Jobs      map[string]int `json:"jobs"`
}

// JobStatuses is the job statuses' fixed vocabulary, in the order the
// plain-text renderings print them: live first, then terminal. Status
// zero-fills its counts over it, and the renderers — the client's
// status command, the MCP bridge's status tool — share the order.
var JobStatuses = []string{
	statusQueued, statusLeased, statusRunning,
	statusDone, statusFailed, statusCancelled, statusStalled,
}

// Status reads the engine's own state (§13). Like every engine call it
// sweeps first, so the counts are the rules' current word: a lease a
// dead runner left is expired here, not counted as work.
func (e *Engine) Status(ctx context.Context) (StatusInfo, error) {
	var info StatusInfo
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := sweepTx(tx, e.cfg, e.cfg.Now()); err != nil {
			return err
		}
		var pid sql.NullInt64
		var started sql.NullString
		if err := tx.QueryRow("SELECT engine_pid, engine_started_at FROM meta WHERE id = 1").
			Scan(&pid, &started); err != nil {
			return fmt.Errorf("read the engine instance: %w", err)
		}
		info.Pid = pid.Int64
		info.StartedAt = started.String
		rows, err := tx.Query("SELECT status, COUNT(*) FROM jobs GROUP BY status")
		if err != nil {
			return fmt.Errorf("count jobs by status: %w", err)
		}
		defer rows.Close()
		counts := make(map[string]int, len(JobStatuses))
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				return fmt.Errorf("count jobs by status: %w", err)
			}
			counts[status] = n
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("count jobs by status: %w", err)
		}
		for _, s := range JobStatuses {
			if _, ok := counts[s]; !ok {
				counts[s] = 0 // every status appears in the answer, at zero
			}
		}
		info.Jobs = counts
		return nil
	})
	if err != nil {
		return StatusInfo{}, fmt.Errorf("read status: %w", err)
	}
	return info, nil
}

// RecordInstance notes which process is the engine and since when
// (§13): the lifecycle writes it once it holds the single-instance lock,
// and `brigada status` reads it through the API.
func (e *Engine) RecordInstance(ctx context.Context, pid int64, started time.Time) error {
	return e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE meta SET engine_pid = ?, engine_started_at = ? WHERE id = 1",
			pid, fmtTime(started)); err != nil {
			return fmt.Errorf("record the engine instance: %w", err)
		}
		return nil
	})
}

// LiveJobs counts the live jobs — queued, leased and running — which is
// the §13 idle rule's "no live jobs": a job waiting for a runner counts
// as work exactly like one a runner holds.
func (e *Engine) LiveJobs(ctx context.Context) (int, error) {
	var n int
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE status IN ('queued', 'leased', 'running')").Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("count live jobs: %w", err)
	}
	return n, nil
}
