package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// statusFinishedKeep is how many finished plans the default status
// lists (§10): the newest done or cancelled ones — enough to see what
// landed lately — while the rest stay behind the all view, counted in
// one summary line. A foreman's session is resumed on every wake, so
// an unbounded list would grow its transcript without end.
const statusFinishedKeep = 3

// needsYouReasonLimit is the most a needs-you entry's reason line
// carries (§10): one line, about this many bytes, never a paragraph.
const needsYouReasonLimit = 160

// lastEventLimit is the most a running job's last event carries in the
// status views (§10): one line, about this many bytes.
const lastEventLimit = 120

// statusReportLines is how much of the latest foreman report the status
// shows (§10): the first few lines, the rest left to the history.
// statusReportBytes is the most report text the block carries: about
// what the event's old shared 500-byte bound kept, so even a report of
// one long line stays as short as it always was. The event keeps the
// report whole within its own bound (boundReport); the cut happens
// here, at render time, and a cut text ends with the pointer to the
// history (§9).
const (
	statusReportLines = 5
	statusReportBytes = 500
)

// reportPointer is what a cut view of a long text ends with — a foreman
// report, a review's findings: the command that shows the whole of it
// (§9).
func reportPointer(job int64) string {
	return fmt.Sprintf("… (full: brigada history --job %d)", job)
}

// statusReportView cuts the report to its first statusReportLines lines
// and about statusReportBytes — one long line is cut too — and, when it
// cut and the report names its job, ends the text with the pointer to
// the history (§9): the status block stays short while the event keeps
// the whole report.
func statusReportView(report string, job int64) string {
	text := cutRune(firstLines(report, statusReportLines), statusReportBytes)
	if text != report && job > 0 {
		return text + reportPointer(job)
	}
	return text
}

// StatusInfo is the engine's own state as the API shows it (§13): which
// process is the engine, since when, the jobs by status — and the plans
// (§6.1, §6.2) with the latest foreman report (§3, §10) and the "needs
// you" queue (§10): the tasks parked for a person and the plans waiting
// for approval. The pid and start are what the lifecycle recorded
// (RecordInstance); a store no engine of this binary has recorded shows
// zero values.
type StatusInfo struct {
	Pid       int64          `json:"pid"`
	StartedAt string         `json:"started_at"`
	Jobs      map[string]int `json:"jobs"`
	Running   []StatusJob    `json:"running"`
	// RunnerDraining is true while a runner has a restart drain pending
	// (§5.1): it finishes its running jobs and exits, and the status
	// renders the restart that waits behind them.
	RunnerDraining bool `json:"runner_draining,omitempty"`
	// ForemanMessages counts the user's messages waiting in the foreman
	// inbox (§4.2 item 3): queued while a chat is attached, a turn runs,
	// or the wake cannot happen; delivered ones are not counted.
	ForemanMessages int `json:"foreman_messages"`
	// Slots is who holds the foreman's live slot, per project (§4.6):
	// free, a headless turn or an attach, with the holder's label and
	// since when.
	Slots []Slot       `json:"slots"`
	Plans []StatusPlan `json:"plans"`
	// HiddenFinishedPlans counts the finished plans — done or
	// cancelled — the plans list left out (§10): the default keeps the
	// newest statusFinishedKeep, the all view lists every one. The
	// status views render it as the one summary line that says the
	// rest exist.
	HiddenFinishedPlans int            `json:"hidden_finished_plans,omitempty"`
	Report              *StatusReport  `json:"report,omitempty"`
	NeedsYou            []NeedsYouItem `json:"needs_you"`
	// Cooling lists the agents held back for quota (§8, §10): shown
	// beside the running jobs, never in the needs-you queue.
	Cooling []CoolingAgent `json:"cooling,omitempty"`
	// Held lists the queued jobs a full cap layer holds (§8.1); the
	// runner layer's are jobs waiting for a runner slot.
	Held []HeldJob `json:"held,omitempty"`
	// Instructions lists the projects in scope that have project
	// instructions (§5.9): source, version, size and digest; the text only
	// from StatusWithInstructions.
	Instructions []StatusInstructions `json:"instructions,omitempty"`
}

// StatusJob is one job a runner holds right now (§10): which job, for
// which task, of what kind, on which agent, since when, and the
// worker's latest progress event — what it is doing, in one line. A
// foreman turn has no task; an agent-less runner's job names no agent.
type StatusJob struct {
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id,omitempty"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Harness   string `json:"harness,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"` // the agent's effort level; shown as model@effort
	LeasedAt  string `json:"leased_at,omitempty"`
	LastEvent string `json:"last_event,omitempty"`
}

// StatusReport is the latest foreman report the status shows (§3, §10):
// the final message of the project's last done foreman turn — when it
// ended, and the first lines of what it said. A cut Text ends with
// `… (full: brigada history --job N)`, naming the job whose history
// holds the whole report (§9). The foreman token's scope is one
// project; a caller reading every project sees the latest report among
// them, the project named by its id.
type StatusReport struct {
	ProjectID int64  `json:"project_id"`
	JobID     int64  `json:"job_id"`
	CreatedAt string `json:"created_at"`
	Text      string `json:"text"`
}

// StatusPlan is one plan the status lists (§6.1, §6.2, §10): every
// live plan — active, reviewing, parked for the foreman, or waiting for
// the user's approval — and the newest finished ones, done or
// cancelled. The repo URL names whose plan it is; the tasks are the
// plan's, each with the state and branch the graph is in.
type StatusPlan struct {
	ID      int64  `json:"id"`
	RepoURL string `json:"repo_url"`
	Title   string `json:"title"`
	State   string `json:"state"`
	// ReviewRound, ReviewVerdict and ReviewFindings are the plan review's
	// state (§6.2): the round under way while the plan reviews, and
	// the latest verdict and findings once a round has ended. The
	// findings are the event's whole text; the short views that render
	// them cut at their own bound and point to ReviewJob's history.
	ReviewRound    int          `json:"review_round,omitempty"`
	ReviewVerdict  string       `json:"review_verdict,omitempty"`
	ReviewFindings string       `json:"review_findings,omitempty"`
	ReviewJob      int64        `json:"review_job,omitempty"`
	Tasks          []StatusTask `json:"tasks"`
}

// StatusTask is one task of a status plan (§6.1).
type StatusTask struct {
	Tier          string `json:"tier,omitempty"`
	EffectiveTier string `json:"effective_tier,omitempty"`
	Effort        string `json:"effort,omitempty"`
	// EffectiveEffort is the effort the escalation ladder runs the task
	// at (§8); it equals Effort until an effort step.
	EffectiveEffort string `json:"effective_effort,omitempty"`
	// ReviewTier and ReviewEffort are the task's own tier and effort for its
	// reviews (§7, §8); empty = the engine default (review.tier,
	// review.effort), which status leaves unsaid.
	ReviewTier   string `json:"review_tier,omitempty"`
	ReviewEffort string `json:"review_effort,omitempty"`
	// Caps are the task's own round-cap overrides (§6.1); absent keys run
	// at the engine default.
	Caps   map[string]int `json:"caps,omitempty"`
	ID     int64          `json:"id"`
	Title  string         `json:"title"`
	Class  string         `json:"class"`
	State  string         `json:"state"`
	Branch string         `json:"branch"` // empty while the task has no result branch yet
	// Landed is true when the task's landing is recorded and not waiting
	// at the push gate (§7.1): the land job squashed it, or a Brigada-Task
	// trailer on the default branch was detected and recorded.
	Landed bool `json:"landed,omitempty"`
	// WaitingOn names the unfinished dependencies of a task that waits
	// on them (§6.1): each by its plan name, or "#id" when unnamed.
	WaitingOn []string `json:"waiting_on,omitempty"`
	// Continues names the dependency whose author's session the task's
	// first work job resumes (§6.1), by plan name or "#id"; empty when
	// the task continues none.
	Continues string `json:"continues,omitempty"`
}

// NeedsYouItem is one entry of the "needs you" queue (§10): something a
// person must look at — a task parked at needs_foreman or needs_user, a
// plan waiting for approval, or a plan its review parked for the
// foreman (§6.2). Kind is "task" or "plan"; Reason is what the parked
// task's latest event said, flattened to one short line —
// the worker's question for a needs_help, the review's findings for
// review changes, the check's tail or the failure's words for the rest.
type NeedsYouItem struct {
	Kind   string `json:"kind"` // "task", "plan", "foreman" (a limited foreman, §4.4; ID is the project's, Title its repo URL) or "agent" (a broken one, §8; ID is the agent's, Title harness/model)
	ID     int64  `json:"id"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Reason string `json:"reason,omitempty"`
	PlanID int64  `json:"plan_id,omitempty"` // the task's plan, when the entry is a task
	// A plan awaiting approval also carries what a coordinator shows the
	// user and quotes back (§4.6): the project's repo URL, the
	// plan's version — an amendment bumps it — and its tasks.
	RepoURL string       `json:"repo_url,omitempty"`
	Version int64        `json:"version,omitempty"`
	Tasks   []StatusTask `json:"tasks,omitempty"`
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
// admin callers read all of them. The plans list carries every live plan
// and only the newest statusFinishedKeep finished ones; StatusAll lists
// every finished one.
func (e *Engine) Status(ctx context.Context, project int64) (StatusInfo, error) {
	return e.status(ctx, project, false, false)
}

// StatusWithInstructions is Status with the full project instruction text
// in StatusInfo.Instructions (§5.9): the foreman reads it as data before
// it plans. Plain Status names source, version, size and digest only.
func (e *Engine) StatusWithInstructions(ctx context.Context, project int64) (StatusInfo, error) {
	return e.status(ctx, project, true, false)
}

// StatusAll is Status with every finished plan listed, not only the
// newest statusFinishedKeep (§10): the status tools' all view — the
// MCP status tool's all parameter, `brigada status --all`. withText is
// StatusWithInstructions's flag.
func (e *Engine) StatusAll(ctx context.Context, project int64, withText bool) (StatusInfo, error) {
	return e.status(ctx, project, withText, true)
}

func (e *Engine) status(ctx context.Context, project int64, withText, all bool) (StatusInfo, error) {
	var info StatusInfo
	info.Running = []StatusJob{}
	info.Plans = []StatusPlan{}
	info.NeedsYou = []NeedsYouItem{}
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
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
		query := "SELECT status, COUNT(*) FROM jobs WHERE project_id IN (SELECT id FROM projects WHERE archived_at IS NULL)"
		var args []any
		if project != 0 {
			query += " AND project_id = ?"
			args = append(args, project)
		}
		rows, err := tx.Query(query+" GROUP BY status", args...)
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
		if info.Running, err = statusRunningTx(tx, project); err != nil {
			return err
		}
		if info.ForemanMessages, err = pendingUserMessagesTx(tx, project); err != nil {
			return err
		}
		if info.Slots, err = statusSlotsTx(tx, project, now); err != nil {
			return err
		}
		if info.Plans, info.HiddenFinishedPlans, err = statusPlansTx(tx, project, all); err != nil {
			return err
		}
		if info.Report, err = statusReportTx(tx, project); err != nil {
			return err
		}
		if info.NeedsYou, err = statusNeedsYouTx(tx, project, now); err != nil {
			return err
		}
		// A wake the day's budget holds is its own entry (§4.3): the
		// limited-foreman line above says the CLI's limit; this one says
		// the budget spent, and what to do about it.
		budgetItems, err := e.statusWakeBudgetTx(tx, project, now)
		if err != nil {
			return err
		}
		info.NeedsYou = append(info.NeedsYou, budgetItems...)
		if info.Cooling, err = statusCoolingTx(tx, now); err != nil {
			return err
		}
		if info.Held, err = statusHeldTx(tx, project); err != nil {
			return err
		}
		if info.Instructions, err = statusInstructionsTx(tx, project, withText); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return StatusInfo{}, fmt.Errorf("read status: %w", err)
	}
	// The drain mark is the engine's memory, not store state (§5.1), so
	// it is read beside the store's answer, after it.
	info.RunnerDraining = e.runnerDraining()
	return info, nil
}

// statusSlotsTx lists the foreman slot of one project, or of every
// project with 0 (§4.6).
func statusSlotsTx(tx *sql.Tx, project int64, now time.Time) ([]Slot, error) {
	query := "SELECT id FROM projects WHERE archived_at IS NULL"
	var args []any
	if project != 0 {
		query += " AND id = ?"
		args = append(args, project)
	}
	rows, err := tx.Query(query+" ORDER BY id", args...)
	if err != nil {
		return nil, fmt.Errorf("list the projects for the slots: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the projects for the slots: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list the projects for the slots: %w", err)
	}
	rows.Close()
	slots := []Slot{}
	for _, id := range ids {
		s, err := slotTx(tx, id, now)
		if err != nil {
			return nil, err
		}
		slots = append(slots, s)
	}
	return slots, nil
}

// pendingUserMessagesTx counts the user's messages still pending in the
// foreman inbox (§4.2 item 3), for one project or, with 0, every one.
func pendingUserMessagesTx(tx *sql.Tx, project int64) (int, error) {
	query := `SELECT COUNT(*) FROM foreman_inbox i JOIN events e ON e.id = i.event_id
		WHERE i.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL) AND i.state = ? AND e.kind = ?`
	args := []any{inboxPending, evUserMessage}
	if project != 0 {
		query += " AND i.project_id = ?"
		args = append(args, project)
	}
	var n int
	if err := tx.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the pending user messages: %w", err)
	}
	return n, nil
}

// statusRunningTx lists the jobs a runner holds (§10), oldest lease
// first: leased or running, with the agent they were dispatched to and
// the worker's latest progress event.
func statusRunningTx(tx *sql.Tx, project int64) ([]StatusJob, error) {
	query := `SELECT j.id, COALESCE(j.task_id, 0), j.kind, j.status,
			COALESCE(a.harness, ''), COALESCE(a.model, ''), COALESCE(a.effort, ''),
			COALESCE(j.last_leased, j.first_leased, ''), COALESCE(j.last_event, '')
		FROM jobs j LEFT JOIN agents a ON a.id = j.agent_id
		WHERE j.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL) AND j.status IN (?, ?)`
	args := []any{statusLeased, statusRunning}
	if project != 0 {
		query += " AND j.project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY COALESCE(j.last_leased, j.first_leased, j.created_at), j.id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the running jobs: %w", err)
	}
	defer rows.Close()
	jobs := []StatusJob{}
	for rows.Next() {
		var j StatusJob
		if err := rows.Scan(&j.ID, &j.TaskID, &j.Kind, &j.Status, &j.Harness, &j.Model, &j.Effort, &j.LeasedAt, &j.LastEvent); err != nil {
			return nil, fmt.Errorf("list the running jobs: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the running jobs: %w", err)
	}
	return jobs, nil
}

// statusPlansTx lists the plans the status shows (§6.1, §6.2, §10):
// every live plan — active, reviewing, parked for the foreman (§6.2),
// or waiting for the user's approval — and the newest
// statusFinishedKeep finished ones, done or cancelled, by
// their updated_at: a finished plan's updated_at is the moment it
// finished, nothing else writes one after. all lists every finished
// plan instead; the second return is how many finished plans the list
// left out, the default view's one summary line.
func statusPlansTx(tx *sql.Tx, project int64, all bool) ([]StatusPlan, int, error) {
	scope := "pr.archived_at IS NULL"
	args := []any{}
	if project != 0 {
		scope += " AND p.project_id = ?"
		args = append(args, project)
	}
	var finished int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM plans p JOIN projects pr ON pr.id = p.project_id
		WHERE `+scope+` AND p.state IN ('done', 'cancelled')`, args...).Scan(&finished); err != nil {
		return nil, 0, fmt.Errorf("count the finished plans: %w", err)
	}
	// The review's latest job is where the history holds the verdict
	// whole (§9): the short views that cut the findings point at it.
	planCols := `p.id, pr.repo_url, p.title, p.state, p.review_round, p.review_verdict, p.review_findings,
		COALESCE((SELECT j.id FROM jobs j WHERE j.key = 'plan_review:' || p.id ORDER BY j.id DESC LIMIT 1), 0)`
	finishedQuery := `SELECT ` + planCols + `
		FROM plans p JOIN projects pr ON pr.id = p.project_id
		WHERE ` + scope + ` AND p.state IN ('done', 'cancelled')
		ORDER BY p.updated_at DESC, p.id DESC`
	if !all {
		finishedQuery += fmt.Sprintf(" LIMIT %d", statusFinishedKeep)
	}
	fin, err := statusPlanRows(tx, finishedQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	live, err := statusPlanRows(tx, `SELECT `+planCols+`
		FROM plans p JOIN projects pr ON pr.id = p.project_id
		WHERE `+scope+` AND p.state IN ('active', 'reviewing', 'needs_foreman', 'pending_approval') ORDER BY p.id`, args...)
	if err != nil {
		return nil, 0, err
	}
	plans := append(fin, live...)
	sort.Slice(plans, func(i, j int) bool { return plans[i].ID < plans[j].ID })
	for i := range plans {
		if plans[i].Tasks, err = statusPlanTasksTx(tx, plans[i].ID); err != nil {
			return nil, 0, err
		}
	}
	return plans, finished - len(fin), nil
}

// statusPlanRows reads the plan rows a status query selects, in the
// query's own order; the tasks are the caller's to load.
func statusPlanRows(tx *sql.Tx, query string, args ...any) ([]StatusPlan, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the status plans: %w", err)
	}
	defer rows.Close()
	plans := []StatusPlan{}
	for rows.Next() {
		var p StatusPlan
		if err := rows.Scan(&p.ID, &p.RepoURL, &p.Title, &p.State, &p.ReviewRound, &p.ReviewVerdict, &p.ReviewFindings, &p.ReviewJob); err != nil {
			return nil, fmt.Errorf("list the status plans: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the status plans: %w", err)
	}
	return plans, nil
}

// statusPlanTasksTx reads one status plan's tasks, in id order.
func statusPlanTasksTx(tx *sql.Tx, plan int64) ([]StatusTask, error) {
	rows, err := tx.Query(`SELECT t.id, t.title, t.class, t.state, COALESCE(t.branch, ''), t.tier, t.effective_tier, t.effort, t.effective_effort, t.review_tier, t.review_effort, t.caps, COALESCE(l.pushed, '')
		FROM tasks t LEFT JOIN landings l ON l.task_id = t.id
		WHERE t.plan_id = ? ORDER BY t.id`, plan)
	if err != nil {
		return nil, fmt.Errorf("list the tasks of plan %d: %w", plan, err)
	}
	defer rows.Close()
	tasks := []StatusTask{}
	for rows.Next() {
		var t StatusTask
		var capsJSON, pushed string
		if err := rows.Scan(&t.ID, &t.Title, &t.Class, &t.State, &t.Branch, &t.Tier, &t.EffectiveTier, &t.Effort, &t.EffectiveEffort, &t.ReviewTier, &t.ReviewEffort, &capsJSON, &pushed); err != nil {
			return nil, fmt.Errorf("list the tasks of plan %d: %w", plan, err)
		}
		t.Caps = parseTaskCaps(capsJSON)
		// A landing recorded as waiting sits at the push gate, not yet
		// landed (§7.1); every other landing record — pushed or local, by
		// the land job or a detected trailer — marks the task landed.
		t.Landed = pushed != "" && pushed != "waiting"
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the tasks of plan %d: %w", plan, err)
	}
	rows.Close()
	for i := range tasks {
		if tasks[i].State == taskPending {
			if tasks[i].WaitingOn, err = statusWaitingOnTx(tx, tasks[i].ID); err != nil {
				return nil, err
			}
		}
		if tasks[i].Continues, err = statusContinuesTx(tx, tasks[i].ID); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

// statusWaitingOnTx names the dependencies of a task that are not done
// yet, in id order: the plan name, or "#id" for a task without one.
func statusWaitingOnTx(tx *sql.Tx, task int64) ([]string, error) {
	rows, err := tx.Query(`SELECT x.id, x.name FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
		WHERE d.task_id = ? AND x.state <> ? ORDER BY x.id`, task, taskDone)
	if err != nil {
		return nil, fmt.Errorf("list what task %d waits on: %w", task, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("list what task %d waits on: %w", task, err)
		}
		if name == "" {
			name = fmt.Sprintf("#%d", id)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// statusContinuesTx names the dependency whose author's session the task
// continues (§6.1), by plan name or "#id"; "" when it continues none.
func statusContinuesTx(tx *sql.Tx, task int64) (string, error) {
	var id int64
	var name string
	err := tx.QueryRow(`SELECT x.id, x.name FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
		WHERE d.task_id = ? AND d.is_continue = 1`, task).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read what task %d continues: %w", task, err)
	}
	if name == "" {
		name = fmt.Sprintf("#%d", id)
	}
	return name, nil
}

// statusReportTx reads the latest foreman report in scope (§3, §10):
// the newest foreman_report event of the project — of every project
// when the caller reads all — with the moment it landed and the first
// lines of its text. A scope with no report yet reads nil.
func statusReportTx(tx *sql.Tx, project int64) (*StatusReport, error) {
	query := `SELECT COALESCE(project_id, 0), COALESCE(job_id, 0), payload, created_at
		FROM events WHERE project_id IN (SELECT id FROM projects WHERE archived_at IS NULL) AND kind = ?`
	args := []any{evForemanReport}
	if project != 0 {
		query += " AND project_id = ?"
		args = append(args, project)
	}
	query += " ORDER BY id DESC LIMIT 1"
	var rep StatusReport
	var payload string
	err := tx.QueryRow(query, args...).Scan(&rep.ProjectID, &rep.JobID, &payload, &rep.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the latest foreman report: %w", err)
	}
	var p eventPayload
	if json.Unmarshal([]byte(payload), &p) == nil {
		rep.Text = statusReportView(p.Report, rep.JobID)
	}
	return &rep, nil
}

// firstLines keeps s's first max lines, the rest left to the history.
func firstLines(s string, max int) string {
	if max <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n")
}

// needsYouEventKinds are the event kinds whose payload carries a parked
// task's reason (§10). The set is the queue's vocabulary: a task is in
// the queue because one of these parked it — except that
// task_check_failed and review_changes also land without parking, as
// the history's record of a failed check or a changes verdict that a
// fix round then answered (§3); the queue reads them only for a task
// already parked, whose latest of these kinds they may still be.
var needsYouEventKinds = []string{
	evTaskCheckFailed, evTaskNeedsHelp, evTaskFailed, evTaskOutOfRounds, evTaskLoop, evReviewChanges, evReviewNoVerdict, evTaskLandFailed, evTaskPlanHeld,
}

// statusNeedsYouTx lists the "needs you" queue (§10): the tasks parked
// at needs_foreman or needs_user — each with what its latest parking
// event said — and the plans waiting for approval. Entries read in id
// order; project scopes the queue to one project, 0 to all.
func statusNeedsYouTx(tx *sql.Tx, project int64, now time.Time) ([]NeedsYouItem, error) {
	kinds := "'" + strings.Join(needsYouEventKinds, "', '") + "'"
	query := fmt.Sprintf(`SELECT t.id, t.plan_id, t.title, t.state, COALESCE(e.kind, ''), COALESCE(e.payload, '')
		FROM tasks t
		JOIN plans p ON p.id = t.plan_id
		LEFT JOIN events e ON e.id = (
			SELECT e2.id FROM events e2
			WHERE e2.task_id = t.id AND e2.kind IN (%s)
			ORDER BY e2.id DESC LIMIT 1)
		WHERE p.project_id IN (SELECT id FROM projects WHERE archived_at IS NULL) AND t.state IN ('needs_foreman', 'needs_user')`, kinds)
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

	// A task whose review no agent could ever take waits at reviewing:
	// listed beside the parked tasks until a reviewer registers.
	noReviewer, err := statusNoReviewerTx(tx, project)
	if err != nil {
		return nil, err
	}
	items = append(items, noReviewer...)

	// A task held at the land gate stays done; the hold's row is what
	// names it in the queue (§6.3).
	landHolds, err := statusLandHoldsTx(tx, project)
	if err != nil {
		return nil, err
	}
	items = append(items, landHolds...)

	// A landing recorded as waiting stays done too; it waits at the push
	// gate until `brigada push approve` (§6.3).
	pushWaits, err := statusPushWaitsTx(tx, project)
	if err != nil {
		return nil, err
	}
	items = append(items, pushWaits...)

	// A landing gate_push never recorded as local stays done too; its
	// push is the user's alone, so the queue carries the exact command
	// (§6.3).
	localLands, err := statusLocalLandsTx(tx, project)
	if err != nil {
		return nil, err
	}
	items = append(items, localLands...)

	pquery := `SELECT p.id, p.title, p.version, pr.repo_url, p.state FROM plans p JOIN projects pr ON pr.id = p.project_id
		WHERE pr.archived_at IS NULL AND p.state IN ('pending_approval', 'needs_foreman')`
	pargs := []any{}
	if project != 0 {
		pquery += " AND p.project_id = ?"
		pargs = append(pargs, project)
	}
	pquery += " ORDER BY p.id"
	prows, err := tx.Query(pquery, pargs...)
	if err != nil {
		return nil, fmt.Errorf("list the plans awaiting a decision: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var it NeedsYouItem
		if err := prows.Scan(&it.ID, &it.Title, &it.Version, &it.RepoURL, &it.State); err != nil {
			return nil, fmt.Errorf("list the plans awaiting a decision: %w", err)
		}
		it.Kind = "plan"
		items = append(items, it)
	}
	if err := prows.Err(); err != nil {
		return nil, fmt.Errorf("list the plans awaiting a decision: %w", err)
	}
	prows.Close()
	// Why each waits (§6.3, §6.2): the awaiting plan's
	// plan_awaiting_approval event's words; the parked plan's findings,
	// cut to the queue's one line and, when the cut landed, pointing at
	// the history that holds them whole.
	for i := range items {
		if items[i].Kind != "plan" {
			continue
		}
		if items[i].State == planNeedsForeman {
			reason, job, err := parkedPlanReasonTx(tx, items[i].ID)
			if err != nil {
				return nil, err
			}
			items[i].Reason = oneLine(reason, needsYouReasonLimit)
			if flat := strings.Join(strings.Fields(reason), " "); job > 0 && len(flat) > needsYouReasonLimit {
				items[i].Reason += reportPointer(job)
			}
			continue
		}
		reason, err := awaitingReasonTx(tx, items[i].ID)
		if err != nil {
			return nil, err
		}
		items[i].Reason = oneLine(reason, needsYouReasonLimit)
		if items[i].Tasks, err = statusPlanTasksTx(tx, items[i].ID); err != nil {
			return nil, err
		}
	}

	// A limited foreman is waiting for the user's choice (§4.4): wait,
	// switch it, or let approved plans run without it. A reset time that
	// passed is not listed; the next wake clears it.
	lquery := `SELECT id, repo_url, foreman_limited FROM projects WHERE archived_at IS NULL AND foreman_limited IS NOT NULL`
	largs := []any{}
	if project != 0 {
		lquery += " AND id = ?"
		largs = append(largs, project)
	}
	lquery += " ORDER BY id"
	lrows, err := tx.Query(lquery, largs...)
	if err != nil {
		return nil, fmt.Errorf("list the limited foremen: %w", err)
	}
	defer lrows.Close()
	for lrows.Next() {
		var it NeedsYouItem
		var state string
		if err := lrows.Scan(&it.ID, &it.Title, &state); err != nil {
			return nil, fmt.Errorf("list the limited foremen: %w", err)
		}
		it.Kind = "foreman"
		it.State = "limited"
		if state == foremanLimitUnknown {
			it.Reason = "limited, reset time unknown"
		} else if t, err := parseTime(state); err == nil && t.After(now) {
			it.Reason = "limited until " + state
		} else {
			continue
		}
		items = append(items, it)
	}
	if err := lrows.Err(); err != nil {
		return nil, fmt.Errorf("list the limited foremen: %w", err)
	}
	lrows.Close()

	// Two projects that name one repository were left apart at startup
	// (design §13.1); a person decides which stays.
	dups, err := duplicateProjectItemsTx(tx, project)
	if err != nil {
		return nil, err
	}
	items = append(items, dups...)

	// A broken agent takes nothing until it is cleared — a repeated
	// failure's probe, a runner's return, or the user's reset (§8
	// "Health"). Agents belong to no project, so only the unscoped view
	// lists them. Cooling is not here: it waits for a clock.
	if project == 0 {
		brows, err := tx.Query(`SELECT a.id, a.harness, a.model, a.health_reason, r.started_at
			FROM agents a JOIN runners r ON r.id = a.runner_id WHERE a.health = 'broken' ORDER BY a.id`)
		if err != nil {
			return nil, fmt.Errorf("list the broken agents: %w", err)
		}
		defer brows.Close()
		for brows.Next() {
			var it NeedsYouItem
			var harness, model, started string
			if err := brows.Scan(&it.ID, &harness, &model, &it.Reason, &started); err != nil {
				return nil, fmt.Errorf("list the broken agents: %w", err)
			}
			it.Reason = brokenReason(it.Reason, harness, started)
			it.Kind = "agent"
			it.State = "broken"
			it.Title = harness + "/" + model
			items = append(items, it)
		}
		if err := brows.Err(); err != nil {
			return nil, fmt.Errorf("list the broken agents: %w", err)
		}
	}
	return items, nil
}

// brokenReason is the needs-you reason for a broken agent. An auth break
// on a harness whose credential comes from the environment names the
// variable and the time the runner process started, so a runner started
// without the variable shows; every other reason is unchanged.
func brokenReason(reason, harness, started string) string {
	env := adapter.CredentialEnv(harness)
	if reason != brokenAuth || env == "" {
		return reason
	}
	out := reason + ": " + env + " is read from the runner's environment"
	if started == "" {
		return out + "; the runner has not reported its start"
	}
	return out + "; the runner started " + started
}

// awaitingReasonTx reads the reasons the plan's latest
// plan_awaiting_approval event gave (§6.3); "" for a plan with no such
// event.
func awaitingReasonTx(tx *sql.Tx, plan int64) (string, error) {
	rows, err := tx.Query(`SELECT payload FROM events WHERE kind = ? ORDER BY id DESC`, evPlanAwaitingApproval)
	if err != nil {
		return "", fmt.Errorf("read why plan %d awaits approval: %w", plan, err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return "", fmt.Errorf("read why plan %d awaits approval: %w", plan, err)
		}
		var p eventPayload
		if json.Unmarshal([]byte(payload), &p) == nil && p.Plan == plan {
			return p.Reason, nil
		}
	}
	return "", rows.Err()
}

// parkedPlanReasonTx reads why a plan its review parked waits (§6.2):
// the findings of its latest plan_reviewed event, else the event's own
// reason, with the job whose history holds the verdict whole.
func parkedPlanReasonTx(tx *sql.Tx, plan int64) (reason string, job int64, err error) {
	var payload string
	var jobID sql.NullInt64
	err = tx.QueryRow(`SELECT payload, job_id FROM events
		WHERE kind = ? AND json_extract(payload, '$.plan') = ? ORDER BY id DESC LIMIT 1`,
		evPlanReviewed, plan).Scan(&payload, &jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("read why plan %d is parked: %w", plan, err)
	}
	var p eventPayload
	if json.Unmarshal([]byte(payload), &p) != nil {
		return "", jobID.Int64, nil
	}
	job = jobID.Int64
	if job == 0 {
		job = p.Job
	}
	reason = p.Findings
	if reason == "" {
		reason = p.Reason
	}
	return reason, job, nil
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
	if text, ok := checkEventText(kind, p); ok {
		reason = text
	}
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

// LiveAttaches counts the live attach holds (§4.2 item 1), which is the
// §13 idle rule's "attached sessions": a heartbeated hold is a user's
// open chat with the foreman, and the engine that served the attach
// stays — the hold's token, its events tool and its wake rule all
// answer on this engine's socket. A hold past its expiry is the sweep's
// to settle, not a session still attached.
func (e *Engine) LiveAttaches(ctx context.Context) (int, error) {
	var n int
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM attach_holds WHERE state = 'live' AND expires_at > ?",
			fmtTime(e.cfg.Now())).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("count live attach holds: %w", err)
	}
	return n, nil
}
