package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// reviewRequest is a standalone review (§7): the branch to review, and
// the project's repo URL — empty names the engine's only project.
type reviewRequest struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
}

// handleReview queues a review of a branch with no plan or task behind
// it (§7, §12.1). Only client and admin tokens reach it — the route's
// roles — and it is idempotent like the other submits. The answer names
// the job: the one created, or the live review of the branch a repeat
// coalesced behind.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req reviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.eng.ReviewBranch(r.Context(), req.RepoURL, req.Branch)
	switch {
	case errors.Is(err, engine.ErrInvalidReview), errors.Is(err, engine.ErrInvalidRepoURL):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	case errors.Is(err, engine.ErrNoProject):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
		return
	case errors.Is(err, engine.ErrNoReviewer):
		writeError(w, http.StatusConflict, codeConflict, err.Error())
		return
	case err != nil:
		s.writeInternal(w, r, "queue the review", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ReviewBranch asks the engine for a standalone review of a branch
// (§7) through /v1/reviews and returns the job it queued. An empty
// repoURL names the engine's only project; idemKey, when set, makes a
// retried call queue one review (§12.1).
func (c *Client) ReviewBranch(ctx context.Context, repoURL, branch, idemKey string) (engine.ReviewBranchResult, error) {
	var res engine.ReviewBranchResult
	if err := c.callKeyed(ctx, http.MethodPost, "/v1/reviews", idemKey, reviewRequest{RepoURL: repoURL, Branch: branch}, &res); err != nil {
		return engine.ReviewBranchResult{}, err
	}
	return res, nil
}
