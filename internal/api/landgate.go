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
		var req struct {
			AndStopAsking bool `json:"and_stop_asking"`
			ApprovalProof
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
		// The role was checked by the route; the landing's yes or no
		// needs the user's signature for this task (§12.2).
		act := engine.GatedActRequest{Action: engine.GatedLandReject, Project: project, Task: taskID}
		if approve {
			act = engine.GatedActRequest{Action: engine.GatedLandApprove, Project: project, Task: taskID, StopAsking: req.AndStopAsking}
		}
		gated, written, err := s.runGated(w, r, act, req.ApprovalProof)
		if written {
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		var res engine.LandDecision
		if gated.Land != nil {
			res = *gated.Land
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
		ApprovalProof
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// The role was checked by the route; the push's yes needs the user's
	// signature for this project (§12.2).
	gated, written, err := s.runGated(w, r, engine.GatedActRequest{Action: engine.GatedPushApprove, RepoURL: req.RepoURL}, req.ApprovalProof)
	if written {
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	var res engine.PushApproveResult
	if gated.Push != nil {
		res = *gated.Push
	}
	writeJSON(w, http.StatusOK, res)
}
