package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// Health probes /v1/health (§12.1): the one unauthenticated endpoint,
// "is the engine up" and nothing more. nil means an engine answered.
func (c *Client) Health(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "/v1/health", nil, nil)
}

// Status is the engine's own state as the client reads it (§13).
type Status struct {
	Pid       int64          `json:"pid"`
	StartedAt string         `json:"started_at"`
	Jobs      map[string]int `json:"jobs"`
}

// Status reads the engine's own state (§13): which process it is, since
// when, and the jobs by status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	if err := c.call(ctx, http.MethodGet, "/v1/status", nil, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// TaskSubmit is one task submitted whole, as the debug path sends it
// (§12.1): the repo and base ref, the spec verbatim, the check, and the
// composed worker prompt of the job it queues.
type TaskSubmit struct {
	RepoURL  string
	BaseRef  string
	Title    string
	Spec     string
	CheckCmd string
	Class    string
	Prompt   string
	Harness  string
	Labels   []string
}

// TaskSubmitResult names what the submit created.
type TaskSubmitResult struct {
	Answer    engine.Answer
	ProjectID int64
	TaskID    int64
	JobID     int64
	Run       int
}

// SubmitTask creates one task with its first work job queued (§12.1, the
// debug path until the foreman's plan tools exist).
func (c *Client) SubmitTask(ctx context.Context, t TaskSubmit) (TaskSubmitResult, error) {
	body := taskSubmitRequest{
		RepoURL:  t.RepoURL,
		BaseRef:  t.BaseRef,
		Title:    t.Title,
		Spec:     t.Spec,
		CheckCmd: t.CheckCmd,
		Class:    t.Class,
		Prompt:   t.Prompt,
		Harness:  t.Harness,
		Labels:   t.Labels,
	}
	var out taskSubmitResponse
	if err := c.call(ctx, http.MethodPost, "/v1/tasks", body, &out); err != nil {
		return TaskSubmitResult{}, err
	}
	return TaskSubmitResult{
		Answer:    engine.Answer(out.Answer),
		ProjectID: out.ProjectID,
		TaskID:    out.TaskID,
		JobID:     out.JobID,
		Run:       out.Run,
	}, nil
}

// CancelJob stops a live job (§12.1). The engine's answers — ok, already,
// gone — come back as engine answers, not errors.
func (c *Client) CancelJob(ctx context.Context, jobID int64) (engine.Answer, error) {
	var out struct {
		Answer string `json:"answer"`
	}
	path := "/v1/jobs/" + strconv.FormatInt(jobID, 10) + "/cancel"
	if err := c.call(ctx, http.MethodPost, path, struct{}{}, &out); err != nil {
		return "", err
	}
	return engine.Answer(out.Answer), nil
}

// HealthTimeout bounds one Health probe when a caller polls for the
// engine to come up (§13): short, because a socket that is not there
// refuses at once and a live engine answers in milliseconds.
const HealthTimeout = 500 * time.Millisecond

// HealthProbe is one health probe on a short timeout, the start-on-
// demand loop's single step (§13).
func (c *Client) HealthProbe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, HealthTimeout)
	defer cancel()
	return c.Health(ctx)
}
