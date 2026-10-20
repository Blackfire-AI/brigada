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
//  2. a live job past its kind's hard cap from first_leased fails with
//     the cap named in the error (§14); a job with a task parks the task
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

// sweepPolledTx is sweepTx run for a lease poll by runner polling (0 for
// any other call): a runner asking for work has a free slot, which the
// busy-runner rule of the queue TTL takes into account.
func (e *Engine) sweepPolledTx(tx *sql.Tx, now time.Time, polling int64) (SweepReport, error) {
	var rep SweepReport
	nowStr := fmtTime(now)

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
		rows, err := tx.Query(`SELECT id, project_id, COALESCE(task_id, 0), COALESCE(first_leased, ''), COALESCE(last_leased, '')
			FROM jobs
			WHERE status IN ('queued', 'leased', 'running') AND kind = ? AND first_leased IS NOT NULL AND first_leased <= ?`,
			string(kind), fmtTime(now.Add(-limit)))
		if err != nil {
			return rep, fmt.Errorf("sweep hard caps: %w", err)
		}
		type capped struct {
			id            int64
			project, task int64
			first, last   string
		}
		var cappedAll []capped
		for rows.Next() {
			var x capped
			if err := rows.Scan(&x.id, &x.project, &x.task, &x.first, &x.last); err != nil {
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
			if first, err := parseTime(x.first); err == nil {
				// the cap, not its discovery, ends the lease
				accrue = accrualFrom(x.last, first.Add(limit))
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
			if x.task != 0 && x.requireAgent == 0 {
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
