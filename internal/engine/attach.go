package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// attachHoldTTL is how long an attach hold lives without a heartbeat
// (§4.2 item 1): lease-like, so a killed terminal loses the foreman after
// a bounded silence — long enough for a pause in the chat, short enough
// that judgment events do not wait on a dead client.
const attachHoldTTL = 90 * time.Second

// AttachResult is what Attach hands the caller (§4.2 item 1): the busy
// answer carries what made it busy — a live turn's id, or a live hold's —
// while a created hold carries everything the client needs to run its
// interactive session: the hold's id, the hold-scoped foreman token (the
// secret's one appearance, as in the lease grant), the foreman's harness
// and model, the key's current harness session id to resume — empty when
// the key never ran — and the opening prompt, the foreman instructions
// plus the wake template with the project's pending events, the same
// composition a headless turn gets.
type AttachResult struct {
	Answer Answer
	HoldID int64 // the created hold; a busy second attach names the live hold
	JobID  int64 // a busy answer names the live foreman turn, when that made it busy
	// ForemanToken is the hold-scoped foreman token (§12.1): it works
	// while the hold lives and dies with the hold. Empty on busy.
	ForemanToken  string
	RepoURL       string
	DefaultBranch string
	Harness       string
	Model         string
	Effort        string // the foreman's reasoning-effort level; '' = unset
	SessionID     string // the key's current harness session id; '' = none
	Prompt        string // the opening prompt
	ExpiresAt     time.Time
}

// Attach takes the foreman's key for the user's interactive session with
// it (§4.2 item 1). A second live attach is refused busy, and a live
// foreman turn makes it busy too — unless force, which cancels the turn
// through the cancel path: its delivered events return to pending without
// counting an attempt, and the attach's opening prompt takes them, so
// cancelling a turn to attach never drops a task worker's question.
// Otherwise the engine creates a hold, mints its foreman token, and puts
// the project's pending events — delivered to the hold — into the
// opening prompt. While a hold lives, wakeForemanTx submits nothing:
// events wait for the attached session.
func (e *Engine) Attach(ctx context.Context, project int64, force bool) (AttachResult, error) {
	var res AttachResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.attachTx(tx, project, force, now)
		if err != nil {
			return fmt.Errorf("attach to project %d: %w", project, err)
		}
		res = r
		return nil
	})
	return res, err
}

func (e *Engine) attachTx(tx *sql.Tx, project int64, force bool, now time.Time) (AttachResult, error) {
	if err := activeProjectTx(tx, project); err != nil {
		return AttachResult{}, err
	}
	var harness, model, effort, repo, branch string
	err := tx.QueryRow("SELECT foreman_harness, foreman_model, foreman_effort, COALESCE(NULLIF(transport_url, ''), repo_url), default_branch FROM projects WHERE id = ?", project).
		Scan(&harness, &model, &effort, &repo, &branch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AttachResult{}, fmt.Errorf("no such project %d", project)
	case err != nil:
		return AttachResult{}, fmt.Errorf("read the foreman of project %d: %w", project, err)
	}
	if harness == "" || model == "" {
		// Nobody to attach to: the foreman is the user's own choice
		// (§4.1), and a project that never named one has nobody to run.
		return AttachResult{}, fmt.Errorf("the project has no foreman harness; set it with `brigada project foreman`")
	}
	// The second-attach rule (§4.2 item 1): one live hold at a time, so
	// two terminals never resume one session at once.
	var liveHold int64
	err = tx.QueryRow(`SELECT id FROM attach_holds WHERE project_id = ? AND state = 'live' AND expires_at > ?`,
		project, fmtTime(now)).Scan(&liveHold)
	switch {
	case err == nil:
		return AttachResult{Answer: AnsBusy, HoldID: liveHold}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return AttachResult{}, fmt.Errorf("read the attach holds of project %d: %w", project, err)
	}
	key := fmt.Sprintf("foreman:%d", project)
	live, err := liveJobTx(tx, key)
	if err != nil {
		return AttachResult{}, err
	}
	if live != nil {
		if !force {
			// The client says "foreman is busy" and waits on this job.
			return AttachResult{Answer: AnsBusy, JobID: live.ID}, nil
		}
		// Force: the user insists, so the turn stops. The cancelled rule
		// already on main returns its events to pending, uncounted, and
		// wakes nothing at once — the opening prompt below takes them.
		if _, err := e.cancelTx(tx, live, "attach", now); err != nil {
			return AttachResult{}, err
		}
	}
	// A session grown past the project's threshold starts over before the
	// chat (§4.2), ahead of the session id read below, so the chat never
	// resumes the old one; the opening prompt then carries the digest.
	if _, err := e.rotateForemanSessionTx(tx, project, now); err != nil {
		return AttachResult{}, err
	}
	res := AttachResult{
		Answer:        AnsCreated,
		RepoURL:       repo,
		DefaultBranch: branch,
		Harness:       harness,
		Model:         model,
		Effort:        effort,
		ExpiresAt:     now.Add(attachHoldTTL),
	}
	resHold, err := tx.Exec(`INSERT INTO attach_holds (project_id, state, expires_at, created_at) VALUES (?, 'live', ?, ?)`,
		project, fmtTime(res.ExpiresAt), fmtTime(now))
	if err != nil {
		return AttachResult{}, fmt.Errorf("create the attach hold of project %d: %w", project, err)
	}
	if res.HoldID, err = resHold.LastInsertId(); err != nil {
		return AttachResult{}, fmt.Errorf("create the attach hold of project %d: last insert id: %w", project, err)
	}
	secret, err := e.cfg.NewLeaseToken()
	if err != nil {
		return AttachResult{}, fmt.Errorf("mint the hold token of hold %d: %w", res.HoldID, err)
	}
	// The token dies with the hold, and only the client's heartbeat moves
	// the hold: a tool call does not extend it (§4.2 item 1) — the
	// harness's MCP work is no sign the terminal is still there — so the
	// attached client beats while its foreman works.
	if _, err := tx.Exec(`INSERT INTO hold_tokens (token_hash, project_id, hold_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		hashToken(secret), project, res.HoldID, fmtTime(res.ExpiresAt), fmtTime(now)); err != nil {
		return AttachResult{}, fmt.Errorf("store the hold token of hold %d: %w", res.HoldID, err)
	}
	res.ForemanToken = secret
	// What the key's last run ended on, if it left a session id behind
	// (§4.2): the empty string resumes nothing and the harness starts
	// fresh, as a lease grant's resume field reads it.
	var sid sql.NullString
	if err := tx.QueryRow("SELECT harness_session_id FROM sessions WHERE key = ?", key).Scan(&sid); err == nil {
		res.SessionID = sid.String
	} else if !errors.Is(err, sql.ErrNoRows) {
		return AttachResult{}, fmt.Errorf("read the session of %s: %w", key, err)
	}
	// The opening prompt is the composition a headless turn gets (§4.2
	// item 1): the instructions, then the wake template with the pending
	// events verbatim. Marking them delivered to the hold is the same
	// move the wake makes for a turn, with the hold in job_id's place —
	// and job_id cleared: a forced attach takes rows a cancelled turn
	// carried, and the stale job_id would send the recovery's heal after
	// them (§13) while the hold still lives.
	inbox, err := pendingInboxTx(tx, project)
	if err != nil {
		return AttachResult{}, err
	}
	if res.Prompt, err = foremanPromptTx(tx, project, inbox, now); err != nil {
		return AttachResult{}, err
	}
	if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ?, hold_id = ?, job_id = NULL WHERE project_id = ? AND state = ?`,
		inboxDelivered, res.HoldID, project, inboxPending); err != nil {
		return AttachResult{}, fmt.Errorf("deliver the inbox of project %d to hold %d: %w", project, res.HoldID, err)
	}
	_, err = appendEventTx(tx, evForemanAttached, project, 0, 0,
		eventPayload{Hold: res.HoldID, Count: int64(len(inbox.Events) + len(inbox.UserMessages))}, now)
	return res, err
}

// AttachHeartbeat extends a live hold (§4.2 item 1): the hold's expiry and
// its foreman token's move out together, as a lease's and its token's do.
// A hold that was released or expired answers gone.
func (e *Engine) AttachHeartbeat(ctx context.Context, holdID int64) (Answer, time.Time, error) {
	var ans Answer
	var expires time.Time
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var state string
		err := tx.QueryRow("SELECT state FROM attach_holds WHERE id = ?", holdID).Scan(&state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			ans = AnsGone
			return nil
		case err != nil:
			return fmt.Errorf("read attach hold %d: %w", holdID, err)
		case state != "live":
			ans = AnsGone
			return nil
		}
		expires = now.Add(attachHoldTTL)
		if _, err := tx.Exec("UPDATE attach_holds SET expires_at = ? WHERE id = ?",
			fmtTime(expires), holdID); err != nil {
			return fmt.Errorf("extend attach hold %d: %w", holdID, err)
		}
		if _, err := tx.Exec("UPDATE hold_tokens SET expires_at = ? WHERE hold_id = ?",
			fmtTime(expires), holdID); err != nil {
			return fmt.Errorf("extend the hold token of hold %d: %w", holdID, err)
		}
		ans = AnsOK
		return nil
	})
	return ans, expires, err
}

// ReleaseAttach ends an attached chat (§4.2 item 1). The session id the
// client ended on is written back to the key's sessions row — created if
// the key never had one — because a resume forks session ids, and the
// next wake must resume the id the chat actually reached. The rows
// delivered to the hold are consumed: the attached foreman read them, or
// the chat would not end on them. The hold's token is revoked, and the
// wake is re-evaluated, for the events that arrived while the hold lived
// and were never read. A release that arrives after the hold lapsed —
// the laptop slept through the expiry — still writes the session id (the
// chat forked it whatever the engine did about the hold) and answers
// expired; the expiry has already returned the rows, revoked the token
// and re-evaluated the wake, and no detach is logged for a hold that was
// not live to detach.
func (e *Engine) ReleaseAttach(ctx context.Context, holdID int64, sessionID string) (Answer, error) {
	var ans Answer
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		var state string
		err := tx.QueryRow("SELECT project_id, state FROM attach_holds WHERE id = ?", holdID).
			Scan(&project, &state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			ans = AnsGone
			return nil
		case err != nil:
			return fmt.Errorf("read attach hold %d: %w", holdID, err)
		case state == "expired":
			// The late release's one duty: the write-back. The id the chat
			// ended on outlives the hold, because the next wake resumes
			// the key's session and the chat is what moved it.
			if sessionID != "" {
				if err := upsertSessionTx(tx, fmt.Sprintf("foreman:%d", project), 0, sessionID, now); err != nil {
					return err
				}
			}
			ans = AnsExpired
			return nil
		case state != "live":
			ans = AnsGone
			return nil
		}
		if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ? WHERE hold_id = ? AND state = ?`,
			inboxConsumed, holdID, inboxDelivered); err != nil {
			return fmt.Errorf("consume the inbox delivered to hold %d: %w", holdID, err)
		}
		// The write-back (§4.2): resume forks session ids, so the key's
		// row must carry the id the attached chat ended on. The runner
		// is no engine's — an attached chat runs in the user's own
		// terminal — so the row holds the session id alone.
		if err := upsertSessionTx(tx, fmt.Sprintf("foreman:%d", project), 0, sessionID, now); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM hold_tokens WHERE hold_id = ?", holdID); err != nil {
			return fmt.Errorf("revoke the hold token of hold %d: %w", holdID, err)
		}
		if _, err := tx.Exec("UPDATE attach_holds SET state = 'released' WHERE id = ?", holdID); err != nil {
			return fmt.Errorf("release attach hold %d: %w", holdID, err)
		}
		if _, err := appendEventTx(tx, evForemanDetached, project, 0, 0, eventPayload{Hold: holdID}, now); err != nil {
			return err
		}
		ans = AnsOK
		// Events that arrived while the hold lived and were not read
		// through `events` get their wake now (§4.2 item 1).
		return e.wakeForemanTx(tx, project, now)
	})
	return ans, err
}

// ForemanEvents reads the project's pending inbox (§4.2): the one check
// the `events` tool's long-poll re-asks — the judgment events render
// exactly as the wake prompt renders them, the user's messages (§4.2
// item 3) apart — and reading them here is consuming them (§4.2: events
// are consumed when the attached foreman reads them through `events`).
// An empty inbox says "no new events"; the API's long-poll loops on it,
// so a headless turn's token may call it too.
func (e *Engine) ForemanEvents(ctx context.Context, project int64) (ForemanInbox, error) {
	var inbox ForemanInbox
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		if err := activeProjectTx(tx, project); err != nil {
			return err
		}
		var err error
		inbox, err = pendingInboxTx(tx, project)
		if err != nil {
			return err
		}
		if inbox.Empty() {
			return nil
		}
		if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ? WHERE project_id = ? AND state = ?`,
			inboxConsumed, project, inboxPending); err != nil {
			return fmt.Errorf("consume the inbox of project %d: %w", project, err)
		}
		return nil
	})
	return inbox, err
}

// HoldTokenScope resolves the secret of an attach hold's foreman token to
// the project and hold it was minted for: what the API's authentication
// asks about a bearer secret the token file does not know, beside the
// lease tokens. ok is false for a secret nobody minted or one past its
// expiry — the expiry is checked here, at authentication time, so a token
// dies with its hold even before the sweep has expired the row.
func (e *Engine) HoldTokenScope(ctx context.Context, secret string) (project, hold int64, ok bool, err error) {
	err = e.store.View(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT project_id, hold_id FROM hold_tokens
			WHERE token_hash = ? AND expires_at > ?`, hashToken(secret), fmtTime(e.cfg.Now())).
			Scan(&project, &hold)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // unknown or expired: not an error, just not ours
		}
		if err != nil {
			return fmt.Errorf("read the hold token: %w", err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return 0, 0, false, err
	}
	return project, hold, ok, nil
}

// expireAttachHoldTx settles one hold the sweep found expired (§4.2 item
// 1): the rows its opening prompt delivered return to pending without
// counting an attempt — a dead terminal is not a failed answer — the
// token is revoked, and the wake is re-evaluated for what the hold never
// read. The hold itself is marked expired, kept for the record.
func (e *Engine) expireAttachHoldTx(tx *sql.Tx, holdID, project int64, now time.Time) error {
	if _, err := tx.Exec(`UPDATE foreman_inbox SET state = ? WHERE hold_id = ? AND state = ?`,
		inboxPending, holdID, inboxDelivered); err != nil {
		return fmt.Errorf("return the inbox delivered to hold %d: %w", holdID, err)
	}
	if _, err := tx.Exec("DELETE FROM hold_tokens WHERE hold_id = ?", holdID); err != nil {
		return fmt.Errorf("revoke the hold token of hold %d: %w", holdID, err)
	}
	if _, err := tx.Exec("UPDATE attach_holds SET state = 'expired' WHERE id = ?", holdID); err != nil {
		return fmt.Errorf("expire attach hold %d: %w", holdID, err)
	}
	if _, err := appendEventTx(tx, evForemanAttachExpired, project, 0, 0, eventPayload{Hold: holdID}, now); err != nil {
		return err
	}
	return e.wakeForemanTx(tx, project, now)
}
