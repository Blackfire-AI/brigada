package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
)

// handleTaskAnswer is the foreman's answer behind the API (§3, §12.1,
// §12.2): the task's id names it — a foreman token may answer only its
// own project's tasks, and the engine refuses a task that is not parked
// by its worker's question. The idempotency wrapper around it replays a
// repeated call's first answer (§12.1).
func (s *Server) handleTaskAnswer(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	taskID, ok := parseTaskID(w, r)
	if !ok {
		return
	}
	var req taskAnswerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	project, ok := s.taskProject(w, r, id, taskID)
	if !ok {
		return
	}
	res, err := s.eng.Answer(r.Context(), project, taskID, req.Text)
	if err != nil {
		// The engine's refusals — no text, a task not parked, a task parked
		// by something other than a question — are the caller's mistakes,
		// not the engine's failures.
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskAnswerResponse{
		Answer:     string(res.Answer),
		QuestionID: res.QuestionID,
		JobID:      res.JobID,
		Run:        res.Run,
	})
}

// handleTaskRestart is the foreman's task_restart behind the API (§3,
// §5.6, §12.1, §12.2): a fresh work attempt for a task parked at
// needs_foreman, any parker, the foreman's note as the worker's
// feedback. Scoped and idempotent like the answer.
func (s *Server) handleTaskRestart(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	taskID, ok := parseTaskID(w, r)
	if !ok {
		return
	}
	var req taskRestartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	project, ok := s.taskProject(w, r, id, taskID)
	if !ok {
		return
	}
	res, err := s.eng.RestartTask(r.Context(), project, taskID, req.Note)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskRestartResponse{
		Answer: string(res.Answer),
		JobID:  res.JobID,
		Run:    res.Run,
	})
}

// taskProject resolves the project a task belongs to and checks the
// caller's reach (§12.1): the task's id names the project — no repo URL
// to send — so a foreman token is simply walled into its own, while a
// client or admin may reach every project's tasks. ok=false means the
// answer is already written: a 404 for a task that is not there, a 403
// for a foreman outside its project.
func (s *Server) taskProject(w http.ResponseWriter, r *http.Request, id identity, taskID int64) (int64, bool) {
	var project int64
	err := s.st.View(r.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT p.project_id FROM tasks t JOIN plans p ON p.id = t.plan_id
			WHERE t.id = ?`, taskID).Scan(&project)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, codeNotFound,
			"no such task "+strconv.FormatInt(taskID, 10))
		return 0, false
	case err != nil:
		s.writeInternal(w, r, "resolve the task", err)
		return 0, false
	}
	if !id.mayTouchProject(project) {
		writeError(w, http.StatusForbidden, codeForbidden,
			"foreman token is scoped to project "+strconv.FormatInt(id.project, 10))
		return 0, false
	}
	return project, true
}

// handleTaskGet reads one task (§12.1): its repo, base ref, result
// branch and latest job — what a client needs to find the change. A
// foreman token is walled into its own project's tasks.
func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	taskID, ok := parseTaskID(w, r)
	if !ok {
		return
	}
	if _, ok := s.taskProject(w, r, id, taskID); !ok {
		return
	}
	task, err := s.eng.Task(r.Context(), taskID)
	if err != nil {
		s.writeInternal(w, r, "read task", err)
		return
	}
	if task == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no such task "+strconv.FormatInt(taskID, 10))
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// parseTaskID reads the {id} path value as a task id.
func parseTaskID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	taskID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "task id must be an integer")
		return 0, false
	}
	return taskID, true
}

// taskAnswerRequest is the answer body (§12.1: questions, answer): the
// answer text, verbatim — the engine composes the worker's prompt from
// it, never the caller.
type taskAnswerRequest struct {
	Text string `json:"text"`
}

// taskAnswerResponse names what the answer did: the questions row it
// filled and the job that carries the answer to its worker.
type taskAnswerResponse struct {
	Answer     string `json:"answer"`
	QuestionID int64  `json:"question_id"`
	JobID      int64  `json:"job_id"`
	Run        int    `json:"run"`
}

// taskRestartRequest is the task_restart body: the foreman's note for
// the worker's fresh attempt, verbatim, optional.
type taskRestartRequest struct {
	Note string `json:"note"`
}

// taskRestartResponse names what the restart did: the fresh work job.
type taskRestartResponse struct {
	Answer string `json:"answer"`
	JobID  int64  `json:"job_id"`
	Run    int    `json:"run"`
}
