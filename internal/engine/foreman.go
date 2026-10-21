package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SubmitForemanTurn queues one headless foreman turn for the project
// (§4.2 item 2): a job of kind foreman on the key foreman:<project>,
// in session mode — the key's long-lived session (§4.1) — under the
// harness the user chose for the project, with a prompt composed from
// the versioned foreman instructions and wake template plus the events
// verbatim (§1.4). The one-live-job-per-key rule does the coalescing
// (§12.1, §14): a wake while a turn runs sets the live job's follow-up
// flag and answers pending.
func (e *Engine) SubmitForemanTurn(ctx context.Context, project int64, events []string) (SubmitResult, error) {
	var res SubmitResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.submitForemanTurnTx(tx, project, ForemanInbox{Events: events}, now)
		if err != nil {
			return fmt.Errorf("submit foreman turn for project %d: %w", project, err)
		}
		res = r
		return nil
	})
	return res, err
}

func (e *Engine) submitForemanTurnTx(tx *sql.Tx, project int64, inbox ForemanInbox, now time.Time) (SubmitResult, error) {
	if err := activeProjectTx(tx, project); err != nil {
		return SubmitResult{}, err
	}
	var harness, model string
	err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", project).
		Scan(&harness, &model)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SubmitResult{}, fmt.Errorf("no such project %d", project)
	case err != nil:
		return SubmitResult{}, fmt.Errorf("read the foreman of project %d: %w", project, err)
	}
	if harness == "" || model == "" {
		// The foreman is the user's own choice of harness and model
		// (§4.1); a project that never named one has nobody to wake.
		// `brigada project foreman` is the command that records it.
		return SubmitResult{}, fmt.Errorf("the project has no foreman harness; set it with `brigada project foreman`")
	}
	// A session grown past the project's threshold starts over before
	// this turn (§4.2); the turn's prompt then carries the digest.
	if _, err := e.rotateForemanSessionTx(tx, project, now); err != nil {
		return SubmitResult{}, err
	}
	prompt, err := foremanPromptTx(tx, project, inbox, now)
	if err != nil {
		return SubmitResult{}, err
	}
	// The harness and model are the job's placement constraint (§8): only
	// an agent offering exactly that pair may take the turn, so it runs on
	// the model the user chose (foremanPairTx).
	return e.submitTx(tx, SubmitParams{
		Key:       fmt.Sprintf("foreman:%d", project),
		ProjectID: project,
		Kind:      KindForeman,
		Mode:      ModeSession,
		Prompt:    prompt,
		Harness:   harness,
		Model:     model,
	}, now)
}
