package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
)

// maxEventsWait caps the events long-poll's client-given wait (§4.2):
// long enough for the attached session to idle cheaply, short enough
// that a dead client is noticed.
const maxEventsWait = 25 * time.Second

// handleForemanAttach is the attach behind the API (§4.2 item 1, §12.1):
// the client or admin names the project by its repo URL, the way
// plan_create does for those roles. A busy answer — a live foreman turn,
// or a second live attach — is 409, so the client can say "foreman is
// busy" and wait; the message names what made it busy.
func (s *Server) handleForemanAttach(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req attachRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RepoURL == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"repo_url is required: name the project's repo URL")
		return
	}
	project, ok, err := s.projectOfRepo(r.Context(), req.RepoURL)
	if errors.Is(err, engine.ErrInvalidRepoURL) {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if err != nil {
		s.writeInternal(w, r, "resolve the repo", err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no project for repo "+engine.RedactRepoURL(req.RepoURL))
		return
	}
	res, err := s.eng.Attach(r.Context(), project, req.Force)
	if err != nil {
		// An attach the engine will not take — no foreman harness — is
		// the caller's mistake, not the engine's failure.
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if res.Answer == engine.AnsBusy {
		what := fmt.Sprintf("attach hold %d is live", res.HoldID)
		if res.JobID != 0 {
			what = fmt.Sprintf("turn %d is live", res.JobID)
		}
		// The refusal carries what made it busy as fields, not just
		// words: the client waits a turn out and refuses a second
		// attach by branch, never by parsing the message.
		writeJSON(w, http.StatusConflict, attachConflict{
			Error:  "foreman is busy: " + what,
			Code:   codeConflict,
			JobID:  res.JobID,
			HoldID: res.HoldID,
		})
		return
	}
	writeJSON(w, http.StatusOK, attachResponse{
		Answer:        string(res.Answer),
		HoldID:        res.HoldID,
		ForemanToken:  res.ForemanToken,
		RepoURL:       res.RepoURL,
		DefaultBranch: res.DefaultBranch,
		Harness:       res.Harness,
		Model:         res.Model,
		Effort:        res.Effort,
		SessionID:     res.SessionID,
		Prompt:        res.Prompt,
		ExpiresAt:     res.ExpiresAt,
	})
}

// handleForemanHeartbeat extends a live attach hold (§4.2 item 1), the
// way the runner's heartbeat extends a lease; the engine's answers — ok,
// gone — ride in the body.
func (s *Server) handleForemanHeartbeat(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req holdRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.HoldID <= 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest, "hold_id is required")
		return
	}
	ans, expires, err := s.eng.AttachHeartbeat(r.Context(), req.HoldID)
	if err != nil {
		s.writeInternal(w, r, fmt.Sprintf("heartbeat attach hold %d", req.HoldID), err)
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

// handleForemanRelease ends an attached chat (§4.2 item 1): the body
// carries the harness session id the client ended on, written back to
// the key's sessions row, and the rows the hold held are consumed. The
// engine's answers — ok; expired for a late release whose hold lapsed
// but whose session id was still written; gone — ride in the body.
func (s *Server) handleForemanRelease(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req releaseAttachRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.HoldID <= 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest, "hold_id is required")
		return
	}
	ans, err := s.eng.ReleaseAttach(r.Context(), req.HoldID, req.SessionID)
	if err != nil {
		s.writeInternal(w, r, fmt.Sprintf("release attach hold %d", req.HoldID), err)
		return
	}
	writeJSON(w, http.StatusOK, answerBody{Answer: string(ans)})
}

// handleForemanEvents long-polls the token's project for pending
// judgment events (§4.2): it re-asks the engine until the client-given
// wait (capped at 25 s) runs out, answering {"events": []} on timeout.
// Each check consumes what it returns — the attached foreman reads its
// project's events here, and a headless turn's token may too. A caller
// that went away is never answered into the void, and its rows are not
// consumed: the check below each wait refuses to read on a dead request
// context, so events the caller never saw stay for the next read.
func (s *Server) handleForemanEvents(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req eventsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	wait := time.Duration(req.WaitSeconds * float64(time.Second))
	if wait < 0 {
		wait = 0
	}
	if wait > maxEventsWait {
		wait = maxEventsWait
	}
	deadline := time.Now().Add(wait)
	for {
		if r.Context().Err() != nil {
			return
		}
		inbox, err := s.eng.ForemanEvents(r.Context(), id.project)
		if err != nil {
			s.writeInternal(w, r, "read the foreman events", err)
			return
		}
		if !inbox.Empty() {
			writeJSON(w, http.StatusOK, eventsResponse{Events: inbox.Events, UserMessages: inbox.UserMessages})
			return
		}
		if remain := time.Until(deadline); remain > 0 {
			timer := time.NewTimer(min(pollInterval, remain))
			select {
			case <-r.Context().Done(): // the caller went away
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		writeJSON(w, http.StatusOK, eventsResponse{Events: []string{}, UserMessages: []foreman.UserMessage{}})
		return
	}
}

// attachRequest is the attach body (§4.2 item 1, §12.1): the project by
// its repo URL, and force to cancel a live turn rather than wait it out.
type attachRequest struct {
	RepoURL string `json:"repo_url"`
	Force   bool   `json:"force"`
}

// attachConflict is the busy attach's 409 body: the error shape every
// refusal shares, plus what made it busy — the live turn's job id, or
// the live hold's id; the one that made it busy is set, the other zero
// — so the client can wait a turn out and refuse a second attach
// without parsing the message.
type attachConflict struct {
	Error  string `json:"error"`
	Code   string `json:"code"`
	JobID  int64  `json:"job_id"`
	HoldID int64  `json:"hold_id"`
}

// attachResponse names what attach created: the hold, its foreman token
// (the secret's one appearance on the wire, as in the lease grant), the
// foreman's harness and model, the key's current harness session id to
// resume, the opening prompt, and when the hold lapses without a beat.
type attachResponse struct {
	Answer        string    `json:"answer"`
	HoldID        int64     `json:"hold_id"`
	ForemanToken  string    `json:"foreman_token"`
	RepoURL       string    `json:"repo_url"`
	DefaultBranch string    `json:"default_branch"`
	Harness       string    `json:"harness"`
	Model         string    `json:"model"`
	Effort        string    `json:"effort,omitempty"`
	SessionID     string    `json:"session_id"`
	Prompt        string    `json:"prompt"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// holdRequest names one attach hold.
type holdRequest struct {
	HoldID int64 `json:"hold_id"`
}

// releaseAttachRequest ends one attach hold, carrying the harness
// session id the attached chat ended on (§4.2: resume forks session
// ids, so the key's row must carry what the chat reached).
type releaseAttachRequest struct {
	HoldID    int64  `json:"hold_id"`
	SessionID string `json:"session_id"`
}

// eventsRequest is the events body (§4.2): the client-given long-poll
// wait in seconds, capped at maxEventsWait.
type eventsRequest struct {
	WaitSeconds float64 `json:"wait_seconds"`
}

// eventsResponse is the events answer: the pending events, rendered
// exactly as the wake prompt renders them, consumed by the read. Empty
// says "no new events".
type eventsResponse struct {
	Events       []string              `json:"events"`
	UserMessages []foreman.UserMessage `json:"user_messages"` // the user's messages, apart from the events (§4.2 item 3)
}
