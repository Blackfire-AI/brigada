package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// ErrInvalidAgent marks a refusal the caller caused: an advertisement
// the engine will not take, whatever its reason. The API turns it into
// the caller's 400; every other Register error is the engine's own and
// surfaces as an internal failure.
var ErrInvalidAgent = errors.New("invalid agent advertisement")

// AgentAdvert is one advertised (adapter, harness, model) combination a
// runner offers (§5.1): what the dispatcher will choose between (§8).
// The account label names the quota pool — cooling covers the whole
// account, not one agent — and family is the model family a reviewer
// must differ from (§7).
type AgentAdvert struct {
	Adapter string
	Harness string
	Model   string
	Family  string
	Account string
	Classes []string
	// Effort is the reasoning-effort level the agent runs at; empty when
	// unset. It is not part of the agent's key: outcomes group by harness
	// and model.
	Effort string
	// Tools is the claude worker scope the runner recorded (§5.2), the
	// patterns joined by "+" or "none"; "" for a harness brigada does not
	// scope.
	Tools string
}

func (a AgentAdvert) valid() error {
	switch {
	case a.Adapter == "":
		return fmt.Errorf("%w: adapter is required", ErrInvalidAgent)
	case a.Harness == "":
		return fmt.Errorf("%w: harness is required", ErrInvalidAgent)
	case a.Model == "":
		return fmt.Errorf("%w: model is required", ErrInvalidAgent)
	}
	if err := adapter.ValidateEffort(a.Harness, a.Effort); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAgent, err)
	}
	return nil
}

// classesJSON encodes the agent's classes; an agent offering none takes
// no classes, stored as the empty array.
func (a AgentAdvert) classesJSON() (string, error) {
	classes := a.Classes
	if classes == nil {
		classes = []string{}
	}
	b, err := json.Marshal(classes)
	if err != nil {
		return "", fmt.Errorf("encode classes: %w", err)
	}
	return string(b), nil
}

// Register upserts the runner's row and its advertisement (§5.1):
// registering again refreshes last_seen and keeps the id, and the
// agents the call carries replace the runner's set, keyed by runner,
// adapter and model (§11). Agents the new set no longer names are
// deleted; ones it keeps keep their rows, so the outcomes that reference
// them survive a restart. maxParallel is the runner's own cap on
// parallel jobs (§8.1): 0 means it advertised none. The row's id is what
// every lease the runner takes references, so a runner must register
// before it can lease.
func (e *Engine) Register(ctx context.Context, name string, maxParallel int, agents []AgentAdvert) (int64, error) {
	return e.RegisterStarted(ctx, name, time.Time{}, maxParallel, agents)
}

// RegisterStarted is Register for a runner that reports when its process
// started. The engine keeps that time as the runner's started_at and
// moves it only when the reported time differs from the stored one: a
// re-poll of the same process leaves it, a restarted process sets it. A
// zero startedAt reports nothing and leaves the stored time alone.
func (e *Engine) RegisterStarted(ctx context.Context, name string, startedAt time.Time, maxParallel int, agents []AgentAdvert) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("register runner: name is required")
	}
	if maxParallel < 0 {
		return 0, fmt.Errorf("register runner %q: %w: max_parallel must be 0 or more, got %d", name, ErrInvalidAgent, maxParallel)
	}
	for _, a := range agents {
		if err := a.valid(); err != nil {
			return 0, fmt.Errorf("register runner %q: agent %s/%s: %w", name, a.Adapter, a.Model, err)
		}
	}
	var id int64
	// A register that began is finished on a context of its own, like a
	// heartbeat: a request that gives up mid-call must not roll the
	// advertisement back while its statements still run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeDeadline)
	defer cancel()
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		nowStr := fmtTime(e.cfg.Now())
		startedStr := ""
		if !startedAt.IsZero() {
			startedStr = fmtTime(startedAt)
		}
		err := tx.QueryRow("SELECT id FROM runners WHERE name = ?", name).Scan(&id)
		switch {
		case err == nil:
			if _, err := tx.Exec("UPDATE runners SET max_parallel = ? WHERE id = ?", maxParallel, id); err != nil {
				return fmt.Errorf("touch: %w", err)
			}
			if startedStr != "" {
				if _, err := tx.Exec("UPDATE runners SET started_at = ? WHERE id = ? AND started_at <> ?", startedStr, id, startedStr); err != nil {
					return fmt.Errorf("record start: %w", err)
				}
			}
			// The runner is back: its seen time moves and the agents
			// broken only for being stale come back (§8 "Health").
			if err := e.runnerSeenTx(tx, id, e.cfg.Now()); err != nil {
				return err
			}
		case errors.Is(err, sql.ErrNoRows):
			res, err := tx.Exec("INSERT INTO runners (name, max_parallel, created_at, last_seen_at, started_at) VALUES (?, ?, ?, ?, ?)",
				name, maxParallel, nowStr, nowStr, startedStr)
			if err != nil {
				return fmt.Errorf("insert: %w", err)
			}
			v, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("insert: last id: %w", err)
			}
			id = v
		default:
			return fmt.Errorf("read: %w", err)
		}
		if err := advertiseTx(tx, id, agents, nowStr); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("register runner %q: %w", name, err)
	}
	// The runner is back, so any drain it reported is over: the mark
	// lives only while the runner it names is on its way out.
	e.clearDraining(id)
	return id, nil
}

// MarkDraining records that the runner has a restart drain pending
// (§5.1): it takes no new lease and exits once its running jobs
// finish. The mark is the engine's memory of a runner's own report —
// not store state — because it can only be true while that engine
// lives: a restarted engine meets either the draining runner's next
// report or its replacement's register, and the drain itself needs no
// engine at all.
func (e *Engine) MarkDraining(ctx context.Context, runner int64) error {
	if runner <= 0 {
		return fmt.Errorf("mark the runner draining: no such runner")
	}
	e.drainMu.Lock()
	defer e.drainMu.Unlock()
	if e.draining == nil {
		e.draining = map[int64]bool{}
	}
	e.draining[runner] = true
	return nil
}

// clearDraining ends a runner's drain mark; its register is one way,
// the engine's death the other.
func (e *Engine) clearDraining(runner int64) {
	e.drainMu.Lock()
	defer e.drainMu.Unlock()
	delete(e.draining, runner)
}

// runnerDraining reports whether any runner has a restart drain
// pending, for the status's restarting line.
func (e *Engine) runnerDraining() bool {
	e.drainMu.Lock()
	defer e.drainMu.Unlock()
	return len(e.draining) > 0
}

// advertiseTx replaces the runner's agents with adverts, keyed by
// (runner, adapter, model): the ones kept are updated in place, the new
// ones inserted, the ones no longer named deleted by id (§5.1: a
// re-register replaces the runner's set).
func advertiseTx(tx *sql.Tx, runner int64, adverts []AgentAdvert, nowStr string) error {
	type key struct{ adapter, model string }
	seen := make(map[key]bool, len(adverts))
	for _, a := range adverts {
		k := key{a.Adapter, a.Model}
		if seen[k] {
			return fmt.Errorf("%w: agent %s/%s advertised twice", ErrInvalidAgent, a.Adapter, a.Model)
		}
		seen[k] = true
		classes, err := a.classesJSON()
		if err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE agents SET harness = ?, family = ?, account = ?, classes = ?, effort = ?, tools = ?, updated_at = ?
			WHERE runner_id = ? AND adapter = ? AND model = ?`,
			a.Harness, a.Family, a.Account, classes, a.Effort, a.Tools, nowStr, runner, a.Adapter, a.Model)
		if err != nil {
			return fmt.Errorf("update agent %s/%s: %w", a.Adapter, a.Model, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("update agent %s/%s: rows: %w", a.Adapter, a.Model, err)
		}
		if n > 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, effort, tools, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runner, a.Adapter, a.Harness, a.Model, a.Family, a.Account, classes, a.Effort, a.Tools, nowStr, nowStr); err != nil {
			return fmt.Errorf("insert agent %s/%s: %w", a.Adapter, a.Model, err)
		}
	}
	rows, err := tx.Query("SELECT id, adapter, model FROM agents WHERE runner_id = ?", runner)
	if err != nil {
		return fmt.Errorf("list the runner's agents: %w", err)
	}
	type held struct {
		id int64
		key
	}
	var heldAll []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.adapter, &h.model); err != nil {
			rows.Close()
			return fmt.Errorf("list the runner's agents: %w", err)
		}
		heldAll = append(heldAll, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list the runner's agents: %w", err)
	}
	rows.Close()
	for _, h := range heldAll {
		if seen[h.key] {
			continue
		}
		if _, err := tx.Exec("DELETE FROM agents WHERE id = ?", h.id); err != nil {
			return fmt.Errorf("drop agent %s/%s: %w", h.adapter, h.model, err)
		}
	}
	return nil
}

// RunnerID reads the id of the runner named name; 0 when no such runner
// has registered.
func (e *Engine) RunnerID(ctx context.Context, name string) (int64, error) {
	var id int64
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow("SELECT id FROM runners WHERE name = ?", name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // id stays 0: no such runner
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("read runner %q: %w", name, err)
	}
	return id, nil
}
