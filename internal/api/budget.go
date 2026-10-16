package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handleBudget serves the foreman wake budget (§4.3, §12.1's budget
// group): GET reads one project's budget, today's use and the reset
// time; PUT sets the per-project daily budget and re-evaluates the
// waiting wake. Both are the user's (client, admin): the foreman does
// not raise the budget that bounds its own turns.
func (s *Server) handleBudget(w http.ResponseWriter, r *http.Request, id identity) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			"method "+r.Method+" is not allowed here; want GET or PUT")
		return
	}
	if id.role != roleClient && id.role != roleAdmin {
		writeError(w, http.StatusForbidden, codeForbidden,
			fmt.Sprintf("role %q may not call this endpoint", id.role))
		return
	}
	if r.Method == http.MethodPut {
		s.idempotent(s.handleBudgetSet)(w, r, id)
		return
	}
	repo := r.URL.Query().Get("repo")
	info, err := s.eng.WakeBudget(r.Context(), repo)
	if err != nil {
		if errors.Is(err, engine.ErrNoProject) {
			writeError(w, http.StatusNotFound, codeNotFound, err.Error())
			return
		}
		if errors.Is(err, engine.ErrInvalidRepoURL) {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.writeInternal(w, r, "read the wake budget", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleBudgetSet applies the budget; the engine's refusals (a budget
// out of bounds) are the caller's own and answer 400.
func (s *Server) handleBudgetSet(w http.ResponseWriter, r *http.Request, _ identity) {
	var req budgetSetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	info, err := s.eng.SetWakeBudget(r.Context(), req.RepoURL, req.Wakes)
	if err != nil {
		if errors.Is(err, engine.ErrInvalidSetting) || errors.Is(err, engine.ErrNoProject) ||
			errors.Is(err, engine.ErrInvalidRepoURL) {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.writeInternal(w, r, "set the wake budget", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// budgetSetRequest is the PUT body's shape: which repo, and the daily
// wake budget it takes.
type budgetSetRequest struct {
	RepoURL string `json:"repo_url"`
	Wakes   int    `json:"wakes"`
}

// WakeBudget reads the project's foreman wake budget (§4.3): the
// effective budget, today's use, the reset time and the waiting events.
// An empty repoURL names the engine's only project.
func (c *Client) WakeBudget(ctx context.Context, repoURL string) (*engine.WakeBudgetInfo, error) {
	var out *engine.WakeBudgetInfo
	if err := c.call(ctx, http.MethodGet, "/v1/budget?repo="+url.QueryEscape(repoURL), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetWakeBudget sets the project's daily foreman wake budget and
// re-evaluates the waiting wake (§4.3).
func (c *Client) SetWakeBudget(ctx context.Context, repoURL string, wakes int) (*engine.WakeBudgetInfo, error) {
	var out *engine.WakeBudgetInfo
	if err := c.call(ctx, http.MethodPut, "/v1/budget", budgetSetRequest{RepoURL: repoURL, Wakes: wakes}, &out); err != nil {
		return nil, err
	}
	return out, nil
}
