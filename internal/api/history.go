package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
)

// handleArtifactUpload takes one artifact blob from the run's holder
// (§9, §12.3): the session's transcript or the check's log, keyed by
// job and lease and fenced like a complete. The blob rides base64 in
// the JSON body — the payload the engine caps at its configured 64 MiB
// — and the same content for the same job and kind answers already with
// the first row: uploads are idempotent by digest, so a retried
// transport failure costs nothing.
//
// The shape costs memory: a capped blob is held twice over, ~85 MB of
// base64 plus the ~64 MB the engine decodes. Acceptable while uploads
// are one runner's transcript at a time; a raw-body stream with the
// job and lease in headers is the leaner shape, if transcripts ever
// crowd the cap.
func (s *Server) handleArtifactUpload(w http.ResponseWriter, r *http.Request, runnerID int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	// Room for the capped blob's base64 plus the JSON around it; the
	// engine re-checks the decoded size against its own cap.
	capBytes := s.eng.MaxArtifactBytes()
	if capBytes < maxBody {
		capBytes = maxBody
	}
	bodyCap := int64(base64.StdEncoding.EncodedLen(int(capBytes)) + 4096)
	body, ok := readBodyCap(w, r, bodyCap)
	if !ok {
		return
	}
	var req artifactUploadRequest
	if err := decodeJSONBytes(r, body, &req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "content: malformed base64: "+err.Error())
		return
	}
	ans, info, err := s.eng.UploadArtifact(r.Context(), runnerID, req.JobID, req.LeaseID, req.Kind, data)
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrArtifactTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, codeTooLarge, err.Error())
		case errors.Is(err, engine.ErrInvalidArtifact):
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		default:
			s.writeInternal(w, r, fmt.Sprintf("upload artifact for job %d", req.JobID), err)
		}
		return
	}
	out := struct {
		Answer     string `json:"answer"`
		ArtifactID int64  `json:"artifact_id,omitempty"`
		SHA256     string `json:"sha256,omitempty"`
	}{Answer: string(ans)}
	if info != nil {
		out.ArtifactID, out.SHA256 = info.ID, info.SHA256
	}
	writeJSON(w, http.StatusOK, out)
}

// handleHistory reads the timeline (§9): events and messages merged,
// oldest first, one page at a time. A client or admin may read any
// project or all of them; a foreman token is walled into its own (§12.1)
// — an explicit ask for another project is refused, no ask means its
// own.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	q := engine.HistoryQuery{Since: r.URL.Query().Get("since")}
	if q.Since != "" {
		if _, ok := history.ParseCursor(q.Since); !ok {
			writeError(w, http.StatusBadRequest, codeBadRequest, "since: malformed cursor")
			return
		}
	}
	var err error
	if q.ProjectID, err = queryInt64(w, r, "project"); err != nil {
		return
	}
	if q.TaskID, err = queryInt64(w, r, "task"); err != nil {
		return
	}
	if q.JobID, err = queryInt64(w, r, "job"); err != nil {
		return
	}
	if q.Limit, err = queryInt(w, r, "limit"); err != nil {
		return
	}
	if id.role == roleForeman {
		if q.ProjectID != 0 && q.ProjectID != id.project {
			writeError(w, http.StatusForbidden, codeForbidden,
				"foreman token is scoped to project "+strconv.FormatInt(id.project, 10))
			return
		}
		q.ProjectID = id.project
	}
	page, err := s.eng.History(r.Context(), q)
	if err != nil {
		s.writeInternal(w, r, "read history", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleArtifactGet streams one artifact's blob (§9): the transcript or
// check log a run left behind, content-addressed on the engine's own
// disk. A foreman token may read only its own project's artifacts.
func (s *Server) handleArtifactGet(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	artID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "artifact id must be an integer")
		return
	}
	info, blob, err := s.eng.Artifact(r.Context(), artID)
	if err != nil {
		s.writeInternal(w, r, "read artifact", err)
		return
	}
	if info == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no such artifact")
		return
	}
	if !id.mayTouchProject(info.ProjectID) {
		// Not-found, not forbidden: answering 403 here would tell a
		// foreman whether an id exists in a project not its own, and
		// the artifact's existence is not that token's business.
		writeError(w, http.StatusNotFound, codeNotFound, "no such artifact")
		return
	}
	defer blob.Close()
	// ServeContent hands the range and resume mechanics; the name carries
	// the kind, the modtime stays unset so nothing caches on it.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=artifact-%d-%s", info.ID, info.Kind))
	http.ServeContent(w, r, "artifact-"+info.Kind, time.Time{}, blob)
}

// artifactUploadRequest is one artifact blob as the runner sends it
// (§9): the job and lease it belongs to, the artifact's kind, and the
// blob itself, base64 in the JSON the API otherwise speaks.
type artifactUploadRequest struct {
	jobLeaseRequest
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// queryInt64 reads one optional integer query parameter; 0 when absent,
// and the answer for a malformed one is already written when the error
// comes back.
func queryInt64(w http.ResponseWriter, r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			name+" must be a non-negative integer, got "+strconv.Quote(v))
		return 0, errInvalidQuery
	}
	return n, nil
}

// queryInt is queryInt64 for the int fields.
func queryInt(w http.ResponseWriter, r *http.Request, name string) (int, error) {
	n, err := queryInt64(w, r, name)
	return int(n), err
}

// errInvalidQuery only marks that the answer was already written; its
// text is never shown.
var errInvalidQuery = errors.New("invalid query parameter")

// decodeJSONBytes is decodeJSON over bytes the wrapper already read, so
// a handler with its own body cap decodes exactly what it capped.
func decodeJSONBytes(_ *http.Request, body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("malformed JSON body: %w", err)
	}
	return nil
}
