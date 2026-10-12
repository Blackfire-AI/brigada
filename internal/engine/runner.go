package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Register upserts the runner's row (§5.1): registering again refreshes
// last_seen and keeps the id. The row's id is what every lease the runner
// takes references, so a runner must register before it can lease; the
// API resolves a runner token's name to it.
func (e *Engine) Register(ctx context.Context, name string) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("register runner: name is required")
	}
	var id int64
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		nowStr := fmtTime(e.cfg.Now())
		err := tx.QueryRow("SELECT id FROM runners WHERE name = ?", name).Scan(&id)
		switch {
		case err == nil:
			if _, err := tx.Exec("UPDATE runners SET last_seen_at = ? WHERE id = ?", nowStr, id); err != nil {
				return fmt.Errorf("touch: %w", err)
			}
			return nil
		case errors.Is(err, sql.ErrNoRows):
			res, err := tx.Exec("INSERT INTO runners (name, created_at, last_seen_at) VALUES (?, ?, ?)",
				name, nowStr, nowStr)
			if err != nil {
				return fmt.Errorf("insert: %w", err)
			}
			v, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("insert: last id: %w", err)
			}
			id = v
			return nil
		default:
			return fmt.Errorf("read: %w", err)
		}
	})
	if err != nil {
		return 0, fmt.Errorf("register runner %q: %w", name, err)
	}
	return id, nil
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
