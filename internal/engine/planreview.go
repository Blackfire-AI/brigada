package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// The plan review (§6.2): a plan — and an amendment of an active plan
// that adds or edits tasks — is reviewed by an agent of a family other than the
// foreman's before any of its work is queued. The plan waits in
// planReviewing with all its tasks pending and one plan_review job on
// the key plan_review:<plan>; the verdict is the reviewer's result block
// (§7's verdict block), approve or changes with findings. Approve runs
// the approval rules (§6.3) as a no-review creation does; changes
// carries the findings to the foreman, whose amendment starts the next
// round. At most two rounds: the second changes verdict proceeds through
// the same evaluation as approve, the findings recorded and shown. A
// reviewer that cannot be had — no agent of another family offers the
// review class, or the job never runs, runs past its hard cap or loses
// its runner too often — proceeds the same way. A reviewer that runs and
// gives no verdict is retried once on another agent, as a task review is
// (§7); a second run with no verdict keeps the plan reviewing and wakes
// the foreman, whose amendment answers it.

// maxPlanReviewRounds is the plan review's round cap (§6.2): the
// creation's review, and one more round after a changes verdict the
// foreman answered with an amendment.
const maxPlanReviewRounds = 2

// planReviewKey is the plan review job's key: one live job per plan, so
// a plan is never reviewed twice concurrently.
func planReviewKey(plan int64) string {
	return fmt.Sprintf("plan_review:%d", plan)
}

// planReviewWanted reports whether a plan of these tasks is reviewed
// before it starts (§6.2): the project turned it off, or the plan
// is exactly one task of the review class — a review of a branch, which
// needs no review of its own.
func planReviewWanted(pr planReviewSettings, tasks []PlanTask) bool {
	if !pr.On {
		return false
	}
	if len(tasks) == 1 {
		class := tasks[0].Class
		if class == "" {
			class = classImplement
		}
		if class == string(KindReview) {
			return false
		}
	}
	return true
}

// planReviewTaskView is one task of the plan under review, as the input
// names it: the spec verbatim, the check, the class, the tier and the
// effort, and the graph — deps by name, the base and the continued dep.
type planReviewTaskView struct {
	ID       int64
	Name     string
	Title    string
	Spec     string
	Check    string
	Class    string
	Tier     string
	Effort   string
	Deps     []string
	Base     string
	Continue string
}

// planReviewPlanView is the plan under review: its id, title and tasks.
type planReviewPlanView struct {
	ID    int64
	Title string
	Tasks []planReviewTaskView
}

// otherPlanTaskView is one task of another active plan, as the input's
// summary names it: its state and its result branch.
type otherPlanTaskView struct {
	ID     int64
	Name   string
	State  string
	Branch string
}

// otherPlanView is one other active plan of the project.
type otherPlanView struct {
	ID    int64
	Title string
	Tasks []otherPlanTaskView
}

// composePlanReviewInput builds the plan review's input (§6.2): the
// plan — every task with its spec verbatim, its check, class, tier,
// effort and place in the graph — and a summary of the project's other
// active plans' tasks and branches, so the reviewer sees what the plan
// touches beside itself. A round past the first also carries the
// previous round's verdict and findings. A pure function: the same views
// render the same input.
func composePlanReviewInput(round int, plan planReviewPlanView, others []otherPlanView, prevVerdict, prevFindings string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan review input, round %d of %d.\n", round, maxPlanReviewRounds)
	fmt.Fprintf(&b, "The plan under review is plan %d %q, %d tasks.\n", plan.ID, plan.Title, len(plan.Tasks))
	if prevVerdict != "" {
		b.WriteString("\nThe previous round's verdict was " + prevVerdict + ".\n")
		if prevFindings != "" {
			b.WriteString("Its findings, verbatim:\n" + prevFindings + "\n")
		}
		b.WriteString("\n")
	}
	for _, t := range plan.Tasks {
		label := fmt.Sprintf("task %d", t.ID)
		if t.Name != "" {
			label += " " + t.Name
		}
		class := t.Class
		if class == "" {
			class = classImplement
		}
		tier := t.Tier
		if tier == "" {
			tier = adapter.TierStandard
		}
		effort := t.Effort
		if effort == "" {
			effort = "default"
		}
		fmt.Fprintf(&b, "\n%s: class %s, tier %s, effort %s.\n", label, class, tier, effort)
		if len(t.Deps) > 0 {
			graph := "it waits on " + strings.Join(t.Deps, ", ")
			if t.Base != "" {
				graph += "; its work starts from " + t.Base
			}
			if t.Continue != "" {
				graph += "; it continues " + t.Continue
			}
			b.WriteString(graph + ".\n")
		}
		check := t.Check
		if check == "" {
			check = "none"
		}
		b.WriteString("check: " + check + "\n")
		b.WriteString("spec, verbatim:\n" + t.Spec + "\n")
	}
	if len(others) == 0 {
		b.WriteString("\nThe project has no other active plans.\n")
	} else {
		b.WriteString("\nThe project's other active plans, summarized:\n")
		for _, o := range others {
			fmt.Fprintf(&b, "plan %d %q, %d tasks.\n", o.ID, o.Title, len(o.Tasks))
			for _, t := range o.Tasks {
				label := fmt.Sprintf("task %d", t.ID)
				if t.Name != "" {
					label += " " + t.Name
				}
				state := t.State
				if t.Branch != "" {
					state += ", branch " + t.Branch
				}
				b.WriteString("  " + label + ": " + state + ".\n")
			}
		}
	}
	return b.String()
}

// foremanFamilyTx reads the family a plan review must differ from
// (§6.2): the family of the project's foreman agent, the one
// registered for the project's foreman harness and model. A project with
// no foreman chosen, or whose pair no runner advertises, has no family to
// exclude: any agent offering the review class may review.
func foremanFamilyTx(tx *sql.Tx, project int64) (string, error) {
	var harness, model string
	if err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", project).
		Scan(&harness, &model); err != nil {
		return "", fmt.Errorf("read the foreman of project %d: %w", project, err)
	}
	if harness == "" || model == "" {
		return "", nil
	}
	var family string
	err := tx.QueryRow("SELECT family FROM agents WHERE harness = ? AND model = ? ORDER BY id LIMIT 1", harness, model).
		Scan(&family)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the foreman's agent of project %d: %w", project, err)
	}
	return family, nil
}

// planReviewerAvailableTx reports whether any registered agent could
// take the project's plan review: the same structural test a review
// job's placement applies (§7), over the foreman's family exclusion and
// the review class, health and cooling aside — a broken or cooling
// reviewer is a wait, a missing one is not.
func planReviewerAvailableTx(tx *sql.Tx, project int64, family string) (bool, error) {
	review := &jobRow{ProjectID: project, Kind: KindPlanReview, ExcludeFamily: family}
	agents, err := qualifyingAgentRowsTx(tx, 0, review, string(KindReview), "")
	if err != nil {
		return false, err
	}
	return len(agents) > 0, nil
}

// planReviewTierTx reads the tier a plan review asks for (§6.2):
// the project's plan_review.tier setting, else standard.
func planReviewTierTx(tx *sql.Tx, cfg *Config, j *jobRow) (string, error) {
	settings, err := projectApprovalTx(tx, j.ProjectID)
	if err != nil {
		return "", err
	}
	return planReviewOf(cfg, settings).Tier, nil
}

// planReviewPlanTx loads the plan under review as the input's views: its
// title, and each task with the spec verbatim, the check, the class, the
// tier and effort, and the graph by name.
func planReviewPlanTx(tx *sql.Tx, plan int64) (planReviewPlanView, error) {
	var view planReviewPlanView
	if err := tx.QueryRow("SELECT id, title FROM plans WHERE id = ?", plan).Scan(&view.ID, &view.Title); err != nil {
		return planReviewPlanView{}, fmt.Errorf("read plan %d: %w", plan, err)
	}
	rows, err := tx.Query(`SELECT t.id, t.name, t.title, t.spec, COALESCE(t.check_cmd, ''), t.class,
			t.tier, COALESCE(NULLIF(t.effort, ''), 'default')
		FROM tasks t WHERE t.plan_id = ? ORDER BY t.id`, plan)
	if err != nil {
		return planReviewPlanView{}, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
	}
	var tasks []planReviewTaskView
	for rows.Next() {
		var t planReviewTaskView
		if err := rows.Scan(&t.ID, &t.Name, &t.Title, &t.Spec, &t.Check, &t.Class, &t.Tier, &t.Effort); err != nil {
			rows.Close()
			return planReviewPlanView{}, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return planReviewPlanView{}, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
	}
	rows.Close()
	idx := map[int64]int{}
	for i := range tasks {
		idx[tasks[i].ID] = i
	}
	edges, err := tx.Query(`SELECT d.task_id, x.id, x.name, d.is_base, d.is_continue
		FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
		WHERE d.plan_id = ? ORDER BY d.task_id, x.id`, plan)
	if err != nil {
		return planReviewPlanView{}, fmt.Errorf("read the dependencies of plan %d: %w", plan, err)
	}
	defer edges.Close()
	for edges.Next() {
		var owner, dep int64
		var name string
		var isBase, isContinue bool
		if err := edges.Scan(&owner, &dep, &name, &isBase, &isContinue); err != nil {
			return planReviewPlanView{}, fmt.Errorf("read the dependencies of plan %d: %w", plan, err)
		}
		i, ok := idx[owner]
		if !ok {
			continue
		}
		if name != "" {
			tasks[i].Deps = append(tasks[i].Deps, name)
		} else {
			tasks[i].Deps = append(tasks[i].Deps, fmt.Sprintf("#%d", dep))
		}
		if isBase {
			tasks[i].Base = name
		}
		if isContinue {
			tasks[i].Continue = name
		}
	}
	view.Tasks = tasks
	return view, edges.Err()
}

// otherActivePlansTx summarizes the project's other active plans (§6.2):
// each task's state and result branch, the context the reviewer
// reads beside the plan under review.
func otherActivePlansTx(tx *sql.Tx, project, plan int64) ([]otherPlanView, error) {
	rows, err := tx.Query(`SELECT id, title FROM plans WHERE project_id = ? AND state = 'active' AND id <> ? ORDER BY id`, project, plan)
	if err != nil {
		return nil, fmt.Errorf("read the active plans of project %d: %w", project, err)
	}
	var plans []otherPlanView
	for rows.Next() {
		var o otherPlanView
		if err := rows.Scan(&o.ID, &o.Title); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the active plans of project %d: %w", project, err)
		}
		plans = append(plans, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the active plans of project %d: %w", project, err)
	}
	rows.Close()
	for i := range plans {
		tasks, err := tx.Query(`SELECT t.id, t.name, t.state, COALESCE(t.branch, '')
			FROM tasks t WHERE t.plan_id = ? ORDER BY t.id`, plans[i].ID)
		if err != nil {
			return nil, fmt.Errorf("read the tasks of plan %d: %w", plans[i].ID, err)
		}
		for tasks.Next() {
			var t otherPlanTaskView
			if err := tasks.Scan(&t.ID, &t.Name, &t.State, &t.Branch); err != nil {
				tasks.Close()
				return nil, fmt.Errorf("read the tasks of plan %d: %w", plans[i].ID, err)
			}
			plans[i].Tasks = append(plans[i].Tasks, t)
		}
		if err := tasks.Err(); err != nil {
			tasks.Close()
			return nil, fmt.Errorf("read the tasks of plan %d: %w", plans[i].ID, err)
		}
		tasks.Close()
	}
	return plans, nil
}

// planReviewBatchJSON encodes the task ids a round covers; "" names the
// whole plan, as a creation's review does.
func planReviewBatchJSON(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(b)
}

// planReviewBatchTx reads the batch a round covers, nil for the whole
// plan.
func planReviewBatchTx(tx *sql.Tx, plan int64) ([]int64, error) {
	var raw string
	if err := tx.QueryRow("SELECT review_batch FROM plans WHERE id = ?", plan).Scan(&raw); err != nil {
		return nil, fmt.Errorf("read the review batch of plan %d: %w", plan, err)
	}
	if raw == "" {
		return nil, nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil, fmt.Errorf("parse the review batch of plan %d: %w", plan, err)
	}
	return ids, nil
}

// queuePlanReviewRoundTx queues the plan's review round (§6.2): one
// plan_review job on the plan's key, placed on an agent of a family other
// than the foreman's, carrying the input composed from the plan and the
// project's other active plans. The one-live-job-per-key rule keeps a
// plan from being reviewed twice concurrently: a submit that coalesced
// behind a live review queues nothing and changes nothing.
func (e *Engine) queuePlanReviewRoundTx(tx *sql.Tx, project, plan, round int64, batch []int64, now time.Time) error {
	family, err := foremanFamilyTx(tx, project)
	if err != nil {
		return err
	}
	available, err := planReviewerAvailableTx(tx, project, family)
	if err != nil {
		return err
	}
	if !available {
		// The reviewer cannot be run (§6.2): the plan proceeds
		// through the same evaluation as approve, the reason recorded.
		// The round and its batch are written first, so the evaluation
		// judges what this round covers — an amendment's tasks alone,
		// or the whole plan — not an earlier round's batch.
		if _, err := tx.Exec(`UPDATE plans SET review_round = ?, review_batch = ?, updated_at = ? WHERE id = ?`,
			round, planReviewBatchJSON(batch), fmtTime(now), plan); err != nil {
			return fmt.Errorf("record the review round of plan %d: %w", plan, err)
		}
		reason := "no agent of a family other than the foreman's offers the review class"
		if family == "" {
			reason = "no agent offers the review class"
		}
		return e.planReviewConcludedTx(tx, project, plan, planReviewSkipped, "", reason, now, true)
	}
	view, err := planReviewPlanTx(tx, plan)
	if err != nil {
		return err
	}
	others, err := otherActivePlansTx(tx, project, plan)
	if err != nil {
		return err
	}
	prevVerdict, prevFindings := "", ""
	if round > 1 {
		if err := tx.QueryRow("SELECT review_verdict, review_findings FROM plans WHERE id = ?", plan).
			Scan(&prevVerdict, &prevFindings); err != nil {
			return fmt.Errorf("read the review of plan %d: %w", plan, err)
		}
	}
	text := prompt.ComposePlanReviewPrompt(composePlanReviewInput(int(round), view, others, prevVerdict, prevFindings))
	sr, err := e.submitTx(tx, SubmitParams{
		Key:           planReviewKey(plan),
		ProjectID:     project,
		Kind:          KindPlanReview,
		Mode:          ModeFresh,
		Prompt:        text,
		ExcludeFamily: family,
	}, now)
	if err != nil {
		return err
	}
	if sr.Answer != AnsCreated {
		// A live review holds the key: nothing to queue, nothing to
		// change — the running round is the plan's review.
		return nil
	}
	if _, err := tx.Exec(`UPDATE plans SET review_round = ?, review_verdict = '', review_findings = '', review_batch = ?, updated_at = ?
		WHERE id = ?`, round, planReviewBatchJSON(batch), fmtTime(now), plan); err != nil {
		return fmt.Errorf("record the review round of plan %d: %w", plan, err)
	}
	_, err = appendEventTx(tx, evPlanReviewQueued, project, 0, sr.JobID,
		eventPayload{Plan: plan, Run: round}, now)
	return err
}

// The plan review's verdict words: the reviewer's own (§7), and the
// engine's word for a round that never ran.
const (
	planReviewSkipped = "skipped"
	planReviewNoRun   = "no_verdict"
)

// planReviewBeginTx starts the plan's review at round 1 over a batch of
// task ids (nil = the whole plan), on a plan that just entered
// planReviewing.
func (e *Engine) planReviewBeginTx(tx *sql.Tx, project, plan int64, batch []int64, now time.Time) error {
	return e.queuePlanReviewRoundTx(tx, project, plan, 1, batch, now)
}

// planReviewAnswerTx queues the next round after the foreman's amendment
// answered a changes verdict (§6.2): over the whole plan again when the
// review is a creation's, so its verdict still runs the whole-plan rules
// (§6.3), else over the tasks the earlier round covered and the
// amendment's added and respecified ones. An
// amendment that adds and respecifies none — a cancel, a re-point, a
// retier — asks for no review, and the plan proceeds through the same
// evaluation as approve, as it does past the round cap; a round still
// running stays the plan's review (amendPlanTx refuses new tasks or
// specs while it runs).
func (e *Engine) planReviewAnswerTx(tx *sql.Tx, project, plan int64, batch []int64, now time.Time) error {
	var live int
	if err := tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE key = ? AND status IN ('queued', 'leased', 'running')",
		planReviewKey(plan)).Scan(&live); err != nil {
		return fmt.Errorf("read the live review of plan %d: %w", plan, err)
	}
	if live > 0 {
		return nil
	}
	var round int64
	if err := tx.QueryRow("SELECT review_round FROM plans WHERE id = ?", plan).Scan(&round); err != nil {
		return fmt.Errorf("read the review round of plan %d: %w", plan, err)
	}
	if round >= maxPlanReviewRounds {
		return e.planReviewConcludedTx(tx, project, plan, planReviewSkipped, "",
			"the plan review's rounds are spent; the plan proceeds", now, true)
	}
	if len(batch) == 0 {
		return e.planReviewConcludedTx(tx, project, plan, planReviewSkipped, "",
			"the amendment added or respecified no task; the plan proceeds", now, true)
	}
	reviewed, err := planReviewBatchTx(tx, plan)
	if err != nil {
		return err
	}
	switch {
	case reviewed == nil:
		batch = nil // a creation's review: the whole plan, as amended
	default:
		// An amendment's review covers every task its rounds held,
		// the earlier round's with the answer's, so none starts at
		// the verdict without its per-task rules.
		seen := make(map[int64]bool, len(reviewed)+len(batch))
		var union []int64
		for _, id := range append(reviewed, batch...) {
			if !seen[id] {
				seen[id] = true
				union = append(union, id)
			}
		}
		batch = union
	}
	return e.queuePlanReviewRoundTx(tx, project, plan, round+1, batch, now)
}

// planReviewCompletedTx applies a plan_review job's completion (§6.2):
// the verdict block read as a task review reads it (§7). Approve
// runs the approval rules as a no-review creation does; changes carries
// the findings to the foreman and waits for its amendment, unless the
// rounds are spent, when the plan proceeds with the findings recorded;
// anything else — no block, a malformed one, an approve without
// substantive findings, a run that ended in an error or a limit — is no
// verdict, retried once on another reviewer and then held for the
// foreman (planReviewFailedTx).
func (e *Engine) planReviewCompletedTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	plan := planReviewOfJobTx(job)
	if plan == 0 {
		return nil
	}
	var state string
	if err := tx.QueryRow("SELECT state FROM plans WHERE id = ?", plan).Scan(&state); err != nil {
		return fmt.Errorf("read plan %d: %w", plan, err)
	}
	if state != planReviewing {
		return nil // a late answer for a plan the foreman or user ended
	}
	var v verdictBlock
	haveVerdict := c.Error == "" && json.Unmarshal([]byte(c.Result), &v) == nil &&
		(v.Verdict == verdictApprove || v.Verdict == verdictChanges)
	switch {
	case haveVerdict && v.Verdict == verdictApprove && len(v.Findings) >= minSubstantiveFindings:
		// A substantive approve counts on the reviewer's outcome
		// record, as a task review's does (§7).
		if err := recordOutcomeTx(tx, job, now); err != nil {
			return err
		}
		return e.planReviewConcludedTx(tx, job.ProjectID, plan, verdictApprove, v.Findings, "", now, false)
	case haveVerdict && v.Verdict == verdictChanges:
		return e.planReviewConcludedTx(tx, job.ProjectID, plan, verdictChanges, v.Findings, "", now, false)
	default:
		reason := "no verdict in the plan review result"
		switch {
		case c.Error != "":
			reason = c.Error
		case haveVerdict:
			reason = "approve without substantive findings"
		}
		return e.planReviewFailedTx(tx, job, plan, reason, now)
	}
}

// planReviewFailedTx answers a plan review that ran and gave no verdict
// as a task review's unusable answer is answered (§7): once, with
// another reviewer — the same question on the plan's key, the failed
// agent excluded — and when that retry also gives none, or no other
// agent can take it, the plan stays reviewing and the foreman is woken
// with the reason (planReviewHeldTx). The retry is the job whose
// ExcludeAgent is set: a plan_review job is never leased by an
// agent-less runner, so a first run always names its agent.
func (e *Engine) planReviewFailedTx(tx *sql.Tx, job *jobRow, plan int64, reason string, now time.Time) error {
	if job.ExcludeAgent != 0 || job.AgentID == 0 {
		return e.planReviewHeldTx(tx, job.ProjectID, plan, reason+"; the retry on another reviewer gave no verdict either", now)
	}
	retry := &jobRow{ProjectID: job.ProjectID, Kind: KindPlanReview, ExcludeFamily: job.ExcludeFamily, ExcludeAgent: job.AgentID}
	noAgent, err := noReviewAgentTx(tx, retry)
	if err != nil {
		return err
	}
	if noAgent {
		return e.planReviewHeldTx(tx, job.ProjectID, plan, reason+"; no other agent can take the retry", now)
	}
	sr, err := e.submitTx(tx, SubmitParams{
		Key:           job.Key,
		ProjectID:     job.ProjectID,
		Kind:          KindPlanReview,
		Mode:          ModeFresh,
		Prompt:        job.Prompt,
		ExcludeFamily: job.ExcludeFamily,
		ExcludeAgent:  job.AgentID,
	}, now)
	if err != nil {
		return err
	}
	if sr.Answer != AnsCreated {
		// The failed run freed the key earlier in this transaction: no
		// answer but a created job is expected.
		return fmt.Errorf("queue the review retry of plan %d: submit %s answered %s", plan, job.Key, sr.Answer)
	}
	var round int64
	if err := tx.QueryRow("SELECT review_round FROM plans WHERE id = ?", plan).Scan(&round); err != nil {
		return fmt.Errorf("read the review round of plan %d: %w", plan, err)
	}
	_, err = appendEventTx(tx, evPlanReviewQueued, job.ProjectID, 0, sr.JobID,
		eventPayload{Plan: plan, Run: round, Reason: boundNote(reason + "; retried on another reviewer")}, now)
	return err
}

// planReviewHeldTx keeps a plan whose review gave no verdict reviewing
// (§6.2): the verdict is recorded as no verdict, and the reason reaches
// the foreman with a wake. Its amendment answers: one that adds or
// respecifies tasks starts the next round while one is left, and one that
// adds and respecifies none — a cancel, a retier — lets the plan proceed.
func (e *Engine) planReviewHeldTx(tx *sql.Tx, project, plan int64, reason string, now time.Time) error {
	var round int64
	if err := tx.QueryRow("SELECT review_round FROM plans WHERE id = ?", plan).Scan(&round); err != nil {
		return fmt.Errorf("read the review round of plan %d: %w", plan, err)
	}
	if _, err := tx.Exec(`UPDATE plans SET review_verdict = ?, review_findings = '', updated_at = ? WHERE id = ?`,
		planReviewNoRun, fmtTime(now), plan); err != nil {
		return fmt.Errorf("record the review verdict of plan %d: %w", plan, err)
	}
	note := reason + "; the plan stays reviewing: amend it to answer — adding or respecifying tasks starts another round, " +
		"an amendment that adds and respecifies none (a cancel, a retier) lets the plan proceed"
	if err := inboxEventTx(tx, evPlanReviewed, project, 0, 0,
		eventPayload{Plan: plan, Status: planReviewNoRun, Reason: boundNote(note), Run: round}, now); err != nil {
		return err
	}
	return e.wakeForemanTx(tx, project, now)
}

// planReviewRecoveredTx requeues the review round of a plan recovery
// restored as reviewing (§6.4): the restored store holds no job for it,
// so without one the plan would wait forever. A live round is left as it
// is; a verdict the snapshot holds is older than the file and is not
// waited on — the round is asked again. A project whose review is now
// off proceeds at once, and a round no reviewer can take proceeds as
// queuePlanReviewRoundTx decides.
func (e *Engine) planReviewRecoveredTx(tx *sql.Tx, project, plan int64, now time.Time) error {
	live, err := liveJobTx(tx, planReviewKey(plan))
	if err != nil {
		return err
	}
	if live != nil {
		return nil
	}
	var round int64
	if err := tx.QueryRow("SELECT review_round FROM plans WHERE id = ?", plan).Scan(&round); err != nil {
		return fmt.Errorf("read the review round of plan %d: %w", plan, err)
	}
	round = max(round, 1)
	batch, err := planReviewBatchTx(tx, plan)
	if err != nil {
		return err
	}
	settings, err := projectApprovalTx(tx, project)
	if err != nil {
		return err
	}
	if !e.planReview(settings).On {
		if _, err := tx.Exec(`UPDATE plans SET review_round = ?, updated_at = ? WHERE id = ?`, round, fmtTime(now), plan); err != nil {
			return fmt.Errorf("record the review round of plan %d: %w", plan, err)
		}
		return e.planReviewConcludedTx(tx, project, plan, planReviewSkipped, "",
			"the project's plan review is off; the recovered plan proceeds", now, true)
	}
	return e.queuePlanReviewRoundTx(tx, project, plan, round, batch, now)
}

// planReviewOfJobTx reads the plan a plan_review job's key names; 0 when
// the key names none.
func planReviewOfJobTx(job *jobRow) int64 {
	id, err := strconv.ParseInt(strings.TrimPrefix(job.Key, "plan_review:"), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// planReviewConcludedTx records a verdict with its findings and applies
// it (§6.2): the plan leaves reviewing through the approval rules'
// evaluation, or — for a changes verdict with a round left — stays
// reviewing with the findings delivered to the foreman and a wake. The
// plan_reviewed event lands in the foreman's inbox whenever the foreman
// has a judgment to read: the findings it must answer, or a plan that
// now waits for the user's yes.
func (e *Engine) planReviewConcludedTx(tx *sql.Tx, project, plan int64, verdict, findings, note string, now time.Time, force bool) error {
	var round int64
	if err := tx.QueryRow("SELECT review_round FROM plans WHERE id = ?", plan).Scan(&round); err != nil {
		return fmt.Errorf("read the review round of plan %d: %w", plan, err)
	}
	findings = boundNote(findings)
	if verdict == verdictChanges && note == "" && round >= maxPlanReviewRounds && !force {
		note = "the review rounds are spent; the plan proceeds"
	}
	if _, err := tx.Exec(`UPDATE plans SET review_verdict = ?, review_findings = ?, updated_at = ? WHERE id = ?`,
		verdict, findings, fmtTime(now), plan); err != nil {
		return fmt.Errorf("record the review verdict of plan %d: %w", plan, err)
	}
	payload := eventPayload{Plan: plan, Status: verdict, Findings: findings, Reason: note, Run: round}
	settings, err := projectApprovalTx(tx, project)
	if err != nil {
		return err
	}
	// A changes verdict with a round left is the foreman's to answer:
	// the plan stays reviewing, the findings reach it, and its
	// amendment starts the next round. Everything else concludes.
	if verdict == verdictChanges && !force && round < maxPlanReviewRounds {
		if err := inboxEventTx(tx, evPlanReviewed, project, 0, 0, payload, now); err != nil {
			return err
		}
		return e.wakeForemanTx(tx, project, now)
	}
	batch, err := planReviewBatchTx(tx, plan)
	if err != nil {
		return err
	}
	state, err := e.planReviewAppliedTx(tx, project, plan, batch, settings, now)
	if err != nil {
		return err
	}
	if state != "active" || force || verdict != verdictApprove {
		// The foreman reads every verdict but a clean approve that
		// starts the plan: a changes it must answer or that proceeded
		// over spent rounds, a skipped or unusable review, and a plan
		// the rules now hold for the user's yes.
		if err := inboxEventTx(tx, evPlanReviewed, project, 0, 0, payload, now); err != nil {
			return err
		}
		return e.wakeForemanTx(tx, project, now)
	}
	_, err = appendEventTx(tx, evPlanReviewed, project, 0, 0, payload, now)
	return err
}

// planReviewAppliedTx moves a plan out of reviewing the way a no-review
// creation moves (§6.3): the approval rules over the whole
// plan when the round covered it — a creation's review — with an
// awaiting plan holding its tasks and its reasons event, an active one
// queueing its first wave; and over the batch alone when the round
// covered an amendment's added and respecified tasks, each released or
// held by the existing per-task rules while the plan's earlier work goes
// on. Returns the state the plan reached.
func (e *Engine) planReviewAppliedTx(tx *sql.Tx, project, plan int64, batch []int64, settings approvalSettings, now time.Time) (string, error) {
	nowStr := fmtTime(now)
	if _, err := tx.Exec(`UPDATE plans SET state = ?, review_batch = '', updated_at = ? WHERE id = ?`, "active", nowStr, plan); err != nil {
		return "", fmt.Errorf("activate plan %d: %w", plan, err)
	}
	if len(batch) == 0 {
		// The whole plan, as createPlanTx judges it: every task
		// against the rules, the whole-plan triggers included.
		nodes, err := loadAmendNodesTx(tx, plan)
		if err != nil {
			return "", err
		}
		var tasks []PlanTask
		for _, n := range nodes {
			if n.state != taskCancelled {
				tasks = append(tasks, PlanTask{Spec: n.spec, Check: n.check, Class: n.class, ExtraRepos: n.extra})
			}
		}
		verdict := settings.evaluate(tasks)
		if verdict.Needed {
			reasons := verdict.reasons(settings.Classes)
			if _, err := tx.Exec(`UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`,
				planAwaitingApproval, nowStr, plan); err != nil {
				return "", fmt.Errorf("hold plan %d for approval: %w", plan, err)
			}
			if _, err := appendEventTx(tx, evPlanAwaitingApproval, project, 0, 0, eventPayload{
				Plan:    plan,
				Count:   int64(len(tasks)),
				Classes: verdict.Classes,
				Paths:   verdict.Paths,
				Reason:  strings.Join(reasons, "; "),
			}, now); err != nil {
				return "", err
			}
			return planAwaitingApproval, nil
		}
	} else {
		// An amendment's batch: each of its tasks still pending is
		// released, or held alone by the per-task rules (§6.3), as an
		// amendment's own verdict would hold it.
		for _, id := range batch {
			var state, spec, check, class string
			var extraRec string
			err := tx.QueryRow(`SELECT state, spec, COALESCE(check_cmd, ''), class, COALESCE(extra_repos, '')
				FROM tasks WHERE id = ?`, id).Scan(&state, &spec, &check, &class, &extraRec)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("read task %d: %w", id, err)
			}
			if state != taskPending {
				continue
			}
			var extra []ExtraRepo
			if specRec := parseExtraReposRecord(extraRec); specRec != nil && specRec.Source == ExtraSourceTask {
				extra = specRec.Repos
			}
			reasons := settings.taskHoldReasons(PlanTask{Spec: spec, Check: check, Class: class, ExtraRepos: extra})
			if len(reasons) == 0 {
				continue
			}
			why := strings.Join(reasons, "; ")
			if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
				taskNeedsForeman, nowStr, id); err != nil {
				return "", fmt.Errorf("hold task %d at the plan gate: %w", id, err)
			}
			if _, err := appendEventTx(tx, evTaskPlanHeld, project, id, 0,
				eventPayload{Status: taskNeedsForeman, Reason: why}, now); err != nil {
				return "", err
			}
		}
	}
	if _, _, err := e.readyPlanTasksTx(tx, project, plan, now); err != nil {
		return "", err
	}
	closed, err := e.finishPlanIfClosedTx(tx, project, plan, 0, 0, now)
	if err != nil {
		return "", err
	}
	if closed {
		return planDone, nil
	}
	return "active", nil
}

// planReviewUnrunnableTx concludes the review of the plan a failed
// plan_review job names (§6.2): a job the queue TTL failed — no
// runner ever took it — means the reviewer cannot be run, and the plan
// proceeds through the same evaluation as approve with the reason
// recorded. A plan no longer reviewing is left alone.
func (e *Engine) planReviewUnrunnableTx(tx *sql.Tx, project int64, plan int64, reason string, now time.Time) error {
	var state string
	if err := tx.QueryRow("SELECT state FROM plans WHERE id = ?", plan).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read plan %d: %w", plan, err)
	}
	if state != planReviewing {
		return nil
	}
	return e.planReviewConcludedTx(tx, project, plan, planReviewSkipped, "", reason, now, true)
}

// cancelPlanReviewTx stops the plan's live plan_review job, as a
// cancelled plan cancels its tasks' jobs (§6.2).
func (e *Engine) cancelPlanReviewTx(tx *sql.Tx, plan int64, now time.Time) error {
	job, err := liveJobTx(tx, planReviewKey(plan))
	if err != nil {
		return err
	}
	if job == nil {
		return nil
	}
	_, err = e.cancelTx(tx, job, "plan cancelled", now)
	return err
}
