package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handleProjectForeman serves the foreman's harness and model (§4.1,
// §12.1). One path, two methods with different role sets — PUT chooses
// the foreman's harness and model (client, admin), idempotently; GET
// reads the choice back (client, admin, foreman) — so the method picks
// the roles here instead of the route doing it.
func (s *Server) handleProjectForeman(w http.ResponseWriter, r *http.Request, id identity) {
	roles := map[string][]string{
		http.MethodPut: {roleClient, roleAdmin},
		http.MethodGet: {roleClient, roleAdmin, roleForeman},
	}[r.Method]
	if roles == nil {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			"method "+r.Method+" is not allowed here; want PUT or GET")
		return
	}
	if !slices.Contains(roles, id.role) {
		writeError(w, http.StatusForbidden, codeForbidden,
			fmt.Sprintf("role %q may not call this endpoint", id.role))
		return
	}
	if r.Method == http.MethodPut {
		s.idempotent(s.handleForemanSet)(w, r, id)
		return
	}
	s.handleForemanGet(w, r, id)
}

// handleForemanSet stores the user's choice of the foreman's harness
// and model (§4.1): the harness must resume headless, so a foreman turn
// can wake and continue the project's session. The engine's refusals —
// an empty field, a harness that cannot resume — are the caller's own
// mistakes, so they answer 400 like the task submit's; anything else —
// the store, the engine's own bookkeeping — is an internal failure,
// logged and named as one.
func (s *Server) handleForemanSet(w http.ResponseWriter, r *http.Request, _ identity) {
	var req foremanSetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	setting, err := s.eng.SetProjectForemanEffort(r.Context(), req.RepoURL, req.Harness, req.Model, req.Effort)
	if err != nil {
		if errors.Is(err, engine.ErrInvalidForeman) {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.writeInternal(w, r, "choose the foreman", err)
		return
	}
	writeJSON(w, http.StatusOK, setting)
}

// handleForemanGet reads the foreman setting back (§4.1): which harness
// and model the repo's project runs its foreman on. A foreman token is
// walled into its own project (§12.1): another project's repo and a repo
// the engine has no project for both answer the same 403 — a 404 would
// leak that the repo exists at all. Anyone else hears the plain 404 for
// a repo with no project, not an empty setting.
func (s *Server) handleForemanGet(w http.ResponseWriter, r *http.Request, id identity) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "the repo query parameter is required")
		return
	}
	setting, err := s.eng.ProjectForeman(r.Context(), repo)
	if errors.Is(err, engine.ErrInvalidRepoURL) {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if err != nil {
		s.writeInternal(w, r, "read the foreman setting", err)
		return
	}
	// A repo with no project has no project id to check against; for a
	// foreman that is the same as an out-of-scope one, so both refusals
	// stay indistinguishable.
	project := int64(0)
	if setting != nil {
		project = setting.ProjectID
	}
	if !id.mayTouchProject(project) {
		writeError(w, http.StatusForbidden, codeForbidden,
			"foreman token is scoped to project "+strconv.FormatInt(id.project, 10))
		return
	}
	if setting == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no project for repo "+engine.RedactRepoURL(repo))
		return
	}
	writeJSON(w, http.StatusOK, setting)
}

// handleProjectList serves the list of every project (client, admin): a
// foreman token is walled into its own project (§12.1) and has no use
// for the others' URLs.
func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	projects, err := s.eng.Projects(r.Context())
	if err != nil {
		s.writeInternal(w, r, "list the projects", err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

// Projects lists active projects with their foreman, active
// plans and needs-you count.
func (c *Client) Projects(ctx context.Context) ([]engine.ProjectInfo, error) {
	var out []engine.ProjectInfo
	if err := c.call(ctx, http.MethodGet, "/v1/projects", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// foremanSetRequest is the PUT body's shape (§4.1): which repo, and the
// harness and model the foreman runs on.
type foremanSetRequest struct {
	RepoURL string `json:"repo_url"`
	Harness string `json:"harness"`
	Model   string `json:"model"`
	// Effort is the foreman's reasoning-effort level; empty = unset.
	Effort string `json:"effort,omitempty"`
}

// SetProjectForeman chooses the foreman's harness and model for a repo
// (§4.1), creating the project when the engine has none for the URL. The
// engine refuses an empty field and a harness that cannot resume
// headless; those refusals come back as the API's 400s.
func (c *Client) SetProjectForeman(ctx context.Context, repoURL, harness, model, effort string) (engine.ForemanSetting, error) {
	body := foremanSetRequest{RepoURL: repoURL, Harness: harness, Model: model, Effort: effort}
	var out engine.ForemanSetting
	if err := c.call(ctx, http.MethodPut, "/v1/projects/foreman", body, &out); err != nil {
		return engine.ForemanSetting{}, err
	}
	return out, nil
}

// ProjectForeman reads the foreman's harness and model back (§4.1). The
// API's 404 — the repo has no project — comes back as the call's error.
func (c *Client) ProjectForeman(ctx context.Context, repoURL string) (engine.ForemanSetting, error) {
	var out engine.ForemanSetting
	path := "/v1/projects/foreman?repo=" + url.QueryEscape(repoURL)
	if err := c.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return engine.ForemanSetting{}, err
	}
	return out, nil
}

// projectArchiveRequest names the repository to remove and whether its live
// work should be cancelled. Only client and admin callers may remove projects.
type projectArchiveRequest struct {
	RepoURL string `json:"repo_url"`
	Cancel  bool   `json:"cancel"`
}

func (s *Server) handleProjectArchive(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req projectArchiveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RepoURL == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "repo URL is required")
		return
	}
	answer, err := s.eng.ArchiveProject(r.Context(), req.RepoURL, req.Cancel)
	switch {
	case errors.Is(err, engine.ErrProjectBusy):
		writeError(w, http.StatusConflict, codeConflict, err.Error())
	case errors.Is(err, engine.ErrNoProject):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
	case errors.Is(err, engine.ErrInvalidRepoURL):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
	case err != nil:
		s.writeInternal(w, r, "archive the project", err)
	default:
		writeJSON(w, http.StatusOK, answerBody{Answer: string(answer)})
	}
}

// ArchiveProject removes a repository from active service, preserving history.
// cancel authorizes cancellation of its live jobs and unfinished plans.
func (c *Client) ArchiveProject(ctx context.Context, repoURL string, cancel bool) (engine.Answer, error) {
	return c.answer(ctx, "/v1/projects/archive", projectArchiveRequest{RepoURL: repoURL, Cancel: cancel})
}
