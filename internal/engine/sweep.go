package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SweepReport names the jobs the sweep dealt with, so a caller (and a
// test) can tell an expiry that opens a revive window from a failure.
type SweepReport struct {
	Expired       []int64 // leases that lapsed; back to queued, keeping their lease ids
	HardCapped    []int64 // live jobs failed past their kind's hard cap
	QueueTimedOut []int64 // queued jobs that waited past the queue TTL without a lease
}

// Sweep runs the engine's maintenance pass by hand. Every engine call
// runs it first; the exported form is for tests and for an idle engine
// keeping the store honest without leasing anything (§13). A configured
// retention (§9) rides along: the sweep prunes what aged out of its
// window, messages and artifacts, leaving the event log whole.
func (e *Engine) Sweep(ctx context.Context) (SweepReport, error) {
	var rep SweepReport
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		var err error
		rep, err = e.sweepTx(tx, e.cfg.Now())
		return err
	})
	if err != nil {
		return SweepReport{}, fmt.Errorf("sweep: %w", err)
	}
	if e.cfg.Retention > 0 {
		if _, err := e.Prune(ctx, e.cfg.Now().Add(-e.cfg.Retention)); err != nil {
			return rep, fmt.Errorf("sweep: %w", err)
		}
	}
	return rep, nil
}

// sweepTx is the maintenance pass, in five rounds:
//
//  1. an expired lease goes back to queued keeping its lease id, so its
//     holder can still revive or complete it — only a re-lease, which
//     stamps a fresh id, locks them out (§12.3); its queue-TTL clock
//     restarts at the requeue (§8.1);
//  2. a live job past its kind's hard cap from last_leased — the
//     current lease, so a job that lost a lease and was requeued runs
//     its next lease with the whole cap again — fails with
//     the cap named in the error (§5.6); a job with a task parks the task
//     at needs_foreman with a task_failed event naming the cap, and the
//     foreman is woken (needsForemanTx does both);
//  3. a queued job nobody leased within the queue TTL fails, so it
//     cannot wedge its key — unless a runner keeps finding it held by a
//     full cap layer, which restarts the clock (§8.1); a job with a task
//     parks the task at needs_foreman with a task_failed event naming the
//     TTL, and the foreman is woken, as in round 2;
//  4. an expired attach hold returns its delivered events to pending
//     uncounted, revokes its token, and re-evaluates the wake (§4.2
//     item 1);
//  5. the agents of a runner unseen past the stale window are marked
//     broken("runner stale") (§8 "Health");
//
// Leased wall time is banked wherever a lease ends: expiry at the
// expiry itself, a cap at the cap moment, never past them.
func (e *Engine) sweepTx(tx *sql.Tx, now time.Time) (SweepReport, error) {
	return e.sweepPolledTx(tx, now, 0)
}

// suspensionFactor is how many sweep intervals a gap between two sweep
// ticks must exceed to count as a suspension (§13): the wall clock
// jumped minutes or hours in one gap, so the host slept — "far larger
// than the tick interval", as one number.
const suspensionFactor = 4

// suspensionGap is the gap the sweep reads as a suspension (§13): a few
// sweep intervals. Tests drive the injected clock past it to play a host
// sleep, and short of it to play a wakeful engine's deadlines.
func suspensionGap(interval time.Duration) time.Duration {
	return suspensionFactor * interval
}

// sweepGap reads the wall-clock gap from the previous sweep tick (§13)
// without recording this one. The first tick has no gap to measure.
func (e *Engine) sweepGap(now time.Time) (gap time.Duration, suspended bool) {
	e.sweepMu.Lock()
	defer e.sweepMu.Unlock()
	if e.lastSweep.IsZero() {
		return 0, false
	}
	gap = now.Sub(e.lastSweep)
	return gap, gap > suspensionGap(e.cfg.SweepInterval)
}

// recordSweep records a sweep tick at now (§13): the next sweep measures
// its gap from here. It is called only once a sweep has done its work,
// so a sweep that fails and rolls back does not consume the gap — the
// retry still sees the pre-sleep jump and re-detects the suspension.
func (e *Engine) recordSweep(now time.Time) {
	e.sweepMu.Lock()
	e.lastSweep = now
	e.sweepMu.Unlock()
}

// restartDeadlinesTx restarts the wall-clock deadlines the sweep acts on
// (§13): after a suspension the engine's own wall clock jumped, so a
// deadline that lapsed during the gap means nothing about the runner or
// the client — they slept too. Each deadline moves to now plus its
// window (or past the gap), and no expiry, stale, attach-expiry, hard-cap
// or queue-TTL event is written for the gap. gap is how far the wall
// clock jumped between the sweep ticks, the best measure of the sleep.
func (e *Engine) restartDeadlinesTx(tx *sql.Tx, now time.Time, gap time.Duration) error {
	nowStr := fmtTime(now)
	leaseExpiry := fmtTime(now.Add(e.cfg.LeaseTTL))
	// Live leases: the runner slept with the engine, so its lease is
	// restarted, not requeued (§13). The lease-scoped foreman token moves
	// with the lease.
	if _, err := tx.Exec(`UPDATE jobs SET lease_expires_at = ?, updated_at = ?
		WHERE status IN ('leased', 'running') AND lease_expires_at IS NOT NULL`,
		leaseExpiry, nowStr); err != nil {
		return fmt.Errorf("restart the lease deadlines: %w", err)
	}
	if _, err := tx.Exec(`UPDATE lease_tokens SET expires_at = ?
		WHERE job_id IN (SELECT id FROM jobs WHERE status IN ('leased', 'running') AND lease_expires_at IS NOT NULL)`,
		leaseExpiry); err != nil {
		return fmt.Errorf("restart the lease-token deadlines: %w", err)
	}
	// The hard-cap round reads every non-terminal job with last_leased
	// set — a live lease — so the cap clock must shift past the gap for
	// that set, or the first wakeful tick (or the holder's first
	// heartbeat) would hard-cap a job the sleep never ran (§5.6). first_leased shifts with it so the job's first lease also
	// stays past the sleep; last_leased's NULL stays NULL (a queued job
	// has no lease running, and its cap starts at its next lease).
	rows, err := tx.Query(`SELECT id, first_leased, last_leased
		FROM jobs WHERE status IN ('queued', 'leased', 'running') AND first_leased IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("read the hard-cap clocks to restart them: %w", err)
	}
	type clockShift struct {
		id    int64
		first string
		last  sql.NullString
	}
	var shifts []clockShift
	for rows.Next() {
		var s clockShift
		if err := rows.Scan(&s.id, &s.first, &s.last); err != nil {
			rows.Close()
			return fmt.Errorf("read the hard-cap clocks to restart them: %w", err)
		}
		shifts = append(shifts, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read the hard-cap clocks to restart them: %w", err)
	}
	rows.Close()
	for _, s := range shifts {
		first := s.first
		if t, err := parseTime(first); err == nil {
			first = fmtTime(t.Add(gap))
		}
		var last any
		if s.last.Valid {
			if t, err := parseTime(s.last.String); err == nil {
				last = fmtTime(t.Add(gap))
			} else {
				last = s.last.String
			}
		}
		if _, err := tx.Exec(`UPDATE jobs SET first_leased = ?, last_leased = ? WHERE id = ?`,
			first, last, s.id); err != nil {
			return fmt.Errorf("restart the hard-cap and accrual clocks of job %d: %w", s.id, err)
		}
	}
	// Queued jobs: the wait during the gap was not a wait at all (§8.1),
	// so the queue-TTL clock restarts from now — queued_at and the hold
	// stamp cap_held_at together.
	if _, err := tx.Exec(`UPDATE jobs SET queued_at = ?, cap_held_at = ?, updated_at = ?
		WHERE status = 'queued'`, nowStr, nowStr, nowStr); err != nil {
		return fmt.Errorf("restart the queue-TTL clocks: %w", err)
	}
	// Attach holds: a terminal that slept with the engine keeps its hold,
	// and its hold-scoped token moves with it.
	holdExpiry := fmtTime(now.Add(attachHoldTTL))
	if _, err := tx.Exec(`UPDATE attach_holds SET expires_at = ? WHERE state = 'live'`, holdExpiry); err != nil {
		return fmt.Errorf("restart the attach-hold deadlines: %w", err)
	}
	if _, err := tx.Exec(`UPDATE hold_tokens SET expires_at = ?
		WHERE hold_id IN (SELECT id FROM attach_holds WHERE state = 'live')`, holdExpiry); err != nil {
		return fmt.Errorf("restart the hold-token deadlines: %w", err)
	}
	// Runners: a missing heartbeat while the engine slept proves nothing
	// (§8 "Health"), so the stale window restarts from now.
	if _, err := tx.Exec(`UPDATE runners SET last_seen_at = ? WHERE last_seen_at IS NOT NULL`, nowStr); err != nil {
		return fmt.Errorf("restart the runner-stale clocks: %w", err)
	}
	return nil
}

// sweepPolledTx is sweepTx run for a lease poll by runner polling (0 for
// any other call): a runner asking for work has a free slot, which the
// busy-runner rule of the queue TTL takes into account.
func (e *Engine) sweepPolledTx(tx *sql.Tx, now time.Time, polling int64) (rep SweepReport, err error) {
	nowStr := fmtTime(now)

	// Record the tick only once the sweep's own work has succeeded: a
	// sweep that fails rolls its work back, and the next tick must still
	// see the pre-sleep gap to re-detect the suspension (§13).
	defer func() {
		if err == nil {
			e.recordSweep(now)
		}
	}()

	// A suspension (§13): the wall clock jumped far past the sweep
	// interval between ticks, so the engine itself slept. Deadlines are
	// restarted from now instead of acted on, and one event records the
	// detection.
	if gap, suspended := e.sweepGap(now); suspended {
		if err := e.restartDeadlinesTx(tx, now, gap); err != nil {
			return rep, err
		}
		if _, err := appendEventTx(tx, evEngineSuspended, 0, 0, 0,
			eventPayload{SuspendedFor: int64(gap / time.Second)}, now); err != nil {
			return rep, err
		}
		return rep, nil
	}

	rows, err := tx.Query(`SELECT id, project_id, COALESCE(task_id, 0), COALESCE(lease_id, ''),
		COALESCE(last_leased, ''), COALESCE(lease_expires_at, '')
		FROM jobs
		WHERE status IN ('leased', 'running') AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?`, nowStr)
	if err != nil {
		return rep, fmt.Errorf("sweep expired leases: %w", err)
	}
	type lapsed struct {
		id            int64
		project, task int64
		lease         string
		last, expires string
	}
	var lapsedAll []lapsed
	for rows.Next() {
		var x lapsed
		if err := rows.Scan(&x.id, &x.project, &x.task, &x.lease, &x.last, &x.expires); err != nil {
			rows.Close()
			return rep, fmt.Errorf("sweep expired leases: %w", err)
		}
		lapsedAll = append(lapsedAll, x)
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("sweep expired leases: %w", err)
	}
	rows.Close()
	for _, x := range lapsedAll {
		var accrue int64
		if expired, err := parseTime(x.expires); err == nil {
			accrue = accrualFrom(x.last, expired)
		}
		// The queue-TTL clock restarts at the requeue the sweep observes:
		// a leased job was being served, and the gap from the expiry to
		// now (a host sleep) is not queue wait, because only the engine
		// leases jobs out (§8.1). The lease id survives (see above).
		if _, err := tx.Exec(`UPDATE jobs SET status = 'queued', queued_at = ?, last_leased = NULL,
			lease_expires_at = NULL, leased_seconds = leased_seconds + ?, updated_at = ?
			WHERE id = ?`, nowStr, accrue, nowStr, x.id); err != nil {
			return rep, fmt.Errorf("requeue expired job %d: %w", x.id, err)
		}
		// The lapsed lease's foreman token is dead at its expiry; a
		// later revive may bring the lease back, but not the token —
		// the next lease of the job mints a fresh one (§12.1).
		if err := revokeLeaseTokensTx(tx, x.id); err != nil {
			return rep, err
		}
		if _, err := appendEventTx(tx, evLeaseRequeued, x.project, x.task, x.id,
			eventPayload{Lease: leasePrefix(x.lease)}, now); err != nil {
			return rep, err
		}
		rep.Expired = append(rep.Expired, x.id)
	}

	for _, kind := range []Kind{KindWork, KindFix, KindReview, KindForeman} {
		limit, ok := e.cfg.HardCaps[kind]
		if !ok || limit <= 0 {
			continue
		}
		rows, err := tx.Query(`SELECT id, project_id, COALESCE(task_id, 0), COALESCE(last_leased, '')
			FROM jobs
			WHERE status IN ('queued', 'leased', 'running') AND kind = ? AND last_leased IS NOT NULL AND last_leased <= ?`,
			string(kind), fmtTime(now.Add(-limit)))
		if err != nil {
			return rep, fmt.Errorf("sweep hard caps: %w", err)
		}
		type capped struct {
			id            int64
			project, task int64
			last          string
		}
		var cappedAll []capped
		for rows.Next() {
			var x capped
			if err := rows.Scan(&x.id, &x.project, &x.task, &x.last); err != nil {
				rows.Close()
				return rep, fmt.Errorf("sweep hard caps: %w", err)
			}
			cappedAll = append(cappedAll, x)
		}
		if err := rows.Err(); err != nil {
			return rep, fmt.Errorf("sweep hard caps: %w", err)
		}
		rows.Close()
		for _, x := range cappedAll {
			var accrue int64
			if last, err := parseTime(x.last); err == nil {
				// the cap, not its discovery, ends the lease
				accrue = accrualFrom(x.last, last.Add(limit))
			}
			reason := capError(kind, limit)
			if err := failJobTx(tx, x.id, reason, accrue, now); err != nil {
				return rep, err
			}
			if _, err := appendEventTx(tx, evJobHardCapped, x.project, x.task, x.id,
				eventPayload{Status: statusFailed, Reason: reason}, now); err != nil {
				return rep, err
			}
			if err := e.sweptForemanFinishedTx(tx, x.id, x.project, now); err != nil {
				return rep, err
			}
			if x.task != 0 {
				if err := e.needsForemanTx(tx, &jobRow{ID: x.id, ProjectID: x.project, TaskID: x.task},
					evTaskFailed, eventPayload{Reason: boundNote(reason)}, now); err != nil {
					return rep, err
				}
			}
			rep.HardCapped = append(rep.HardCapped, x.id)
		}
	}

	// A queued review no agent could ever take is flagged before the TTL
	// reads it, so the notification precedes the park.
	if err := flagUnservableReviewsTx(tx, now); err != nil {
		return rep, err
	}

	// A foreman turn whose harness/model pair no registered agent offers
	// waits for one to register, however long that takes; it is not an
	// unserved job the TTL may fail. It is stamped whether or not the TTL
	// is on, so the status always names the wait, and it runs before the
	// other holds so a hold the pair's registration made stale is lifted
	// before they read it. A registered agent that is merely busy or
	// cooling is the other holds' wait, not this one.
	if err := e.holdNoPairTx(tx, now); err != nil {
		return rep, err
	}

	if e.cfg.QueueTTL > 0 {
		// A job whose every candidate runner is at capacity is waiting
		// for a slot, not unserved: stamp it before the TTL reads it.
		if err := e.holdBusyRunnersTx(tx, now, polling); err != nil {
			return rep, err
		}
		// A job whose every qualifying agent is cooling waits for the
		// cooling to end. It runs after the runner rule: see holdCoolingTx
		// for which hold wins.
		if err := e.holdCoolingTx(tx, now); err != nil {
			return rep, err
		}
		// The TTL is for jobs no runner can serve (§8.1); a job held only
		// by a full cap layer, or waiting for its cooling agent, waits as
		// long as it takes, so the clock counts from the later of
		// queued_at and the last hold.
		rows, err := tx.Query(`SELECT id, project_id, COALESCE(task_id, 0), kind, COALESCE(require_agent, 0) FROM jobs
			WHERE status = 'queued' AND queued_at IS NOT NULL
			AND max(queued_at, COALESCE(cap_held_at, queued_at)) <= ?`, fmtTime(now.Add(-e.cfg.QueueTTL)))
		if err != nil {
			return rep, fmt.Errorf("sweep queue TTL: %w", err)
		}
		type waiting struct {
			id            int64
			project, task int64
			kind          string
			requireAgent  int64
		}
		var waitingAll []waiting
		for rows.Next() {
			var x waiting
			if err := rows.Scan(&x.id, &x.project, &x.task, &x.kind, &x.requireAgent); err != nil {
				rows.Close()
				return rep, fmt.Errorf("sweep queue TTL: %w", err)
			}
			waitingAll = append(waitingAll, x)
		}
		if err := rows.Err(); err != nil {
			return rep, fmt.Errorf("sweep queue TTL: %w", err)
		}
		rows.Close()
		for _, x := range waitingAll {
			reason := fmt.Sprintf("queue TTL exceeded: no runner leased the job within %s", capText(e.cfg.QueueTTL))
			if x.requireAgent != 0 {
				// A pin on a broken agent has no end in sight, so it is not
				// held (a cooling one is, see holdCoolingTx); the park names
				// the pin and the state instead of blaming the runners.
				var harness, model, health, why string
				err := tx.QueryRow("SELECT harness, model, health, health_reason FROM agents WHERE id = ?", x.requireAgent).
					Scan(&harness, &model, &health, &why)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return rep, fmt.Errorf("read the pinned agent %d: %w", x.requireAgent, err)
				}
				if err == nil && health == "broken" {
					state := "broken"
					if why != "" {
						state += ": " + why
					}
					reason = fmt.Sprintf("queue TTL exceeded: job pinned to %s/%s (%s); no runner leased the job within %s",
						harness, model, state, capText(e.cfg.QueueTTL))
				}
			}
			if x.task != 0 && x.requireAgent == 0 && x.kind != string(KindLand) {
				j, err := loadJobTx(tx, x.id)
				if err != nil {
					return rep, err
				}
				agents, err := eligibleAgentsTx(tx, &e.cfg, nil, 0, j, kindClass(j.Kind), nowStr)
				if err != nil {
					return rep, err
				}
				if len(agents) == 0 {
					asked, err := jobTierTx(tx, e.cfg.Review, j)
					if err != nil {
						return rep, err
					}
					if asked != "" {
						reason += fmt.Sprintf("; no agent of tier %s, or of any tier, offers %s", asked, kindClass(j.Kind))
					}
				}
			}
			if x.kind == string(KindReview) {
				// A review no agent could ever take says so, not the bare TTL.
				j, err := loadJobTx(tx, x.id)
				if err != nil {
					return rep, err
				}
				bad, err := reviewUnservableTx(tx, j)
				if err != nil {
					return rep, err
				}
				if bad {
					reason, err = noReviewerReasonTx(tx, j, false)
					if err != nil {
						return rep, err
					}
				}
			}
			if err := failJobTx(tx, x.id, reason, 0, now); err != nil {
				return rep, err
			}
			if _, err := appendEventTx(tx, evJobQueueTimedOut, x.project, x.task, x.id,
				eventPayload{Status: statusFailed, Reason: reason}, now); err != nil {
				return rep, err
			}
			if err := e.sweptForemanFinishedTx(tx, x.id, x.project, now); err != nil {
				return rep, err
			}
			if x.task != 0 {
				if err := e.needsForemanTx(tx, &jobRow{ID: x.id, ProjectID: x.project, TaskID: x.task},
					evTaskFailed, eventPayload{Reason: boundNote(reason)}, now); err != nil {
					return rep, err
				}
			}
			rep.QueueTimedOut = append(rep.QueueTimedOut, x.id)
		}
	}

	rows, err = tx.Query(`SELECT id, project_id FROM attach_holds WHERE state = 'live' AND expires_at <= ?`, nowStr)
	if err != nil {
		return rep, fmt.Errorf("sweep expired attach holds: %w", err)
	}
	type lapsedHold struct {
		id, project int64
	}
	var holds []lapsedHold
	for rows.Next() {
		var h lapsedHold
		if err := rows.Scan(&h.id, &h.project); err != nil {
			rows.Close()
			return rep, fmt.Errorf("sweep expired attach holds: %w", err)
		}
		holds = append(holds, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return rep, fmt.Errorf("sweep expired attach holds: %w", err)
	}
	rows.Close()
	for _, h := range holds {
		if err := e.expireAttachHoldTx(tx, h.id, h.project, now); err != nil {
			return rep, err
		}
	}

	// A runner unseen past the stale window loses its agents' health (§8).
	if err := e.sweepStaleRunnersTx(tx, now); err != nil {
		return rep, err
	}

	return rep, nil
}

// sweptForemanFinishedTx settles a foreman turn the sweep just failed
// (§4.2): its delivered events return to pending and the next wake is
// evaluated, in the sweep's own transaction. Jobs of other kinds, and a
// job that is not a failed foreman turn, are a no-op.
func (e *Engine) sweptForemanFinishedTx(tx *sql.Tx, id, project int64, now time.Time) error {
	var kind Kind
	var status string
	if err := tx.QueryRow("SELECT kind, status FROM jobs WHERE id = ?", id).Scan(&kind, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read job %d for the foreman inbox: %w", id, err)
	}
	if kind != KindForeman || status != statusFailed {
		return nil
	}
	return e.foremanFinishedTx(tx, &jobRow{ID: id, ProjectID: project, Kind: kind}, status, now)
}
