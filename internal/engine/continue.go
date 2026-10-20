package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A continuing task (§6.1): a task whose continue field names one of its
// deps. When that dep is done and the task starts, its first work job
// runs in session mode on the agent that authored the dep's approved
// result, resuming that session, so the worker that already holds the
// area's files in its head starts the new task warm instead of paying
// the cold start a fresh session pays. Everything else about the task —
// its check, its review, its rounds, escalation — is unchanged. Every
// way the session cannot be had falls back to the fresh work job of
// today, and a restart of the continuing task always starts fresh (the
// restart paths queue ModeFresh, §5.6).

// continuedPlacement is the session a continuing task's first work job
// resumes: the dep author's agent and harness, the runner the agent sits
// on, and the harness session the dep's key ended on.
type continuedPlacement struct {
	agent   int64
	harness string
	runner  int64
	session string
}

// continuePlacementTx decides whether task taskID continues a dep's
// session (§6.1), in the caller's transaction: nil when the task
// continues nothing, and nil with no error for every fallback too — the
// unknown session id, the author with no agent, the agent gone, broken,
// capped at 0 or cooling beyond longCooling (§8), and the agent outside
// the task's effective tier. The checks are the dispatcher's own words
// for an agent that cannot take the job now, so a continuation never
// waits on an agent the fresh work job would not have been pinned to.
func (e *Engine) continuePlacementTx(tx *sql.Tx, taskID int64, now time.Time) (*continuedPlacement, error) {
	var dep int64
	err := tx.QueryRow("SELECT depends_on_task_id FROM task_deps WHERE task_id = ? AND is_continue = 1", taskID).Scan(&dep)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the continued dep of task %d: %w", taskID, err)
	}
	// The author of the dep's approved result: the agent its latest work
	// or fix job ran on, as a fix round's pin reads it (§3).
	var agent int64
	err = tx.QueryRow(`SELECT COALESCE(agent_id, 0) FROM jobs
		WHERE task_id = ? AND kind IN ('work', 'fix')
		ORDER BY id DESC LIMIT 1`, dep).Scan(&agent)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && agent == 0) {
		return nil, nil // no author job, or an agent-less runner
	}
	if err != nil {
		return nil, fmt.Errorf("read the author job of task %d: %w", dep, err)
	}
	// The session the dep's key ended on (§4.2): what a resumed run
	// hands its CLI.
	var session sql.NullString
	err = tx.QueryRow("SELECT harness_session_id FROM sessions WHERE key = ?", fmt.Sprintf("task:%d", dep)).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !session.Valid) {
		return nil, nil // the session id is unknown
	}
	if err != nil {
		return nil, fmt.Errorf("read the session of task %d: %w", dep, err)
	}
	var runner sql.NullInt64
	var harness, model, account, health, cooling string
	err = tx.QueryRow(`SELECT harness, model, COALESCE(account, ''), health, COALESCE(cooling_until, ''), runner_id
		FROM agents WHERE id = ?`, agent).Scan(&harness, &model, &account, &health, &cooling, &runner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // the agent a re-register has since dropped
	}
	if err != nil {
		return nil, fmt.Errorf("read agent %d: %w", agent, err)
	}
	var tier string
	if err := tx.QueryRow("SELECT effective_tier FROM tasks WHERE id = ?", taskID).Scan(&tier); err != nil {
		return nil, fmt.Errorf("read the tier of task %d: %w", taskID, err)
	}
	switch {
	case health != "ready":
		return nil, nil
	case cappedAtZero(e.cfg.Caps, account, model):
		return nil, nil
	case cooling > fmtTime(now.Add(longCooling)):
		return nil, nil
	case e.cfg.Caps.TierOf(harness, model) != tier:
		return nil, nil
	}
	return &continuedPlacement{agent: agent, harness: harness, runner: runner.Int64, session: session.String}, nil
}
