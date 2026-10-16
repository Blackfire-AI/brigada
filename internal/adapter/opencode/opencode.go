// Package opencode drives OpenCode 1.19.0 headless: one `opencode run`
// per worker job, resumed by session id. Credentials remain in the CLI's
// own store (`opencode auth login`); the adapter supplies none.
package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Adapter drives the opencode CLI. Its zero value resolves opencode on PATH.
type Adapter struct {
	Binary string // empty means "opencode"
	Agent  string // optional --agent preset
}

var _ adapter.Adapter = (*Adapter)(nil)

// DefaultModel names the CLI's own default, without passing a model flag.
const DefaultModel = "default"

func (a *Adapter) Name() string { return "opencode" }

// Command passes the prompt as one positional argument after --. OpenCode
// permits edits and denies bash headless by default, matching worker policy.
// Never pass --auto (or its permission-bypass aliases). A later task can
// scope bash through the CLI's project config; this adapter does not write it.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	if job.MCPConfig != "" {
		return nil, errors.New("opencode: the CLI run path has no MCP-config flag; a job carrying an MCP config cannot run on it")
	}
	if err := adapter.ValidateEffort("opencode", job.Effort); err != nil {
		return nil, fmt.Errorf("opencode: %w", err)
	}
	binary := a.Binary
	if binary == "" {
		binary = "opencode"
	}
	args := []string{"run", "--format", "json"}
	if job.Model != "" && job.Model != DefaultModel {
		args = append(args, "-m", job.Model)
	}
	if job.Effort != "" {
		// --variant is the CLI's provider-specific reasoning effort.
		args = append(args, "--variant", job.Effort)
	}
	if resumeSessionID != "" {
		args = append(args, "-s", resumeSessionID)
	}
	if a.Agent != "" {
		args = append(args, "--agent", a.Agent)
	}
	args = append(args, "--", job.Prompt)
	cmd := exec.Command(binary, args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	cmd.Dir = workdir
	cmd.Env = workerEnv(os.Environ())
	return cmd, nil
}

// Keep the CLI's ambient login state, but never the runner's secrets (§5.1).
func workerEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "BRIGADA_") {
			out = append(out, kv)
		}
	}
	return out
}

// The JSONL envelope is defined by the v1.19.0 CLI's cli/cmd/run.ts;
// nested parts and errors by sdk/js/src/v2/gen/types.gen.ts. This is
// deliberately not the server's message.part.updated/session.error stream.
// Declare only fields we read; unknown shapes are ignored, not guessed.
type lineJSON struct {
	Type      string     `json:"type"`
	SessionID string     `json:"sessionID"`
	Part      partJSON   `json:"part"`
	Error     *errorJSON `json:"error"`
}

type partJSON struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State struct {
		Status string `json:"status"`
	} `json:"state"`
}

type errorJSON struct {
	Name string `json:"name"`
	Data struct {
		Message    string `json:"message"`
		StatusCode int    `json:"statusCode"`
	} `json:"data"`
}

// ParseEvent maps completed assistant text, tool states and provider errors.
// step_start supplies the session id without stealing a message/tool event.
// The CLI currently emits tool_use only for completed/error states, so real
// runs have tool ends without starts; do not invent starts from step_start
// (a model step is not a tool). If a running ToolPart is emitted, its schema
// maps directly to a start. Unknown tool states and event types stay ignored.
func (a *Adapter) ParseEvent(line []byte) (adapter.Event, bool) {
	var lj lineJSON
	if err := json.Unmarshal(line, &lj); err != nil {
		return adapter.Event{}, false
	}
	switch lj.Type {
	case "step_start":
		if lj.Part.Type == "step-start" && lj.SessionID != "" {
			return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID}, true
		}
	case "text":
		if lj.Part.Type == "text" && lj.Part.Text != "" {
			return adapter.Event{Kind: adapter.EvMessage, Text: lj.Part.Text}, true
		}
	case "tool_use":
		if lj.Part.Type != "tool" || lj.Part.Tool == "" {
			break
		}
		switch lj.Part.State.Status {
		case "running":
			return adapter.Event{Kind: adapter.EvToolStart, Text: lj.Part.Tool}, true
		case "completed", "error":
			// A tool failure is a finished call, not a provider failure.
			return adapter.Event{Kind: adapter.EvToolEnd, Text: lj.Part.Tool}, true
		}
	case "error":
		if lj.Error != nil && lj.Error.Name != "" {
			text := lj.Error.Data.Message
			if text == "" {
				text = lj.Error.Name
			}
			// Errors can precede step_start, so keep their envelope's id
			// as well as the failure: the runner must not lose either.
			return adapter.Event{Kind: adapter.EvError, Text: text, SessionID: lj.SessionID, ErrorKind: errorKind(*lj.Error)}, true
		}
	}
	return adapter.Event{}, false
}

// ParseErrEvent recognizes the same JSON shapes on stderr, never arbitrary
// prose containing words such as "quota" or "key".
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	return a.ParseEvent(line)
}

type errorPhrase struct {
	kind   adapter.ErrorKind
	phrase string
}

var errorPhrases = []errorPhrase{
	{adapter.ErrLimit, "429"},
	{adapter.ErrLimit, "rate limit"},
	{adapter.ErrLimit, "rate-limit"},
	{adapter.ErrLimit, "usage limit"},
	{adapter.ErrLimit, "usage-limit"},
	{adapter.ErrLimit, "quota"},
	{adapter.ErrLimit, "too many requests"},
	{adapter.ErrAuth, "401"},
	{adapter.ErrAuth, "403"},
	{adapter.ErrAuth, "unauthorized"},
	{adapter.ErrAuth, "forbidden"},
	{adapter.ErrAuth, "invalid api key"},
	{adapter.ErrAuth, "invalid_api_key"},
	{adapter.ErrAuth, "invalid key"},
	{adapter.ErrAuth, "invalid-key"},
	{adapter.ErrAuth, "authentication"},
}

func errorKind(e errorJSON) adapter.ErrorKind {
	switch e.Data.StatusCode {
	case 429:
		return adapter.ErrLimit
	case 401, 403:
		return adapter.ErrAuth
	}
	if e.Name == "ProviderAuthError" {
		return adapter.ErrAuth
	}
	lower := strings.ToLower(e.Data.Message)
	for _, p := range errorPhrases {
		if strings.Contains(lower, p.phrase) {
			return p.kind
		}
	}
	return adapter.ErrOther
}

// ResultText is the run's last assistant message, which the worker/review
// contract reads (§5.4). Tool, session, reasoning and usage events cannot
// replace it, and earlier assistant messages are never concatenated into it.
func (a *Adapter) ResultText(events []adapter.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == adapter.EvMessage {
			return events[i].Text
		}
	}
	return ""
}
