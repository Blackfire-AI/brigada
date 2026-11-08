package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
	if err := shiftFreedJobsTx(tx, project, now); err != nil {
		return err
	}
	return e.evaluateHeldWakesTx(tx, now)
}

// pauseSpan is one stored pause interval as the held time reads it
// (§8.3 "Clocks"): a pause that still stands spans up to now.
type pauseSpan struct {
	start, end time.Time
}

// scopeSpansTx reads every stored pause interval that covers the
// project — the engine's own and the project's — each ended at its
// lift, or at now while it stands (§8.3 "Clocks"). The rows are the
// whole record: a lifted pause keeps its interval, so the held time is
// derived from them, never stored beside them.
func scopeSpansTx(tx *sql.Tx, project int64, now time.Time) ([]pauseSpan, error) {
	rows, err := tx.Query(`SELECT started_at, COALESCE(ended_at, '') FROM pauses
		WHERE project_id IS NULL OR project_id = ? ORDER BY started_at`, project)
	if err != nil {
		return nil, fmt.Errorf("read the pause intervals of project %d: %w", project, err)
	}
	defer rows.Close()
	var spans []pauseSpan
	for rows.Next() {
		var started, ended string
		if err := rows.Scan(&started, &ended); err != nil {
			return nil, fmt.Errorf("read the pause intervals of project %d: %w", project, err)
		}
		s, err := parseTime(started)
		if err != nil {
			continue // an interval with no readable start holds nothing
		}
		end := now
		if ended != "" {
			if t, err := parseTime(ended); err == nil && t.Before(now) {
				end = t
			}
		}
		if end.After(s) {
			spans = append(spans, pauseSpan{start: s, end: end})
		}
	}
	return spans, rows.Err()
}

// mergeSpans folds overlapping and touching pause intervals into their
// union, ordered by start: an engine pause and a project pause that
// overlap count once (§8.3 "Clocks").
func mergeSpans(spans []pauseSpan) []pauseSpan {
	sorted := append([]pauseSpan(nil), spans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })
	var out []pauseSpan
	for _, s := range sorted {
		if n := len(out); n > 0 && !s.start.After(out[n-1].end) {
			if s.end.After(out[n-1].end) {
				out[n-1].end = s.end
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// heldWithin is the paused time within [from, to]: the length of the
// union of the spans, each cut to the window — so a span that began
// before from counts only from it, and overlapping spans count once.
func heldWithin(spans []pauseSpan, from, to time.Time) time.Duration {
	var held time.Duration
	for _, s := range mergeSpans(spans) {
		start, end := s.start, s.end
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if end.After(start) {
			held += end.Sub(start)
		}
	}
	return held
}

// heldSinceTx is the time a pause of the project or of the engine held
// it since from (§8.3 "Clocks"): the union of the scope's pause
// intervals within [from, now].
func heldSinceTx(tx *sql.Tx, project int64, from, now time.Time) (time.Duration, error) {
	spans, err := scopeSpansTx(tx, project, now)
	if err != nil {
		return 0, err
	}
	return heldWithin(spans, from, now), nil
}

// shiftFreedJobsTx is the clocks' half of a lift (§8.3 "Clocks"): every
// queued job the lift frees — of a project neither scope pauses any
// longer — has its queue-TTL clock and its retry's not-before shifted
// by its held time, as the suspension rule shifts deadlines past a gap
// (restartDeadlinesTx, §13), so a pause by itself never fails or parks
// a job. The held time is the paused stretch that ends with this lift —
// the merged engine and project intervals, so overlapping pauses count
// once — taken from the later of the job's queuing and the stretch's
// start, so a job queued mid-pause counts only its own wait. An earlier
// stretch was shifted at its own lift and is never counted again. The
// queue TTL reads queued_at, cap_held_at and not_before (sweepPolledTx
// round 3), so all three move: queued_at and not_before by the held
// time, a cap hold stamped during the pause no further than now. A lift
// under a remaining scope frees nothing and shifts nothing yet. The
// infra-retry window is not stored here: infraStreakSpentTx computes
// its held time against the streak's start.
func shiftFreedJobsTx(tx *sql.Tx, lifted int64, now time.Time) error {
	engine, paused, err := livePausesTx(tx)
	if err != nil {
		return err
	}
	if engine || (lifted != 0 && paused[lifted]) {
		return nil
	}
	query := `SELECT id, project_id, queued_at, COALESCE(cap_held_at, ''), COALESCE(not_before, '')
		FROM jobs WHERE status = 'queued' AND queued_at IS NOT NULL`
	var args []any
	if lifted != 0 {
		query += " AND project_id = ?"
		args = append(args, lifted)
	}
	rows, err := tx.Query(query+" ORDER BY id", args...)
	if err != nil {
		return fmt.Errorf("read the queued jobs a lift frees: %w", err)
	}
	type queuedClock struct {
		id, project                int64
		queued, capHeld, notBefore string
	}
	var jobs []queuedClock
	for rows.Next() {
		var q queuedClock
		if err := rows.Scan(&q.id, &q.project, &q.queued, &q.capHeld, &q.notBefore); err != nil {
			rows.Close()
			return fmt.Errorf("read the queued jobs a lift frees: %w", err)
		}
		jobs = append(jobs, q)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read the queued jobs a lift frees: %w", err)
	}
	rows.Close()
	// The stretch that ends now, per freed project: the merged segment
	// of its scope's intervals whose end is this lift.
	stretches := map[int64]*pauseSpan{}
	for _, q := range jobs {
		if paused[q.project] {
			continue // the project's own pause still holds it
		}
		stretch, ok := stretches[q.project]
		if !ok {
			spans, err := scopeSpansTx(tx, q.project, now)
			if err != nil {
				return err
			}
			for _, s := range mergeSpans(spans) {
				if !s.end.Before(now) {
					s := s
					stretch = &s
				}
			}
			stretches[q.project] = stretch
		}
		if stretch == nil {
			continue
		}
		queued, err := parseTime(q.queued)
		if err != nil {
			continue
		}
		from := stretch.start
		if queued.After(from) {
			from = queued
		}
		held := now.Sub(from)
		if held <= 0 {
			continue
		}
		var capHeld, notBefore any
		if t, err := parseTime(q.capHeld); err == nil {
			shifted := t.Add(held)
			if shifted.After(now) {
				shifted = now
			}
			capHeld = fmtTime(shifted)
		}
		if t, err := parseTime(q.notBefore); err == nil {
			notBefore = fmtTime(t.Add(held))
		}
		if _, err := tx.Exec(`UPDATE jobs SET queued_at = ?, cap_held_at = COALESCE(?, cap_held_at),
			not_before = COALESCE(?, not_before), updated_at = ? WHERE id = ?`,
			fmtTime(queued.Add(held)), capHeld, notBefore, fmtTime(now), q.id); err != nil {
			return fmt.Errorf("shift the clocks of job %d past its pause: %w", q.id, err)
		}
	}
	return nil
}

// notPausedSQL is the clause that keeps a jobs query to the jobs no
// pause covers (§8.3): neither the engine's live pause nor their
// project's. alias names the jobs table in the query.
func notPausedSQL(alias string) string {
	return `NOT EXISTS (SELECT 1 FROM pauses p WHERE p.ended_at IS NULL
		AND (p.project_id IS NULL OR p.project_id = ` + alias + `.project_id))`
}

// LivePause is one pause that stands, as the status and the capacity
// view carry it (§8.3 "What shows it"): its scope, its start, its
// origin and the scope's jobs still draining. The JSON fields are
// stable; the renderings of `brigada status`, `brigada top` and the
// MCP tools read them.
type LivePause struct {
	// ProjectID names the paused project; 0 is the engine's own pause,
	// the scope of every project.
	ProjectID int64 `json:"project_id"`
	// RepoURL is the paused project's repo URL; empty for the engine's.
	RepoURL string `json:"repo_url,omitempty"`
	// Origin is who set the pause: user or drain (§8.3).
	Origin string `json:"origin"`
	// StartedAt is when the pause began (the store's UTC text).
	StartedAt string `json:"started_at"`
	// Draining lists the scope's jobs still leased or running, oldest
	// first — the drain the pause waits on; empty once drained.
	Draining []PauseJob `json:"draining"`
	// Drained is true when none of the scope's jobs is leased or running.
	Drained bool `json:"drained"`
	// Queued counts the scope's queued jobs the pause holds: they are
	// counted here, never as a held: wait for a slot (§8.1). A job both
	// the engine's and its project's pause cover counts under each.
	Queued int `json:"queued"`
}

// PauseJob is one job a pause's drain still waits on (§8.3): leased or
// running in the paused scope.
type PauseJob struct {
	ID     int64  `json:"id"`
	TaskID int64  `json:"task_id,omitempty"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// livePausesViewTx lists the pauses that stand (§8.3), the engine's
// first, then per project in id order, each with the jobs its scope
// still drains and the queued jobs it holds. project scopes the view as
// the status does: 0 is every pause and every job; a project lists the
// engine's pause and its own, with the jobs of that project alone.
func livePausesViewTx(tx *sql.Tx, project int64) ([]LivePause, error) {
	query := `SELECT COALESCE(p.project_id, 0), COALESCE(pr.repo_url, ''), p.origin, p.started_at
		FROM pauses p LEFT JOIN projects pr ON pr.id = p.project_id
		WHERE p.ended_at IS NULL AND (p.project_id IS NULL OR pr.archived_at IS NULL)`
	var args []any
	if project != 0 {
		query += " AND (p.project_id IS NULL OR p.project_id = ?)"
		args = append(args, project)
	}
	rows, err := tx.Query(query+" ORDER BY COALESCE(p.project_id, 0)", args...)
	if err != nil {
		return nil, fmt.Errorf("list the live pauses: %w", err)
	}
	var out []LivePause
	for rows.Next() {
		var p LivePause
		if err := rows.Scan(&p.ProjectID, &p.RepoURL, &p.Origin, &p.StartedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the live pauses: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list the live pauses: %w", err)
	}
	rows.Close()
	for i := range out {
		p := &out[i]
		scope := p.ProjectID
		if scope == 0 {
			scope = project // the engine's scope, as far as the caller sees
		}
		jobsQuery := "FROM jobs WHERE project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)"
		var jobArgs []any
		if scope != 0 {
			jobsQuery += " AND project_id = ?"
			jobArgs = append(jobArgs, scope)
		}
		p.Draining = []PauseJob{}
		jrows, err := tx.Query(`SELECT id, COALESCE(task_id, 0), kind, status `+jobsQuery+
			` AND status IN ('leased', 'running') ORDER BY id`, jobArgs...)
		if err != nil {
			return nil, fmt.Errorf("list the jobs a pause drains: %w", err)
		}
		for jrows.Next() {
			var j PauseJob
			if err := jrows.Scan(&j.ID, &j.TaskID, &j.Kind, &j.Status); err != nil {
				jrows.Close()
				return nil, fmt.Errorf("list the jobs a pause drains: %w", err)
			}
			p.Draining = append(p.Draining, j)
		}
		if err := jrows.Err(); err != nil {
			jrows.Close()
			return nil, fmt.Errorf("list the jobs a pause drains: %w", err)
		}
		jrows.Close()
		p.Drained = len(p.Draining) == 0
		if err := tx.QueryRow(`SELECT COUNT(*) `+jobsQuery+` AND status = 'queued'`, jobArgs...).Scan(&p.Queued); err != nil {
			return nil, fmt.Errorf("count the jobs a pause holds: %w", err)
		}
	}
	return out, nil
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
