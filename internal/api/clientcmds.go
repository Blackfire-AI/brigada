package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
)

// Health probes /v1/health (§12.1): the one unauthenticated endpoint,
// "is the engine up" and nothing more. nil means an engine answered.
func (c *Client) Health(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "/v1/health", nil, nil)
}

// Task reads one task (§12.1): its repo, base ref, result branch and
// latest job. A task that is not there is a *StatusError with 404.
func (c *Client) Task(ctx context.Context, id int64) (engine.TaskInfo, error) {
	var t engine.TaskInfo
	if err := c.call(ctx, http.MethodGet, "/v1/tasks/"+strconv.FormatInt(id, 10), nil, &t); err != nil {
		return engine.TaskInfo{}, err
	}
	return t, nil
}

// TaskLand reads what `brigada land` needs of a task (§7.1). A task
// that is not there is a *StatusError with 404.
func (c *Client) TaskLand(ctx context.Context, id int64) (engine.TaskLandInfo, error) {
	var t engine.TaskLandInfo
	if err := c.call(ctx, http.MethodGet, "/v1/tasks/"+strconv.FormatInt(id, 10)+"/land", nil, &t); err != nil {
		return engine.TaskLandInfo{}, err
	}
	return t, nil
}

// Job reads one job (§12.1): its status, and the ids of its transcript
// and check log artifacts. A job that is not there is a *StatusError
// with 404.
func (c *Client) Job(ctx context.Context, id int64) (engine.JobInfo, error) {
	var j engine.JobInfo
	if err := c.call(ctx, http.MethodGet, "/v1/jobs/"+strconv.FormatInt(id, 10), nil, &j); err != nil {
		return engine.JobInfo{}, err
	}
	return j, nil
}

// Status is the engine's own state as the client reads it (§13): the
// pid, the start, the jobs by status — and the plans with their tasks,
// the latest foreman report (§3, §10) and the "needs you" queue (§10),
// scoped by the token: a foreman token reads its own project, a client
// or admin every project.
type Status struct {
	Pid       int64              `json:"pid"`
	StartedAt string             `json:"started_at"`
	Jobs      map[string]int     `json:"jobs"`
	Running   []engine.StatusJob `json:"running"`
	// ForemanMessages counts the user's messages waiting for the
	// foreman (§4.2 item 3).
	ForemanMessages int `json:"foreman_messages"`
	// Slots is who holds the foreman's slot per project (§4.6).
	Slots    []engine.Slot         `json:"slots"`
	Plans    []engine.StatusPlan   `json:"plans"`
	Report   *engine.StatusReport  `json:"report,omitempty"`
	NeedsYou []engine.NeedsYouItem `json:"needs_you"`
	// Cooling lists the agents held back for quota (§8, §10).
	Cooling []engine.CoolingAgent `json:"cooling,omitempty"`
	// Held lists the queued jobs a full cap layer holds (§8.1).
	Held []engine.HeldJob `json:"held,omitempty"`
	// Instructions names the projects with project instructions (§5.9):
	// source, version, size and digest; the text only when asked for.
	Instructions []engine.StatusInstructions `json:"instructions,omitempty"`
}

// Status reads the engine's own state (§13): which process it is, since
// when, and the jobs by status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	return c.status(ctx, false)
}

// StatusWithInstructions is Status with the full project instruction
// text (§5.9), for the foreman's status tool.
func (c *Client) StatusWithInstructions(ctx context.Context) (Status, error) {
	return c.status(ctx, true)
}

func (c *Client) status(ctx context.Context, withText bool) (Status, error) {
	var st Status
	path := "/v1/status"
	sep := "?"
	if c.readProject != 0 {
		path += sep + "project=" + strconv.FormatInt(c.readProject, 10)
		sep = "&"
	}
	if withText {
		path += sep + "instructions=1"
	}
	if err := c.call(ctx, http.MethodGet, path, nil, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// Stats reads the outcome record per project (§8) through /v1/stats:
// each agent's attempts, successes and score per class, the
// registered-but-unrecorded ones at attempts 0. projectID names one
// project; 0 names none, which for a foreman token means its own
// project (§12.1) and for a client or admin every project.
func (c *Client) Stats(ctx context.Context, projectID int64) ([]engine.ProjectStats, error) {
	if projectID == 0 {
		projectID = c.readProject
	}
	path := "/v1/stats"
	if projectID != 0 {
		path += "?project=" + strconv.FormatInt(projectID, 10)
	}
	var out []engine.ProjectStats
	if err := c.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Capacity reads the capacity snapshot (§12.2) through /v1/capacity:
// per agent the effective cap, the layer that sets it, the running
// jobs, the quota state and the cost weight; per layer — each runner,
// each account, each capped model, each active plan of the project and
// global — the cap, the use and the queue depth. projectID limits the
// plan layer to one project's active plans; 0 names none, which for a
// foreman token means its own project (§12.1) and for a client or
// admin means every project's.
func (c *Client) Capacity(ctx context.Context, projectID int64) (engine.CapacitySnapshot, error) {
	if projectID == 0 {
		projectID = c.readProject
	}
	path := "/v1/capacity"
	if projectID != 0 {
		path += "?project=" + strconv.FormatInt(projectID, 10)
	}
	var snap engine.CapacitySnapshot
	if err := c.call(ctx, http.MethodGet, path, nil, &snap); err != nil {
		return engine.CapacitySnapshot{}, err
	}
	return snap, nil
}

// TaskSubmit is one task submitted whole, as the debug path sends it
// (§12.1): the repo and base ref, the spec verbatim, and the check. The
// worker prompt is the engine's to compose (§1.4), never the caller's.
type TaskSubmit struct {
	RepoURL  string
	BaseRef  string
	Title    string
	Spec     string
	CheckCmd string
	Class    string
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

// PlanCreate is a plan_create call as the bridge sends it (§6.2, §12.1,
// §12.2): the plan's title, its tasks — each may depend on
// others of the plan by name — and, for a caller that is not the project's foreman,
// the repo URL naming the project. A foreman token's own project is the
// only one it may plan in, so it leaves RepoURL empty.
type PlanCreate struct {
	RepoURL string
	Title   string
	Tasks   []PlanTask
}

// PlanTask is one task of the plan (§6.1): title, name, spec,
// check, class and dependencies; an empty class means implement.
type PlanTask struct {
	Tier   string `json:"tier,omitempty"`
	Effort string `json:"effort,omitempty"`
	Title  string
	Name   string   // the handle other tasks name in Deps; '' = unnamed
	Deps   []string // names of tasks of the same plan this one waits for
	Base   string   // the one of Deps whose branch the work starts from; '' = the default rule
	Spec   string
	Check  string
	Class  string
	// ExtraRepos are read-only repositories for the task (§5.8): nil
	// inherits the project default, a non-nil list (even empty) replaces
	// it.
	ExtraRepos []engine.ExtraRepo
}

// PlanCreateResult names what the call created: the plan, the first
// ready task with its queued work job, and every task.
type PlanCreateResult struct {
	Answer engine.Answer
	PlanID int64
	TaskID int64
	JobID  int64
	Tasks  []PlanCreatedTask
	// State and Reasons: the plan's state, and why it waits when the
	// approval limit holds it (§6.3).
	State   string
	Reasons []string
}

// PlanCreatedTask is one task the plan made; JobID is 0 while the task
// waits on its dependencies.
type PlanCreatedTask struct {
	TaskID int64
	Name   string
	JobID  int64
}

// CreatePlan creates a plan with its tasks and the work job of each
// task without dependencies queued
// (§6.2, §12.1). idemKey is the call's idempotency key, minted per
// logical call by the caller that knows what makes one — the bridge
// hashes the tool call's arguments — so a repeated call replays the
// first answer instead of planning twice.
func (c *Client) CreatePlan(ctx context.Context, p PlanCreate, idemKey string) (PlanCreateResult, error) {
	body := planCreateRequest{RepoURL: p.RepoURL, Title: p.Title, Tasks: make([]planTaskBody, len(p.Tasks))}
	for i, t := range p.Tasks {
		body.Tasks[i] = planTaskBody{Title: t.Title, Name: t.Name, Deps: t.Deps, Base: t.Base, Spec: t.Spec, Check: t.Check, Class: t.Class, Tier: t.Tier, Effort: t.Effort, ExtraRepos: t.ExtraRepos}
	}
	var out planCreateResponse
	if err := c.callKeyed(ctx, http.MethodPost, "/v1/plans", idemKey, body, &out); err != nil {
		return PlanCreateResult{}, err
	}
	res := PlanCreateResult{
		Answer:  engine.Answer(out.Answer),
		PlanID:  out.PlanID,
		TaskID:  out.TaskID,
		JobID:   out.JobID,
		State:   out.State,
		Reasons: out.Reasons,
	}
	for _, t := range out.Tasks {
		res.Tasks = append(res.Tasks, PlanCreatedTask{TaskID: t.TaskID, Name: t.Name, JobID: t.JobID})
	}
	return res, nil
}

// PlanAmend is a plan_amend call as the bridge sends it (§12.2): tasks to
// add, tasks to cancel, and waiting tasks whose dependencies are
// re-pointed, plus tier or effort changes for waiting or parked tasks.
// Tasks and dependencies name tasks of the plan by name, or
// by id as a decimal.
type PlanAmend struct {
	Add      []PlanTask
	Cancel   []string
	Retarget []PlanRetarget
	Retier   []PlanRetier
}

// PlanRetier changes a waiting or parked task's tier or effort; empty keeps it.
type PlanRetier struct {
	Task   string
	Tier   string
	Effort string
}

// PlanRetarget replaces one waiting task's whole dependency set.
type PlanRetarget struct {
	Task string
	Deps []string
	Base string
}

// PlanAmendResult names what the amendment did: the plan's state after it,
// the added tasks, the ids cancelled and re-pointed, the tasks it started
// (a work job queued), and why the plan waits for approval, if it does.
type PlanAmendResult struct {
	PlanID     int64
	State      string
	Added      []PlanCreatedTask
	Cancelled  []int64
	Retargeted []int64
	Retiered   []int64
	Started    []PlanCreatedTask
	Reasons    []string
}

// AmendPlan amends a live plan (§12.2). idemKey is the call's
// idempotency key, minted per logical call by the bridge.
func (c *Client) AmendPlan(ctx context.Context, plan int64, a PlanAmend, idemKey string) (PlanAmendResult, error) {
	body := planAmendRequest{Cancel: a.Cancel}
	for _, t := range a.Add {
		body.Add = append(body.Add, planTaskBody{Title: t.Title, Name: t.Name, Deps: t.Deps, Base: t.Base, Spec: t.Spec, Check: t.Check, Class: t.Class, Tier: t.Tier, Effort: t.Effort, ExtraRepos: t.ExtraRepos})
	}
	for _, rt := range a.Retarget {
		body.Retarget = append(body.Retarget, planRetargetBody{Task: rt.Task, Deps: rt.Deps, Base: rt.Base})
	}
	for _, rt := range a.Retier {
		body.Retier = append(body.Retier, planRetierBody{Task: rt.Task, Tier: rt.Tier, Effort: rt.Effort})
	}
	var out planAmendResponse
	path := "/v1/plans/" + strconv.FormatInt(plan, 10) + "/amend"
	if err := c.callKeyed(ctx, http.MethodPost, path, idemKey, body, &out); err != nil {
		return PlanAmendResult{}, err
	}
	res := PlanAmendResult{PlanID: out.PlanID, State: out.State, Cancelled: out.Cancelled, Retargeted: out.Retargeted, Retiered: out.Retiered, Reasons: out.Reasons}
	for _, t := range out.Added {
		res.Added = append(res.Added, PlanCreatedTask{TaskID: t.TaskID, Name: t.Name, JobID: t.JobID})
	}
	for _, t := range out.Started {
		res.Started = append(res.Started, PlanCreatedTask{TaskID: t.TaskID, Name: t.Name, JobID: t.JobID})
	}
	return res, nil
}

// PlanDecisionResult is what approving or rejecting a plan did (§6.3):
// the plan's new state and, for an approval, the first wave's tasks
// with their queued work jobs.
type PlanDecisionResult struct {
	PlanID int64
	State  string
	Tasks  []PlanCreatedTask
}

// ApprovePlan is the user's yes (§6.3): the plan's first wave queues.
// A plan that is not there is the API's not-found error, one not
// awaiting approval its conflict.
func (c *Client) ApprovePlan(ctx context.Context, plan int64) (PlanDecisionResult, error) {
	return c.decidePlan(ctx, plan, "approve")
}

// RejectPlan is the user's no (§6.3): the plan's tasks are cancelled.
func (c *Client) RejectPlan(ctx context.Context, plan int64) (PlanDecisionResult, error) {
	return c.decidePlan(ctx, plan, "reject")
}

// ApprovePlanVersion is ApprovePlan for the plan version the user was
// shown (§4.6 mode B): the plan must belong to repoURL's project and
// still be at version, else the API refuses with its stale_plan 409 and
// nothing is applied.
func (c *Client) ApprovePlanVersion(ctx context.Context, repoURL string, plan, version int64) (PlanDecisionResult, error) {
	return c.decidePlanBody(ctx, plan, "approve", planDecisionRequest{RepoURL: repoURL, Version: version})
}

// RejectPlanVersion is RejectPlan for the plan version the user was
// shown; see ApprovePlanVersion.
func (c *Client) RejectPlanVersion(ctx context.Context, repoURL string, plan, version int64) (PlanDecisionResult, error) {
	return c.decidePlanBody(ctx, plan, "reject", planDecisionRequest{RepoURL: repoURL, Version: version})
}

func (c *Client) decidePlan(ctx context.Context, plan int64, verb string) (PlanDecisionResult, error) {
	return c.decidePlanBody(ctx, plan, verb, planDecisionRequest{})
}

func (c *Client) decidePlanBody(ctx context.Context, plan int64, verb string, body planDecisionRequest) (PlanDecisionResult, error) {
	var out planDecisionResponse
	path := "/v1/plans/" + strconv.FormatInt(plan, 10) + "/" + verb
	if err := c.call(ctx, http.MethodPost, path, body, &out); err != nil {
		return PlanDecisionResult{}, err
	}
	res := PlanDecisionResult{PlanID: out.PlanID, State: out.State}
	for _, t := range out.Tasks {
		res.Tasks = append(res.Tasks, PlanCreatedTask{TaskID: t.TaskID, Name: t.Name, JobID: t.JobID})
	}
	return res, nil
}

// TaskAnswerResult names what an answer did: the questions row it
// filled and the job that carries the answer to its worker.
type TaskAnswerResult struct {
	Answer     engine.Answer
	QuestionID int64
	JobID      int64
	Run        int
}

// Answer answers a task worker's question (§3, §12.1, §12.2): the task
// must be parked at needs_foreman by its question; the answer resumes
// the author's session, verbatim. idemKey is the call's idempotency
// key, minted per logical call by the bridge (§12.1).
func (c *Client) Answer(ctx context.Context, task int64, text, idemKey string) (TaskAnswerResult, error) {
	var out taskAnswerResponse
	path := "/v1/tasks/" + strconv.FormatInt(task, 10) + "/answer"
	if err := c.callKeyed(ctx, http.MethodPost, path, idemKey, taskAnswerRequest{Text: text}, &out); err != nil {
		return TaskAnswerResult{}, err
	}
	return TaskAnswerResult{
		Answer:     engine.Answer(out.Answer),
		QuestionID: out.QuestionID,
		JobID:      out.JobID,
		Run:        out.Run,
	}, nil
}

// TaskRestartResult names what a task_restart did: the fresh work job.
type TaskRestartResult struct {
	Answer engine.Answer
	JobID  int64
	Run    int
}

// RestartTask restarts a parked task (§3, §5.6, §12.1, §12.2): a fresh
// work attempt from the task's base, the note handed to the worker
// verbatim when there is one. idemKey as in Answer.
func (c *Client) RestartTask(ctx context.Context, task int64, note, idemKey string) (TaskRestartResult, error) {
	var out taskRestartResponse
	path := "/v1/tasks/" + strconv.FormatInt(task, 10) + "/restart"
	if err := c.callKeyed(ctx, http.MethodPost, path, idemKey, taskRestartRequest{Note: note}, &out); err != nil {
		return TaskRestartResult{}, err
	}
	return TaskRestartResult{
		Answer: engine.Answer(out.Answer),
		JobID:  out.JobID,
		Run:    out.Run,
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

// ResetAgent clears broken and cooling (§8) on the agents of a harness
// and model through /v1/agents/reset; it returns how many agents
// matched. An unknown pair is the API's not-found error.
func (c *Client) ResetAgent(ctx context.Context, harness, model string) (int, error) {
	body := agentResetRequest{Harness: harness, Model: model}
	var out struct {
		Reset int `json:"reset"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/agents/reset", body, &out); err != nil {
		return 0, err
	}
	return out.Reset, nil
}

// ForemanEvents long-polls the token's project for pending judgment
// events (§4.2, §12.2): the events tool's call, and a headless turn's
// token may use it too. wait bounds the long-poll; the engine caps it.
// The events come back rendered exactly as the wake prompt renders them,
// consumed by the read — an empty answer says "no new events".
func (c *Client) ForemanEvents(ctx context.Context, wait time.Duration) (engine.ForemanInbox, error) {
	body := struct {
		WaitSeconds float64 `json:"wait_seconds"`
	}{wait.Seconds()}
	var out engine.ForemanInbox
	if err := c.call(ctx, http.MethodPost, "/v1/events", body, &out); err != nil {
		return engine.ForemanInbox{}, err
	}
	if out.UserMessages == nil {
		out.UserMessages = []foreman.UserMessage{}
	}
	return engine.ForemanInbox{Events: nonNil(out.Events), UserMessages: out.UserMessages}, nil
}

// AttachResult is the attach answer as the client reads it (§4.2 item
// 1): a created hold carries everything the attached session needs;
// the busy answer carries what made it busy — the live turn's job id,
// or the live hold's id — so the caller can wait one out and refuse
// the other without parsing words.
type AttachResult struct {
	Answer engine.Answer
	HoldID int64 // the created hold; a busy answer names the live hold
	JobID  int64 // a busy answer names the live foreman turn; 0 otherwise
	// InteractiveHoldID is the user's own session's hold (§4.6), when
	// that made the attach busy; 0 otherwise.
	InteractiveHoldID int64
	ForemanToken      string
	RepoURL           string
	DefaultBranch     string
	Harness           string
	Model             string
	Effort            string // the foreman's reasoning-effort level; '' = unset
	SessionID         string // the key's current harness session id; '' = none
	Prompt            string // the opening prompt
	ExpiresAt         time.Time
}

// Attach takes the foreman's key for an attached chat (§4.2 item 1),
// naming the project by its repo URL the way every caller does (§12.1).
// force cancels a live foreman turn rather than waiting it out. A busy
// answer — a live turn, or a second live attach — is not an error: it
// comes back with Answer busy and the id that made it busy, for the
// caller to branch on.
func (c *Client) Attach(ctx context.Context, repoURL string, force bool) (AttachResult, error) {
	var out attachResponse
	err := c.call(ctx, http.MethodPost, "/v1/foreman/attach", attachRequest{RepoURL: repoURL, Force: force}, &out)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Status == http.StatusConflict {
			var busy attachConflict
			if json.Unmarshal(se.Body, &busy) == nil {
				return AttachResult{Answer: engine.AnsBusy, JobID: busy.JobID, HoldID: busy.HoldID,
					InteractiveHoldID: busy.InteractiveHoldID}, nil
			}
		}
		return AttachResult{}, err
	}
	return AttachResult{
		Answer:        engine.Answer(out.Answer),
		HoldID:        out.HoldID,
		ForemanToken:  out.ForemanToken,
		RepoURL:       out.RepoURL,
		DefaultBranch: out.DefaultBranch,
		Harness:       out.Harness,
		Model:         out.Model,
		Effort:        out.Effort,
		SessionID:     out.SessionID,
		Prompt:        out.Prompt,
		ExpiresAt:     out.ExpiresAt,
	}, nil
}

// AttachHeartbeat extends a live attach hold (§4.2 item 1), the way the
// runner's heartbeat extends a lease. The engine's answers — ok, gone —
// ride in the return, not as errors.
func (c *Client) AttachHeartbeat(ctx context.Context, holdID int64) (engine.Answer, error) {
	var out struct {
		Answer string `json:"answer"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/foreman/heartbeat", holdRequest{HoldID: holdID}, &out); err != nil {
		return "", err
	}
	return engine.Answer(out.Answer), nil
}

// ReleaseAttach ends an attached chat (§4.2 item 1), carrying the
// harness session id the chat ended on: a resume forks session ids,
// and the key's row must carry the id the chat reached. The engine's
// answers — ok; expired for a late release whose hold lapsed but whose
// session id was still written; gone — ride in the return.
func (c *Client) ReleaseAttach(ctx context.Context, holdID int64, sessionID string) (engine.Answer, error) {
	return c.answer(ctx, "/v1/foreman/release", releaseAttachRequest{HoldID: holdID, SessionID: sessionID})
}

// ReleaseHold is the user's forced release of a project's stuck
// interactive hold (§4.6): client authority, no credential. The answer is
// ok, or gone when no interactive hold was live.
func (c *Client) ReleaseHold(ctx context.Context, repoURL string) (engine.Answer, error) {
	return c.answer(ctx, "/v1/foreman/hold/release", releaseHoldRequest{RepoURL: repoURL})
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

// AskForeman sends the user's message to a project's foreman (§4.2
// item 3) through /v1/foreman/messages and returns what it did: the
// project, the event that holds it, and the wake's outcome. An empty
// repoURL names the engine's only project; idemKey, when set, makes a
// retried call send one message (§12.1).
func (c *Client) AskForeman(ctx context.Context, repoURL, text, idemKey string) (engine.AskResult, error) {
	var res engine.AskResult
	if err := c.callKeyed(ctx, http.MethodPost, "/v1/foreman/messages", idemKey, askRequest{RepoURL: repoURL, Text: text}, &res); err != nil {
		return engine.AskResult{}, err
	}
	return res, nil
}
