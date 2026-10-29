package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// handleIssueGet reads one issue or pull request of the foreman token's
// own project (§12.1 issues, §12.2 issue_read): the number names it on
// the project's own repository — the repository is never an argument,
// for the token's project alone picks it — and comments=1 asks for the
// comments too. GET alone: the endpoint reads, never writes. Every
// reason the read fails comes back as the API's own refusal carrying
// the reason, so the foreman can report it; nothing else is searched.
func (s *Server) handleIssueGet(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest, "the issue number must be a positive integer")
		return
	}
	issue, err := s.eng.ReadIssue(r.Context(), id.project, number, r.URL.Query().Get("comments") == "1")
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrIssueNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, err.Error())
		case errors.Is(err, engine.ErrIssueRead):
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		default:
			s.writeInternal(w, r, "read the issue", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, issue)
}
