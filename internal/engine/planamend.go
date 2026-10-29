package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// classPlan is the outcome class of the planner itself (§8):
// each plan's completion counts one attempt on it for the project's
// foreman agent. It is measurement only — no dispatch behavior keys off
// it yet: the lease ranks agents by the class of the task's own jobs, and
// no job carries this class.
const classPlan = "plan"

// PlanRetarget re-points one waiting task's dependencies (§6.1): Deps
// replaces the task's whole dependency set, an empty list drops them all.
// Task and Deps name tasks of the plan by name, or by id as a decimal.
type PlanRetarget struct {
	Task string
	Deps []string
	// Base names the one of Deps the task stacks on, as PlanTask.Base
	// does; '' = the default rule. The task's earlier base goes with
	// its earlier dependencies.
	Base string
	// Continue names the one of Deps whose author's session the task
	// continues, as PlanTask.Continue does; '' = the earlier continued
	// dep goes with the earlier dependencies.
	Continue string
}

// PlanRetier changes a waiting or parked task's tier, effort, review tier,
// review effort, caps or — for a task that has not started — skills.
// Empty fields preserve their current values; Task accepts a plan task's
// name or decimal id.
type PlanRetier struct {
	Task         string
	Tier         string
	Effort       string
	ReviewTier   string
	ReviewEffort string
	// Caps merges key by key into the task's own caps (fix_rounds,
	// review_rounds, restart; 0 to 10); nil changes none.
	Caps map[string]int
	// Skills, in order, replaces the task's recorded skills (§5.10) when
	// the task has not started; nil keeps them. The re-resolution copies
	// the current texts.
	Skills []string
}

// Spec edit modes of a PlanRespec.
const (
	RespecReplace = "replace"
	RespecAppend  = "append"
)

// PlanRespec edits the spec of a pending task, one with no job yet: Mode
// replace (the default) swaps the whole spec for Spec, append adds Spec
// after the current one. Task accepts a plan task's name or decimal id.
// The edit re-resolves the task's role texts and skills (§5.9, §5.10):
// Skills, in order, replaces the recorded skills — nil keeps them — and
// the current texts are copied whatever Skills says.
type PlanRespec struct {
	Task   string
	Spec   string
	Mode   string
	Skills []string
}

// PlanAmendment is what the foreman changes in a live plan (§12.2): tasks
// to add (their Deps name tasks of the plan, old or added), tasks to
// cancel, waiting tasks whose dependencies are re-pointed, waiting or
// parked tasks whose tier or effort is changed, and pending tasks whose
// spec is edited.
type PlanAmendment struct {
	Add      []PlanTask
	Cancel   []string
	Retarget []PlanRetarget
	Retier   []PlanRetier
	Respec   []PlanRespec
	// SkipApproved says the user themself approved the cancellations of
	// this amendment: only a user's token can say so, never the foreman's
	// own. Without it a cancelled task is an abandoned one, and the
	// planner's outcome counts it against the plan.
	SkipApproved bool
}

// HeldTask is one task the amendment added or respecified that a plan rule held (§6.3):
// it parks at needs_foreman with Reason — the rule's name — while the
// plan stays active and the rest runs on.
type HeldTask struct {
	TaskID int64
	Name   string
	Reason string
}

// AmendPlanResult is what AmendPlan did: the plan's state after it, the
// added tasks, the ids cancelled, re-pointed or retiered, and every task the
// amendment started (a work job queued), added or not. Reasons says why
// the plan waits for approval, when it does; Held names the added or
// respecified tasks a plan rule held. Respecced lists the tasks whose spec
// was edited.
type AmendPlanResult struct {
	PlanID     int64
	State      string
	Added      []CreatedTask
	Cancelled  []int64
	Retargeted []int64
	Retiered   []int64
	Respecced  []int64
	Started    []CreatedTask
	Held       []HeldTask
	Reasons    []string
}

// ErrPlanNotLive refuses an amendment of a plan that is done or cancelled.
var ErrPlanNotLive = errors.New("plan is not live")

// AmendPlan is the foreman's plan_amend (§12.2): add tasks, cancel tasks
// re-point dependencies, change waiting or parked tasks' tier or effort, and
// replace or append to a pending task's spec, validated whole — names
// unique, deps in the plan and live, no cycle — before anything is
// written. Cancelling a task stops its live jobs. In the same transaction
// the waiting state re-evaluates as creation does (a pending task whose
// dependencies are all done turns ready and queues its work job), the
// approval limit is evaluated again for the amended plan, and a plan the
// amendment closed is finished. One plan_amended event names the change.
// project scopes the plan to one project; 0 names any.
func (e *Engine) AmendPlan(ctx context.Context, project, plan int64, am PlanAmendment) (AmendPlanResult, error) {
	var res AmendPlanResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.amendPlanTx(tx, project, plan, am, now)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return AmendPlanResult{}, fmt.Errorf("amend plan %d: %w", plan, err)
	}
	return res, nil
}

// amendNode is one task of the plan as the amendment sees it: id 0 for a
// task the amendment adds.
type amendNode struct {
	id                      int64
	name, state             string
	spec, check, class      string
	title                   string
	deps                    []int // indexes into the node list
	base                    int   // the named base, an index into the node list; -1 = none named
	cont                    int   // the named continued dep, an index into the node list; -1 = none named
	cancelNow, retargetedBy bool
	skipApproved            bool // a cancelled task whose skip the user approved
	// extra is the list the task itself named (§5.8), nil when it inherits
	// the project default; extraRec is the record an added task carries.
	extra    []ExtraRepo
	extraRec string
}

func (n amendNode) label() string {
	switch {
	case n.name != "" && n.id != 0:
		return fmt.Sprintf("task %d %s", n.id, n.name)
	case n.name != "":
		return n.name
	case n.id != 0:
		return fmt.Sprintf("task %d", n.id)
	}
	return "an added task"
}

func (e *Engine) amendPlanTx(tx *sql.Tx, project, plan int64, am PlanAmendment, now time.Time) (AmendPlanResult, error) {
	if len(am.Add) == 0 && len(am.Cancel) == 0 && len(am.Retarget) == 0 && len(am.Retier) == 0 && len(am.Respec) == 0 {
		return AmendPlanResult{}, fmt.Errorf("nothing to amend: give tasks to add, cancel, retarget, retier or respec")
	}
	var proj int64
	var state, baseRef string
	err := tx.QueryRow(`SELECT p.project_id, p.state, j.default_branch FROM plans p
		JOIN projects j ON j.id = p.project_id WHERE p.id = ?`, plan).Scan(&proj, &state, &baseRef)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && project != 0 && proj != project) {
		return AmendPlanResult{}, ErrNoPlan
	}
	if err != nil {
		return AmendPlanResult{}, fmt.Errorf("read plan %d: %w", plan, err)
	}
	if err := activeProjectTx(tx, proj); err != nil {
		return AmendPlanResult{}, err
	}
	if state != "active" && state != planAwaitingApproval && state != planReviewing && state != planNeedsForeman {
		return AmendPlanResult{}, fmt.Errorf("%w: plan %d is %s; only an active, awaiting, reviewing or parked plan is amended", ErrPlanNotLive, plan, state)
	}
	// A running review round reads the plan as it was queued (§6.2):
	// tasks added or respecified under it stay pending and wait for the
	// round's verdict, which queues the next round over them — approve
	// included — instead of starting their work.
	reviewRoundLive := false
	if state == planReviewing && (len(am.Add) > 0 || len(am.Respec) > 0) {
		live, err := liveJobTx(tx, planReviewKey(plan))
		if err != nil {
			return AmendPlanResult{}, err
		}
		reviewRoundLive = live != nil
	}

	nodes, err := loadAmendNodesTx(tx, plan)
	if err != nil {
		return AmendPlanResult{}, err
	}
	existing := len(nodes)
	byID := map[int64]int{}
	byName := map[string]int{}
	for i, n := range nodes {
		byID[n.id] = i
		if n.name != "" {
			byName[n.name] = i
		}
	}
	settings, err := projectApprovalTx(tx, proj)
	if err != nil {
		return AmendPlanResult{}, err
	}
	// A task the amendment adds starts from the project's land target
	// when one is set (§6.1, §6.3), as a created task does; the default
	// branch read above is the fallback.
	if t := settings.LandTarget; t != "" {
		baseRef = t
	}
	am.Add = settings.withDefaultCheck(am.Add)
	// The extra repositories (§5.8) of an added task resolve now, as the
	// default check does.
	extraDef, err := projectExtraReposTx(tx, proj)
	if err != nil {
		return AmendPlanResult{}, err
	}
	var addExtraRecs []string
	am.Add, addExtraRecs, err = resolveTaskExtras(extraDef, am.Add, func(i int) string { return fmt.Sprintf("added task %d", i+1) })
	if err != nil {
		return AmendPlanResult{}, err
	}
	// What an added task's prompts carry resolves here, as a created
	// task's does (§5.9, §5.10), before anything is written.
	addSnaps := make([]TaskInstructions, len(am.Add))
	for i, t := range am.Add {
		if err := validateTaskTierEffort(t); err != nil {
			return AmendPlanResult{}, fmt.Errorf("added task %d %q: %w", i+1, t.Name, err)
		}
		if err := validateTaskCaps(t.Caps); err != nil {
			return AmendPlanResult{}, fmt.Errorf("added task %d %q: %w", i+1, t.Name, err)
		}
		if t.Spec == "" {
			return AmendPlanResult{}, fmt.Errorf("added task %d: spec is required: the worker's task, verbatim (§1.4)", i+1)
		}
		label := t.Name
		if label == "" {
			label = fmt.Sprintf("added task %d", i+1)
		}
		ti, terr := resolveTaskInstructionsTx(tx, proj, label, t.Skills)
		if terr != nil {
			return AmendPlanResult{}, terr
		}
		addSnaps[i] = ti
		if t.Name != "" {
			if j, dup := byName[t.Name]; dup {
				return AmendPlanResult{}, fmt.Errorf("task name %q is already used by %s: names are unique in a plan", t.Name, nodes[j].label())
			}
			byName[t.Name] = len(nodes)
		}
		class := t.Class
		if class == "" {
			class = classImplement
		}
		title := t.Title
		if title == "" {
			title = taskTitleOf(t.Spec)
		}
		nodes = append(nodes, amendNode{base: -1, cont: -1, name: t.Name, state: taskPending, spec: t.Spec, check: t.Check, class: class, title: title,
			extra: t.ExtraRepos, extraRec: addExtraRecs[i]})
	}
	resolve := func(ref string) (int, error) {
		if i, ok := byName[ref]; ok {
			return i, nil
		}
		if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
			if i, ok := byID[id]; ok {
				return i, nil
			}
		}
		return 0, fmt.Errorf("%q names no task of plan %d", ref, plan)
	}
	// liveDeps resolves a dependency list; a dependency that ended failed
	// or cancelled will never be done and is refused. A ref naming no
	// task of the plan may name a landed task of another plan (§6.1):
	// already satisfied, so no edge waits on it.
	liveDeps := func(owner int, refs []string) ([]int, error) {
		var out []int
		seen := map[int]bool{}
		for _, ref := range refs {
			j, err := resolve(ref)
			if err != nil {
				sat, why, cerr := crossPlanDepTx(tx, proj, ref)
				if cerr != nil {
					return nil, cerr
				}
				if sat {
					continue
				}
				if why != "" {
					return nil, fmt.Errorf("%s depends on %q: %s", nodes[owner].label(), ref, why)
				}
				return nil, fmt.Errorf("%s depends on %w", nodes[owner].label(), err)
			}
			if j == owner {
				return nil, fmt.Errorf("%s depends on itself", nodes[owner].label())
			}
			if s := nodes[j].state; s == taskCancelled || s == taskFailed {
				return nil, fmt.Errorf("%s depends on %s, which is %s and will never be done: re-point it or drop the dependency", nodes[owner].label(), nodes[j].label(), s)
			}
			if !seen[j] {
				seen[j] = true
				out = append(out, j)
			}
		}
		return out, nil
	}

	var cancelled []int
	for _, ref := range am.Cancel {
		i, err := resolve(ref)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("cancel: %w", err)
		}
		switch {
		case i >= existing:
			return AmendPlanResult{}, fmt.Errorf("cancel: %s is added by this same amendment", nodes[i].label())
		case nodes[i].state == taskDone:
			return AmendPlanResult{}, fmt.Errorf("cancel: %s is done", nodes[i].label())
		case nodes[i].state == taskCancelled || nodes[i].cancelNow:
			continue
		}
		nodes[i].skipApproved = am.SkipApproved
		nodes[i].cancelNow = true
		nodes[i].state = taskCancelled
		cancelled = append(cancelled, i)
	}
	var retargeted []int
	// A re-point may also free a stranded done task (§6.2): one whose
	// landing can never happen because a dependency was cancelled or
	// failed. The blockage is read as the closing rule reads it, over
	// the node set the cancels above already shaped.
	strandedNow, err := planStrandedTx(tx, nodes)
	if err != nil {
		return AmendPlanResult{}, err
	}
	for _, rt := range am.Retarget {
		i, err := resolve(rt.Task)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("retarget: %w", err)
		}
		if i >= existing {
			return AmendPlanResult{}, fmt.Errorf("retarget: %s is added by this same amendment", nodes[i].label())
		}
		if nodes[i].state != taskPending {
			// The one done task that may be re-pointed (§6.2) is a
			// stranded one: its approved result stands, its branch is
			// unchanged, and dropping or replacing the dead dependency
			// makes it landable without running again. A task stacked
			// on the dead dependency's branch cannot (§7.1): its diff
			// is built on the cancelled work, and a follow-up task on
			// a new base is the way.
			if _, stranded := strandedNow[i]; !stranded {
				return AmendPlanResult{}, fmt.Errorf("retarget: %s is %s; only a waiting (pending) task's dependencies are re-pointed", nodes[i].label(), nodes[i].state)
			}
			stacked, serr := stackedOnDeadDepTx(tx, nodes, i)
			if serr != nil {
				return AmendPlanResult{}, serr
			}
			if stacked >= 0 {
				return AmendPlanResult{}, fmt.Errorf("retarget: %s is stacked on %s, which is %s: it carries or relies on the cancelled work and must be restarted on a new base by a follow-up task",
					nodes[i].label(), nodes[stacked].label(), nodes[stacked].state)
			}
		}
		if nodes[i].retargetedBy {
			return AmendPlanResult{}, fmt.Errorf("retarget: %s is re-pointed twice", nodes[i].label())
		}
		nodes[i].retargetedBy = true
		retargeted = append(retargeted, i)
	}
	var retiered []int
	seenRetier := map[int]bool{}
	for _, rt := range am.Retier {
		i, err := resolve(rt.Task)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("retier: %w", err)
		}
		switch nodes[i].state {
		case taskPending, taskReady, taskNeedsForeman:
		default:
			return AmendPlanResult{}, fmt.Errorf("retier: %s is %s; only pending, ready or needs_foreman tasks can change tier or effort", nodes[i].label(), nodes[i].state)
		}
		if i >= existing || seenRetier[i] {
			return AmendPlanResult{}, fmt.Errorf("retier: %s is added or retiered twice in this amendment", nodes[i].label())
		}
		if err := adapter.ValidateTier(rt.Tier); err != nil {
			return AmendPlanResult{}, fmt.Errorf("retier %s: %w", nodes[i].label(), err)
		}
		if err := adapter.ValidateTaskEffort(rt.Effort); err != nil {
			return AmendPlanResult{}, fmt.Errorf("retier %s: %w", nodes[i].label(), err)
		}
		if err := validateReviewTierEffort(rt.ReviewTier, rt.ReviewEffort); err != nil {
			return AmendPlanResult{}, fmt.Errorf("retier %s: %w", nodes[i].label(), err)
		}
		if err := validateTaskCaps(rt.Caps); err != nil {
			return AmendPlanResult{}, fmt.Errorf("retier %s: %w", nodes[i].label(), err)
		}
		seenRetier[i] = true
		retiered = append(retiered, i)
	}
	// A spec edit takes a task that has no job yet: only a pending one.
	// respecText is what the edit adds, which is all the plan rules judge.
	var respecced []int
	respecText := map[int]string{}
	respecSkills := map[int][]string{}
	for _, rs := range am.Respec {
		i, err := resolve(rs.Task)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("respec: %w", err)
		}
		if i >= existing {
			return AmendPlanResult{}, fmt.Errorf("respec: %s is added by this same amendment; give its final spec in add", nodes[i].label())
		}
		if nodes[i].state != taskPending {
			return AmendPlanResult{}, fmt.Errorf("respec: %s is %s; only a pending task, with no job yet, has its spec changed: for a task that has started, add a follow-up task", nodes[i].label(), nodes[i].state)
		}
		if _, dup := respecText[i]; dup {
			return AmendPlanResult{}, fmt.Errorf("respec: %s is respecified twice in this amendment", nodes[i].label())
		}
		if strings.TrimSpace(rs.Spec) == "" {
			return AmendPlanResult{}, fmt.Errorf("respec %s: spec is required", nodes[i].label())
		}
		switch rs.Mode {
		case "", RespecReplace:
			nodes[i].spec = rs.Spec
		case RespecAppend:
			nodes[i].spec = strings.TrimRight(nodes[i].spec, "\n") + "\n\n" + rs.Spec
		default:
			return AmendPlanResult{}, fmt.Errorf("respec %s: mode %q is not replace or append", nodes[i].label(), rs.Mode)
		}
		respecText[i] = rs.Spec
		respecSkills[i] = rs.Skills
		respecced = append(respecced, i)
	}
	// Dependencies resolve after the cancels, against the whole amended
	// node set, so a task cannot wait on one this amendment cancels.
	// pickBase resolves a named base, which must be one of the owner's
	// deps; -1 when none is named. pickContinue does the same for a
	// named continued dep. alignContinue then applies the base rule
	// (§6.1): the continued dep is the task's base, so a base naming
	// another dep is refused and an unnamed base defaults to it.
	pickBase := func(owner int, ref string) (int, error) {
		if ref == "" {
			return -1, nil
		}
		if j, err := resolve(ref); err == nil {
			for _, d := range nodes[owner].deps {
				if d == j {
					return j, nil
				}
			}
		}
		if err := CheckBaseFetchable(ref); err != nil {
			return -1, fmt.Errorf("%s has %w", nodes[owner].label(), err)
		}
		return -1, fmt.Errorf("%s has base %q, which is not one of its deps: a base is a task it depends on", nodes[owner].label(), ref)
	}
	pickContinue := func(owner int, ref string) (int, error) {
		if ref == "" {
			return -1, nil
		}
		if j, err := resolve(ref); err == nil {
			for _, d := range nodes[owner].deps {
				if d == j {
					return j, nil
				}
			}
		}
		return -1, fmt.Errorf("%s continues %q, which is not one of its deps: a continued dep is a task it depends on", nodes[owner].label(), ref)
	}
	alignContinue := func(owner, base, cont int, baseRef, contRef string) (int, error) {
		switch {
		case cont < 0:
			return base, nil
		case base < 0:
			return cont, nil
		case base != cont:
			return -1, fmt.Errorf("%s has base %q but continues %q: a continuing task's base is its continued dep",
				nodes[owner].label(), baseRef, contRef)
		}
		return base, nil
	}
	for _, i := range retargeted {
		var refs []string
		var base, cont string
		for _, rt := range am.Retarget {
			if j, _ := resolve(rt.Task); j == i {
				refs, base, cont = rt.Deps, rt.Base, rt.Continue
			}
		}
		deps, err := liveDeps(i, refs)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("retarget: %w", err)
		}
		nodes[i].deps = deps
		if nodes[i].base, err = pickBase(i, base); err != nil {
			return AmendPlanResult{}, fmt.Errorf("retarget: %w", err)
		}
		if nodes[i].cont, err = pickContinue(i, cont); err != nil {
			return AmendPlanResult{}, fmt.Errorf("retarget: %w", err)
		}
		if nodes[i].base, err = alignContinue(i, nodes[i].base, nodes[i].cont, base, cont); err != nil {
			return AmendPlanResult{}, fmt.Errorf("retarget: %w", err)
		}
	}
	for k, t := range am.Add {
		i := existing + k
		deps, err := liveDeps(i, t.Deps)
		if err != nil {
			return AmendPlanResult{}, err
		}
		nodes[i].deps = deps
		if nodes[i].base, err = pickBase(i, t.Base); err != nil {
			return AmendPlanResult{}, err
		}
		if nodes[i].cont, err = pickContinue(i, t.Continue); err != nil {
			return AmendPlanResult{}, err
		}
		if nodes[i].base, err = alignContinue(i, nodes[i].base, nodes[i].cont, t.Base, t.Continue); err != nil {
			return AmendPlanResult{}, err
		}
	}

	// The whole graph again, as plan_create validates: no cycle. Names
	// and deps were checked above; the search runs on node indexes, so no
	// user name can collide with anything.
	graph := make([][]int, len(nodes))
	for i, n := range nodes {
		graph[i] = n.deps
	}
	if c := findCycle(graph); c != nil {
		names := make([]string, len(c))
		for k, idx := range c {
			names[k] = nodes[idx].label()
		}
		return AmendPlanResult{}, fmt.Errorf("dependency cycle: %s", strings.Join(names, " -> "))
	}

	// Cancelling with dependents (§6.2): a cancelled task must not leave
	// a dependent pointing at it. A dependent is covered in this same
	// amendment only by being cancelled too or — when pending, with no
	// job yet — re-pointed off the cancelled task, which the dep
	// resolution above already moved. A dependent that already ended
	// cancelled or failed cannot continue work, so it never blocks.
	// Every other state has a job, a branch or an approved result built
	// on the cancelled task's branch, and the whole amendment is
	// refused, each offender named with its state.
	for _, i := range cancelled {
		var offenders []string
		for d := range nodes {
			if d == i || !slices.Contains(nodes[d].deps, i) {
				continue
			}
			if nodes[d].state == taskCancelled || nodes[d].state == taskFailed {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s (%s)", nodes[d].label(), nodes[d].state))
		}
		if len(offenders) > 0 {
			return AmendPlanResult{}, fmt.Errorf("cancel: %s has dependents built on its branch: %s; cancel each in this same amendment, or re-point a pending one off it",
				nodes[i].label(), strings.Join(offenders, ", "))
		}
	}

	// The approval rules (§6.3) over the amendment. An active plan is
	// judged by what it adds: only the added tasks and the text a respec
	// adds are checked against the per-task rules — a class outside the
	// allowed set, a protected path, an extra repository (added tasks
	// only), a check other than the project's default_check — and one
	// that trips is held alone, its
	// own needs-you line naming the rule, while the plan stays active.
	// The whole-plan triggers (the task count, gate_plan) do not apply to
	// a plan already running. An awaiting plan is re-evaluated whole, as
	// at creation, and stays awaiting. An active plan the project reviews
	// (§6.2) holds its added and respecified tasks pending instead:
	// the plan turns reviewing, the per-task rules wait for the verdict,
	// and a reviewing plan amended is the foreman's answer to a changes
	// verdict, which starts the next round.
	review := e.planReview(settings)
	amendReview := review.On && state == "active" && (len(am.Add) > 0 || len(respecced) > 0)
	var kept []PlanTask
	for _, n := range nodes {
		if n.state != taskCancelled {
			kept = append(kept, PlanTask{Spec: n.spec, Check: n.check, Class: n.class, ExtraRepos: n.extra})
		}
	}
	newState := state
	// A parked plan's amendment is the foreman's answer to the changes
	// verdict that parked it (§6.2): the answer re-enters the review —
	// round 2 — and the review's own conclusion sets the state the plan
	// reaches; the review code below runs after this write.
	if state == planNeedsForeman {
		newState = planReviewing
	}
	var verdict approvalVerdict
	addedHold := make([]string, len(am.Add)) // per added task, the hold reasons joined; "" when none
	respecHold := map[int]string{}           // per respecified task, likewise
	switch {
	case amendReview:
		newState = planReviewing
	case state == "active":
		for _, i := range respecced {
			if reasons := settings.taskHoldReasons(PlanTask{Spec: respecText[i], Check: nodes[i].check, Class: nodes[i].class}); len(reasons) > 0 {
				respecHold[i] = strings.Join(reasons, "; ")
			}
		}
		for k := range am.Add {
			n := nodes[existing+k]
			if reasons := settings.taskHoldReasons(PlanTask{Spec: n.spec, Check: n.check, Class: n.class, ExtraRepos: n.extra}); len(reasons) > 0 {
				addedHold[k] = strings.Join(reasons, "; ")
			}
		}
	default:
		if state == planAwaitingApproval {
			verdict = settings.evaluate(kept)
		}
	}

	// Write. Cancels first: a cancelled task's live jobs stop with it.
	res := AmendPlanResult{PlanID: plan, State: newState}
	nowStr := fmtTime(now)
	for _, i := range cancelled {
		if err := e.cancelTaskJobsTx(tx, nodes[i].id, now); err != nil {
			return AmendPlanResult{}, err
		}
		if _, err := tx.Exec(`UPDATE tasks SET state = ?, skip_approved = ?, updated_at = ? WHERE id = ?`,
			taskCancelled, nodes[i].skipApproved, nowStr, nodes[i].id); err != nil {
			return AmendPlanResult{}, fmt.Errorf("cancel task %d: %w", nodes[i].id, err)
		}
		res.Cancelled = append(res.Cancelled, nodes[i].id)
	}
	for k, i := range retiered {
		rt := am.Retier[k]
		// A new effort sets the effective effort with it; a tier change
		// alone resets the effective effort to the task's own (§8: the
		// escalation ladder starts over on the new tier).
		if _, err := tx.Exec(`UPDATE tasks SET
			tier = CASE WHEN ? = '' THEN tier ELSE ? END,
			effective_tier = CASE WHEN ? = '' THEN effective_tier ELSE ? END,
			effective_effort = CASE WHEN ? <> '' THEN ? WHEN ? <> '' THEN effort ELSE effective_effort END,
			effort = CASE WHEN ? = '' THEN effort ELSE ? END,
			review_tier = CASE WHEN ? = '' THEN review_tier ELSE ? END,
			review_effort = CASE WHEN ? = '' THEN review_effort ELSE ? END,
			updated_at = ? WHERE id = ?`,
			rt.Tier, rt.Tier, rt.Tier, rt.Tier, rt.Effort, rt.Effort, rt.Tier, rt.Effort, rt.Effort,
			rt.ReviewTier, rt.ReviewTier, rt.ReviewEffort, rt.ReviewEffort, nowStr, nodes[i].id); err != nil {
			return AmendPlanResult{}, fmt.Errorf("retier %s: %w", nodes[i].label(), err)
		}
		if len(rt.Caps) > 0 {
			var recorded string
			if err := tx.QueryRow(`SELECT caps FROM tasks WHERE id = ?`, nodes[i].id).Scan(&recorded); err != nil {
				return AmendPlanResult{}, fmt.Errorf("retier %s: read caps: %w", nodes[i].label(), err)
			}
			if _, err := tx.Exec(`UPDATE tasks SET caps = ?, updated_at = ? WHERE id = ?`,
				mergeTaskCaps(recorded, rt.Caps), nowStr, nodes[i].id); err != nil {
				return AmendPlanResult{}, fmt.Errorf("retier %s: %w", nodes[i].label(), err)
			}
		}
		// A task that has not started takes the current texts and
		// skills (§5.9 'Versioning'); a started task keeps its record.
		started, serr := taskStartedTx(tx, nodes[i].id)
		if serr != nil {
			return AmendPlanResult{}, fmt.Errorf("retier %s: %w", nodes[i].label(), serr)
		}
		if !started {
			if err := reresolveTaskInstructionsTx(tx, proj, nodes[i].id, nodes[i].label(), rt.Skills, now); err != nil {
				return AmendPlanResult{}, fmt.Errorf("retier %s: %w", nodes[i].label(), err)
			}
		}
		res.Retiered = append(res.Retiered, nodes[i].id)
	}
	for _, i := range respecced {
		taskState := taskPending
		if respecHold[i] != "" {
			taskState = taskNeedsForeman
		}
		if _, err := tx.Exec(`UPDATE tasks SET spec = ?, state = ?, updated_at = ? WHERE id = ?`,
			nodes[i].spec, taskState, nowStr, nodes[i].id); err != nil {
			return AmendPlanResult{}, fmt.Errorf("respec %s: %w", nodes[i].label(), err)
		}
		if respecHold[i] != "" {
			if _, err := appendEventTx(tx, evTaskPlanHeld, proj, nodes[i].id, 0,
				eventPayload{Status: taskNeedsForeman, Reason: respecHold[i]}, now); err != nil {
				return AmendPlanResult{}, err
			}
			res.Held = append(res.Held, HeldTask{TaskID: nodes[i].id, Name: nodes[i].name, Reason: respecHold[i]})
		}
		// A pending task has no job yet, so it has not started: the
		// edit re-resolves its texts and skills (§5.9 'Versioning').
		if err := reresolveTaskInstructionsTx(tx, proj, nodes[i].id, nodes[i].label(), respecSkills[i], now); err != nil {
			return AmendPlanResult{}, fmt.Errorf("respec %s: %w", nodes[i].label(), err)
		}
		res.Respecced = append(res.Respecced, nodes[i].id)
	}
	setupRec, err := taskProjectSetupTx(tx, proj)
	if err != nil {
		return AmendPlanResult{}, err
	}
	for k := range am.Add {
		i := existing + k
		taskState := taskPending
		if addedHold[k] != "" {
			taskState = taskNeedsForeman
		}
		r, err := tx.Exec(`INSERT INTO tasks (plan_id, name, title, spec, check_cmd, class, state, base_ref, caps, setup, extra_repos, tier, effective_tier, effort, effective_effort, review_tier, review_effort, created_at, updated_at)
			VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?)`,
			plan, nodes[i].name, nodes[i].title, nodes[i].spec, nodes[i].check, nodes[i].class, taskState, baseRef, taskCapsJSON(am.Add[k].Caps), setupRec, nodes[i].extraRec, taskTier(am.Add[k].Tier), taskTier(am.Add[k].Tier), am.Add[k].Effort, am.Add[k].Effort, am.Add[k].ReviewTier, am.Add[k].ReviewEffort, nowStr, nowStr)
		if err != nil {
			return AmendPlanResult{}, fmt.Errorf("create the task: %w", err)
		}
		if nodes[i].id, err = r.LastInsertId(); err != nil {
			return AmendPlanResult{}, fmt.Errorf("create the task: last insert id: %w", err)
		}
		if err := recordTaskInstructionsTx(tx, nodes[i].id, addSnaps[k], now); err != nil {
			return AmendPlanResult{}, err
		}
		if addedHold[k] != "" {
			if _, err := appendEventTx(tx, evTaskPlanHeld, proj, nodes[i].id, 0,
				eventPayload{Status: taskNeedsForeman, Reason: addedHold[k]}, now); err != nil {
				return AmendPlanResult{}, err
			}
			res.Held = append(res.Held, HeldTask{TaskID: nodes[i].id, Name: nodes[i].name, Reason: addedHold[k]})
		}
		res.Added = append(res.Added, CreatedTask{TaskID: nodes[i].id, Name: nodes[i].name})
	}
	writeDeps := func(i int) error {
		for _, j := range nodes[i].deps {
			if _, err := tx.Exec(`INSERT INTO task_deps (plan_id, task_id, depends_on_task_id, is_base, is_continue) VALUES (?, ?, ?, ?, ?)`,
				plan, nodes[i].id, nodes[j].id, nodes[i].base == j, nodes[i].cont == j); err != nil {
				return fmt.Errorf("record the dependencies: %w", err)
			}
		}
		return nil
	}
	for _, i := range retargeted {
		if _, err := tx.Exec(`DELETE FROM task_deps WHERE task_id = ?`, nodes[i].id); err != nil {
			return AmendPlanResult{}, fmt.Errorf("drop the dependencies of task %d: %w", nodes[i].id, err)
		}
		// The row update marks the plan for the mirror: a dependency
		// delete has no trigger of its own.
		if _, err := tx.Exec(`UPDATE tasks SET updated_at = ? WHERE id = ?`, nowStr, nodes[i].id); err != nil {
			return AmendPlanResult{}, fmt.Errorf("re-point task %d: %w", nodes[i].id, err)
		}
		if err := writeDeps(i); err != nil {
			return AmendPlanResult{}, err
		}
		res.Retargeted = append(res.Retargeted, nodes[i].id)
		// A re-pointed done task (§6.2) is landable again the moment its
		// dependencies are: the land rule runs for it here, so its
		// landing — and the plan's closing behind it — waits on nothing
		// else.
		if nodes[i].state == taskDone {
			if err := e.maybeLandTaskTx(tx, nodes[i].id, now); err != nil {
				return AmendPlanResult{}, err
			}
		}
	}
	for k := range am.Add {
		if err := writeDeps(existing + k); err != nil {
			return AmendPlanResult{}, err
		}
	}
	if newState != state {
		if _, err := tx.Exec(`UPDATE plans SET state = ?, version = version + 1, updated_at = ? WHERE id = ?`, newState, nowStr, plan); err != nil {
			return AmendPlanResult{}, fmt.Errorf("hold plan %d for approval: %w", plan, err)
		}
	} else if _, err := tx.Exec(`UPDATE plans SET version = version + 1, updated_at = ? WHERE id = ?`, nowStr, plan); err != nil {
		return AmendPlanResult{}, fmt.Errorf("touch plan %d: %w", plan, err)
	}
	if newState == planAwaitingApproval && verdict.Needed {
		res.Reasons = verdict.reasons(settings.Classes, settings.DefaultCheck)
		if _, err := appendEventTx(tx, evPlanAwaitingApproval, proj, 0, 0, eventPayload{
			Plan:    plan,
			Count:   int64(len(kept)),
			Classes: verdict.Classes,
			Paths:   verdict.Paths,
			Reason:  strings.Join(res.Reasons, "; "),
		}, now); err != nil {
			return AmendPlanResult{}, err
		}
	}

	// The plan review (§6.2): an active plan the amendment gives
	// tasks to turns reviewing over its added and respecified tasks,
	// which stay pending until the verdict; a reviewing or parked
	// plan's amendment is the foreman's answer to a changes verdict,
	// and starts the next round over the tasks it names. Tasks the
	// amendment adds or respecifies under a round still running are
	// recorded as the plan's waiting amendment work instead: the round
	// keeps judging the plan as it was queued, and its verdict —
	// approve included — queues the next round over them.
	if reviewRoundLive {
		var waiting []int64
		for _, i := range respecced {
			waiting = append(waiting, nodes[i].id)
		}
		for _, a := range res.Added {
			waiting = append(waiting, a.TaskID)
		}
		if err := addAmendBatchTx(tx, plan, waiting, now); err != nil {
			return AmendPlanResult{}, err
		}
	}
	if amendReview || state == planReviewing || state == planNeedsForeman {
		var batch []int64
		for _, i := range respecced {
			batch = append(batch, nodes[i].id)
		}
		for _, a := range res.Added {
			batch = append(batch, a.TaskID)
		}
		var err error
		if amendReview {
			err = e.planReviewBeginTx(tx, proj, plan, batch, now)
		} else {
			err = e.planReviewAnswerTx(tx, proj, plan, batch, now)
		}
		if err != nil {
			return AmendPlanResult{}, err
		}
		// A review that concluded at once — no reviewer to be had, the
		// rounds spent, an answer with no task to review — moved the
		// plan on already: the result reads the state it reached and the
		// work its conclusion started.
		if err := tx.QueryRow("SELECT state FROM plans WHERE id = ?", plan).Scan(&newState); err != nil {
			return AmendPlanResult{}, fmt.Errorf("read plan %d: %w", plan, err)
		}
		res.State = newState
		if newState == "active" {
			started, err := startedWorkTx(tx, plan, now)
			if err != nil {
				return AmendPlanResult{}, err
			}
			res.Started = started
		}
	}

	// The waiting state re-evaluates as creation does: in an active plan,
	// every pending task whose dependencies are all done starts — unless
	// the plan's spend has passed its budget, when it parks at the plan
	// gate like a rule-held addition.
	if newState == "active" {
		started, held, err := e.readyPlanTasksTx(tx, proj, plan, now)
		if err != nil {
			return AmendPlanResult{}, err
		}
		res.Started = append(res.Started, started...)
		res.Held = append(res.Held, held...)
		for k := range res.Added {
			for _, s := range res.Started {
				if s.TaskID == res.Added[k].TaskID {
					res.Added[k].JobID = s.JobID
				}
			}
		}
	}

	var change []string
	if len(res.Added) > 0 {
		names := make([]string, len(res.Added))
		for k, a := range res.Added {
			names[k] = fmt.Sprintf("task %d", a.TaskID)
			if a.Name != "" {
				names[k] += " " + a.Name
			}
		}
		change = append(change, "added "+strings.Join(names, ", "))
	}
	if len(res.Cancelled) > 0 {
		change = append(change, "cancelled "+joinIDs(res.Cancelled))
	}
	for _, i := range retargeted {
		to := make([]string, len(nodes[i].deps))
		for k, j := range nodes[i].deps {
			to[k] = nodes[j].label()
		}
		if len(to) == 0 {
			to = []string{"nothing"}
		}
		change = append(change, fmt.Sprintf("re-pointed %s to wait on %s", nodes[i].label(), strings.Join(to, ", ")))
	}
	if len(res.Retiered) > 0 {
		change = append(change, "retiered "+joinIDs(res.Retiered))
	}
	if len(res.Respecced) > 0 {
		change = append(change, "respecified "+joinIDs(res.Respecced))
	}
	for _, h := range res.Held {
		label := fmt.Sprintf("task %d", h.TaskID)
		if h.Name != "" {
			label += " " + h.Name
		}
		change = append(change, fmt.Sprintf("held %s for approval: %s", label, h.Reason))
	}
	if _, err := appendEventTx(tx, evPlanAmended, proj, 0, 0, eventPayload{
		Retiered: res.Retiered,
		Plan:     plan,
		Count:    int64(len(res.Added) + len(res.Cancelled) + len(res.Retargeted) + len(res.Retiered) + len(res.Respecced)),
		Status:   newState,
		Reason:   strings.Join(change, "; "),
	}, now); err != nil {
		return AmendPlanResult{}, err
	}

	if newState == "active" {
		closed, err := e.finishPlanIfClosedTx(tx, proj, plan, 0, 0, now)
		if err != nil {
			return AmendPlanResult{}, err
		}
		if closed {
			res.State = planDone
		}
	}
	return res, nil
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("task %d", id)
	}
	return strings.Join(parts, ", ")
}

// loadAmendNodesTx reads a plan's tasks, by id, with their dependencies
// as indexes into the same list.
func loadAmendNodesTx(tx *sql.Tx, plan int64) ([]amendNode, error) {
	rows, err := tx.Query(`SELECT id, name, state, title, spec, COALESCE(check_cmd, ''), class, skip_approved, COALESCE(extra_repos, '')
		FROM tasks WHERE plan_id = ? ORDER BY id`, plan)
	if err != nil {
		return nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
	}
	var nodes []amendNode
	for rows.Next() {
		n := amendNode{base: -1, cont: -1}
		var extraRec string
		if err := rows.Scan(&n.id, &n.name, &n.state, &n.title, &n.spec, &n.check, &n.class, &n.skipApproved, &extraRec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
		}
		// Only a list the task named is the plan's to re-check (§5.8); an
		// inherited default never needed approval.
		if spec := parseExtraReposRecord(extraRec); spec != nil && spec.Source == ExtraSourceTask {
			n.extra = spec.Repos
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
	}
	rows.Close()
	idx := map[int64]int{}
	for i, n := range nodes {
		idx[n.id] = i
	}
	edges, err := tx.Query(`SELECT task_id, depends_on_task_id, is_base, is_continue FROM task_deps WHERE plan_id = ? ORDER BY task_id, depends_on_task_id`, plan)
	if err != nil {
		return nil, fmt.Errorf("read the dependencies of plan %d: %w", plan, err)
	}
	defer edges.Close()
	for edges.Next() {
		var a, b int64
		var isBase, isContinue bool
		if err := edges.Scan(&a, &b, &isBase, &isContinue); err != nil {
			return nil, fmt.Errorf("read the dependencies of plan %d: %w", plan, err)
		}
		i, ok1 := idx[a]
		j, ok2 := idx[b]
		if ok1 && ok2 {
			nodes[i].deps = append(nodes[i].deps, j)
			if isBase {
				nodes[i].base = j
			}
			if isContinue {
				nodes[i].cont = j
			}
		}
	}
	return nodes, edges.Err()
}

// cancelTaskJobsTx stops every live job of a task, so a cancelled task
// leaves no worker running or queued.
func (e *Engine) cancelTaskJobsTx(tx *sql.Tx, task int64, now time.Time) error {
	rows, err := tx.Query(`SELECT id FROM jobs WHERE task_id = ? ORDER BY id`, task)
	if err != nil {
		return fmt.Errorf("read the jobs of task %d: %w", task, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("read the jobs of task %d: %w", task, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read the jobs of task %d: %w", task, err)
	}
	rows.Close()
	for _, id := range ids {
		job, err := loadJobTx(tx, id)
		if err != nil {
			return err
		}
		if job == nil || isTerminal(job.Status) {
			continue
		}
		if _, err := e.cancelTx(tx, job, "task cancelled by plan_amend", now); err != nil {
			return err
		}
	}
	return nil
}

// startedWorkTx lists the work jobs of the plan's tasks queued at now:
// those this transaction's review conclusion started, which readied the
// tasks before the caller could see them.
func startedWorkTx(tx *sql.Tx, plan int64, now time.Time) ([]CreatedTask, error) {
	rows, err := tx.Query(`SELECT t.id, t.name, j.id FROM jobs j JOIN tasks t ON t.id = j.task_id
		WHERE t.plan_id = ? AND j.kind = ? AND j.created_at = ? ORDER BY j.id`, plan, KindWork, fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("read the started work of plan %d: %w", plan, err)
	}
	defer rows.Close()
	var out []CreatedTask
	for rows.Next() {
		var c CreatedTask
		if err := rows.Scan(&c.TaskID, &c.Name, &c.JobID); err != nil {
			return nil, fmt.Errorf("read the started work of plan %d: %w", plan, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// readyPlanTasksTx starts every pending task of an active plan whose
// dependencies are all done — none counts — turning it ready with its
// first work job queued, as creation and approval do. A task of a plan
// whose spend passed its budget parks instead, at the plan gate (§6.3,
// planbudget.go), and is returned among the held.
func (e *Engine) readyPlanTasksTx(tx *sql.Tx, project, plan int64, now time.Time) ([]CreatedTask, []HeldTask, error) {
	rows, err := tx.Query(`SELECT t.id, t.name, t.spec FROM tasks t
		WHERE t.plan_id = ? AND t.state = ?
		AND NOT EXISTS (SELECT 1 FROM task_deps o JOIN tasks x ON x.id = o.depends_on_task_id
			WHERE o.task_id = t.id AND x.state <> ?)
		ORDER BY t.id`, plan, taskPending, taskDone)
	if err != nil {
		return nil, nil, fmt.Errorf("read the ready tasks of plan %d: %w", plan, err)
	}
	type wave struct {
		id         int64
		name, spec string
	}
	var first []wave
	for rows.Next() {
		var w wave
		if err := rows.Scan(&w.id, &w.name, &w.spec); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read the ready tasks of plan %d: %w", plan, err)
		}
		first = append(first, w)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("read the ready tasks of plan %d: %w", plan, err)
	}
	rows.Close()
	gate, err := planBudgetGateTx(tx, project, plan)
	if err != nil {
		return nil, nil, err
	}
	var out []CreatedTask
	var held []HeldTask
	for _, w := range first {
		if h, err := gate.parkTx(tx, project, w.id, now); err != nil {
			return nil, nil, err
		} else if h {
			held = append(held, HeldTask{TaskID: w.id, Name: w.name, Reason: gate.heldReason})
			continue
		}
		if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
			taskReady, fmtTime(now), w.id); err != nil {
			return nil, nil, fmt.Errorf("ready task %d: %w", w.id, err)
		}
		sr, err := e.queueTaskWorkTx(tx, project, w.id, w.spec, now)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, CreatedTask{TaskID: w.id, Name: w.name, JobID: sr.JobID, answer: sr.Answer, run: sr.Run})
	}
	return out, held, nil
}

// finishPlanIfClosedTx finishes an active plan with no live work left
// (§6.2): every task is done, failed or cancelled, or pending behind one
// that will never be done. Those permanently waiting tasks are the plan's
// open end, and the plan_done event — the foreman's done report — names
// them, with any failed task, so the user sees what never ran. A blocked
// plan never reaches that: done work that can never land (§6.2, a
// cancelled or failed dependency under it) keeps the plan active, one
// wake per blockage telling the foreman. The plan also counts one
// attempt on the "plan" class for the foreman's agent: a success when
// every task ended done or was cancelled with the user's approval
// (skip_approved), a failure when a task failed out, was cancelled
// without that approval, or never ran. It reports whether it finished
// the plan.
func (e *Engine) finishPlanIfClosedTx(tx *sql.Tx, project, plan, task, job int64, now time.Time) (bool, error) {
	var state string
	if err := tx.QueryRow("SELECT state FROM plans WHERE id = ?", plan).Scan(&state); err != nil {
		return false, fmt.Errorf("read plan %d: %w", plan, err)
	}
	if state != "active" {
		return false, nil
	}
	nodes, err := loadAmendNodesTx(tx, plan)
	if err != nil {
		return false, err
	}
	blocked, err := e.evalPlanBlockageTx(tx, project, plan, nodes, now)
	if err != nil {
		return false, err
	}
	if blocked {
		return false, nil
	}
	var failed, waiting, cancelled []string
	for _, n := range nodes {
		switch n.state {
		case taskDone:
			// A done task whose land job is queued or running is still
			// live work (§7.1): the plan closes when the landing has an
			// outcome, so the done report names it and a failed landing
			// counts against the plan. A hold at the land gate has no job
			// and lets the plan close, named under "waits at a gate".
			if live, err := liveLandJobTx(tx, n.id); err != nil {
				return false, err
			} else if live {
				return false, nil
			}
		case taskCancelled:
			// Only a skip the user approved is a skip: the foreman's own
			// cancellation is an abandoned task, whatever state it was in
			// (a restarted failure is ready, too). The report says each
			// as it counts: an abandonment among the failures, an
			// approved skip as cancelled.
			if !n.skipApproved {
				failed = append(failed, fmt.Sprintf("%s (cancelled without the user's approval)", n.label()))
			} else {
				cancelled = append(cancelled, n.label())
			}
		case taskFailed:
			failed = append(failed, n.label())
		case taskPending:
			why := make([]string, 0, len(nodes))
			for _, j := range n.deps {
				if nodes[j].state != taskDone {
					why = append(why, fmt.Sprintf("%s %s", nodes[j].label(), nodes[j].state))
				}
			}
			waiting = append(waiting, fmt.Sprintf("%s (waits on %s)", n.label(), strings.Join(why, ", ")))
		default:
			return false, nil // live work remains
		}
	}
	if _, err := tx.Exec(`UPDATE plans SET state = ?, updated_at = ? WHERE id = ?`,
		planDone, fmtTime(now), plan); err != nil {
		return false, fmt.Errorf("finish plan %d: %w", plan, err)
	}
	if err := recordPlanOutcomeTx(tx, project, len(failed) == 0 && len(waiting) == 0, now); err != nil {
		return false, err
	}
	// What landed, and what waits (§7.1): a done task with a landing
	// record landed — or waits at the push gate when its landing is
	// recorded waiting — and a land_holds row holds one at the land gate.
	// A done, landable task with neither is not yet landed; the land
	// rule's own eligibility (§7.1) decides, so a class that never lands
	// is not named. The done report names all of it, beside what was
	// cancelled with the user's approval, what failed and what never ran.
	var landed, gated, notLanded []string
	for _, n := range nodes {
		if n.state != taskDone {
			continue
		}
		if l, err := landingTx(tx, n.id); err != nil {
			return false, err
		} else if l != nil {
			if l.Pushed == "waiting" {
				gated = append(gated, fmt.Sprintf("%s (waits to push)", n.label()))
			} else {
				landed = append(landed, fmt.Sprintf("%s (%s@%s)", n.label(), l.TargetBranch, l.Commit))
			}
			continue
		}
		var reason string
		err := tx.QueryRow(`SELECT reason FROM land_holds WHERE task_id = ? AND state = 'held'`, n.id).Scan(&reason)
		if err == nil {
			gated = append(gated, fmt.Sprintf("%s (%s)", n.label(), reason))
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("read the land hold of task %d: %w", n.id, err)
		}
		// Done with no record and no hold: only a task the land rule
		// would land is "not yet landed" — another class never lands.
		landable, err := landableDoneTx(tx, n.id)
		if err != nil {
			return false, err
		}
		if landable {
			notLanded = append(notLanded, n.label())
		}
	}
	var report []string
	if len(failed) == 0 && len(waiting) == 0 && len(gated) == 0 && len(notLanded) == 0 && len(cancelled) == 0 && len(landed) > 0 {
		report = append(report, "all tasks landed")
	} else {
		if len(landed) > 0 {
			report = append(report, "landed: "+strings.Join(landed, ", "))
		}
		if len(notLanded) > 0 {
			report = append(report, "not yet landed: "+strings.Join(notLanded, ", "))
		}
		if len(gated) > 0 {
			report = append(report, "waits at a gate: "+strings.Join(gated, "; "))
		}
		if len(failed) > 0 {
			report = append(report, "failed: "+strings.Join(failed, ", "))
		}
		if len(cancelled) > 0 {
			report = append(report, "cancelled: "+strings.Join(cancelled, ", ")+" (with the user's approval)")
		}
		if len(waiting) > 0 {
			report = append(report, "never ran: "+strings.Join(waiting, "; "))
		}
	}
	if err := inboxEventTx(tx, evPlanDone, project, task, job,
		eventPayload{Status: planDone, Plan: plan, Reason: strings.Join(report, "; ")}, now); err != nil {
		return false, err
	}
	return true, e.wakeForemanTx(tx, project, now)
}

// recordPlanOutcomeTx counts one finished plan on the outcome record's
// plan class (§8) for the project's foreman: the agent a runner
// advertises for the project's foreman harness and model. A project with
// no foreman chosen, or whose foreman no runner advertises, has nobody
// to score and records nothing. Measurement only: no dispatch behavior
// keys off the plan class yet.
func recordPlanOutcomeTx(tx *sql.Tx, project int64, success bool, now time.Time) error {
	var harness, model string
	if err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", project).
		Scan(&harness, &model); err != nil {
		return fmt.Errorf("read the foreman of project %d: %w", project, err)
	}
	if harness == "" || model == "" {
		return nil
	}
	var agent int64
	err := tx.QueryRow("SELECT id FROM agents WHERE harness = ? AND model = ? ORDER BY id LIMIT 1", harness, model).Scan(&agent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the foreman's agent: %w", err)
	}
	successes := 0
	if success {
		successes = 1
	}
	if _, err := tx.Exec(`INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_id, agent_id, class, skills) DO UPDATE SET
			attempts = attempts + 1, successes = successes + excluded.successes, updated_at = excluded.updated_at`,
		project, agent, classPlan, successes, fmtTime(now)); err != nil {
		return fmt.Errorf("record the outcome of the plan: %w", err)
	}
	return nil
}
