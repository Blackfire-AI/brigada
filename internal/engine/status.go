package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// statusDoneWindow is how long a finished plan stays in status's plans
// list (§10): a day is long enough to see what landed, short enough to
// keep the list readable.
const statusDoneWindow = 24 * time.Hour

// needsYouReasonLimit is the most a needs-you entry's reason line
// carries (§10): one line, about this many bytes, never a paragraph.
const needsYouReasonLimit = 160

// StatusInfo is the engine's own state as the API shows it (§13): which
// process is the engine, since when, the jobs by status — and the plans
// (§6.1, §6.2) with the "needs you" queue (§10): the tasks parked for a
// person and the plans waiting for approval. The pid and start are what
// the lifecycle recorded (RecordInstance); a store no engine of this
// binary has recorded shows zero values.
type StatusInfo struct {
	Pid       int64          `json:"pid"`
	StartedAt string         `json:"started_at"`
	Jobs      map[string]int `json:"jobs"`
	Plans     []StatusPlan   `json:"plans"`
	NeedsYou  []NeedsYouItem `json:"needs_you"`
}

// StatusPlan is one plan the status lists (§6.1, §6.2, §10): every
// active plan, and every plan that went done inside the last day. The
// repo URL names whose plan it is; the tasks are the plan's, each with
// the state and branch the graph is in.
type StatusPlan struct {
	ID      int64        `json:"id"`
	RepoURL string       `json:"repo_url"`
	Title   string       `json:"title"`
	State   string       `json:"state"`
	Tasks   []StatusTask `json:"tasks"`
}

// StatusTask is one task of a status plan (§6.1).
type StatusTask struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Class  string `json:"class"`
	State  string `json:"state"`
	Branch string `json:"branch"` // empty while the task has no result branch yet
}

// NeedsYouItem is one entry of the "needs you" queue (§10): something a
// person must look at — a task parked at needs_foreman or needs_user, or
// a plan waiting for approval. Kind is "task" or "plan"; Reason is what
// the parked task's latest event said, flattened to one short line —
// the worker's question for a needs_help, the review's findings for
// review changes, the check's tail or the failure's words for the rest.
type NeedsYouItem struct {
	Kind   string `json:"kind"` // "task" or "plan"
	ID     int64  `json:"id"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Reason string `json:"reason,omitempty"`
	PlanID int64  `json:"plan_id,omitempty"` // the task's plan, when the entry is a task
}

// JobStatuses is the job statuses' fixed vocabulary, in the order the
// plain-text renderings print them: live first, then terminal. Status
// zero-fills its counts over it, and the renderers — the client's
// status command, the MCP bridge's status tool — share the order.
var JobStatuses = []string{
	statusQueued, statusLeased, statusRunning,
	statusDone, statusFailed, statusCancelled, statusStalled,
}

// Status reads the engine's own state (§13, §10). Like every engine call
// it sweeps first, so the counts are the rules' current word: a lease a
// dead runner left is expired here, not counted as work. project scopes
// the plans and the needs-you queue to one project; 0 names every
// project — the API passes a foreman caller's own project, and client or
// admin callers read all of them.
func (e *Engine) Status(ctx context.Context, project int64) (StatusInfo, error) {
	var info StatusInfo
	info.Plans = []StatusPlan{}
	info.NeedsYou = []NeedsYouItem{}
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := sweepTx(tx, e.cfg, now); err != nil {
			return err
		}
		var pid sql.NullInt64
		var started sql.NullString
		if err := tx.QueryRow("SELECT engine_pid, engine_started_at FROM meta WHERE id = 1").
			Scan(&pid, &started); err != nil {
			return fmt.Errorf("read the engine instance: %w", err)
		}
		info.Pid = pid.Int64
		info.StartedAt = started.String
		rows, err := tx.Query("SELECT status, COUNT(*) FROM jobs GROUP BY status")
		if err != nil {
			return fmt.Errorf("count jobs by status: %w", err)
		}
		defer rows.Close()
		counts := make(map[string]int, len(JobStatuses))
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				return fmt.Errorf("count jobs by status: %w", err)
			}
			counts[status] = n
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("count jobs by status: %w", err)
		}
		for _, s := range JobStatuses {
			if _, ok := counts[s]; !ok {
				counts[s] = 0 // every status appears in the answer, at zero
			}
		}
		info.Jobs = counts
		if info.Plans, err = statusPlansTx(tx, project, now); err != nil {
			return err
		}
		if info.NeedsYou, err = statusNeedsYouTx(tx, project); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return StatusInfo{}, fmt.Errorf("read status: %w", err)
	}
	return info, nil
}

// statusPlansTx lists the plans the status shows (§6.1, §6.2, §10):
// every active plan — its tasks are live work — and every plan that
// went done inside the last day. A done plan's updated_at is the
// moment it went done: nothing else writes a done plan after.
func statusPlansTx(tx *sql.Tx, project int64, now time.Time) ([]StatusPlan, error) {
	cutoff := fmtTime(now.Add(-statusDoneWindow))
	query := `SELECT p.id, pr.repo_url, p.title, p.state
		FROM plans p JOIN projects pr ON pr.id = p.project_id
		WHERE (p.state = 'active' OR (p.state = 'done' AND p.updated_at >= ?))`
	args := []any{cutoff}
	if project != 0 {
		query += " AND p.project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY p.id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the status plans: %w", err)
	}
	plans := []StatusPlan{}
	for rows.Next() {
		var p StatusPlan
		if err := rows.Scan(&p.ID, &p.RepoURL, &p.Title, &p.State); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the status plans: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list the status plans: %w", err)
	}
	rows.Close()
	for i := range plans {
		if plans[i].Tasks, err = statusPlanTasksTx(tx, plans[i].ID); err != nil {
			return nil, err
		}
	}
	return plans, nil
}

// statusPlanTasksTx reads one status plan's tasks, in id order.
func statusPlanTasksTx(tx *sql.Tx, plan int64) ([]StatusTask, error) {
	rows, err := tx.Query(`SELECT id, title, class, state, COALESCE(branch, '')
		FROM tasks WHERE plan_id = ? ORDER BY id`, plan)
	if err != nil {
		return nil, fmt.Errorf("list the tasks of plan %d: %w", plan, err)
	}
	defer rows.Close()
	tasks := []StatusTask{}
	for rows.Next() {
		var t StatusTask
		if err := rows.Scan(&t.ID, &t.Title, &t.Class, &t.State, &t.Branch); err != nil {
			return nil, fmt.Errorf("list the tasks of plan %d: %w", plan, err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the tasks of plan %d: %w", plan, err)
	}
	return tasks, nil
}

// needsYouEventKinds are the event kinds whose payload carries a parked
// task's reason (§10). The set is the queue's vocabulary: a task is in
// the queue because one of these parked it — except that
// task_check_failed and review_changes also land without parking, as
// the history's record of a failed check or a changes verdict that a
// fix round then answered (§3); the queue reads them only for a task
// already parked, whose latest of these kinds they may still be.
var needsYouEventKinds = []string{
	evTaskCheckFailed, evTaskNeedsHelp, evTaskFailed, evTaskOutOfRounds, evReviewChanges, evReviewNoVerdict,
}

// statusNeedsYouTx lists the "needs you" queue (§10): the tasks parked
// at needs_foreman or needs_user — each with what its latest parking
// event said — and the plans waiting for approval. Entries read in id
// order; project scopes the queue to one project, 0 to all.
func statusNeedsYouTx(tx *sql.Tx, project int64) ([]NeedsYouItem, error) {
	kinds := "'" + strings.Join(needsYouEventKinds, "', '") + "'"
	query := fmt.Sprintf(`SELECT t.id, t.plan_id, t.title, t.state, COALESCE(e.kind, ''), COALESCE(e.payload, '')
		FROM tasks t
		JOIN plans p ON p.id = t.plan_id
		LEFT JOIN events e ON e.id = (
			SELECT e2.id FROM events e2
			WHERE e2.task_id = t.id AND e2.kind IN (%s)
			ORDER BY e2.id DESC LIMIT 1)
		WHERE t.state IN ('needs_foreman', 'needs_user')`, kinds)
	args := []any{}
	if project != 0 {
		query += " AND p.project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY t.id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the needs-you tasks: %w", err)
	}
	items := []NeedsYouItem{}
	for rows.Next() {
		var it NeedsYouItem
		var kind, payload string
		if err := rows.Scan(&it.ID, &it.PlanID, &it.Title, &it.State, &kind, &payload); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the needs-you tasks: %w", err)
		}
		it.Kind = "task"
		it.Reason = needsYouReason(kind, payload)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list the needs-you tasks: %w", err)
	}
	rows.Close()

	pquery := `SELECT id, title FROM plans WHERE state = 'pending_approval'`
	pargs := []any{}
	if project != 0 {
		pquery += " AND project_id = ?"
		pargs = append(pargs, project)
	}
	pquery += " ORDER BY id"
	prows, err := tx.Query(pquery, pargs...)
	if err != nil {
		return nil, fmt.Errorf("list the plans awaiting approval: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var it NeedsYouItem
		if err := prows.Scan(&it.ID, &it.Title); err != nil {
			return nil, fmt.Errorf("list the plans awaiting approval: %w", err)
		}
		it.Kind = "plan"
		it.State = "pending_approval"
		items = append(items, it)
	}
	if err := prows.Err(); err != nil {
		return nil, fmt.Errorf("list the plans awaiting approval: %w", err)
	}
	return items, nil
}

// needsYouReason renders one parked task's reason from its latest
// parking event's payload (§10): the worker's question for a needs_help,
// the review's findings for review changes, the named reason — the
// check's tail, the failure's words, the no-verdict's — for the rest,
// the event kind itself when the payload names nothing. A task with no
// parking event — a needs_user parked by a later path — shows no
// reason. The line flattens to one line of at most about
// needsYouReasonLimit bytes.
func needsYouReason(kind, payload string) string {
	if payload == "" {
		return ""
	}
	var p eventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return oneLine(kind, needsYouReasonLimit)
	}
	reason := p.Reason
	switch kind {
	case evTaskNeedsHelp:
		reason = p.Question
	case evReviewChanges:
		reason = p.Findings
	}
	if reason == "" {
		reason = kind
	}
	return oneLine(reason, needsYouReasonLimit)
}

// oneLine flattens s to a single line of at most about max bytes:
// whitespace runs become one space — an event's tail may carry newlines
// — and the cut, when it lands inside the limit, backs off to a
// complete rune.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return cutRune(s, max)
}

// RecordInstance notes which process is the engine and since when
// (§13): the lifecycle writes it once it holds the single-instance lock,
// and `brigada status` reads it through the API.
func (e *Engine) RecordInstance(ctx context.Context, pid int64, started time.Time) error {
	return e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE meta SET engine_pid = ?, engine_started_at = ? WHERE id = 1",
			pid, fmtTime(started)); err != nil {
			return fmt.Errorf("record the engine instance: %w", err)
		}
		return nil
	})
}

// LiveJobs counts the live jobs — queued, leased and running — which is
// the §13 idle rule's "no live jobs": a job waiting for a runner counts
// as work exactly like one a runner holds.
func (e *Engine) LiveJobs(ctx context.Context) (int, error) {
	var n int
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE status IN ('queued', 'leased', 'running')").Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("count live jobs: %w", err)
	}
	return n, nil
}
