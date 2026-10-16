package api

import (
	"context"
	"errors"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"net/http"
)

func (s *Server) handleAskUser(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	question, err := s.eng.AskUser(r.Context(), id.project, req.Text)
	switch {
	case errors.Is(err, engine.ErrInvalidMessage):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
	case err != nil:
		s.writeInternal(w, r, "record user question", err)
	default:
		writeJSON(w, http.StatusOK, struct {
			QuestionID int64 `json:"question_id"`
		}{question})
	}
}
func (c *Client) AskUser(ctx context.Context, text, key string) (int64, error) {
	var out struct {
		QuestionID int64 `json:"question_id"`
	}
	err := c.callKeyed(ctx, http.MethodPost, "/v1/foreman/ask-user", key, struct {
		Text string `json:"text"`
	}{text}, &out)
	return out.QuestionID, err
}
