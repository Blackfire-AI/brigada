package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// Server is the engine's HTTP API (design §12.1, §12.3): JSON over a
// Unix socket locally (Listen), the same handler behind TLS remotely.
// Clients, the foreman's MCP tools and runners reach the engine only
// through here, authenticating with bearer tokens in the roles of
// client, foreman (scoped to one project), runner and admin.
type Server struct {
	eng       *engine.Engine
	st        *store.Store
	log       *slog.Logger
	tokens    []token
	idemLocks keyLocks
}

// New builds the API server: it loads the token file — refusing one
// looser than mode 0600 — and re-syncs the sha256 hashes into the
// store's tokens table (§6.4: the secrets stay in the file, the store
// keeps only hashes). The store is also where idempotency responses
// live (§12.1).
func New(ctx context.Context, st *store.Store, eng *engine.Engine, log *slog.Logger, tokenFile string) (*Server, error) {
	tokens, err := loadTokenFile(tokenFile)
	if err != nil {
		return nil, err
	}
	if err := syncTokens(ctx, st, tokens); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{eng: eng, st: st, log: log, tokens: tokens}, nil
}

// Handler returns the API's routes behind authentication, so the same
// mux can serve a Unix socket (Listen) or TLS (§12.1).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/runner/register", s.authed([]string{roleRunner}, s.handleRegister))
	mux.HandleFunc("/v1/runner/lease", s.authed([]string{roleRunner}, s.withRunner(s.handleLease)))
	mux.HandleFunc("/v1/runner/heartbeat", s.authed([]string{roleRunner}, s.withRunner(s.handleHeartbeat)))
	mux.HandleFunc("/v1/runner/complete", s.authed([]string{roleRunner}, s.withRunner(s.handleComplete)))
	mux.HandleFunc("/v1/runner/release", s.authed([]string{roleRunner}, s.withRunner(s.handleRelease)))
	mux.HandleFunc("/v1/jobs", s.authed([]string{roleAdmin}, s.idempotent(s.handleSubmit)))
	mux.HandleFunc("/v1/jobs/{id}", s.authed([]string{roleClient, roleAdmin, roleForeman}, s.handleJobGet))
	mux.HandleFunc("/v1/jobs/{id}/cancel", s.authed([]string{roleClient, roleAdmin, roleForeman}, s.idempotent(s.handleJobCancel)))
	mux.HandleFunc("/v1/admin/fence-epoch", s.authed([]string{roleAdmin}, s.idempotent(s.handleFenceEpoch)))
	// The mux's own 404 is plain text; the API speaks JSON everywhere.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "no such endpoint")
	})
	return mux
}

// handleHealth is the one endpoint without authentication (§12.1): a
// probe for "is the engine up", not a source of state.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

// handleSubmit is the admin submit, the debug path until the foreman's
// tools exist (§12.1). The engine's answers — created, pending, capped —
// ride in the response body, not the status code.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req submitRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := req.params()
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	res, err := s.eng.Submit(r.Context(), *p)
	if err != nil {
		// Submit's errors are caller mistakes (an empty key, a kind that
		// needs no task, an unknown project or task), so they surface as
		// the client's, not the engine's.
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, submitResponse{
		Answer: string(res.Answer),
		JobID:  res.JobID,
		Run:    res.Run,
	})
}

// submitResponse is the submit's answer in the API's shape: the engine's
// answer word, the job to wait on and the key's run number.
type submitResponse struct {
	Answer string `json:"answer"`
	JobID  int64  `json:"job_id"`
	Run    int    `json:"run"`
}

// handleJobGet reads one job (§12.1). A foreman token may read only its
// own project's jobs (§12.1: foreman is scoped to its project).
func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}
	job, err := s.eng.Job(r.Context(), jobID)
	if err != nil {
		s.writeInternal(w, r, "read job", err)
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no such job")
		return
	}
	if !id.mayTouchProject(job.ProjectID) {
		writeError(w, http.StatusForbidden, codeForbidden,
			"foreman token is scoped to project "+strconv.FormatInt(id.project, 10))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleJobCancel stops a live job (§12.1). The engine's answers — ok,
// already, gone — ride in the response body; a foreman token may cancel
// only its own project's jobs.
func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}
	// The scope check needs the job's project, so a foreman token
	// cancelling a missing job hears not-found, not a scope error.
	job, err := s.eng.Job(r.Context(), jobID)
	if err != nil {
		s.writeInternal(w, r, "read job", err)
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no such job")
		return
	}
	if !id.mayTouchProject(job.ProjectID) {
		writeError(w, http.StatusForbidden, codeForbidden,
			"foreman token is scoped to project "+strconv.FormatInt(id.project, 10))
		return
	}
	ans, err := s.eng.Cancel(r.Context(), jobID)
	if err != nil {
		s.writeInternal(w, r, "cancel job", err)
		return
	}
	writeJSON(w, http.StatusOK, answerBody{Answer: string(ans)})
}

// handleFenceEpoch bumps the store-wide fence epoch (§6.4), for
// recover: leases from the older epoch are refused from here on.
func (s *Server) handleFenceEpoch(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	epoch, err := s.eng.BumpFenceEpoch(r.Context())
	if err != nil {
		s.writeInternal(w, r, "bump fence epoch", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Epoch int `json:"epoch"`
	}{epoch})
}

// parseJobID reads the {id} path value as a job id.
func parseJobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "job id must be an integer")
		return 0, false
	}
	return jobID, true
}

// pollInterval is how often a long-polling lease re-asks the engine.
const pollInterval = 200 * time.Millisecond

// maxLeaseWait caps the client-given lease wait (§12.3).
const maxLeaseWait = 30 * time.Second
