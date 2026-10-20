package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handlePlanCreate is the foreman's plan_create behind the API (§6.2,
// §12.1, §12.2): one plan with its tasks, active from the start,
// the work job of every task without dependencies queued. A foreman token names no project — its own
// is the only one it may plan in, and a repo URL that says otherwise is
// a 403 — while a client or admin must name the project's repo URL. The
// idempotency wrapper around it replays a repeated call's first answer
// (§12.1).
func (s *Server) handlePlanCreate(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req planCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var project int64
	if id.role == roleForeman {
		project = id.project
		if req.RepoURL != "" {
			named, ok, err := s.projectOfRepo(r.Context(), req.RepoURL)
			if errors.Is(err, engine.ErrInvalidRepoURL) {
				writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
				return
			}
			if err != nil {
				s.writeInternal(w, r, "resolve the repo", err)
				return
			}
			if !ok || named != project {
				writeError(w, http.StatusForbidden, codeForbidden,
					"foreman token is scoped to project "+strconv.FormatInt(id.project, 10))
				return
			}
		}
	} else {
		if req.RepoURL == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"repo_url is required: name the project's repo URL")
			return
		}
		named, ok, err := s.projectOfRepo(r.Context(), req.RepoURL)
		if errors.Is(err, engine.ErrInvalidRepoURL) {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		if err != nil {
			s.writeInternal(w, r, "resolve the repo", err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, codeNotFound,
				"no project for repo "+engine.RedactRepoURL(req.RepoURL))
			return
		}
		project = named
	}
	tasks := make([]engine.PlanTask, len(req.Tasks))
	for i, t := range req.Tasks {
		tasks[i] = engine.PlanTask{Title: t.Title, Name: t.Name, Deps: t.Deps, Base: t.Base, Spec: t.Spec, Check: t.Check, Class: t.Class, Tier: t.Tier, Effort: t.Effort, Caps: t.Caps, ExtraRepos: t.ExtraRepos}
	}
	res, err := s.eng.CreatePlan(r.Context(), project, req.Title, tasks)
	if err != nil {
		// Like the task submit: a plan the engine will not take — no
		// title, no tasks, a dependency cycle — is the caller's
		// mistake, not the engine's failure.
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	out := planCreateResponse{
		Answer:  string(res.Answer),
		PlanID:  res.PlanID,
		TaskID:  res.TaskID,
		JobID:   res.JobID,
		State:   res.State,
		Reasons: res.Reasons,
	}
	for _, t := range res.Tasks {
		out.Tasks = append(out.Tasks, planCreatedTask{TaskID: t.TaskID, Name: t.Name, JobID: t.JobID})
	}
	writeJSON(w, http.StatusOK, out)
}

// projectOfRepo resolves the project a repo URL names (product §5 rule
// 6): repos are addressed by URL, so the URL is the caller's only way
// to point at a project. ok=false means no project carries the repo.
func (s *Server) projectOfRepo(ctx context.Context, repo string) (int64, bool, error) {
	var project int64
	err := s.st.View(ctx, func(tx *sql.Tx) error {
		var err error
		project, err = engine.LookupProjectTx(tx, repo)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return 0, false, fmt.Errorf("read the project of %s: %w", engine.RedactRepoURL(repo), err)
	}
	return project, project != 0, nil
}

// planCreateRequest is the plan_create body (§6.1, §6.2): the plan's
// title and its tasks. A foreman caller may leave repo_url out — its
// token names the project — and any repo_url it sends must agree with
// that token; every other role must send it.
type planCreateRequest struct {
	RepoURL string         `json:"repo_url"`
	Title   string         `json:"title"`
	Tasks   []planTaskBody `json:"tasks"`
}

// planTaskBody is one task of the plan as the body carries it (§6.1):
// title, name, spec, check, class and the names of the tasks it
// depends on.
type planTaskBody struct {
	Tier   string         `json:"tier,omitempty"`
	Effort string         `json:"effort,omitempty"`
	Caps   map[string]int `json:"caps,omitempty"`
	Title  string         `json:"title"`
	Name   string         `json:"name,omitempty"`
	Deps   []string       `json:"deps,omitempty"`
	Base   string         `json:"base,omitempty"`
	Spec   string         `json:"spec"`
	Check  string         `json:"check"`
	Class  string         `json:"class"`
	// ExtraRepos (§5.8): absent or null inherits the project default; a
	// list, even [], replaces it. No omitempty: [] must survive the trip.
	ExtraRepos []engine.ExtraRepo `json:"extra_repos"`
}

// planCreateResponse names what the call created: the plan, the
// first ready task with its queued work job to wait on, and every task.
type planCreateResponse struct {
	Answer string            `json:"answer"`
	PlanID int64             `json:"plan_id"`
	TaskID int64             `json:"task_id"`
	JobID  int64             `json:"job_id"`
	Tasks  []planCreatedTask `json:"tasks,omitempty"`
	// State is the plan's, and Reasons why it waits when the approval
	// limit holds it (§6.3): the foreman tells the user.
	State   string   `json:"state,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

// handlePlanDecision is the user's yes or no on a plan awaiting approval
// (§6.3): approve queues its first wave, reject cancels its tasks.
// Client and admin tokens only — the foreman never confirms its own
// plans. A plan that is not there is a 404, one not awaiting a 409.
func (s *Server) handlePlanDecision(approve bool) handler {
	return func(w http.ResponseWriter, r *http.Request, _ identity) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		planID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, "plan id must be an integer")
			return
		}
		// The body is optional: the CLI sends none that matters. A
		// coordinator names the project and the plan version the user was
		// shown (§4.6 mode B).
		var req planDecisionRequest
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, codeBadRequest, "malformed JSON body: "+err.Error())
				return
			}
		}
		var project int64
		if req.RepoURL != "" {
			named, found, err := s.projectOfRepo(r.Context(), req.RepoURL)
			if errors.Is(err, engine.ErrInvalidRepoURL) {
				writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
				return
			}
			if err != nil {
				s.writeInternal(w, r, "resolve the repo", err)
				return
			}
			if !found {
				writeError(w, http.StatusNotFound, codeNotFound, "no project for repo "+engine.RedactRepoURL(req.RepoURL))
				return
			}
			project = named
		}
		var res engine.PlanDecision
		if approve {
			res, err = s.eng.ApprovePlanVersion(r.Context(), project, planID, req.Version)
		} else {
			res, err = s.eng.RejectPlanVersion(r.Context(), project, planID, req.Version)
		}
		switch {
		case errors.Is(err, engine.ErrNoPlan):
			writeError(w, http.StatusNotFound, codeNotFound, err.Error())
			return
		case errors.Is(err, engine.ErrPlanVersionStale):
			writeError(w, http.StatusConflict, codeStalePlan, err.Error())
			return
		case errors.Is(err, engine.ErrPlanNotAwaiting):
			writeError(w, http.StatusConflict, codeConflict, err.Error())
			return
		case err != nil:
			s.writeInternal(w, r, "decide the plan", err)
			return
		}
		out := planDecisionResponse{PlanID: res.PlanID, State: res.State}
		for _, t := range res.Tasks {
			out.Tasks = append(out.Tasks, planCreatedTask{TaskID: t.TaskID, Name: t.Name, JobID: t.JobID})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// planDecisionRequest is the optional approve/reject body (§4.6 mode B):
// the project the plan must belong to and the plan version the user was
// shown. A version that is not the plan's current one is refused whole
// (code stale_plan); both omitted checks nothing, as the CLI does.
type planDecisionRequest struct {
	RepoURL string `json:"repo_url,omitempty"`
	Version int64  `json:"version,omitempty"`
}

// planDecisionResponse names the plan's new state and, for an approval,
// the first wave's tasks with their queued jobs.
type planDecisionResponse struct {
	PlanID int64             `json:"plan_id"`
	State  string            `json:"state"`
	Tasks  []planCreatedTask `json:"tasks,omitempty"`
}

// handlePlanAmend is the foreman's plan_amend behind the API (§12.2):
// tasks added, tasks cancelled, a waiting task's dependencies re-pointed,
// in one transaction. The plan's id names it: a foreman token is walled
// into its own project, a client or admin reaches every project's plans.
// A plan that is not there is a 404, one that is done or cancelled a 409,
// and an amendment the engine refuses — a cycle, an unknown name — a 400.
// Idempotent like the other foreman writes.
func (s *Server) handlePlanAmend(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	planID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "plan id must be an integer")
		return
	}
	var req planAmendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var project int64
	err = s.st.View(r.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT project_id FROM plans WHERE id = ?", planID).Scan(&project)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, codeNotFound, "no such plan "+strconv.FormatInt(planID, 10))
		return
	case err != nil:
		s.writeInternal(w, r, "resolve the plan", err)
		return
	}
	if !id.mayTouchProject(project) {
		writeError(w, http.StatusForbidden, codeForbidden,
			"foreman token is scoped to project "+strconv.FormatInt(id.project, 10))
		return
	}
	// Only a user's token can attest that the user approved the skips;
	// the foreman's own cancellations are never approved.
	am := engine.PlanAmendment{Cancel: req.Cancel, SkipApproved: id.role != roleForeman}
	for _, t := range req.Add {
		am.Add = append(am.Add, engine.PlanTask{Title: t.Title, Name: t.Name, Deps: t.Deps, Base: t.Base, Spec: t.Spec, Check: t.Check, Class: t.Class, Tier: t.Tier, Effort: t.Effort, Caps: t.Caps, ExtraRepos: t.ExtraRepos})
	}
	for _, rt := range req.Retarget {
		am.Retarget = append(am.Retarget, engine.PlanRetarget{Task: rt.Task, Deps: rt.Deps, Base: rt.Base})
	}
	for _, rt := range req.Retier {
		am.Retier = append(am.Retier, engine.PlanRetier{Task: rt.Task, Tier: rt.Tier, Effort: rt.Effort, Caps: rt.Caps})
	}
	res, err := s.eng.AmendPlan(r.Context(), project, planID, am)
	switch {
	case errors.Is(err, engine.ErrNoPlan):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
		return
	case errors.Is(err, engine.ErrPlanNotLive):
		writeError(w, http.StatusConflict, codeConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	out := planAmendResponse{
		PlanID:     res.PlanID,
		State:      res.State,
		Cancelled:  res.Cancelled,
		Retargeted: res.Retargeted,
		Retiered:   res.Retiered,
		Reasons:    res.Reasons,
	}
	for _, t := range res.Added {
		out.Added = append(out.Added, planCreatedTask{TaskID: t.TaskID, Name: t.Name, JobID: t.JobID})
	}
	for _, t := range res.Started {
		out.Started = append(out.Started, planCreatedTask{TaskID: t.TaskID, Name: t.Name, JobID: t.JobID})
	}
	writeJSON(w, http.StatusOK, out)
}

// planAmendRequest is the plan_amend body (§12.2): tasks to add (deps
// name tasks of the plan, by name or id), tasks to cancel, and waiting
// tasks whose whole dependency set is replaced.
type planAmendRequest struct {
	Add      []planTaskBody     `json:"add,omitempty"`
	Cancel   []string           `json:"cancel,omitempty"`
	Retarget []planRetargetBody `json:"retarget,omitempty"`
	Retier   []planRetierBody   `json:"retier,omitempty"`
}

type planRetierBody struct {
	Task   string         `json:"task"`
	Tier   string         `json:"tier,omitempty"`
	Effort string         `json:"effort,omitempty"`
	Caps   map[string]int `json:"caps,omitempty"`
}

type planRetargetBody struct {
	Task string   `json:"task"`
	Deps []string `json:"deps"`
	Base string   `json:"base,omitempty"`
}

// planAmendResponse names what the amendment did.
type planAmendResponse struct {
	PlanID     int64             `json:"plan_id"`
	State      string            `json:"state"`
	Added      []planCreatedTask `json:"added,omitempty"`
	Cancelled  []int64           `json:"cancelled,omitempty"`
	Retargeted []int64           `json:"retargeted,omitempty"`
	Retiered   []int64           `json:"retiered,omitempty"`
	Started    []planCreatedTask `json:"started,omitempty"`
	Reasons    []string          `json:"reasons,omitempty"`
}

// planCreatedTask is one task the plan made: its id, name and work job
// (0 while it waits on dependencies).
type planCreatedTask struct {
	TaskID int64  `json:"task_id"`
	Name   string `json:"name,omitempty"`
	JobID  int64  `json:"job_id"`
}
