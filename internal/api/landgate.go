package api

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handleTaskLandDecision serves the user's decision on a land-gate hold
// (§6.3): approve releases it and re-evaluates the landing, reject
// cancels the landing and leaves the branch. An approve body may carry
// and_stop_asking (§6.3), which also changes the one setting each hold
// reason maps to. Client and admin tokens only: the foreman has no tool
// to pass the gate. The sub-path names which decision: /approve or
// /reject.
func (s *Server) handleTaskLandDecision(approve bool) handler {
	return func(w http.ResponseWriter, r *http.Request, id identity) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		taskID, ok := parseTaskID(w, r)
		if !ok {
			return
		}
		project, ok := s.taskProject(w, r, id, taskID)
		if !ok {
			return
		}
		var stopAsking bool
		if approve {
			var req struct {
				AndStopAsking bool `json:"and_stop_asking"`
			}
			if body, ok := readBody(w, r); ok {
				if len(bytes.TrimSpace(body)) > 0 {
					if err := json.Unmarshal(body, &req); err != nil {
						writeError(w, http.StatusBadRequest, codeBadRequest, "malformed JSON body: "+err.Error())
						return
					}
				}
			} else {
				return
			}
			stopAsking = req.AndStopAsking
		}
		var (
			res engine.LandDecision
			err error
		)
		if approve {
			res, err = s.eng.LandApprove(r.Context(), project, taskID, stopAsking)
		} else {
			res, err = s.eng.LandReject(r.Context(), project, taskID)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// handlePushApprove releases the push gate for the project's waiting
// landings (§6.3): each gets a push job. Client and admin tokens only:
// the foreman has no tool to pass the gate.
func (s *Server) handlePushApprove(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		RepoURL string `json:"repo_url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.eng.PushApprove(r.Context(), req.RepoURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
