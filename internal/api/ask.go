package api

import (
	"errors"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// askRequest is a user message for a project's foreman (§4.2 item 3):
// the words, and the project's repo URL — empty names the engine's
// only project.
type askRequest struct {
	RepoURL string `json:"repo_url"`
	Text    string `json:"text"`
}

// handleForemanAsk takes a user message for the foreman (§4.2 item 3,
// §12.1). Only client and admin tokens reach it — the route's roles —
// so no foreman turn and no runner, and through them no worker's
// output, can put words in the user's mouth (§1.4). The route is
// idempotent (§12.1): an LLM caller's retried call sends one message.
// The answer names the wake's outcome: woken, or why the message waits.
func (s *Server) handleForemanAsk(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req askRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// The token's label names the surface the message came from (§12.2);
	// every client token speaks with the user's voice.
	res, err := s.eng.AskForeman(r.Context(), req.RepoURL, req.Text, id.name)
	switch {
	case errors.Is(err, engine.ErrInvalidMessage):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	case errors.Is(err, engine.ErrNoProject):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
		return
	case err != nil:
		s.writeInternal(w, r, "take the user message", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
