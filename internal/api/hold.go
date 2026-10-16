package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// holdFenceHeader carries the hold fence on every foreman mutation made
// with a mode A credential (§4.6, §12.1): the credential is the bearer
// secret, the fence this header.
const holdFenceHeader = "X-Brigada-Hold-Fence"

// The refusal and rejection codes of the interactive hold (§4.6).
const (
	codeStaleHold    = "stale_hold"    // a revoked credential or an old fence; nothing was applied
	codeForemanBusy  = "foreman_busy"  // a headless turn holds the slot
	codeHoldBusy     = "hold_busy"     // an attach or another bridge's hold holds the slot
	codeHoldRequired = "hold_required" // the call needs a mode A credential
)

// staleBody is the stale_hold answer: the uniform error shape, why, and
// the slot as it stands — a headless turn holding it is named with its
// start and events, so the session can say so and offer to wait.
type staleBody struct {
	Error string       `json:"error"`
	Code  string       `json:"code"`
	Slot  *engine.Slot `json:"slot,omitempty"`
	// ForemanBusy is true when a headless turn has the slot meanwhile.
	ForemanBusy bool `json:"foreman_busy,omitempty"`
}

// writeStale answers a stale mode A call 409 stale_hold (§4.6). With fence
// 0 and no message it asks the engine why the credential is dead. Nothing
// is applied; the caller sits in front of the idempotency wrapper, so the
// rejection is never recorded as a call's result.
func (s *Server) writeStale(w http.ResponseWriter, r *http.Request, hold, fence int64, msg string) {
	err := s.eng.CheckHold(r.Context(), hold, fence)
	var stale *engine.StaleHoldError
	if err != nil && !errors.As(err, &stale) {
		s.writeInternal(w, r, "check the hold", err)
		return
	}
	s.writeStaleErr(w, stale, msg)
}

// writeStaleErr renders a stale rejection the engine produced.
func (s *Server) writeStaleErr(w http.ResponseWriter, stale *engine.StaleHoldError, msg string) {
	body := staleBody{Code: codeStaleHold, Error: msg}
	if stale != nil {
		if body.Error == "" {
			body.Error = stale.Error()
		}
		if stale.Slot.State != engine.SlotFree && stale.Slot.State != "" {
			slot := stale.Slot
			body.Slot = &slot
			body.ForemanBusy = slot.State == engine.SlotTurn
		}
	}
	if body.Error == "" {
		body.Error = "stale hold: call hold_acquire, read the digest and decide again"
	}
	writeJSON(w, http.StatusConflict, body)
}

// holdFence reads the fence header; ok is false when it is absent or not
// a number.
func holdFence(r *http.Request) (int64, bool) {
	v := r.Header.Get(holdFenceHeader)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

// fenced gates a foreman mutation behind the hold fence (§4.6, §12.1): a
// call made with a mode A credential must carry its fence in
// X-Brigada-Hold-Fence, and the hold must live and hold that fence, or
// the call is rejected whole as stale_hold before anything is applied or
// recorded. Every other identity — a lease token, an attach hold's, the
// token file's — passes through untouched; their fencing is their own.
// touch says a successful call is activity (§4.6): it moves the idle
// bound. The events route passes false and records activity itself, only
// when it consumed something.
func (s *Server) fenced(touch bool, h handler) handler {
	return func(w http.ResponseWriter, r *http.Request, id identity) {
		if id.hold == 0 {
			h(w, r, id)
			return
		}
		fence, ok := holdFence(r)
		if !ok {
			s.writeStale(w, r, id.hold, 0, "stale hold: the call carries no "+holdFenceHeader+"; call hold_acquire and send the fence it returns")
			return
		}
		if err := s.eng.CheckHold(r.Context(), id.hold, fence); err != nil {
			var stale *engine.StaleHoldError
			if errors.As(err, &stale) {
				s.writeStale(w, r, id.hold, fence, "")
				return
			}
			s.writeInternal(w, r, "check the hold", err)
			return
		}
		// The check above is an early out. The authority is the engine's:
		// the guard rides in the context and every mutation validates hold
		// and fence inside the transaction that applies it, so a hold
		// revoked while this call was in flight applies nothing. The
		// response is held back until then, and a stale rejection replaces
		// whatever the handler made of the engine's error.
		guard := &engine.HoldGuard{Hold: id.hold, Fence: fence}
		rec := httptest.NewRecorder()
		h(rec, r.WithContext(engine.WithHoldGuard(r.Context(), guard)), id)
		if stale := guard.Stale(); stale != nil {
			s.writeStaleErr(w, stale, "")
			return
		}
		if touch && rec.Code/100 == 2 {
			if err := s.eng.HoldActivity(r.Context(), id.hold, fence); err != nil {
				s.log.ErrorContext(r.Context(), "api: record hold activity failed", "hold", id.hold, "err", err)
			}
		}
		for k, vv := range rec.Header() {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}
}

// acquireRequest is the hold_acquire body: the project by repo URL, the
// bridge instance asking, its label, and the session hint it saw.
type acquireRequest struct {
	RepoURL        string `json:"repo_url"`
	BridgeInstance string `json:"bridge_instance"`
	Label          string `json:"label"`
	SessionHint    string `json:"session_hint"`
	// Observations from the bridge's verified harness channel, kept separate
	// from model-supplied hints. These select history, never authority.
	HarnessSessionID         string    `json:"harness_session_id"`
	HarnessSessionSource     string    `json:"harness_session_source"`
	HarnessSessionObservedAt time.Time `json:"harness_session_observed_at"`
}

// AcquireResponse is the created hold: its id, the credential (the
// secret's one appearance), the fence every mutation must carry, the
// lease and activity bounds, and the state digest (§4.6).
type AcquireResponse struct {
	Answer       string             `json:"answer"`
	HoldID       int64              `json:"hold_id"`
	Credential   string             `json:"credential"`
	Fence        int64              `json:"fence"`
	LeaseExpires time.Time          `json:"lease_expires_at"`
	IdleExpires  time.Time          `json:"idle_expires_at"`
	MaxAt        time.Time          `json:"max_at"`
	Digest       *engine.HoldDigest `json:"digest"`
}

// holdRefusal is the busy acquire's 409 body: the holder, its label and
// since when, as fields (§4.6).
type holdRefusal struct {
	Error  string      `json:"error"`
	Code   string      `json:"code"`
	Holder engine.Slot `json:"holder"`
}

// handleHoldAcquire takes the slot for the user's own session (§4.6).
// Client and admin tokens only: minting the credential is a user act, and
// the hold-only bootstrap credential belongs to `brigada init`, not here.
func (s *Server) handleHoldAcquire(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req acquireRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RepoURL == "" || req.BridgeInstance == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "repo_url and bridge_instance are required")
		return
	}
	project, ok, err := s.projectOfRepo(r.Context(), req.RepoURL)
	if err != nil {
		s.writeInternal(w, r, "resolve the repo", err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no project for repo "+req.RepoURL)
		return
	}
	res, err := s.eng.AcquireHold(r.Context(), engine.HoldAcquire{
		Project: project, BridgeInstance: req.BridgeInstance, Label: req.Label, SessionHint: req.SessionHint,
		HarnessSessionID: req.HarnessSessionID, HarnessSessionSource: req.HarnessSessionSource,
		HarnessSessionObservedAt: req.HarnessSessionObservedAt,
	})
	if errors.Is(err, engine.ErrInvalidMessage) {
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if err != nil {
		s.writeInternal(w, r, "acquire the hold", err)
		return
	}
	if res.Answer == engine.AnsBusy {
		h := *res.Busy
		code, what := codeHoldBusy, "the foreman slot is held by "+string(h.State)
		if h.Label != "" {
			what += " " + strconv.Quote(h.Label)
		}
		what += " since " + h.Since
		if h.State == engine.SlotTurn {
			code = codeForemanBusy
		}
		writeJSON(w, http.StatusConflict, holdRefusal{Error: what, Code: code, Holder: h})
		return
	}
	writeJSON(w, http.StatusOK, AcquireResponse{
		Answer: string(res.Answer), HoldID: res.HoldID, Credential: res.Credential, Fence: res.Fence,
		LeaseExpires: res.LeaseExpires, IdleExpires: res.IdleExpires, MaxAt: res.MaxAt, Digest: res.Digest,
	})
}

// handleHoldHeartbeat renews the liveness lease (§4.6). It needs a mode
// A credential and its fence, and never reacquires: an ended hold or an
// old fence is stale_hold, and the bridge answers its tools hold_lost.
func (s *Server) handleHoldHeartbeat(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	fence, ok := s.holdCaller(w, r, id)
	if !ok {
		return
	}
	lease, err := s.eng.HoldHeartbeat(r.Context(), id.hold, fence)
	var stale *engine.StaleHoldError
	switch {
	case errors.As(err, &stale):
		s.writeStale(w, r, id.hold, fence, "")
	case err != nil:
		s.writeInternal(w, r, "heartbeat the hold", err)
	default:
		writeJSON(w, http.StatusOK, struct {
			Answer       string    `json:"answer"`
			LeaseExpires time.Time `json:"lease_expires_at"`
		}{string(engine.AnsOK), lease})
	}
}

// handleHoldYield ends the hold at the end of the session's turn (§4.6),
// honored only for the live hold with its credential and fence.
func (s *Server) handleHoldYield(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	fence, ok := s.holdCaller(w, r, id)
	if !ok {
		return
	}
	err := s.eng.YieldHold(r.Context(), id.hold, fence)
	var stale *engine.StaleHoldError
	switch {
	case errors.As(err, &stale):
		s.writeStale(w, r, id.hold, fence, "")
	case err != nil:
		s.writeInternal(w, r, "yield the hold", err)
	default:
		writeJSON(w, http.StatusOK, answerBody{Answer: string(engine.AnsOK)})
	}
}

// holdCaller requires a mode A credential with a fence header.
func (s *Server) holdCaller(w http.ResponseWriter, r *http.Request, id identity) (int64, bool) {
	if id.hold == 0 {
		writeError(w, http.StatusForbidden, codeHoldRequired, "this call needs a hold credential from hold_acquire")
		return 0, false
	}
	fence, ok := holdFence(r)
	if !ok {
		s.writeStale(w, r, id.hold, 0, "stale hold: the call carries no "+holdFenceHeader)
		return 0, false
	}
	return fence, true
}

// releaseHoldRequest names the project whose stuck hold the user releases.
type releaseHoldRequest struct {
	RepoURL string `json:"repo_url"`
}

// handleHoldRelease is the user's forced release of a stuck hold (§4.6):
// client authority, no credential. The engine revokes the credential and
// bumps the fence before the wake, as for any hold end.
func (s *Server) handleHoldRelease(w http.ResponseWriter, r *http.Request, _ identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req releaseHoldRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RepoURL == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "repo_url is required: name the project's repo URL")
		return
	}
	project, ok, err := s.projectOfRepo(r.Context(), req.RepoURL)
	if err != nil {
		s.writeInternal(w, r, "resolve the repo", err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, codeNotFound, "no project for repo "+req.RepoURL)
		return
	}
	ans, err := s.eng.ReleaseHoldForced(r.Context(), project)
	if err != nil {
		s.writeInternal(w, r, "release the hold", err)
		return
	}
	writeJSON(w, http.StatusOK, answerBody{Answer: string(ans)})
}
