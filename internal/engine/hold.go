package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Blackfire-AI/brigada/internal/foreman"
)

// holdLeaseTTL is the interactive hold's liveness lease (§4.6): the
// bridge heartbeats inside it, and a missed lease ends the hold. Short on
// purpose — it only shows that the bridge process and its transport are
// alive — and never longer than the activity bound.
const holdLeaseTTL = 45 * time.Second

// The reasons an interactive hold ends (§4.6), as the hold_ended event
// and the row keep them.
const (
	HoldEndYield      = "yield"      // the session called hold_yield
	HoldEndIdle       = "idle"       // no activity for hold_idle_minutes
	HoldEndMax        = "max"        // hold_max_minutes passed, activity or not
	HoldEndLease      = "lease"      // the bridge missed its liveness lease
	HoldEndForced     = "forced"     // the user released it (`brigada hold release`)
	HoldEndReacquired = "reacquired" // the same bridge instance acquired again
)

// ErrStaleHold marks a mutation the engine rejected whole because its
// hold credential was revoked or its fence is old (§4.6): nothing was
// applied, and the API does not record the rejection for idempotency.
var ErrStaleHold = errors.New("stale hold")

// StaleHoldError says why a call was stale and, when a headless turn has
// the slot meanwhile, which (§4.6 "foreman_busy"): the session must
// acquire again, read the digest and decide afresh.
type StaleHoldError struct {
	Reason string // why the hold ended, or "the fence is old"
	Slot   Slot   // the slot now: a live headless turn is named in it
}

func (e *StaleHoldError) Error() string {
	return fmt.Sprintf("stale hold: %s; call hold_acquire, read the digest and decide again", e.Reason)
}

func (e *StaleHoldError) Is(target error) bool { return target == ErrStaleHold }

// Slot states (§4.6): the key's live slot has one holder at a time.
type SlotState string

const (
	SlotFree        SlotState = "free"
	SlotInteractive SlotState = "interactive hold"
	SlotTurn        SlotState = "headless turn"
	SlotAttach      SlotState = "attach"
)

// Slot is the foreman slot of one project and who holds it (§4.6): the
// holder's id (hold id, or job id for a turn), its label, since when and,
// for a turn, the events it carries.
type Slot struct {
	ProjectID int64     `json:"project_id"`
	State     SlotState `json:"state"`
	HolderID  int64     `json:"holder_id,omitempty"`
	Label     string    `json:"label,omitempty"`
	Since     string    `json:"since,omitempty"`
	Events    []string  `json:"events,omitempty"`
}

// HoldDigest is what acquire hands back (§4.6): the plans and their
// tasks, and the pending events and messages, so a returning session
// decides from the engine's state, not from memory of a chat.
type HoldDigest struct {
	Plans        []StatusPlan          `json:"plans"`
	Events       []string              `json:"events"`
	UserMessages []foreman.UserMessage `json:"user_messages"`
}

// HoldAcquire is a hold request: the project, the bridge instance asking
// (a process, nothing more, §4.6), the label to show, and the session
// hint the bridge saw — display only, never authority.
type HoldAcquire struct {
	Project        int64
	BridgeInstance string
	Label          string
	SessionHint    string
}

// HoldResult is acquire's answer: created with the credential (its one
// appearance), the fence and the digest; or busy naming the holder.
type HoldResult struct {
	Answer       Answer
	HoldID       int64
	Credential   string
	Fence        int64
	LeaseExpires time.Time
	IdleExpires  time.Time
	MaxAt        time.Time
	Digest       *HoldDigest
	Busy         *Slot // the holder that refused, on busy
}

// slotTx reads who holds the project's slot (§4.6). The caller has swept,
// so a hold or lease that lapsed is gone.
func slotTx(tx *sql.Tx, project int64, now time.Time) (Slot, error) {
	slot := Slot{ProjectID: project, State: SlotFree}
	var id int64
	var label, since string
	err := tx.QueryRow(`SELECT id, label, created_at FROM interactive_holds WHERE project_id = ? AND state = 'live'`,
		project).Scan(&id, &label, &since)
	switch {
	case err == nil:
		return Slot{ProjectID: project, State: SlotInteractive, HolderID: id, Label: label, Since: since}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return slot, fmt.Errorf("read the interactive hold of project %d: %w", project, err)
	}
	err = tx.QueryRow(`SELECT id, created_at FROM attach_holds WHERE project_id = ? AND state = 'live' AND expires_at > ?`,
		project, fmtTime(now)).Scan(&id, &since)
	switch {
	case err == nil:
		return Slot{ProjectID: project, State: SlotAttach, HolderID: id, Label: "attached chat", Since: since}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return slot, fmt.Errorf("read the attach hold of project %d: %w", project, err)
	}
	live, err := liveJobTx(tx, fmt.Sprintf("foreman:%d", project))
	if err != nil {
		return slot, err
	}
	if live != nil {
		since := live.FirstLeased
		if since == "" {
			since = live.QueuedAt
		}
		carried, err := inboxRowsTx(tx, project, inboxDelivered, live.ID)
		if err != nil {
			return slot, err
		}
		events := append([]string{}, carried.Events...)
		for _, m := range carried.UserMessages {
			events = append(events, "user message from "+m.Source)
		}
		return Slot{ProjectID: project, State: SlotTurn, HolderID: live.ID, Label: "foreman turn",
			Since: since, Events: events}, nil
	}
	return slot, nil
}

// ForemanSlot reads the project's slot: what `brigada status` shows.
func (e *Engine) ForemanSlot(ctx context.Context, project int64) (Slot, error) {
	var slot Slot
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var err error
		slot, err = slotTx(tx, project, now)
		return err
	})
	return slot, err
}

// AcquireHold takes the project's slot for the user's own session
// (§4.6). It is refused while a headless turn, an attach or another
// bridge instance's hold has the slot, and the refusal names the holder,
// its label and since when. The same bridge instance acquiring again
// replaces its own hold (the old credential dies, a new fence is
// minted). On success it mints the mode A foreman credential — shown
// once, only its hash stored — and a fence bumped past every earlier
// one, and returns the state digest. The pending events and messages are
// read, not consumed: the `events` tool consumes.
func (e *Engine) AcquireHold(ctx context.Context, req HoldAcquire) (HoldResult, error) {
	if req.BridgeInstance == "" {
		return HoldResult{}, fmt.Errorf("%w: a bridge instance id is required", ErrInvalidMessage)
	}
	var res HoldResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var one int
		if err := tx.QueryRow("SELECT 1 FROM projects WHERE id = ?", req.Project).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no such project %d", ErrNoProject, req.Project)
			}
			return fmt.Errorf("read project %d: %w", req.Project, err)
		}
		slot, err := slotTx(tx, req.Project, now)
		if err != nil {
			return err
		}
		if slot.State != SlotFree {
			if slot.State == SlotInteractive {
				var bridge string
				if err := tx.QueryRow("SELECT bridge_instance FROM interactive_holds WHERE id = ?", slot.HolderID).
					Scan(&bridge); err != nil {
					return fmt.Errorf("read interactive hold %d: %w", slot.HolderID, err)
				}
				if bridge == req.BridgeInstance {
					// The same process asking again: it lost its credential
					// (or never kept it). Its old hold ends quietly — a wake
					// now would find the slot taken again a moment later.
					if err := e.endHoldTx(tx, slot.HolderID, req.Project, HoldEndReacquired, false, now); err != nil {
						return err
					}
					slot = Slot{State: SlotFree}
				}
			}
		}
		if slot.State != SlotFree {
			res = HoldResult{Answer: AnsBusy, Busy: &slot}
			return nil
		}
		idle, maxHold, err := holdBoundsTx(tx, req.Project)
		if err != nil {
			return err
		}
		secret, err := e.cfg.NewLeaseToken()
		if err != nil {
			return fmt.Errorf("mint the hold credential of project %d: %w", req.Project, err)
		}
		fence, err := bumpHoldFenceTx(tx, req.Project)
		if err != nil {
			return err
		}
		res = HoldResult{
			Answer:       AnsCreated,
			Credential:   secret,
			Fence:        fence,
			LeaseExpires: now.Add(holdLeaseTTL),
			IdleExpires:  now.Add(idle),
			MaxAt:        now.Add(maxHold),
		}
		r, err := tx.Exec(`INSERT INTO interactive_holds
			(project_id, bridge_instance, label, credential_hash, fence, state,
			 lease_expires_at, activity_at, idle_expires_at, max_at, session_hint, created_at)
			VALUES (?, ?, ?, ?, ?, 'live', ?, ?, ?, ?, ?, ?)`,
			req.Project, req.BridgeInstance, req.Label, hashToken(secret), fence,
			fmtTime(res.LeaseExpires), fmtTime(now), fmtTime(res.IdleExpires), fmtTime(res.MaxAt),
			nullEmpty(req.SessionHint), fmtTime(now))
		if err != nil {
			return fmt.Errorf("create the interactive hold of project %d: %w", req.Project, err)
		}
		if res.HoldID, err = r.LastInsertId(); err != nil {
			return fmt.Errorf("create the interactive hold of project %d: last insert id: %w", req.Project, err)
		}
		if _, err := appendEventTx(tx, evHoldAcquired, req.Project, 0, 0,
			eventPayload{Hold: res.HoldID, Fence: fence, Source: req.Label}, now); err != nil {
			return err
		}
		plans, err := statusPlansTx(tx, req.Project, now)
		if err != nil {
			return err
		}
		inbox, err := pendingInboxTx(tx, req.Project)
		if err != nil {
			return err
		}
		res.Digest = &HoldDigest{Plans: plans, Events: inbox.Events, UserMessages: inbox.UserMessages}
		return nil
	})
	if err != nil {
		return HoldResult{}, fmt.Errorf("acquire a hold on project %d: %w", req.Project, err)
	}
	return res, nil
}

func nullEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// bumpHoldFenceTx moves the project's hold fence forward and returns it.
func bumpHoldFenceTx(tx *sql.Tx, project int64) (int64, error) {
	if _, err := tx.Exec("UPDATE projects SET hold_fence = hold_fence + 1 WHERE id = ?", project); err != nil {
		return 0, fmt.Errorf("bump the hold fence of project %d: %w", project, err)
	}
	var fence int64
	if err := tx.QueryRow("SELECT hold_fence FROM projects WHERE id = ?", project).Scan(&fence); err != nil {
		return 0, fmt.Errorf("read the hold fence of project %d: %w", project, err)
	}
	return fence, nil
}

// endHoldTx ends one live interactive hold (§4.6) in the order the design
// fixes: revoke the credential, bump the fence, THEN re-evaluate pending
// events and messages for a wake. By the time a headless turn can be
// submitted, the old mutation authority is dead. wake is false only for a
// hold the same bridge replaces at once.
func (e *Engine) endHoldTx(tx *sql.Tx, holdID, project int64, reason string, wake bool, now time.Time) error {
	// 1. Revoke: a credential authenticates only against a live hold.
	r, err := tx.Exec(`UPDATE interactive_holds SET state = 'ended', end_reason = ?, ended_at = ?
		WHERE id = ? AND state = 'live'`, reason, fmtTime(now), holdID)
	if err != nil {
		return fmt.Errorf("end interactive hold %d: %w", holdID, err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return nil // already over
	}
	// 2. Bump: any fence from before this moment is old.
	fence, err := bumpHoldFenceTx(tx, project)
	if err != nil {
		return err
	}
	if _, err := appendEventTx(tx, evHoldEnded, project, 0, 0,
		eventPayload{Hold: holdID, Fence: fence, Reason: reason}, now); err != nil {
		return err
	}
	// 3. Only now the wake.
	if !wake {
		return nil
	}
	return e.wakeForemanTx(tx, project, now)
}

// expireInteractiveHoldsTx ends the holds the clocks have run out on
// (§4.6): the absolute bound first, then inactivity, then a missed
// liveness lease. Each end revokes, bumps and re-evaluates the wake.
func (e *Engine) expireInteractiveHoldsTx(tx *sql.Tx, now time.Time) error {
	nowStr := fmtTime(now)
	rows, err := tx.Query(`SELECT id, project_id, lease_expires_at, idle_expires_at, max_at FROM interactive_holds
		WHERE state = 'live' AND (lease_expires_at <= ? OR idle_expires_at <= ? OR max_at <= ?)`,
		nowStr, nowStr, nowStr)
	if err != nil {
		return fmt.Errorf("sweep expired interactive holds: %w", err)
	}
	type lapsed struct {
		id, project        int64
		lease, idle, maxAt string
	}
	var all []lapsed
	for rows.Next() {
		var l lapsed
		if err := rows.Scan(&l.id, &l.project, &l.lease, &l.idle, &l.maxAt); err != nil {
			rows.Close()
			return fmt.Errorf("sweep expired interactive holds: %w", err)
		}
		all = append(all, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("sweep expired interactive holds: %w", err)
	}
	rows.Close()
	for _, l := range all {
		reason := HoldEndLease
		switch {
		case l.maxAt <= nowStr:
			reason = HoldEndMax
		case l.idle <= nowStr:
			reason = HoldEndIdle
		}
		if err := e.endHoldTx(tx, l.id, l.project, reason, true, now); err != nil {
			return err
		}
	}
	return nil
}

// checkHoldTx reports whether the hold is live and the fence is its own
// and the project's current one (§4.6). Anything else is stale, with the
// reason, and the slot as it stands.
func checkHoldTx(tx *sql.Tx, holdID, fence int64, now time.Time) error {
	var project, heldFence, current int64
	var state string
	var reason sql.NullString
	err := tx.QueryRow(`SELECT h.project_id, h.fence, h.state, h.end_reason, p.hold_fence
		FROM interactive_holds h JOIN projects p ON p.id = h.project_id WHERE h.id = ?`, holdID).
		Scan(&project, &heldFence, &state, &reason, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return &StaleHoldError{Reason: "the hold is unknown"}
	}
	if err != nil {
		return fmt.Errorf("read interactive hold %d: %w", holdID, err)
	}
	why := ""
	switch {
	case state != "live":
		why = "the hold ended (" + reason.String + ")"
	case fence != heldFence || fence != current:
		why = fmt.Sprintf("the fence %d is old (current %d)", fence, current)
	default:
		return nil
	}
	slot, err := slotTx(tx, project, now)
	if err != nil {
		return err
	}
	return &StaleHoldError{Reason: why, Slot: slot}
}

// CheckHold gates a foreman mutation by a mode A credential (§4.6): nil
// when the hold lives and the fence is current, a *StaleHoldError
// otherwise. It applies nothing.
func (e *Engine) CheckHold(ctx context.Context, holdID, fence int64) error {
	var stale error
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		stale = checkHoldTx(tx, holdID, fence, now)
		var s *StaleHoldError
		if errors.As(stale, &s) {
			return nil // the sweep's writes stand; the rejection is the answer
		}
		return stale
	})
	if err != nil {
		return err
	}
	return stale
}

// HoldCredentialScope resolves the secret of a mode A credential to its
// project, hold and whether the hold still lives (§12.1). A revoked
// credential still resolves — live is false — so the API answers it
// stale_hold, not unauthorized. ok is false for a secret nobody minted.
func (e *Engine) HoldCredentialScope(ctx context.Context, secret string) (project, hold int64, live, ok bool, err error) {
	err = e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := e.sweepTx(tx, e.cfg.Now()); err != nil {
			return err
		}
		var state string
		err := tx.QueryRow(`SELECT project_id, id, state FROM interactive_holds WHERE credential_hash = ?`,
			hashToken(secret)).Scan(&project, &hold, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the hold credential: %w", err)
		}
		ok, live = true, state == "live"
		return nil
	})
	if err != nil {
		return 0, 0, false, false, err
	}
	return project, hold, live, ok, nil
}

// HoldActivity records activity on a hold (§4.6): a call the session
// itself made that changed or consumed engine state. It moves the idle
// bound out and nothing else — never the liveness lease, never the
// absolute bound. A hold that is not live with this fence is untouched.
func (e *Engine) HoldActivity(ctx context.Context, holdID, fence int64) error {
	return e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		return e.touchHoldTx(tx, holdID, fence, now)
	})
}

func (e *Engine) touchHoldTx(tx *sql.Tx, holdID, fence int64, now time.Time) error {
	var project int64
	if err := tx.QueryRow(`SELECT project_id FROM interactive_holds WHERE id = ? AND state = 'live' AND fence = ?`,
		holdID, fence).Scan(&project); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read interactive hold %d: %w", holdID, err)
	}
	idle, _, err := holdBoundsTx(tx, project)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE interactive_holds SET activity_at = ?, idle_expires_at = ? WHERE id = ?`,
		fmtTime(now), fmtTime(now.Add(idle)), holdID); err != nil {
		return fmt.Errorf("record activity on interactive hold %d: %w", holdID, err)
	}
	return nil
}

// HoldHeartbeat renews the liveness lease (§4.6). It proves the bridge
// and its transport are alive and nothing more: it never reacquires —
// an ended hold or an old fence is stale — and the lease it grants never
// reaches past the activity bounds. It is not activity.
func (e *Engine) HoldHeartbeat(ctx context.Context, holdID, fence int64) (time.Time, error) {
	var lease time.Time
	var stale error
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		if stale = checkHoldTx(tx, holdID, fence, now); stale != nil {
			var s *StaleHoldError
			if errors.As(stale, &s) {
				return nil
			}
			return stale
		}
		var idle, maxAt string
		if err := tx.QueryRow("SELECT idle_expires_at, max_at FROM interactive_holds WHERE id = ?", holdID).
			Scan(&idle, &maxAt); err != nil {
			return fmt.Errorf("read interactive hold %d: %w", holdID, err)
		}
		lease = now.Add(holdLeaseTTL)
		for _, bound := range []string{idle, maxAt} {
			if t, err := parseTime(bound); err == nil && t.Before(lease) {
				lease = t
			}
		}
		if _, err := tx.Exec("UPDATE interactive_holds SET lease_expires_at = ? WHERE id = ?",
			fmtTime(lease), holdID); err != nil {
			return fmt.Errorf("renew interactive hold %d: %w", holdID, err)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return lease, stale
}

// YieldHold ends the hold at the end of the session's turn (§4.6),
// honored only for the live hold with its fence: revoke the credential,
// bump the fence, then re-evaluate pending events and messages for a
// wake. A stale yield — an old Stop hook, an old bridge — releases
// nothing.
func (e *Engine) YieldHold(ctx context.Context, holdID, fence int64) error {
	var stale error
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		if stale = checkHoldTx(tx, holdID, fence, now); stale != nil {
			var s *StaleHoldError
			if errors.As(stale, &s) {
				return nil
			}
			return stale
		}
		var project int64
		if err := tx.QueryRow("SELECT project_id FROM interactive_holds WHERE id = ?", holdID).Scan(&project); err != nil {
			return fmt.Errorf("read interactive hold %d: %w", holdID, err)
		}
		return e.endHoldTx(tx, holdID, project, HoldEndYield, true, now)
	})
	if err != nil {
		return err
	}
	return stale
}

// ReleaseHoldForced is the user's release of a stuck hold (§4.6): client
// authority, so no credential, but it goes through fencing — the hold's
// credential is revoked and the fence bumped before the wake. gone means
// no interactive hold was live.
func (e *Engine) ReleaseHoldForced(ctx context.Context, project int64) (Answer, error) {
	ans := AnsGone
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var id int64
		err := tx.QueryRow("SELECT id FROM interactive_holds WHERE project_id = ? AND state = 'live'", project).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the interactive hold of project %d: %w", project, err)
		}
		ans = AnsOK
		return e.endHoldTx(tx, id, project, HoldEndForced, true, now)
	})
	return ans, err
}

// HoldGuard binds a foreman mutation to the interactive hold and fence
// its caller authenticated with (§4.6, §12.1). Carried in the request's
// context, it is checked inside the very transaction that applies the
// mutation, so a hold revoked while the call was in flight — a forced
// release, an expiry, a yield — cannot be outrun: the call applies
// nothing and answers stale.
type HoldGuard struct {
	Hold, Fence int64
	mu          sync.Mutex
	stale       *StaleHoldError
}

// Stale is the rejection the guard recorded, if any.
func (g *HoldGuard) Stale() *StaleHoldError {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stale
}

type holdGuardKey struct{}

// WithHoldGuard returns ctx carrying the guard.
func WithHoldGuard(ctx context.Context, g *HoldGuard) context.Context {
	return context.WithValue(ctx, holdGuardKey{}, g)
}

func holdGuardOf(ctx context.Context) *HoldGuard {
	g, _ := ctx.Value(holdGuardKey{}).(*HoldGuard)
	return g
}

// updateGuarded is store.Update for a foreman mutation: with a hold guard
// in ctx it sweeps, then validates hold and fence in the same
// transaction before fn runs. A stale call commits only the sweep's own
// writes (the expiry it found), applies nothing of fn, and returns the
// *StaleHoldError.
func (e *Engine) updateGuarded(ctx context.Context, fn func(tx *sql.Tx) error) error {
	g := holdGuardOf(ctx)
	if g == nil {
		return e.store.Update(ctx, fn)
	}
	var stale *StaleHoldError
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		if err := checkHoldTx(tx, g.Hold, g.Fence, now); err != nil {
			if errors.As(err, &stale) {
				return nil
			}
			return err
		}
		return fn(tx)
	})
	if err != nil {
		return err
	}
	if stale != nil {
		g.mu.Lock()
		g.stale = stale
		g.mu.Unlock()
		return stale
	}
	return nil
}

// ErrSlotHeld marks a foreman turn the engine will not submit because the
// user's own session holds the slot (§4.6): a headless turn only ever
// starts from a free slot.
var ErrSlotHeld = errors.New("the foreman slot is held by the user's own session")
