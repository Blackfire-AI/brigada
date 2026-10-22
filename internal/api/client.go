package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
)

// Client is the engine API's client side, the one callers run: the
// runner speaks the runner protocol (§12.3) through it over the Unix
// socket, authenticating with a runner token. The engine's protocol
// answers — ok, lost, gone, cancelled, stale_epoch — come back as
// engine.Answer values, not errors: they are outcomes the caller branches
// on. Errors are the transport and the API's own refusals.
type Client struct {
	http        *http.Client
	token       string
	readProject int64
}

// NewClient builds a client for the engine's Unix socket, bearing the
// given bearer token.
func NewClient(socketPath, token string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
				DisableKeepAlives: false,
			},
		},
		token: token,
	}
}

// NewClientWithHTTPClient uses a supplied transport, including an in-process
// handler transport. The HTTP client must route http://brigada to the API.
func NewClientWithHTTPClient(client *http.Client, token string) *Client {
	return &Client{http: client, token: token}
}

// ProjectReader returns a copy of the client whose reads — status,
// history, stats — name one project, for a caller whose token is not
// scoped to it (the coordinator's client token).
func (c *Client) ProjectReader(project int64) *Client {
	cp := *c
	cp.readProject = project
	return &cp
}

// Agent is one advertised agent as the client sends it (§5.1): an
// (adapter, harness, model) combination with its family, account label
// and the classes it can take. The account labels the login whose quota
// the agent spends (§8: quota cooling covers the whole account); the
// runner's default is the harness name.
type Agent struct {
	Adapter string   `json:"adapter"`
	Harness string   `json:"harness"`
	Model   string   `json:"model"`
	Family  string   `json:"family"`
	Account string   `json:"account"`
	Classes []string `json:"classes"`
	// Effort is the agent's reasoning-effort level; empty when unset.
	Effort string `json:"effort,omitempty"`
	// Tools is a claude worker's tool scope (§5.2), "+"-joined or
	// "none"; empty for a harness brigada does not scope.
	Tools string `json:"tools,omitempty"`
}

// Register registers the runner under its token's name with its
// advertised agents (§5.1, §12.3) and its own cap on parallel jobs
// (§8.1); maxParallel 0 means the runner advertised none. A re-register
// replaces the runner's set.
func (c *Client) Register(ctx context.Context, agents []Agent, maxParallel int) error {
	body := advertiseRequest{MaxParallel: maxParallel, Agents: make([]agentAdvert, len(agents))}
	for i, a := range agents {
		body.Agents[i] = agentAdvert{
			Adapter: a.Adapter, Harness: a.Harness, Model: a.Model,
			Family: a.Family, Account: a.Account, Classes: a.Classes,
			Effort: a.Effort, Tools: a.Tools,
		}
	}
	var out struct {
		RunnerID int64 `json:"runner_id"`
	}
	return c.call(ctx, http.MethodPost, "/v1/runner/register", body, &out)
}

// Lease long-polls for a job the runner may take (§12.3): one whose
// harness constraint it satisfies — every harness in harnesses is
// offered — and whose labels it covers. nil comes back when the wait
// runs out with nothing to do.
func (c *Client) Lease(ctx context.Context, wait time.Duration, harnesses, labels []string) (*JobGrant, error) {
	body := advertiseRequest{
		WaitSeconds: wait.Seconds(),
		Harnesses:   harnesses,
		Labels:      labels,
	}
	var out struct {
		Job *jobGrant `json:"job"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/runner/lease", body, &out); err != nil {
		return nil, err
	}
	if out.Job == nil {
		return nil, nil
	}
	g := JobGrant(*out.Job)
	return &g, nil
}

// Heartbeat reports the runner is still on the job, carrying the
// worker's progress (§5.6).
func (c *Client) Heartbeat(ctx context.Context, jobID int64, leaseID string, prog Progress) (engine.Answer, time.Time, error) {
	req := heartbeatRequest{
		jobLeaseRequest: jobLeaseRequest{JobID: jobID, LeaseID: leaseID},
		Events:          prog.Events,
		Loop:            prog.Loop,
		LoopCall:        prog.LoopCall,
	}
	if !prog.LastProgressAt.IsZero() {
		req.LastProgressAt = prog.LastProgressAt.UTC().Format(time.RFC3339Nano)
	}
	if !prog.LastActivityAt.IsZero() {
		req.LastActivityAt = prog.LastActivityAt.UTC().Format(time.RFC3339Nano)
	}
	var out struct {
		Answer    string     `json:"answer"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/runner/heartbeat", req, &out); err != nil {
		return "", time.Time{}, err
	}
	var expires time.Time
	if out.ExpiresAt != nil {
		expires = *out.ExpiresAt
	}
	return engine.Answer(out.Answer), expires, nil
}

// Completion is a run's outcome as the client delivers it (§5.4, §5.5):
// exactly one of Result or Error, the check result when the runner ran
// the task's check, and the harness session the run ended on. Outcome,
// when set to stalled or time_cap, names a run the runner stopped
// itself (§5.6) and rides with the Result. Artifacts are not the
// completion's to name: the runner uploads them through
// UploadArtifact (§9), which stores them itself.
type Completion struct {
	Usage       *adapter.Usage
	WallSeconds *float64
	Result      string
	Error       string
	CheckResult string
	SessionID   string
	Outcome     string
	// ErrorKind sorts an Error: the adapter's kind for a CLI failure
	// ("limit", "auth"…), empty when the failure was none the CLI named.
	// LimitUntil is the reset time of a "limit" (RFC3339, best-effort):
	// empty means unknown.
	ErrorKind  string
	LimitUntil string
	// StaleDropped names the target whose stale waiting ref a land job
	// deleted.
	StaleDropped string
}

// Complete delivers the run's outcome (§12.3).
func (c *Client) Complete(ctx context.Context, jobID int64, leaseID string, comp Completion) (engine.Answer, error) {
	req := completeRequest{
		jobLeaseRequest: jobLeaseRequest{JobID: jobID, LeaseID: leaseID},
		Usage:           comp.Usage,
		WallSeconds:     comp.WallSeconds,
		Result:          comp.Result,
		Error:           comp.Error,
		CheckResult:     comp.CheckResult,
		SessionID:       comp.SessionID,
		Outcome:         comp.Outcome,
		ErrorKind:       comp.ErrorKind,
		LimitUntil:      comp.LimitUntil,
		StaleDropped:    comp.StaleDropped,
	}
	return c.answer(ctx, "/v1/runner/complete", req)
}

// Release gives a job back (§12.3): queued again for the next runner,
// with the reason kept.
func (c *Client) Release(ctx context.Context, jobID int64, leaseID, reason string) (engine.Answer, error) {
	req := releaseRequest{
		jobLeaseRequest: jobLeaseRequest{JobID: jobID, LeaseID: leaseID},
		Reason:          reason,
	}
	return c.answer(ctx, "/v1/runner/release", req)
}

// RecordLanded reports landings detected from the project's default
// branch (§7.1): each entry names a task whose Brigada-Task trailer sits
// on a commit of branch, landed outside the land job. The engine records
// the ones that belong to repoURL's project and have no landing yet.
func (c *Client) RecordLanded(ctx context.Context, repoURL, branch string, landed []engine.LandedCommit) error {
	body := struct {
		RepoURL string                `json:"repo_url"`
		Branch  string                `json:"branch"`
		Landed  []engine.LandedCommit `json:"landed"`
	}{repoURL, branch, landed}
	return c.call(ctx, http.MethodPost, "/v1/landed", body, nil)
}

// DueProbes lists the runner's due broken-agent probes (§8 "Health"):
// the failure-broken agents whose next probe is due, for the runner to
// probe and report.
func (c *Client) DueProbes(ctx context.Context) ([]engine.Probe, error) {
	var out struct {
		Probes []engine.Probe `json:"probes"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/runner/probes", struct{}{}, &out); err != nil {
		return nil, err
	}
	return out.Probes, nil
}

// ReportProbe delivers one probe's result (§8 "Health"): ok is whether
// the agent's CLI answered.
func (c *Client) ReportProbe(ctx context.Context, agentID int64, ok bool) error {
	req := struct {
		AgentID int64 `json:"agent_id"`
		OK      bool  `json:"ok"`
	}{agentID, ok}
	return c.call(ctx, http.MethodPost, "/v1/runner/probes/report", req, nil)
}

// UploadArtifact uploads one artifact blob of a run the caller holds
// (§9, §12.3): the session's transcript or the check's log, keyed by
// job and lease and fenced like a complete. The engine caps the blob
// and stores it content-addressed; the same content for the same job
// and kind answers already with the first row's id.
func (c *Client) UploadArtifact(ctx context.Context, jobID int64, leaseID, kind string, data []byte) (engine.Answer, int64, error) {
	req := artifactUploadRequest{
		jobLeaseRequest: jobLeaseRequest{JobID: jobID, LeaseID: leaseID},
		Kind:            kind,
		Content:         base64.StdEncoding.EncodeToString(data),
	}
	var out struct {
		Answer     string `json:"answer"`
		ArtifactID int64  `json:"artifact_id"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/runner/artifacts", req, &out); err != nil {
		return "", 0, err
	}
	return engine.Answer(out.Answer), out.ArtifactID, nil
}

// HistoryQuery is one page of the timeline as the client asks for it
// (§9): the filters, the cursor the previous page ended on, and the
// page size. Last asks for the newest page instead of the next one; it
// cannot be combined with Since.
type HistoryQuery struct {
	ProjectID int64
	TaskID    int64
	JobID     int64
	Since     string
	Limit     int
	Last      bool
}

// History reads one page of the timeline (§9): events and messages
// merged, oldest first. With Last it is the newest page.
func (c *Client) History(ctx context.Context, q HistoryQuery) (history.Page, error) {
	if q.ProjectID == 0 {
		q.ProjectID = c.readProject
	}
	vals := make(url.Values)
	add := func(name string, n int64) {
		if n != 0 {
			vals.Set(name, strconv.FormatInt(n, 10))
		}
	}
	add("project", q.ProjectID)
	add("task", q.TaskID)
	add("job", q.JobID)
	if q.Since != "" {
		vals.Set("since", q.Since)
	}
	if q.Limit > 0 {
		vals.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Last {
		vals.Set("last", "1")
	}
	var page history.Page
	path := "/v1/history"
	if enc := vals.Encode(); enc != "" {
		path += "?" + enc
	}
	if err := c.call(ctx, http.MethodGet, path, nil, &page); err != nil {
		return history.Page{}, err
	}
	return page, nil
}

// Artifact fetches one artifact's blob (§9): the transcript or check
// log a run left behind.
func (c *Client) Artifact(ctx context.Context, id int64) ([]byte, error) {
	return c.raw(ctx, http.MethodGet, "/v1/artifacts/"+strconv.FormatInt(id, 10))
}

// Progress is what a heartbeat carries besides its hold (§5.6).
type Progress struct {
	LastProgressAt time.Time
	LastActivityAt time.Time
	Events         []string
	// Loop reports the worker repeating the same tool call (§5.6);
	// LoopCall names the call. The signal rides every beat once it
	// trips; nothing is stopped for it.
	Loop     bool
	LoopCall string
}

// StatusError is the API refusing a call: the transport worked, the
// engine answered, and the answer was no. Callers branch on Status —
// a 4xx is the caller's own mistake and will not heal with retries,
// while anything else (the store behind the engine, a socket that
// vanished mid-request) may.
type StatusError struct {
	Method  string
	Path    string
	Status  int
	APICode string // the API's own error code, e.g. unknown_runner
	Message string
	// Body is the refusal's raw body, kept for the callers that read a
	// structured refusal the plain message cannot carry — the attach's
	// busy answer names the turn or hold that made it busy.
	Body []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.Status, e.APICode, e.Message)
}

// IsClientError reports whether the refusal is the caller's: a 4xx
// names a mistake in the call itself, permanent until the caller
// changes, so retrying it is noise.
func (e *StatusError) IsClientError() bool {
	return e.Status >= 400 && e.Status < 500
}

// JobGrant is a lease grant as the runner receives it: the job to run,
// the lease to speak about it, and the task's workspace fields.
type JobGrant jobGrant

// call performs one POST with the client's token, decoding a 200 into
// out and turning every other status into an error that names the API's
// own message.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	return c.callKeyed(ctx, method, path, "", body, out)
}

// callKeyed is call with one extra request header: the idempotency key
// a mutating call rides (§12.1), minted by whoever knows what makes the
// call one logical call — the bridge, for a tool call — and never the
// model.
func (c *Client) callKeyed(ctx context.Context, method, path, idemKey string, body, out any) error {
	var rd io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: encode body: %w", method, path, err)
		}
		rd = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://brigada"+path, rd)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var eb struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		json.Unmarshal(data, &eb) // the shape is best-effort for the message
		return &StatusError{
			Method: method, Path: path,
			Status: resp.StatusCode, APICode: eb.Code, Message: eb.Error, Body: data,
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// answer performs one POST whose body carries an engine answer word.
func (c *Client) answer(ctx context.Context, path string, body any) (engine.Answer, error) {
	var out struct {
		Answer string `json:"answer"`
	}
	if err := c.call(ctx, http.MethodPost, path, body, &out); err != nil {
		return "", err
	}
	return engine.Answer(out.Answer), nil
}

// raw performs one call whose answer is bytes, not JSON — an artifact
// blob streams through exactly as it lies (§9) — with the transport and
// refusal handling of call.
func (c *Client) raw(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://brigada"+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var eb struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		json.Unmarshal(data, &eb) // the shape is best-effort for the message
		return nil, &StatusError{
			Method: method, Path: path,
			Status: resp.StatusCode, APICode: eb.Code, Message: eb.Error, Body: data,
		}
	}
	return data, nil
}
