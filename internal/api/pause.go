package api

import (
	"errors"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// pauseRequest is the pause body (§8.3, §12.1 dispatch): the project's
// repo URL — empty pauses the engine itself — and the origin, `user`
// (the default, the caller's own `brigada pause`) or `drain`
// (`stop --engine --drain`).
type pauseRequest struct {
	RepoURL string `json:"repo_url"`
	Origin  string `json:"origin,omitempty"`
}

// resumeRequest is the resume body (§8.3): the scope's repo URL — empty
// lifts the engine's own pause — and drain_only, the lift `brigada
// start` makes: the engine's pause and only when a drain set it.
type resumeRequest struct {
	RepoURL   string `json:"repo_url"`
	DrainOnly bool   `json:"drain_only,omitempty"`
}

// handlePause pauses dispatch for one scope (§8.3): the engine with no
// repo URL, one project with it. Only client and admin tokens reach it
// — the route's roles — as plan approval is (§12.1): a foreman
// credential cannot pause the engine it works inside.
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req pauseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	origin := req.Origin
	if origin == "" {
		origin = engine.PauseOriginUser
	}
	if origin != engine.PauseOriginUser && origin != engine.PauseOriginDrain {
		writeError(w, http.StatusBadRequest, codeBadRequest, "origin must be user or drain")
		return
	}
	project, ok := s.dispatchScope(w, r, req.RepoURL)
	if !ok {
		return
	}
	res, err := s.eng.Pause(r.Context(), project, origin, id.name)
	switch {
	case errors.Is(err, engine.ErrInvalidPause):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
	case err != nil:
		s.writeInternal(w, r, "pause the scope", err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// handleResume lifts the pause of one scope and no other (§8.3). With
// drain_only it is the drain's own lift, `brigada start`'s: the
// engine's pause only when a drain set it, a resume with its wake
// evaluation; a user's pause stands.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req resumeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DrainOnly && req.RepoURL != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "a drain lift is the engine's own scope; name no repo URL")
		return
	}
	project, ok := s.dispatchScope(w, r, req.RepoURL)
	if !ok {
		return
	}
	var (
		res engine.ResumeResult
		err error
	)
	if req.DrainOnly {
		res, err = s.eng.ResumeDrain(r.Context(), id.name)
	} else {
		res, err = s.eng.Resume(r.Context(), project, id.name)
	}
	if err != nil {
		s.writeInternal(w, r, "resume the scope", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// dispatchScope resolves the scope a pause or resume names: 0, the
// engine itself, for an empty repo URL; the project the URL names
// otherwise. ok=false means the answer is already written.
func (s *Server) dispatchScope(w http.ResponseWriter, r *http.Request, repoURL string) (int64, bool) {
	if repoURL == "" {
		return 0, true
	}
	named, ok, err := s.projectOfRepo(r.Context(), repoURL)
	if errors.Is(err, engine.ErrInvalidRepoURL) {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return 0, false
	}
	if err != nil {
		s.writeInternal(w, r, "resolve the repo", err)
		return 0, false
	}
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound,
			"no project for repo "+engine.RedactRepoURL(repoURL))
		return 0, false
	}
	return named, true
}
