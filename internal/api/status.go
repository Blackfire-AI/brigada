package api

import (
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handleStatus reads the engine's own state (§13): which process it is,
// since when, and the jobs by status. One of the two places a client
// learns the engine is alive besides /v1/health, and the one with
// content.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	st, err := s.eng.Status(r.Context())
	if err != nil {
		s.writeInternal(w, r, "read status", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleTaskSubmit is the debug submit path (§12.1: tasks, create) until
// the foreman's plan tools exist in the next phase: one task, created
// whole, with its first work job already queued. Like the job submit it
// is the admin's to call.
func (s *Server) handleTaskSubmit(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req taskSubmitRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.eng.SubmitTask(r.Context(), engine.TaskParams{
		RepoURL:  req.RepoURL,
		BaseRef:  req.BaseRef,
		Title:    req.Title,
		Spec:     req.Spec,
		CheckCmd: req.CheckCmd,
		Class:    req.Class,
		Prompt:   req.Prompt,
		Harness:  req.Harness,
		Labels:   nonNil(req.Labels),
	})
	if err != nil {
		// Like the job submit: a task the engine will not take is the
		// caller's mistake, not the engine's failure.
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskSubmitResponse{
		Answer:    string(res.Answer),
		ProjectID: res.ProjectID,
		TaskID:    res.TaskID,
		JobID:     res.JobID,
		Run:       res.Run,
	})
}

// taskSubmitRequest is the debug task submit's body (§12.1): the repo
// and base ref of the workspace (§5.1), the spec verbatim (§1.4), the
// check the runner runs after a done (§5.5), and the composed worker
// prompt (§5.3) the job carries.
type taskSubmitRequest struct {
	RepoURL  string   `json:"repo_url"`
	BaseRef  string   `json:"base_ref"`
	Title    string   `json:"title"`
	Spec     string   `json:"spec"`
	CheckCmd string   `json:"check_cmd"`
	Class    string   `json:"class"`
	Prompt   string   `json:"prompt"`
	Harness  string   `json:"harness"`
	Labels   []string `json:"labels"`
}

// taskSubmitResponse names what the submit created: the project it
// found or made, the task, and the queued job to wait on.
type taskSubmitResponse struct {
	Answer    string `json:"answer"`
	ProjectID int64  `json:"project_id"`
	TaskID    int64  `json:"task_id"`
	JobID     int64  `json:"job_id"`
	Run       int    `json:"run"`
}
