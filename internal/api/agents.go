package api

import (
	"errors"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// agentResetRequest names the agents to reset (§8): the harness and
// model the status views print.
type agentResetRequest struct {
	Harness string `json:"harness"`
	Model   string `json:"model"`
}

// handleAgentReset clears broken and cooling on the named agents (§8:
// "or the user resets it"). Client and admin tokens only: a foreman or
// a runner cannot declare an agent healthy.
func (s *Server) handleAgentReset(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req agentResetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	n, err := s.eng.ResetAgents(r.Context(), req.Harness, req.Model)
	switch {
	case errors.Is(err, engine.ErrNoAgent):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
		return
	case err != nil:
		s.writeInternal(w, r, "reset agents", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Reset int `json:"reset"`
	}{n})
}
