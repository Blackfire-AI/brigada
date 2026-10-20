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
	"regexp"
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
// permits edits and denies bash headless by default, matching worker policy,
// and workerPermissions denies paths outside the worktree the same way.
// Never pass --auto (or its permission-bypass aliases); permission rules
// ride the environment, not the CLI's project config.
//
// OpenCode picks its directory as options.root ?? process.env.PWD ??
// process.cwd() and chdirs there (packages/cli/src/run/run.ts:74,
// run/v1.ts:29, mini.ts localDirectory), so cmd.Dir alone is not enough:
// the runner sets PWD to the worktree on every command it starts.
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
	cmd.Env = workerPermissions(workerEnv(os.Environ()))
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

// workerPermissions sets OPENCODE_PERMISSION so the CLI denies, rather than
// asks about, tool calls that reach outside the worktree. How the run path
// treats a permission (opencode v1.19.0, github.com/sst/opencode):
//
//   - A permission nothing decides defaults to "ask". On a headless
//     `opencode run` the ask is printed on stderr — "! permission
//     requested: <permission> (<patterns>); auto-rejecting", the "!" and
//     message wrapped in TEXT_WARNING_BOLD and TEXT_NORMAL colours — and
//     replied "reject"; the run loop itself carries on to the session's
//     idle, and the rejected call fails back to the model as
//     PermissionRejectedError (packages/opencode/src/cli/cmd/run.ts, the
//     permission.asked branch; UI.println writes stderr,
//     packages/opencode/src/cli/ui.ts; the rejection fails the tool's
//     deferred, packages/opencode/src/permission/index.ts).
//   - A rule whose action is "deny" never asks: the call fails back to the
//     model at once with PermissionDeniedError and the session runs on
//     (packages/opencode/src/permission/index.ts, the ask() deny branch;
//     rules reach every tool as the agent's merged ruleset,
//     packages/opencode/src/agent/agent.ts fromConfig on the config's
//     permission key, packages/opencode/src/session/tools.ts).
//   - OPENCODE_PERMISSION carries a JSON object merged into that config
//     key after every config file, and an invalid value is skipped with a
//     warning (packages/opencode/src/config/config.ts,
//     Flag.OPENCODE_PERMISSION). external_directory is one of its known
//     keys (packages/core/src/v1/config/permission.ts); upstream defaults
//     it to "ask" (opencode.ai/docs/permissions).
//
// So one deny rule turns an outside-the-worktree call into a plain tool
// error the model can answer — no ask, no auto-reject notice, no run that
// ends wordless. An ambient value keeps its other rules; an invalid one —
// which the CLI would discard — is replaced, never merged.
func workerPermissions(env []string) []string {
	var rules map[string]any
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "OPENCODE_PERMISSION="); ok {
			_ = json.Unmarshal([]byte(v), &rules)
			break
		}
	}
	if rules == nil {
		rules = map[string]any{}
	}
	rules["external_directory"] = "deny"
	value, err := json.Marshal(rules)
	if err != nil {
		return env // unreachable: a map of JSON values marshals
	}
	for i, kv := range env {
		if strings.HasPrefix(kv, "OPENCODE_PERMISSION=") {
			env[i] = "OPENCODE_PERMISSION=" + string(value)
			return env
		}
	}
	return append(env, "OPENCODE_PERMISSION="+string(value))
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
	Tokens json.RawMessage `json:"tokens"`
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Tool   string          `json:"tool"`
	State  struct {
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
	case "step_finish":
		u := adapter.ParseUsage(lj.Part.Tokens, "input", "output", "", "reasoning", "")
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(lj.Part.Tokens, &fields)
		cache := adapter.ParseUsage(fields["cache"], "", "", "read", "", "")
		if u == nil {
			u = cache
		} else {
			u.Add(cache)
		}
		return adapter.Event{Kind: adapter.EvUsage, Usage: u}, u != nil
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

// ParseErrEvent recognizes the same JSON shapes on stderr, plus the one
// fixed-format line the headless run path prints when it auto-rejects a
// permission ask — never arbitrary prose containing words such as "quota"
// or "key".
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	if ev, ok := autoRejectedPermission(line); ok {
		return ev, true
	}
	return a.ParseEvent(line)
}

// ansiRe strips the CLI's SGR colour codes, which it writes to stderr
// whether or not the stream is a TTY.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// autoRejectedPermission recognizes the ask notice `opencode run` prints
// on stderr when nothing answered a permission, whatever --format says:
//
//	! permission requested: external_directory (/worktree-outside/*); auto-rejecting
//
// with the "!" in TEXT_WARNING_BOLD and the message reset to TEXT_NORMAL.
// The rejected call fails back to the model, so the run may still answer;
// the event is ErrOther — a plain failure, not a limit or an auth one —
// and carries the line's own text, so a run that then ends without a word
// says why instead of a bare no-result.
func autoRejectedPermission(line []byte) (adapter.Event, bool) {
	const prefix = "permission requested: "
	const suffix = "; auto-rejecting"
	text := strings.TrimSpace(ansiRe.ReplaceAllString(string(line), ""))
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "!"))
	msg, ok := strings.CutPrefix(text, prefix)
	if !ok || !strings.HasSuffix(msg, suffix) {
		return adapter.Event{}, false
	}
	msg = strings.TrimSuffix(msg, suffix)
	// run.ts always renders "<permission> (<patterns>)"; hold the line to
	// that shape rather than matching any prose with the two markers.
	if !strings.Contains(msg, " (") || !strings.HasSuffix(msg, ")") {
		return adapter.Event{}, false
	}
	return adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: prefix + msg + suffix}, true
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
