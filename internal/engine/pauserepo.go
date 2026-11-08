package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PauseProject resolves the repo URL a pause or resume names (§8.3) to
// the project's id and its stored repo URL. An unknown URL is
// ErrNoProject, an archived project the archive refusal. There is no
// single-project fallback: a pause with no project names the engine, and
// the caller leaves the URL out for it.
func (e *Engine) PauseProject(ctx context.Context, repoURL string) (int64, string, error) {
	var (
		id   int64
		repo string
	)
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = LookupProjectTx(tx, repoURL); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no project for repo %s", ErrNoProject, RedactRepoURL(repoURL))
		} else if err != nil {
			return err
		}
		if err := activeProjectTx(tx, id); err != nil {
			return err
		}
		return tx.QueryRow("SELECT repo_url FROM projects WHERE id = ?", id).Scan(&repo)
	})
	if err != nil {
		return 0, "", err
	}
	return id, repo, nil
}

// LivePauseOrigin is the origin of the scope's live pause (project 0 is
// the engine's), empty when none stands: what a resume reports when the
// scope stays paused.
func (e *Engine) LivePauseOrigin(ctx context.Context, project int64) (string, error) {
	var origin string
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		live, err := livePauseTx(tx, project)
		if live != nil {
			origin = live.origin
		}
		return err
	})
	return origin, err
}
