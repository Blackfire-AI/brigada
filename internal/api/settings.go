package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handleProjectSettings serves a project's editable settings (§6.3): PUT
// applies key=value pairs, GET reads every key's effective value. Both
// are the user's (client, admin); the foreman does not edit the limit
// that governs its own plans.
func (s *Server) handleProjectSettings(w http.ResponseWriter, r *http.Request, id identity) {
	if r.Method != http.MethodPut && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			"method "+r.Method+" is not allowed here; want PUT or GET")
		return
	}
	if id.role != roleClient && id.role != roleAdmin {
		writeError(w, http.StatusForbidden, codeForbidden,
			fmt.Sprintf("role %q may not call this endpoint", id.role))
		return
	}
	if r.Method == http.MethodPut {
		s.idempotent(s.handleSettingsSet)(w, r, id)
		return
	}
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "the repo query parameter is required")
		return
	}
	got, err := s.eng.ProjectSettings(r.Context(), repo)
	if err != nil {
		s.writeInternal(w, r, "read the project settings", err)
		return
	}
	if got == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no project for repo "+repo)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// handleSettingsSet applies the settings; the engine's refusals (unknown
// key, bad value) are the caller's own and answer 400.
func (s *Server) handleSettingsSet(w http.ResponseWriter, r *http.Request, _ identity) {
	var req settingsSetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	got, err := s.eng.SetProjectSettings(r.Context(), req.RepoURL, req.Values)
	if err != nil {
		if errors.Is(err, engine.ErrInvalidSetting) {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.writeInternal(w, r, "set the project settings", err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// settingsSetRequest is the PUT body's shape: which repo, and the
// key → value pairs to apply (an empty value resets the key).
type settingsSetRequest struct {
	RepoURL string            `json:"repo_url"`
	Values  map[string]string `json:"values"`
}

// SetProjectSettings applies key=value pairs to the repo's project.
func (c *Client) SetProjectSettings(ctx context.Context, repoURL string, values map[string]string) (engine.ProjectSettings, error) {
	var out engine.ProjectSettings
	if err := c.call(ctx, http.MethodPut, "/v1/projects/settings", settingsSetRequest{RepoURL: repoURL, Values: values}, &out); err != nil {
		return engine.ProjectSettings{}, err
	}
	return out, nil
}

// ProjectSettings reads the repo's settings. The API's 404 — the repo has
// no project — comes back as the call's error.
func (c *Client) ProjectSettings(ctx context.Context, repoURL string) (engine.ProjectSettings, error) {
	var out engine.ProjectSettings
	if err := c.call(ctx, http.MethodGet, "/v1/projects/settings?repo="+url.QueryEscape(repoURL), nil, &out); err != nil {
		return engine.ProjectSettings{}, err
	}
	return out, nil
}
