package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The runner protocol (§12.3): every endpoint is a POST that maps onto
// the lease engine, and the engine's answers — ok, lost, gone,
// cancelled, stale_epoch — are results in the response body, not HTTP
// errors. The runner's identity is its token's name, never a field of
// the body: what the body carries are references (job id, lease id) and
// payloads, not who is calling.

// handleRegister upserts the runner's row under its token name and
// persists its advertisement (§5.1): the agents the body carries
// replace the runner's set in the agents table, keyed by runner,
// adapter and model (§11). The per-poll harness and label filter of
// lease is unchanged: a runner may still widen or narrow what it offers
// on every poll without re-registering.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request, id identity) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req advertiseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	agents := make([]engine.AgentAdvert, len(req.Agents))
	for i, a := range req.Agents {
		agents[i] = engine.AgentAdvert{
			Adapter: a.Adapter, Harness: a.Harness, Model: a.Model,
			Family: a.Family, Account: a.Account, Classes: a.Classes,
			Effort: a.Effort, Tools: a.Tools,
		}
	}
	runnerID, err := s.eng.Register(r.Context(), id.name, req.MaxParallel, agents)
	if err != nil {
		// An advertisement the engine refuses is the caller's mistake;
		// anything else — the store, the engine's own bookkeeping — is
		// an internal failure, logged and named as one.
		if errors.Is(err, engine.ErrInvalidAgent) {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.writeInternal(w, r, "register runner "+id.name, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		RunnerID  int64    `json:"runner_id"`
		Name      string   `json:"name"`
		Harnesses []string `json:"harnesses"`
		Labels    []string `json:"labels"`
		Agents    int      `json:"agents"`
	}{runnerID, id.name, nonNil(req.Harnesses), nonNil(req.Labels), len(agents)})
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

// handleHeartbeat reports the runner is still on the job (§12.3), with
// the worker's progress riding along (§5.6). The caller must also be
// the lease's holder: the engine checks the runner the token names
// against the runner the job was leased to, and a mismatched runner is
// lost like any other non-holder.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, runnerID int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req heartbeatRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	prog := engine.Progress{Events: nonNil(req.Events), Loop: req.Loop, LoopCall: req.LoopCall}
	if t, err := parseAPITime(req.LastProgressAt); err == nil {
		prog.LastProgressAt = t
	} else if req.LastProgressAt != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "last_progress_at: "+err.Error())
		return
	}
	if t, err := parseAPITime(req.LastActivityAt); err == nil {
		prog.LastActivityAt = t
	} else if req.LastActivityAt != "" {
		writeError(w, http.StatusBadRequest, codeBadRequest, "last_activity_at: "+err.Error())
		return
	}
	ans, expires, err := s.eng.Heartbeat(r.Context(), runnerID, req.JobID, req.LeaseID, prog)
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

// handleComplete delivers the run's outcome (§5.4, §5.5, §12.3):
// exactly one of result or error, the check result when the runner ran
// the task's check, the harness session the run ended on, and the
// artifacts it produced. An outcome set to stalled or time_cap (§5.6)
// names a run the runner stopped itself, which ends the job as stalled.
// As with heartbeat, only the lease's holder may deliver it.
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
	if req.Outcome != "" &&
		(req.Outcome != engine.OutcomeStalled && req.Outcome != engine.OutcomeTimeCap || req.Error != "") {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"outcome must be stalled or time_cap, with a result")
		return
	}
	c := engine.Completion{
		Usage:       req.Usage,
		WallSeconds: req.WallSeconds,
		Result:      req.Result,
		Error:       req.Error,
		CheckResult: req.CheckResult,
		SessionID:   req.SessionID,
		Outcome:     req.Outcome,
		ErrorKind:   req.ErrorKind,
		LimitUntil:  req.LimitUntil,

		StaleDropped: req.StaleDropped,
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

// handleProbes lists the runner's due broken-agent probes (§8 "Health"):
// the agents broken by repeated failures whose next probe is due, for
// the runner to probe and report.
func (s *Server) handleProbes(w http.ResponseWriter, r *http.Request, runnerID int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	probes, err := s.eng.DueProbes(r.Context(), runnerID)
	if err != nil {
		s.writeInternal(w, r, "list broken-agent probes", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Probes []engine.Probe `json:"probes"`
	}{nonNilProbes(probes)})
}

// handleProbeReport delivers one probe's result (§8 "Health"): whether
// the agent's CLI answered. A passing probe clears the agent; a failing
// one backs the next probe off.
func (s *Server) handleProbeReport(w http.ResponseWriter, r *http.Request, runnerID int64) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req struct {
		AgentID int64 `json:"agent_id"`
		OK      bool  `json:"ok"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AgentID == 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest, "agent_id is required")
		return
	}
	if err := s.eng.ReportProbe(r.Context(), runnerID, req.AgentID, req.OK); err != nil {
		if errors.Is(err, engine.ErrNoAgent) {
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		s.writeInternal(w, r, fmt.Sprintf("report probe for agent %d", req.AgentID), err)
		return
	}
	writeJSON(w, http.StatusOK, answerBody{Answer: string(engine.AnsOK)})
}

// nonNilProbes returns an empty list for a nil probe list, so the JSON
// carries [] rather than null.
func nonNilProbes(v []engine.Probe) []engine.Probe {
	if v == nil {
		return []engine.Probe{}
	}
	return v
}

// advertiseRequest is what a runner tells the engine about itself: the
// harnesses and labels it offers (§8) and, in a register, the agents it
// advertises (§5.1) and its own cap on parallel jobs (§8.1). In a lease
// it rides along every poll; in a register it is the row's
// advertisement. WaitSeconds is the client-given long-poll wait in
// seconds (§12.3).
type advertiseRequest struct {
	WaitSeconds float64       `json:"wait"`
	MaxParallel int           `json:"max_parallel"` // the runner's own cap; 0 = unset (§8.1)
	Harnesses   []string      `json:"harnesses"`
	Labels      []string      `json:"labels"`
	Agents      []agentAdvert `json:"agents"`
}

// agentAdvert is one advertised (adapter, harness, model) combination
// (§5.1, §11): what the dispatcher chooses between (§8). Account labels
// the login whose quota the agent spends; the runner defaults it to the
// harness name.
type agentAdvert struct {
	Adapter string   `json:"adapter"`
	Harness string   `json:"harness"`
	Model   string   `json:"model"`
	Family  string   `json:"family"`
	Account string   `json:"account"`
	Classes []string `json:"classes"`
	// Effort is the agent's reasoning-effort level; empty when unset.
	Effort string `json:"effort,omitempty"`
	Tools  string `json:"tools,omitempty"`
}

// jobLeaseRequest names a job and the lease the runner holds on it:
// references the runner earned at lease time, not identity.
type jobLeaseRequest struct {
	JobID   int64  `json:"job_id"`
	LeaseID string `json:"lease_id"`
}

// heartbeatRequest is a jobLeaseRequest plus the worker's progress
// (§5.6): when it last made progress, the last sign of life, the last
// few progress events, and the loop signal once the runner's watch has
// tripped. The times are RFC 3339.
type heartbeatRequest struct {
	jobLeaseRequest
	LastProgressAt string   `json:"last_progress_at"`
	LastActivityAt string   `json:"last_activity_at"`
	Events         []string `json:"events"`
	Loop           bool     `json:"loop,omitempty"`
	LoopCall       string   `json:"loop_call,omitempty"`
}

type completeRequest struct {
	Usage       *adapter.Usage `json:"usage,omitempty"`
	WallSeconds *float64       `json:"wall_seconds,omitempty"`
	jobLeaseRequest
	Result string `json:"result"`
	Error  string `json:"error"`
	// Outcome names a run the runner stopped itself (§5.6) — stalled,
	// or past its class time cap — and rides with the result.
	//
	// Artifacts are deliberately not part of the body: a completion
	// carrying paths of the caller's choosing could point the engine's
	// readers anywhere. The runner uploads artifacts through
	// /v1/runner/artifacts (§9), which stores them itself; an
	// "artifacts" field here is ignored.
	Outcome     string `json:"outcome,omitempty"`
	CheckResult string `json:"check_result"`
	SessionID   string `json:"session_id"`
	// ErrorKind and LimitUntil ride with an error: the failure's kind —
	// the CLI's own, or the job's infrastructure (a failed setup §5.7,
	// a fetch that could not be done §5.1) — and, for a limit, its
	// reset time (§4.4).
	ErrorKind  string `json:"error_kind,omitempty"`
	LimitUntil string `json:"limit_until,omitempty"`
	// StaleDropped names the target whose stale waiting ref a land job
	// deleted (§7.1).
	StaleDropped string `json:"stale_dropped,omitempty"`
}

type releaseRequest struct {
	jobLeaseRequest
	Reason string `json:"reason"`
}

// jobGrant is a lease grant as the runner receives it (§12.3): the job
// to run, the lease to speak about it, and the task's workspace fields
// (§5.1, §5.5, §7).
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
	Harness        string    `json:"harness"`
	// Agent is the harness of the agent the lease picked for the job
	// (§8): the runner runs the job with that agent's adapter and model,
	// never with the job's own harness constraint, which placed the job
	// but does not name the worker. Empty when the lease recorded no
	// agent.
	Agent string `json:"agent,omitempty"`
	// AgentAdapter and AgentModel name the picked agent exactly — its
	// registration identity is its adapter and model, which the harness
	// alone does not give. Set on a foreman turn's grant, whose model the
	// project chose; task grants also name their picked registration exactly.
	AgentAdapter string `json:"agent_adapter,omitempty"`
	AgentModel   string `json:"agent_model,omitempty"`
	RepoURL      string `json:"repo_url"`
	BaseRef      string `json:"base_ref"`
	CheckCmd     string `json:"check_cmd"`
	// WorkerTools is the project's worker tool scope (Claude spelling,
	// "+"-joined, or "none"); empty when unset.
	WorkerTools string `json:"worker_tools,omitempty"`
	// Setup is the environment step (§5.7) the runner runs before the
	// worker and again before the check; absent when the task has none.
	Setup *engine.SetupSpec `json:"setup,omitempty"`
	// ExtraRepos are the read-only repositories (§5.8) the runner checks
	// out beside the worktree; each carries the credential-free URL to
	// fetch from. Empty when the task has none.
	ExtraRepos []engine.ExtraRepo `json:"extra_repos,omitempty"`
	// ReviewBranch is the task's result branch a review job reviews
	// (§7): the worktree sits at it. Empty for every other kind.
	ReviewBranch string `json:"review_branch,omitempty"`
	// ReviewSince is the head a re-review's previous review judged (§7):
	// the diff file then holds the branch's commits since it. Empty for
	// every other job.
	ReviewSince string `json:"review_since,omitempty"`
	// Branch is the branch the engine names a work or fix job to work
	// on (§5.1): the worktree is built on it and the run's work is
	// pushed to it. Empty for a review and a foreman turn — and for an
	// engine that does not name branches, where the runner falls back
	// to its own naming.
	Branch string `json:"branch,omitempty"`
	// LandTarget is, for a land job (§7.1), the branch the task's diff
	// is squashed onto. Empty for every other kind.
	LandTarget string `json:"land_target,omitempty"`
	// DefaultBranch is, for a land job (§7.1), the branch a missing land
	// target is created from. Empty for every other kind.
	DefaultBranch string `json:"default_branch,omitempty"`
	// LandCommit is, for a land job's push phase (§7.1), the commit the
	// landing already recorded: the runner pushes exactly it, no new
	// squash. Empty for the squash phase and every other kind.
	LandCommit string `json:"land_commit,omitempty"`
	// Push is true for a land job's push phase (§7.1): the squash and
	// the check already happened, and the runner pushes the recorded
	// commit, then runs after_land.
	Push bool `json:"push,omitempty"`
	// GatePush is the project's push gate a land job obeys (§6.3): auto
	// pushes, ask records waiting for the user, never records the exact
	// push command without pushing. Empty for every other kind.
	GatePush string `json:"gate_push,omitempty"`
	// AfterLand is the repo-relative command a land job runs after the
	// push, or after the landing when the gate is never (§6.3). Empty =
	// none.
	AfterLand string `json:"after_land,omitempty"`
	// LandMessage is, for a land job's squash phase (§7.1), the task
	// data the squash commit message is built from. nil for the push
	// phase and every other kind.
	LandMessage     *engine.LandMessage `json:"land_message,omitempty"`
	Attempt         int64               `json:"attempt"`
	ResumeSessionID string              `json:"resume_session_id"`
	// SessionGen is the task's session generation (§5.1): the attempt
	// the lease joined, counted in the task's restarts. The runner names
	// the worktree path a resumed session needs from it — one path per
	// task, role and generation — so a fix round runs where its work job
	// did. 0 on a foreman turn and a task-less review.
	SessionGen int64 `json:"session_gen"`
	// ForemanToken is the lease-scoped foreman token (§12.1): minted with
	// the lease of a foreman turn, empty for every task job. This grant
	// is the secret's only appearance on the wire — the engine stores
	// only its hash, and the runner keeps it in memory alone (§5.1: a
	// runner never holds a long-lived foreman token; the lease-scoped one
	// is all it ever has).
	ForemanToken string `json:"foreman_token,omitempty"`
	// Effort is the task job's mapped effort, with the agent default applied.
	Effort string `json:"effort,omitempty"`
	// ForemanEffort is the project's chosen foreman effort level, on a
	// foreman turn's grant only. It overrides the runner agent's effort
	// for the turn; empty keeps the agent's.
	ForemanEffort string `json:"foreman_effort,omitempty"`
}

func jobGrantOf(g *engine.LeaseGrant) *jobGrant {
	return &jobGrant{
		JobID:           g.JobID,
		ProjectID:       g.ProjectID,
		TaskID:          g.TaskID,
		Kind:            string(g.Kind),
		Mode:            string(g.Mode),
		Key:             g.Key,
		Prompt:          g.Prompt,
		LeaseID:         g.LeaseID,
		LeaseExpiresAt:  g.LeaseExpiresAt,
		Harness:         g.Harness,
		Agent:           g.Agent,
		AgentAdapter:    g.AgentAdapter,
		AgentModel:      g.AgentModel,
		RepoURL:         g.RepoURL,
		BaseRef:         g.BaseRef,
		CheckCmd:        g.CheckCmd,
		WorkerTools:     g.WorkerTools,
		Setup:           g.Setup,
		ExtraRepos:      g.ExtraRepos,
		ReviewBranch:    g.ReviewBranch,
		ReviewSince:     g.ReviewSince,
		Branch:          g.Branch,
		LandTarget:      g.LandTarget,
		DefaultBranch:   g.DefaultBranch,
		LandCommit:      g.LandCommit,
		Push:            g.Push,
		GatePush:        g.GatePush,
		AfterLand:       g.AfterLand,
		LandMessage:     g.LandMessage,
		Attempt:         g.Attempt,
		ResumeSessionID: g.ResumeSessionID,
		SessionGen:      g.SessionGen,
		ForemanToken:    g.ForemanToken,
		Effort:          g.Effort,
		ForemanEffort:   g.ForemanEffort,
	}
}

// parseAPITime reads an RFC 3339 timestamp from a request body.
func parseAPITime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
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
