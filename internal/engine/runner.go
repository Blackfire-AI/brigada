package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		nowStr := fmtTime(e.cfg.Now())
		err := tx.QueryRow("SELECT id FROM runners WHERE name = ?", name).Scan(&id)
		switch {
		case err == nil:
			if _, err := tx.Exec("UPDATE runners SET last_seen_at = ?, max_parallel = ? WHERE id = ?", nowStr, maxParallel, id); err != nil {
				return fmt.Errorf("touch: %w", err)
			}
		case errors.Is(err, sql.ErrNoRows):
			res, err := tx.Exec("INSERT INTO runners (name, max_parallel, created_at, last_seen_at) VALUES (?, ?, ?, ?)",
				name, maxParallel, nowStr, nowStr)
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
	return id, nil
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
		res, err := tx.Exec(`UPDATE agents SET harness = ?, family = ?, account = ?, classes = ?, updated_at = ?
			WHERE runner_id = ? AND adapter = ? AND model = ?`,
			a.Harness, a.Family, a.Account, classes, nowStr, runner, a.Adapter, a.Model)
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
		if _, err := tx.Exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runner, a.Adapter, a.Harness, a.Model, a.Family, a.Account, classes, nowStr, nowStr); err != nil {
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
