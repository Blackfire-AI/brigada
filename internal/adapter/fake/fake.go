// Package fake is a scripted CLI adapter for tests (design §5.2): it
// drives a fake worker — a shell script each test writes — instead of a
// real agent CLI, so the runner's tests exercise the lease loop, the
// workspace, the result contract and the check without any vendor CLI.
//
// The contract with the script: it runs in the worktree with the
// composed prompt on stdin and BRIGADA_FAKE_RESUME holding the session
// id to resume (empty for a fresh run); it emits one JSON event per
// line on stdout and edits the worktree like a worker would.
package fake

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Adapter drives the scripted fake CLI at Script.
type Adapter struct {
	Script string
}

// Name implements adapter.Adapter: the fake adapter's identifier, the
// name a runner's advertisement pairs with it (§5.1).
func (a *Adapter) Name() string { return "fake" }

// Command implements adapter.Adapter: run the script in workdir with
// the prompt on stdin. The resume session id rides in the environment,
// never the prompt: it is the adapter's plumbing, not the worker's
// business.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	cmd := exec.Command(a.Script)
	cmd.Dir = workdir
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.Env = append(os.Environ(), "BRIGADA_FAKE_RESUME="+resumeSessionID)
	return cmd, nil
}

// eventJSON is one line of the fake CLI's stream. The shapes mirror
// what the real adapters find in their CLIs: a system line carrying the
// session id, assistant messages, tool calls, a final result message
// and an error line.
type eventJSON struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Name      string `json:"name"`
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
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
	case "result":
		return adapter.Event{Kind: adapter.EvMessage, Text: ev.Text}, true
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
		return adapter.Event{Kind: adapter.EvError, ErrorKind: kind, Text: text}, true
	default:
		return adapter.Event{}, false
	}
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
