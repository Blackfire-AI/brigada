package api

import (
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handleLanded records landings detected from the default branch's
// Brigada-Task trailers (§7.1): a hand squash, or a brigada land in
// another clone, whose commits the land job never made. The runner's
// headless foreman turn and the client's attach both refresh the
// project's checkout and report through here; the engine runs no git
// (product §5 rule 1), so the caller's read of the branch is the
// report's evidence.
func (s *Server) handleLanded(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		RepoURL string                `json:"repo_url"`
		Branch  string                `json:"branch"`
		Landed  []engine.LandedCommit `json:"landed"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.eng.RecordLanded(r.Context(), req.RepoURL, req.Branch, req.Landed); err != nil {
		s.writeInternal(w, r, "record landed tasks", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Recorded int `json:"recorded"`
	}{len(req.Landed)})
}
