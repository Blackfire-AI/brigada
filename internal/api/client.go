package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// Client is the engine API's client side, the one callers run: the
// runner speaks the runner protocol (§12.3) through it over the Unix
// socket, authenticating with a runner token. The engine's protocol
// answers — ok, lost, gone, cancelled, stale_epoch — come back as
// engine.Answer values, not errors: they are outcomes the caller branches
// on. Errors are the transport and the API's own refusals.
type Client struct {
	http  *http.Client
	token string
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

// Agent is one advertised agent as the client sends it (§5.1): an
// (adapter, harness, model) combination with its family, account label
// and the classes it can take.
type Agent struct {
	Adapter string   `json:"adapter"`
	Harness string   `json:"harness"`
	Model   string   `json:"model"`
	Family  string   `json:"family"`
	Account string   `json:"account"`
	Classes []string `json:"classes"`
}

// Register registers the runner under its token's name with its
// advertised agents (§5.1, §12.3). A re-register replaces the runner's
// set.
func (c *Client) Register(ctx context.Context, agents []Agent) error {
	body := advertiseRequest{Agents: make([]agentAdvert, len(agents))}
	for i, a := range agents {
		body.Agents[i] = agentAdvert{
			Adapter: a.Adapter, Harness: a.Harness, Model: a.Model,
			Family: a.Family, Account: a.Account, Classes: a.Classes,
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

// Artifact is one artifact reference a completion carries (§9): the
// session transcript after a job, the log of a check the runner ran.
type Artifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Completion is a run's outcome as the client delivers it (§5.4, §5.5):
// exactly one of Result or Error, the check result when the runner ran
// the task's check, the harness session the run ended on, and the
// artifacts it produced. Outcome, when set to stalled or time_cap,
// names a run the runner stopped itself (§5.6) and rides with the
// Result.
type Completion struct {
	Result      string
	Error       string
	CheckResult string
	SessionID   string
	Outcome     string
	Artifacts   []Artifact
}

// Complete delivers the run's outcome (§12.3).
func (c *Client) Complete(ctx context.Context, jobID int64, leaseID string, comp Completion) (engine.Answer, error) {
	req := completeRequest{
		jobLeaseRequest: jobLeaseRequest{JobID: jobID, LeaseID: leaseID},
		Result:          comp.Result,
		Error:           comp.Error,
		CheckResult:     comp.CheckResult,
		SessionID:       comp.SessionID,
		Outcome:         comp.Outcome,
	}
	for _, a := range comp.Artifacts {
		req.Artifacts = append(req.Artifacts, artifactRequest{
			Kind: a.Kind, Path: a.Path, Size: a.Size, SHA256: a.SHA256,
		})
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

// Progress is what a heartbeat carries besides its hold (§5.6).
type Progress struct {
	LastProgressAt time.Time
	LastActivityAt time.Time
	Events         []string
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
			Status: resp.StatusCode, APICode: eb.Code, Message: eb.Error,
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
