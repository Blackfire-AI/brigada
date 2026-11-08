package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The dispatch group (§12.1, §8.3): pause and resume, for the whole
// engine or one project. Client and admin tokens only — the routes'
// roles, as plan approval is — so a foreman credential, whose turns the
// pause holds, cannot lift it.

// pauseRequest is a pause: the project's repo URL (empty names the
// engine) and the origin, `user` when absent or `drain` for the drain of
// `brigada stop --engine --drain`.
type pauseRequest struct {
	RepoURL string `json:"repo_url"`
	Origin  string `json:"origin,omitempty"`
}

// resumeRequest is a resume: the project's repo URL (empty names the
// engine), and DrainOnly, which lifts the engine's pause only when its
// origin is `drain` — what `brigada start` does.
type resumeRequest struct {
	RepoURL   string `json:"repo_url"`
	DrainOnly bool   `json:"drain_only,omitempty"`
}

// PauseReply is what the pause call did: the engine's result and the
// paused project's repo URL (empty for the engine's own scope).
type PauseReply struct {
	engine.PauseResult
	RepoURL string `json:"repo_url,omitempty"`
}

// ResumeReply is what the resume call did, with the project's repo URL
// (empty for the engine's scope) and the pause that still holds the
// scope: "engine" when a project's scope stays paused by the engine's
// pause, or, for a drain-only lift that left it, the origin of the
// engine's standing pause ("user").
type ResumeReply struct {
	engine.ResumeResult
	RepoURL     string `json:"repo_url,omitempty"`
	StillPaused string `json:"still_paused,omitempty"`
}

// scopeOf resolves the request's repo URL to a project id; an empty URL
// is the engine's own scope, 0.
func (s *Server) scopeOf(w http.ResponseWriter, r *http.Request, repoURL string) (int64, string, bool) {
	if repoURL == "" {
		return 0, "", true
	}
	project, repo, err := s.eng.PauseProject(r.Context(), repoURL)
	if !s.writeDispatchError(w, r, "resolve the project", err) {
		return 0, "", false
	}
	return project, repo, true
}

// writeDispatchError answers the engine's refusals as the caller's own
// and reports whether err was nil.
func (s *Server) writeDispatchError(w http.ResponseWriter, r *http.Request, what string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, engine.ErrNoProject):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
	case errors.Is(err, engine.ErrInvalidRepoURL), errors.Is(err, engine.ErrInvalidPause):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
	default:
		s.writeInternal(w, r, what, err)
	}
	return false
}

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
	project, repo, ok := s.scopeOf(w, r, req.RepoURL)
	if !ok {
		return
	}
	res, err := s.eng.Pause(r.Context(), project, origin, id.name)
	if !s.writeDispatchError(w, r, "pause dispatch", err) {
		return
	}
	writeJSON(w, http.StatusOK, PauseReply{PauseResult: res, RepoURL: repo})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req resumeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DrainOnly && req.RepoURL != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "drain_only lifts the engine's pause alone; leave repo_url out")
		return
	}
	project, repo, ok := s.scopeOf(w, r, req.RepoURL)
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
	if !s.writeDispatchError(w, r, "resume dispatch", err) {
		return
	}
	reply := ResumeReply{ResumeResult: res, RepoURL: repo}
	switch {
	case req.DrainOnly && !res.Resumed:
		reply.StillPaused, _ = s.eng.LivePauseOrigin(r.Context(), 0)
	case project != 0:
		if origin, _ := s.eng.LivePauseOrigin(r.Context(), 0); origin != "" {
			reply.StillPaused = "engine"
		}
	}
	writeJSON(w, http.StatusOK, reply)
}

// Pause pauses dispatch (§8.3): an empty repoURL pauses the engine, a
// repo URL one project; origin is "user" (or "") or "drain".
func (c *Client) Pause(ctx context.Context, repoURL, origin string) (PauseReply, error) {
	var out PauseReply
	err := c.call(ctx, http.MethodPost, "/v1/dispatch/pause", pauseRequest{RepoURL: repoURL, Origin: origin}, &out)
	return out, err
}

// Resume lifts one scope's pause and no other's (§8.3): an empty repoURL
// is the engine's, a repo URL the project's.
func (c *Client) Resume(ctx context.Context, repoURL string) (ResumeReply, error) {
	var out ResumeReply
	err := c.call(ctx, http.MethodPost, "/v1/dispatch/resume", resumeRequest{RepoURL: repoURL}, &out)
	return out, err
}

// ResumeDrain lifts the engine's pause only when a drain set it
// (§8.3), the lift `brigada start` makes.
func (c *Client) ResumeDrain(ctx context.Context) (ResumeReply, error) {
	var out ResumeReply
	err := c.call(ctx, http.MethodPost, "/v1/dispatch/resume", resumeRequest{DrainOnly: true}, &out)
	return out, err
}
