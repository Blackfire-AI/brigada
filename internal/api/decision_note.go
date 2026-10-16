package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The token alone selects the project: tool arguments cannot widen it.
type decisionNoteRequest struct {
	Text string `json:"text"`
}

type DecisionNoteResponse struct {
	EventID int64 `json:"event_id"`
}

func (s *Server) handleDecisionNote(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req decisionNoteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	event, err := s.eng.DecisionNote(r.Context(), id.project, req.Text)
	switch {
	case errors.Is(err, engine.ErrInvalidMessage):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
	case err != nil:
		s.writeInternal(w, r, "record the decision note", err)
	default:
		writeJSON(w, http.StatusOK, DecisionNoteResponse{EventID: event})
	}
}

func (c *Client) DecisionNote(ctx context.Context, text, idemKey string) (DecisionNoteResponse, error) {
	var out DecisionNoteResponse
	err := c.callKeyed(ctx, http.MethodPost, "/v1/foreman/decision-note", idemKey, decisionNoteRequest{Text: text}, &out)
	return out, err
}
