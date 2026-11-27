package api

import (
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handleSync records sync merges detected from the landing branch's
// Brigada-Sync trailer (§7.1): the user's hand push under gate_push
// never, which the land job never made. The runner's headless foreman
// turn and the client's attach both refresh the checkout and report
// through here; the engine reads no checkout (product §5 rule 6), so the
// caller's read of the branch is the report's evidence.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		RepoURL string              `json:"repo_url"`
		Branch  string              `json:"branch"`
		Sync    []engine.SyncCommit `json:"sync"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.eng.RecordSync(r.Context(), req.RepoURL, req.Branch, req.Sync); err != nil {
		s.writeInternal(w, r, "record sync merges", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Recorded int `json:"recorded"`
	}{len(req.Sync)})
}
