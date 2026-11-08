package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The origins a pause carries (§8.3): the user's own `brigada pause`, or
// the drain of `brigada stop --engine --drain`. A user's origin outranks
// a drain's — a drain over a user's pause leaves the origin `user`, and
// a user pause over a drain pause makes it the user's — and `brigada
// start` lifts a drain's alone (ResumeDrain).
const (
	PauseOriginUser  = "user"
	PauseOriginDrain = "drain"
)

// ErrInvalidPause marks a pause call the engine refuses: an origin that
// is neither user nor drain.
var ErrInvalidPause = errors.New("invalid pause")

// PauseResult is what Pause did (§8.3): the origin of the scope's live
// pause after the call, whether this call began it, and when that pause
// started. ProjectID 0 names the engine's own scope.
type PauseResult struct {
	ProjectID int64  `json:"project_id"`
	Origin    string `json:"origin"`
	// Started says this call began the pause; a pause over an already
	// paused scope starts nothing and only upgrades the origin.
	Started bool `json:"started"`
	// StartedAt is when the scope's live pause began (RFC3339).
	StartedAt string `json:"started_at,omitempty"`
}

// ResumeResult is what Resume did (§8.3): whether the scope's live pause
// was lifted, and the origin it carried. ProjectID 0 names the engine.
type ResumeResult struct {
	ProjectID int64 `json:"project_id"`
	// Resumed is false when no pause stood — resuming an unpaused scope
	// says so — and for ResumeDrain when a user's pause does.
	Resumed bool   `json:"resumed"`
	Origin  string `json:"origin,omitempty"` // the lifted pause's origin
}

// Pause pauses dispatch for one scope — the whole engine (project 0) or
// one project — with the origin naming who set it, `user` or `drain`
// (§8.3). While the pause stands, the lease hands out none of the
// scope's jobs and no headless foreman turn is submitted for its
// judgment events; running jobs are untouched. Pausing an already
// paused scope starts nothing and only upgrades the origin: a user's
// outranks a drain's, and a drain over a user's pause leaves it the
// user's. The stored pause survives an idle exit, a crash and a
// restart; dispatch_paused records the scope, the standing origin and
// the caller, in no inbox and waking no turn.
func (e *Engine) Pause(ctx context.Context, project int64, origin, caller string) (PauseResult, error) {
	switch origin {
	case PauseOriginUser, PauseOriginDrain:
	default:
		return PauseResult{}, fmt.Errorf("%w: origin %q is neither %q nor %q",
			ErrInvalidPause, origin, PauseOriginUser, PauseOriginDrain)
	}
	var res PauseResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		if project != 0 {
			if err := activeProjectTx(tx, project); err != nil {
				return err
			}
		}
		live, err := livePauseTx(tx, project)
		if err != nil {
			return err
		}
		res = PauseResult{ProjectID: project, Origin: origin}
		if live == nil {
			if _, err := tx.Exec(`INSERT INTO pauses (project_id, origin, started_at) VALUES (?, ?, ?)`,
				nullIfZero(project), origin, fmtTime(now)); err != nil {
				return fmt.Errorf("pause the scope of project %d: %w", project, err)
			}
			res.Started, res.StartedAt = true, fmtTime(now)
		} else {
			// Origin precedence (§8.3): only a user upgrade moves a live
			// pause — a drain over a user's pause changes nothing.
			res.StartedAt, res.Origin = live.startedAt, live.origin
			if origin == PauseOriginUser && live.origin != PauseOriginUser {
				if _, err := tx.Exec("UPDATE pauses SET origin = ? WHERE id = ?", PauseOriginUser, live.id); err != nil {
					return fmt.Errorf("make the pause of project %d the user's: %w", project, err)
				}
				res.Origin = PauseOriginUser
			}
		}
		_, err = appendEventTx(tx, evDispatchPaused, project, 0, 0, eventPayload{Origin: res.Origin, Caller: caller}, now)
		return err
	})
	if err != nil {
		return PauseResult{}, err
	}
	return res, nil
}

// Resume lifts the pause of one scope — the engine's (project 0) or one
// project's — and no other (§8.3), then evaluates the held wakes at
// once for every project neither scope pauses any longer: the engine's
// pause still holds while one project's lifts, and a project's own
// pause holds while the engine's lifts. Resuming a scope that is not
// paused changes nothing and says so.
func (e *Engine) Resume(ctx context.Context, project int64, caller string) (ResumeResult, error) {
	var res ResumeResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		return e.liftScopeTx(tx, project, func(*pauseRow) bool { return true }, caller, &res, now)
	})
	if err != nil {
		return ResumeResult{}, err
	}
	return res, nil
}

// ResumeDrain is the lift `brigada start` makes (§8.3): it lifts the
// engine's pause only when a drain set it — a resume, with the resume
// evaluation of the held wakes, whether the engine starts then or is
// still up after an interrupted drain — and leaves a user's pause
// alone.
func (e *Engine) ResumeDrain(ctx context.Context, caller string) (ResumeResult, error) {
	var res ResumeResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		return e.liftScopeTx(tx, 0, func(p *pauseRow) bool { return p.origin == PauseOriginDrain }, caller, &res, now)
	})
	if err != nil {
		return ResumeResult{}, err
	}
	return res, nil
}

// liftScopeTx is the shared body of Resume and ResumeDrain: lift the
// scope's live pause when lift allows it — ending its interval, which
// stays stored (§8.3 "Clocks") — record dispatch_resumed, and evaluate
// the held wakes. A scope lift refuses leaves res saying so, with no
// event and no evaluation.
func (e *Engine) liftScopeTx(tx *sql.Tx, project int64, lift func(*pauseRow) bool, caller string, res *ResumeResult, now time.Time) error {
	live, err := livePauseTx(tx, project)
	if err != nil {
		return err
	}
	*res = ResumeResult{ProjectID: project}
	if live == nil || !lift(live) {
		return nil
	}
	if _, err := tx.Exec("UPDATE pauses SET ended_at = ? WHERE id = ?", fmtTime(now), live.id); err != nil {
		return fmt.Errorf("lift the pause of scope %d: %w", project, err)
	}
	res.Resumed, res.Origin = true, live.origin
	if _, err := appendEventTx(tx, evDispatchResumed, project, 0, 0, eventPayload{Origin: live.origin, Caller: caller}, now); err != nil {
		return err
	}
	return e.evaluateHeldWakesTx(tx, now)
}

// pauseRow is one stored pause interval (§11): the row a scope's live
// pause reads from, NULL-ended while it stands.
type pauseRow struct {
	id        int64
	origin    string
	startedAt string
}

// livePauseTx reads the scope's live pause, nil when none stands
// (§8.3): project 0 names the engine itself.
func livePauseTx(tx *sql.Tx, project int64) (*pauseRow, error) {
	var p pauseRow
	err := tx.QueryRow(`SELECT id, origin, started_at FROM pauses
		WHERE ended_at IS NULL AND COALESCE(project_id, 0) = ?`, project).Scan(&p.id, &p.origin, &p.startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the live pause of scope %d: %w", project, err)
	}
	return &p, nil
}

// livePausesTx reads every pause that stands (§8.3): whether the engine
// itself is paused, and the set of paused projects. A job is paused
// when its project or the engine is.
func livePausesTx(tx *sql.Tx) (engine bool, projects map[int64]bool, err error) {
	rows, err := tx.Query("SELECT project_id FROM pauses WHERE ended_at IS NULL")
	if err != nil {
		return false, nil, fmt.Errorf("read the live pauses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project sql.NullInt64
		if err := rows.Scan(&project); err != nil {
			return false, nil, fmt.Errorf("read the live pauses: %w", err)
		}
		if !project.Valid {
			engine = true
			continue
		}
		if projects == nil {
			projects = map[int64]bool{}
		}
		projects[project.Int64] = true
	}
	return engine, projects, rows.Err()
}

// scopePausedTx reports whether a pause covers the project (§8.3): its
// own, or the engine's.
func scopePausedTx(tx *sql.Tx, project int64) (bool, error) {
	engine, projects, err := livePausesTx(tx)
	if err != nil {
		return false, err
	}
	return engine || projects[project], nil
}

// evaluateHeldWakesTx is the resume evaluation (§8.3): every project
// with pending inbox rows that no pause covers any longer gets its wake
// evaluated at once, in the lifting transaction. A project another
// scope still pauses frees nothing — the engine's pause over a lifted
// project's own, a project's own under a lifted engine's — and a
// project whose wake a live turn already holds folds as any event
// arriving during a live turn does, its completion submitting the one
// coalesced turn after it.
func (e *Engine) evaluateHeldWakesTx(tx *sql.Tx, now time.Time) error {
	engine, paused, err := livePausesTx(tx)
	if err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT DISTINCT project_id FROM foreman_inbox WHERE state = ? ORDER BY project_id`, inboxPending)
	if err != nil {
		return fmt.Errorf("list the projects with pending inbox rows: %w", err)
	}
	var projects []int64
	for rows.Next() {
		var p int64
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return fmt.Errorf("list the projects with pending inbox rows: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list the projects with pending inbox rows: %w", err)
	}
	rows.Close()
	for _, p := range projects {
		if engine || paused[p] {
			continue
		}
		if err := e.wakeForemanTx(tx, p, now); err != nil {
			return err
		}
	}
	return nil
}

// foremanTurnCarriesUserTx reports whether a user message waits on this
// foreman turn's key (§4.2 item 3, §8.3): delivered to the turn — its
// prompt carried the words — or still pending while a pause holds the
// turn queued, the case that would trap the message behind a turn the
// pause keeps from ever running.
func foremanTurnCarriesUserTx(tx *sql.Tx, j *jobRow) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM foreman_inbox i JOIN events e ON e.id = i.event_id
		WHERE i.project_id = ? AND e.kind = ?
		AND ((i.job_id = ? AND i.state = ?) OR i.state = ?)`,
		j.ProjectID, evUserMessage, j.ID, inboxDelivered, inboxPending).Scan(&n); err != nil {
		return false, fmt.Errorf("read the user messages waiting on job %d: %w", j.ID, err)
	}
	return n > 0, nil
}
