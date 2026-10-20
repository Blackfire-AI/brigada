package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// userMessageDedupeWindow is how far back a user message counts as a
// repeat of an identical one (§12.1): a double Enter or a shell retry,
// not a deliberate second ask.
const userMessageDedupeWindow = time.Minute

// UserMessageLimit bounds one user message (§4.2 item 3): a request for
// the foreman, a few paragraphs at most, never a file pasted whole.
const UserMessageLimit = 16 << 10

// ErrInvalidMessage marks a user message the engine will not take: an
// empty one, or one past the size bound.
var ErrInvalidMessage = errors.New("invalid user message")

// ErrNoProject marks a user message that names no project the engine
// knows: a repo URL no project has, or none named while the engine
// holds no project, or several.
var ErrNoProject = errors.New("no such project")

// AskResult is what a user message did (§4.2 item 3): the project it
// went to, the event that holds it, and the wake's outcome — woken, or
// why it waits.
type AskResult struct {
	ProjectID int64       `json:"project_id"`
	EventID   int64       `json:"event_id"`
	Outcome   WakeOutcome `json:"outcome"`
	// HoldID names the attach that holds the foreman when the message
	// waits for it (§4.2 item 3); 0 otherwise.
	HoldID int64 `json:"hold_id,omitempty"`
	// LimitedUntil is the reset time (RFC3339) of a limited foreman
	// (§4.4); empty when the outcome is limited and the time unknown, and
	// for every other outcome.
	LimitedUntil string `json:"limited_until,omitempty"`
}

// AskForeman puts the user's message in the project's foreman inbox and
// evaluates a wake for it (§4.2 item 3), in one transaction: the event
// that holds the words, its inbox row, the history's message with
// author user (§9), and the wake. The project is the one whose repo URL
// is repoURL, or, with none named, the engine's only project; source is
// the label of the client token that sent it (§12.2), carried into the
// foreman's prompt. The message reaches a foreman session apart from
// the judgment events, as the user's request (§1.4); who may send one
// is the API's to enforce — client and admin tokens only (§12.1).
func (e *Engine) AskForeman(ctx context.Context, repoURL, text, source string) (AskResult, error) {
	if strings.TrimSpace(text) == "" {
		return AskResult{}, fmt.Errorf("%w: the message is empty", ErrInvalidMessage)
	}
	if len(text) > UserMessageLimit {
		return AskResult{}, fmt.Errorf("%w: the message is %d bytes, over the %d-byte bound",
			ErrInvalidMessage, len(text), UserMessageLimit)
	}
	var res AskResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		project, err := askProjectTx(tx, repoURL)
		if err != nil {
			return err
		}
		// The same words from the same token to the same project moments
		// ago are a repeat (§12.1): the first message answers for both.
		var first int64
		err = tx.QueryRow(`SELECT id FROM events WHERE kind = ? AND project_id = ? AND created_at >= ?
			AND json_extract(payload, '$.text') = ? AND COALESCE(json_extract(payload, '$.source'), '') = ?
			ORDER BY id LIMIT 1`,
			evUserMessage, project, fmtTime(now.Add(-userMessageDedupeWindow)), text, source).Scan(&first)
		switch {
		case err == nil:
			res = AskResult{ProjectID: project, EventID: first, Outcome: WakeDuplicate}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("look for a repeat of the user message: %w", err)
		}
		eventID, err := appendEventTx(tx, evUserMessage, project, 0, 0, eventPayload{Text: text, Source: source}, now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO foreman_inbox (event_id, project_id, state) VALUES (?, ?, ?)`,
			eventID, project, inboxPending); err != nil {
			return fmt.Errorf("inbox the user message: %w", err)
		}
		if err := appendMessageTx(tx, project, eventID, msgKindUserMessage, authorUser, text, now); err != nil {
			return err
		}
		outcome, err := e.wakeOutcomeTx(tx, project, now)
		if err != nil {
			return err
		}
		res = AskResult{ProjectID: project, EventID: eventID, Outcome: outcome}
		if outcome == WakeLimited {
			if _, res.LimitedUntil, err = foremanLimitedTx(tx, project, now); err != nil {
				return err
			}
		}
		if outcome == WakeAttached {
			if err := tx.QueryRow(`SELECT id FROM attach_holds WHERE project_id = ? AND state = 'live' ORDER BY id DESC LIMIT 1`,
				project).Scan(&res.HoldID); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("read the attach hold of project %d: %w", project, err)
			}
		}
		return nil
	})
	if err != nil {
		return AskResult{}, err
	}
	return res, nil
}

// askProjectTx resolves the project a user message goes to: the one
// with repoURL, or, with none named, the only project there is.
func askProjectTx(tx *sql.Tx, repoURL string) (int64, error) {
	var project int64
	if repoURL != "" {
		project, err := LookupProjectTx(tx, repoURL)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: no project for repo %s", ErrNoProject, RedactRepoURL(repoURL))
		}
		if err != nil {
			return 0, fmt.Errorf("resolve the repo %s: %w", RedactRepoURL(repoURL), err)
		}
		return project, activeProjectTx(tx, project)
	}
	var n int
	if err := tx.QueryRow("SELECT COUNT(*), COALESCE(MIN(id), 0) FROM projects WHERE archived_at IS NULL").Scan(&n, &project); err != nil {
		return 0, fmt.Errorf("count the projects: %w", err)
	}
	switch n {
	case 1:
		return project, activeProjectTx(tx, project)
	case 0:
		// Keep the removal guidance for the implicit single-project call.
		var total int
		if err := tx.QueryRow("SELECT COUNT(*), COALESCE(MIN(id), 0) FROM projects").Scan(&total, &project); err != nil {
			return 0, err
		}
		if total == 1 {
			return project, activeProjectTx(tx, project)
		}
		return 0, fmt.Errorf("%w: the engine has no project yet; set one up with `brigada project foreman`", ErrNoProject)
	default:
		return 0, fmt.Errorf("%w: the engine has %d projects; name one by its repo URL", ErrNoProject, n)
	}
}
