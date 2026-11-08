package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The dispatch group (§12.1, §8.3): pause and resume, scoped to the whole
// engine or to one project by its repo URL. Both are `client` and `admin`
// actions, as plan approval is: a foreman credential is refused before the
// handler runs, for the foreman never holds its own dispatch.

// pauseRequest is the pause body: the repo URL of the project to pause,
// empty for the engine, and the pause's origin, `user` unless a drain sets
// it (`brigada stop --engine --drain`).
type pauseRequest struct {
	RepoURL string `json:"repo_url,omitempty"`
	Origin  string `json:"origin,omitempty"`
}

// resumeRequest is the resume body: the scope to lift as the pause names
// it, and DrainOnly for the lift `brigada start` makes, which leaves a
// user's pause in place. DrainOnly lifts the engine's pause alone, so it
// takes no repo URL.
type resumeRequest struct {
	RepoURL   string `json:"repo_url,omitempty"`
	DrainOnly bool   `json:"drain_only,omitempty"`
}

// dispatchScope resolves the body's repo URL to the scope a pause or a
// resume acts on: no URL is the engine (0), a URL the project that
// carries it. On a failure it has answered, and ok is false.
func (s *Server) dispatchScope(w http.ResponseWriter, r *http.Request, repo string) (int64, bool) {
	if repo == "" {
		return 0, true
	}
	project, found, err := s.projectOfRepo(r.Context(), repo)
	switch {
	case errors.Is(err, engine.ErrInvalidRepoURL):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return 0, false
	case err != nil:
		s.writeInternal(w, r, "read the project", err)
		return 0, false
	case !found:
		writeError(w, http.StatusNotFound, codeNotFound, "no project for repo "+engine.RedactRepoURL(repo))
		return 0, false
	}
	return project, true
}

// handlePause pauses dispatch for one scope (§8.3). The answer says
// whether this call began the pause and the origin that stands.
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
	project, ok := s.dispatchScope(w, r, req.RepoURL)
	if !ok {
		return
	}
	res, err := s.eng.Pause(r.Context(), project, origin, id.name)
	switch {
	case errors.Is(err, engine.ErrInvalidPause):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
	case errors.Is(err, engine.ErrNoProject):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
	case err != nil:
		s.writeInternal(w, r, "pause dispatch", err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// handleResume lifts the pause of one scope and no other (§8.3). A scope
// that is not paused answers resumed=false, not an error.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req resumeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DrainOnly && req.RepoURL != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"drain_only lifts the engine's pause alone and takes no repo URL")
		return
	}
	var (
		res engine.ResumeResult
		err error
	)
	if req.DrainOnly {
		res, err = s.eng.ResumeDrain(r.Context(), id.name)
	} else {
		project, ok := s.dispatchScope(w, r, req.RepoURL)
		if !ok {
			return
		}
		res, err = s.eng.Resume(r.Context(), project, id.name)
	}
	if err != nil {
		s.writeInternal(w, r, "resume dispatch", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Pause pauses dispatch for the engine (empty repoURL) or for the project
// the repo URL names (§8.3). origin is engine.PauseOriginUser for the
// user's own pause and engine.PauseOriginDrain for a drain's; empty is the
// user's.
func (c *Client) Pause(ctx context.Context, repoURL, origin string) (engine.PauseResult, error) {
	var out engine.PauseResult
	if err := c.call(ctx, http.MethodPost, "/v1/dispatch/pause", pauseRequest{RepoURL: repoURL, Origin: origin}, &out); err != nil {
		return engine.PauseResult{}, err
	}
	return out, nil
}

// Resume lifts the pause of the engine (empty repoURL) or of the project
// the repo URL names, and no other (§8.3).
func (c *Client) Resume(ctx context.Context, repoURL string) (engine.ResumeResult, error) {
	var out engine.ResumeResult
	if err := c.call(ctx, http.MethodPost, "/v1/dispatch/resume", resumeRequest{RepoURL: repoURL}, &out); err != nil {
		return engine.ResumeResult{}, err
	}
	return out, nil
}

// ResumeDrain lifts the engine's pause only when a drain set it, the lift
// `brigada start` makes (§8.3); a user's pause stands and the result says
// nothing was resumed.
func (c *Client) ResumeDrain(ctx context.Context) (engine.ResumeResult, error) {
	var out engine.ResumeResult
	if err := c.call(ctx, http.MethodPost, "/v1/dispatch/resume", resumeRequest{DrainOnly: true}, &out); err != nil {
		return engine.ResumeResult{}, err
	}
	return out, nil
}
