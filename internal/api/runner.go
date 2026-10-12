package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The runner protocol (§12.3): every endpoint is a POST that maps onto
// the lease engine, and the engine's answers — ok, lost, gone,
// cancelled, stale_epoch — are results in the response body, not HTTP
// errors. The runner's identity is its token's name, never a field of
// the body: what the body carries are references (job id, lease id) and
// payloads, not who is calling.

// handleRegister upserts the runner's row under its token name: the
// name and last_seen are all that is stored. The advertised harnesses
// and labels (§5.1, §8) are deliberately not persisted — the dispatcher
// that would turn them into agent rows is a later phase, and the lease
// call carries them on every poll, so a runner may change what it
// offers without re-registering. The advertisement is echoed back only
// to confirm the body was read.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req advertiseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	runnerID, err := s.eng.Register(r.Context(), id.name)
	if err != nil {
		s.writeInternal(w, r, "register runner "+id.name, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		RunnerID  int64    `json:"runner_id"`
		Name      string   `json:"name"`
		Harnesses []string `json:"harnesses"`
		Labels    []string `json:"labels"`
	}{runnerID, id.name, nonNil(req.Harnesses), nonNil(req.Labels)})
}

// handleLease long-polls for a job (§12.3): it re-asks the engine until
// the client-given wait (capped at 30 s) runs out, answering
// {"job": null} on timeout so an empty poll and a failure stay
// distinguishable.
func (s *Server) handleLease(w http.ResponseWriter, r *http.Request, runnerID int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req advertiseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	wait := time.Duration(req.WaitSeconds * float64(time.Second))
	if wait < 0 {
		wait = 0
	}
	if wait > maxLeaseWait {
		wait = maxLeaseWait
	}
	deadline := time.Now().Add(wait)
	for {
		grant, err := s.eng.Lease(r.Context(), runnerID, req.Harnesses, req.Labels)
		if err != nil {
			s.writeInternal(w, r, "lease", err)
			return
		}
		if grant != nil {
			writeJSON(w, http.StatusOK, struct {
				Job *jobGrant `json:"job"`
			}{jobGrantOf(grant)})
			return
		}
		if remain := time.Until(deadline); remain > 0 {
			pause := min(pollInterval, remain)
			timer := time.NewTimer(pause)
			select {
			case <-r.Context().Done(): // the runner went away
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		writeJSON(w, http.StatusOK, struct {
			Job *jobGrant `json:"job"`
		}{nil})
		return
	}
}

// handleHeartbeat reports the runner is still on the job (§12.3). The
// caller must also be the lease's holder: the engine checks the runner
// the token names against the runner the job was leased to, and a
// mismatched runner is lost like any other non-holder.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, runnerID int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req jobLeaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ans, expires, err := s.eng.Heartbeat(r.Context(), runnerID, req.JobID, req.LeaseID)
	if err != nil {
		s.writeInternal(w, r, fmt.Sprintf("heartbeat job %d", req.JobID), err)
		return
	}
	var expiry *time.Time
	if ans == engine.AnsOK {
		t := expires
		expiry = &t
	}
	writeJSON(w, http.StatusOK, struct {
		Answer    string     `json:"answer"`
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
	}{string(ans), expiry})
}

// handleComplete delivers the run's outcome (§5.4, §12.3): exactly one
// of result or error, the harness session the run ended on, and the
// artifacts it produced. As with heartbeat, only the lease's holder may
// deliver it.
func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request, runnerID int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req completeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if (req.Result == "") == (req.Error == "") {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"exactly one of result or error is required")
		return
	}
	c := engine.Completion{
		Result:    req.Result,
		Error:     req.Error,
		SessionID: req.SessionID,
	}
	for _, a := range req.Artifacts {
		c.Artifacts = append(c.Artifacts, engine.Artifact{
			Kind: a.Kind, Path: a.Path, Size: a.Size, SHA256: a.SHA256,
		})
	}
	ans, err := s.eng.Complete(r.Context(), runnerID, req.JobID, req.LeaseID, c)
	if err != nil {
		s.writeInternal(w, r, fmt.Sprintf("complete job %d", req.JobID), err)
		return
	}
	writeJSON(w, http.StatusOK, answerBody{Answer: string(ans)})
}

// handleRelease gives a job back (§12.3): queued again for the next
// runner, with the reason kept.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request, runnerID int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req releaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ans, err := s.eng.Release(r.Context(), runnerID, req.JobID, req.LeaseID, req.Reason)
	if err != nil {
		s.writeInternal(w, r, fmt.Sprintf("release job %d", req.JobID), err)
		return
	}
	writeJSON(w, http.StatusOK, answerBody{Answer: string(ans)})
}

// advertiseRequest is what a runner tells the engine about itself: the
// harnesses and labels it offers (§8). In a lease it rides along every
// poll; in a register it is the row's advertisement. WaitSeconds is the
// client-given long-poll wait in seconds (§12.3).
type advertiseRequest struct {
	WaitSeconds float64  `json:"wait"`
	Harnesses   []string `json:"harnesses"`
	Labels      []string `json:"labels"`
}

// jobLeaseRequest names a job and the lease the runner holds on it:
// references the runner earned at lease time, not identity.
type jobLeaseRequest struct {
	JobID   int64  `json:"job_id"`
	LeaseID string `json:"lease_id"`
}

type artifactRequest struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type completeRequest struct {
	jobLeaseRequest
	Result    string            `json:"result"`
	Error     string            `json:"error"`
	SessionID string            `json:"session_id"`
	Artifacts []artifactRequest `json:"artifacts"`
}

type releaseRequest struct {
	jobLeaseRequest
	Reason string `json:"reason"`
}

// jobGrant is a lease grant as the runner receives it (§12.3): the job
// to run and the lease to speak about it.
type jobGrant struct {
	JobID          int64     `json:"job_id"`
	ProjectID      int64     `json:"project_id"`
	TaskID         int64     `json:"task_id"`
	Kind           string    `json:"kind"`
	Mode           string    `json:"mode"`
	Key            string    `json:"key"`
	Prompt         string    `json:"prompt"`
	LeaseID        string    `json:"lease_id"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func jobGrantOf(g *engine.LeaseGrant) *jobGrant {
	return &jobGrant{
		JobID:          g.JobID,
		ProjectID:      g.ProjectID,
		TaskID:         g.TaskID,
		Kind:           string(g.Kind),
		Mode:           string(g.Mode),
		Key:            g.Key,
		Prompt:         g.Prompt,
		LeaseID:        g.LeaseID,
		LeaseExpiresAt: g.LeaseExpiresAt,
	}
}

// submitRequest is the admin submit body (§12.1): the debug path until
// the foreman's tools exist.
type submitRequest struct {
	Key       string   `json:"key"`
	ProjectID int64    `json:"project_id"`
	TaskID    int64    `json:"task_id"`
	Kind      string   `json:"kind"`
	Mode      string   `json:"mode"`
	Prompt    string   `json:"prompt"`
	Harness   string   `json:"harness"`
	Labels    []string `json:"labels"`
	Restart   bool     `json:"restart"`
}

// params validates the body into engine terms. Mode defaults to fresh;
// the pairing rules (a foreman turn belongs to no task; the worker
// kinds need one) the engine enforces itself.
func (req submitRequest) params() (*engine.SubmitParams, error) {
	if req.Key == "" {
		return nil, fmt.Errorf("key is required")
	}
	switch req.Kind {
	case string(engine.KindWork), string(engine.KindFix), string(engine.KindReview), string(engine.KindForeman):
	default:
		return nil, fmt.Errorf("kind must be one of work, fix, review or foreman")
	}
	mode := req.Mode
	switch mode {
	case "":
		mode = string(engine.ModeFresh)
	case string(engine.ModeFresh), string(engine.ModeSession):
	default:
		return nil, fmt.Errorf("mode must be fresh or session")
	}
	return &engine.SubmitParams{
		Key:       req.Key,
		ProjectID: req.ProjectID,
		TaskID:    req.TaskID,
		Kind:      engine.Kind(req.Kind),
		Mode:      engine.Mode(mode),
		Prompt:    req.Prompt,
		Harness:   req.Harness,
		Labels:    req.Labels,
		Restart:   req.Restart,
	}, nil
}

// nonNil returns an empty list for a missing field, so the JSON carries
// [] rather than null.
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
