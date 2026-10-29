package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

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
	if errors.Is(err, engine.ErrInvalidRepoURL) {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if err != nil {
		s.writeInternal(w, r, "read the project settings", err)
		return
	}
	if got == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no project for repo "+engine.RedactRepoURL(repo))
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

// handleProjectImport imports a project.yaml the client read (§13.1): the
// body carries the file's content, never a path. A file the engine
// refuses (unknown key, wrong schema, another repo) is the caller's own
// and answers 400.
func (s *Server) handleProjectImport(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	var req importRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	for _, role := range engine.RoleKeys {
		if text, ok := req.Roles[role]; ok {
			if err := engine.ValidateRoleText(role, text); err != nil {
				writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
				return
			}
		}
	}
	for role := range req.Roles {
		if err := engine.ValidateRoleText(role, ""); err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
	}
	if req.Skills != nil {
		if err := engine.ValidateSkills(*req.Skills); err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
	}
	// The worker's file is the instructions setting (§5.9): its text
	// rides the project.yaml import itself, so the setting and the
	// worker row stay one text.
	instructions := req.Instructions
	if text, ok := req.Roles[engine.RoleWorker]; ok && instructions == nil {
		instructions = &text
	}
	got, err := s.eng.ImportProjectConfigWith(r.Context(), req.RepoURL, req.Content, instructions, req.Accept)
	if err != nil {
		if errors.Is(err, engine.ErrInvalidSetting) || errors.Is(err, engine.ErrInvalidRepoURL) {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.writeInternal(w, r, "import the project configuration", err)
		return
	}
	if !got.NeedsConfirm {
		configured := map[string]bool{}
		for role := range req.Roles {
			configured[role] = true
		}
		removals, err := s.eng.UnimportRoleFiles(r.Context(), req.RepoURL, configured)
		if err != nil {
			if errors.Is(err, engine.ErrInvalidSetting) {
				writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
				return
			}
			s.writeInternal(w, r, "unimport the role instructions", err)
			return
		}
		for _, rm := range removals {
			// A role whose key the file dropped moves to not
			// configured; a local override under it keeps its text
			// and shows as the conflict (§5.9).
			if rm.Kept != nil {
				got.Conflicts = append(got.Conflicts, rm.Change())
			} else {
				got.Applied = append(got.Applied, rm.Change())
			}
		}
		for _, role := range engine.RoleKeys {
			text, ok := req.Roles[role]
			if !ok || role == engine.RoleWorker {
				continue
			}
			kept, err := s.eng.ImportRoleFile(r.Context(), req.RepoURL, role, text)
			switch {
			case errors.Is(err, engine.ErrRoleTextKept):
				// The local text stays; the conflict shows both texts (§5.9).
				old := "local override"
				if kept != nil {
					old = kept.Text
				}
				got.Conflicts = append(got.Conflicts, engine.ConfigChange{Key: "instructions." + role, Old: strconv.Quote(old), New: strconv.Quote(text)})
			case errors.Is(err, engine.ErrInvalidSetting):
				writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
				return
			case err != nil:
				s.writeInternal(w, r, "import the role instructions", err)
				return
			}
		}
		if req.Skills != nil {
			if _, err := s.eng.ImportSkills(r.Context(), req.RepoURL, *req.Skills); err != nil {
				if errors.Is(err, engine.ErrInvalidSetting) {
					writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
					return
				}
				s.writeInternal(w, r, "import the skills", err)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, got)
}

// roleInstructionsRequest is the PUT body's shape: which repo, and the
// text of each role to set as a local override.
type roleInstructionsRequest struct {
	RepoURL string            `json:"repo_url"`
	Roles   map[string]string `json:"roles"`
}

// handleRoleInstructionsSet sets role instruction texts as local overrides
// (`project set instructions.<role>=…`). Every role is validated before any
// is stored, so a refusal changes nothing.
func (s *Server) handleRoleInstructionsSet(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	var req roleInstructionsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	for role, text := range req.Roles {
		if err := engine.ValidateRoleText(role, text); err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
	}
	for _, role := range engine.RoleKeys {
		text, ok := req.Roles[role]
		if !ok {
			continue
		}
		if _, err := s.eng.SetRoleInstruction(r.Context(), req.RepoURL, role, text); err != nil {
			if errors.Is(err, engine.ErrInvalidSetting) {
				writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
				return
			}
			s.writeInternal(w, r, "set the role instructions", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// SetRoleInstructions sets the roles' instruction texts as local overrides.
func (c *Client) SetRoleInstructions(ctx context.Context, repoURL string, roles map[string]string) error {
	return c.call(ctx, http.MethodPut, "/v1/projects/role-instructions", roleInstructionsRequest{RepoURL: repoURL, Roles: roles}, nil)
}

// importRequest is the PUT body's shape: the repo's origin, the file's
// content, and whether the user accepted a first import's replacement of
// hand-set values.
type importRequest struct {
	RepoURL string `json:"repo_url"`
	Content []byte `json:"content"` // base64 on the wire: the hash covers the exact bytes
	Accept  bool   `json:"accept,omitempty"`
	// Instructions is the text of the file project.yaml's instructions_file
	// names, read by the client (§5.9); nil when the file names none.
	Instructions *string `json:"instructions,omitempty"`
	// Roles holds the text of each role file the nested instructions map
	// names, read by the client (§5.9). A role absent is not configured;
	// an empty text is empty by choice.
	Roles map[string]string `json:"roles,omitempty"`
	// Skills is every skill the client parsed under skills_dir (§5.10);
	// nil when the file names no skills_dir, [] for a directory with none.
	Skills *[]engine.Skill `json:"skills,omitempty"`
}

// ImportProjectConfig sends a project.yaml's content for import. With
// accept false a first import over hand-set values returns them as diffs
// (NeedsConfirm) and changes nothing.
func (c *Client) ImportProjectConfig(ctx context.Context, repoURL string, content []byte, accept bool) (engine.ImportResult, error) {
	return c.ImportProjectConfigWith(ctx, repoURL, content, nil, accept)
}

// ImportProjectConfigWith is ImportProjectConfig for a file that names an
// instructions_file: instructions is the text the client read from it.
func (c *Client) ImportProjectConfigWith(ctx context.Context, repoURL string, content []byte, instructions *string, accept bool) (engine.ImportResult, error) {
	return c.ImportProjectConfigRoles(ctx, repoURL, content, instructions, nil, accept)
}

// ImportProjectConfigRoles is ImportProjectConfigWith for a file whose
// nested instructions map names role files: roles holds the text the
// client read for each (§5.9).
func (c *Client) ImportProjectConfigRoles(ctx context.Context, repoURL string, content []byte, instructions *string, roles map[string]string, accept bool) (engine.ImportResult, error) {
	return c.ImportProjectConfigFull(ctx, repoURL, content, instructions, roles, nil, accept)
}

// ImportProjectConfigFull is ImportProjectConfigRoles for a file that
// names a skills_dir (§5.10): skills, when not nil, is every skill the
// client parsed under it and replaces the project's set; an empty set
// drops them all.
func (c *Client) ImportProjectConfigFull(ctx context.Context, repoURL string, content []byte, instructions *string, roles map[string]string, skills *[]engine.Skill, accept bool) (engine.ImportResult, error) {
	var out engine.ImportResult
	if err := c.call(ctx, http.MethodPut, "/v1/projects/import", importRequest{RepoURL: repoURL, Content: content, Accept: accept, Instructions: instructions, Roles: roles, Skills: skills}, &out); err != nil {
		return engine.ImportResult{}, err
	}
	return out, nil
}

// FileDigests are the client's current digests of the files a project.yaml
// names (§5.9, §5.10), each over the file text without its version header.
type FileDigests struct {
	// Roles maps a role to the digest of its file.
	Roles map[string]string
	// Skills maps a skill name to the digests of its SKILL.md and its tree.
	Skills map[string]engine.SkillFileDigest
}

// StatusWithFileDigests is Status for one project with the client's current
// file digests, so the engine can flag what changed since import. The
// stored snapshots are not touched.
func (c *Client) StatusWithFileDigests(ctx context.Context, project int64, d FileDigests) (Status, error) {
	q := url.Values{"project": {strconv.FormatInt(project, 10)}}
	for role, digest := range d.Roles {
		q.Set("digest_"+role, digest)
	}
	for name, sd := range d.Skills {
		q.Set("skill_"+name, sd.Digest)
		if sd.Tree != "" {
			q.Set("skilltree_"+name, sd.Tree)
		}
	}
	var st Status
	if err := c.call(ctx, http.MethodGet, "/v1/status?"+q.Encode(), nil, &st); err != nil {
		return Status{}, err
	}
	return st, nil
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
