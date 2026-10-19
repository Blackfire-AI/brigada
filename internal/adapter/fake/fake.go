// Package fake is a scripted CLI adapter for tests (design §5.2): it
// drives a fake worker — a shell script each test writes — instead of a
// real agent CLI, so the runner's tests exercise the lease loop, the
// workspace, the result contract and the check without any vendor CLI.
//
// The contract with the script: it runs in the worktree with the
// composed prompt on stdin, BRIGADA_FAKE_RESUME holding the session
// id to resume (empty for a fresh run) and BRIGADA_FAKE_MCP_CONFIG
// holding the harness's MCP config path the job carried (empty when it
// carried none); it emits one JSON event per line on stdout and edits
// the worktree like a worker would. An attached session (§4.2 item 1)
// runs the script with the terminal's streams — so the prompt rides in
// BRIGADA_FAKE_PROMPT instead — and BRIGADA_FAKE_SESSION holding the
// new session id the client generated.
package fake

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Adapter drives the scripted fake CLI at Script.
type Adapter struct {
	Script string
}

var _ adapter.Adapter = (*Adapter)(nil)

// The fake CLI carries the attach too (§4.2 item 1): the same script,
// driven interactively.
var _ adapter.Interactive = (*Adapter)(nil)

// Name implements adapter.Adapter: the fake adapter's identifier, the
// name a runner's advertisement pairs with it (§5.1).
func (a *Adapter) Name() string { return "fake" }

// Command implements adapter.Adapter: run the script in workdir with
// the prompt on stdin. The resume session id and the MCP config path
// ride in the environment, never the prompt: they are the adapter's
// plumbing, not the worker's business.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	cmd := exec.Command(a.Script)
	cmd.Dir = workdir
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.Env = append(os.Environ(),
		"BRIGADA_FAKE_RESUME="+resumeSessionID,
		"BRIGADA_FAKE_MCP_CONFIG="+job.MCPConfig,
	)
	return cmd, nil
}

// AttachCommand implements adapter.Interactive for the tests' attached
// sessions (§4.2 item 1): the script runs in the working directory with
// the same plumbing the headless contract carries — the resume id, the
// MCP config path — plus the attach's own: the new session id, the
// status bar's command and the opening prompt, which the terminal's stdin cannot bring a script the
// way the harness reads its initial prompt. The streams stay unwired:
// the caller owns them.
func (a *Adapter) AttachCommand(job adapter.Job, workdir, resumeSessionID, newSessionID string) (*exec.Cmd, error) {
	cmd := exec.Command(a.Script)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(),
		"BRIGADA_FAKE_RESUME="+resumeSessionID,
		"BRIGADA_FAKE_SESSION="+newSessionID,
		"BRIGADA_FAKE_MCP_CONFIG="+job.MCPConfig,
		"BRIGADA_FAKE_PROMPT="+job.Prompt,
		"BRIGADA_FAKE_STATUS_LINE="+job.StatusLine,
	)
	return cmd, nil
}

// eventJSON is one line of the fake CLI's stream. The shapes mirror
// what the real adapters find in their CLIs: a system line carrying the
// session id, assistant messages, tool calls, a final result message
// and an error line.
type eventJSON struct {
	Usage     json.RawMessage `json:"usage"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	SessionID string          `json:"session_id"`
	Kind      string          `json:"kind"`
	Message   string          `json:"message"`
	Until     string          `json:"until"` // a limit error's reset time, RFC3339 (§4.4)
}

// ParseEvent implements adapter.Adapter. A line that is not one of the
// fake CLI's events — a banner, a blank line, anything unparseable —
// is not an event at all: only the CLI's own error line is an error,
// so a stray line on its stdout cannot fail an otherwise good run.
func (a *Adapter) ParseEvent(line []byte) (adapter.Event, bool) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return adapter.Event{}, false
	}
	var ev eventJSON
	if err := json.Unmarshal(line, &ev); err != nil {
		return adapter.Event{}, false
	}
	switch ev.Type {
	case "system_init":
		if ev.SessionID == "" {
			return adapter.Event{}, false
		}
		return adapter.Event{Kind: adapter.EvSession, SessionID: ev.SessionID}, true
	case "assistant", "message":
		return adapter.Event{Kind: adapter.EvMessage, Text: ev.Text}, true
	case "tool_start", "tool_end":
		name := ev.Name
		if name == "" {
			name = ev.Text
		}
		return adapter.Event{Kind: adapter.EventKind(ev.Type), Text: name}, true
	case "result", "usage":
		kind := adapter.EvMessage
		if ev.Type == "usage" {
			kind = adapter.EvUsage
		}
		u := adapter.ParseUsage(ev.Usage, "input_tokens", "output_tokens", "cached_input_tokens", "reasoning_tokens", "cost_usd")
		return adapter.Event{Kind: kind, Text: ev.Text, Usage: u}, true
	case "error":
		kind := adapter.ErrorKind(ev.Kind)
		switch kind {
		case adapter.ErrLimit, adapter.ErrAuth:
		default:
			kind = adapter.ErrOther
		}
		text := ev.Message
		if text == "" {
			text = ev.Text
		}
		limitUntil := ""
		if kind == adapter.ErrLimit {
			limitUntil = ev.Until
		}
		return adapter.Event{Kind: adapter.EvError, ErrorKind: kind, Text: text, LimitUntil: limitUntil}, true
	default:
		return adapter.Event{}, false
	}
}

// resumeIDGap is what may sit between the session noun and its absence
// in a resume-failure line: at most the words "id" and the id itself.
// An id is a machine token — it carries a digit or a dash — which is
// what keeps a neighboring plain word ("option", "config") from
// slipping into the gap and masquerading as the missing thing.
const resumeIDGap = `(?: (?:with )?(?:an? )?id)?[ :]*(?:["'` + "`" + `]?[0-9a-z-]*[0-9-][0-9a-z-]*["'` + "`" + `]?)?`

// resumeGone is the shape of a resume that cannot happen (§5.6) as the
// CLIs print it to stderr, plain text before any JSON: the session or
// conversation is the thing missing — no such, not found, gone — or the
// resume itself failed. The noun and its absence sit next to each
// other, with at most an id between (resumeIDGap), so a line that
// merely mentions a session while missing something else — an unknown
// session option, a config file not found — is not read as a lost
// session.
var resumeGone = regexp.MustCompile(`(?:` +
	`no such (?:session|conversation)` +
	`|no (?:session|conversation) (?:was )?found` +
	`|(?:session|conversation)` + resumeIDGap + `(?: was)? not found` +
	`|(?:session|conversation)` + resumeIDGap + ` (?:does not|doesn't) exist` +
	`|(?:failed to|unable to|cannot|can't|could not|couldn't) resume` +
	`|resume failed` +
	`)`)

// ParseErrEvent implements adapter.Adapter: stderr rides the same
// pipeline as stdout. A JSON error line parses as it would on stdout;
// a resume that cannot be found usually prints only plain text there,
// before any JSON, and is recognized by its shape (resumeGone);
// anything else — a warning, a progress note — carries nothing to
// act on.
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return adapter.Event{}, false
	}
	if ev, ok := a.ParseEvent(line); ok && ev.Kind == adapter.EvError {
		return ev, true
	}
	if resumeGone.MatchString(strings.ToLower(string(line))) {
		return adapter.Event{
			Kind:      adapter.EvError,
			ErrorKind: adapter.ErrResumeFailed,
			Text:      strings.TrimSpace(string(line)),
		}, true
	}
	return adapter.Event{}, false
}

// ResultText implements adapter.Adapter: the final message is the last
// result event; a stream that never sent one falls back to the last
// assistant message, and a stream with neither said nothing.
func (a *Adapter) ResultText(events []adapter.Event) string {
	lastAssistant := ""
	for _, e := range events {
		if e.Kind == adapter.EvMessage {
			lastAssistant = e.Text
		}
	}
	return lastAssistant
}
