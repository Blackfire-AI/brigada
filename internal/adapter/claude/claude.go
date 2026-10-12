// Package claude drives the Claude Code CLI headless (design §5.2): one
// run per job, `claude -p` with stream-json output, resumed by session
// id when the job carries one. The CLI's own login state — its tokens,
// its configuration — is the ambient environment's business; brigada
// adds nothing of its own to it and takes the runner's share back out.
package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Adapter drives the official claude CLI (2.x) in its headless print
// mode. The zero value runs `claude` from PATH with the worker's
// default permission mode; the fields carry what the agent's
// configuration passes through, never a hard-coded tool scope.
type Adapter struct {
	// Binary is the CLI to run; "claude" when empty.
	Binary string

	// PermissionMode is --permission-mode for every run. Empty means the
	// worker's default, acceptEdits: file edits inside the worktree the
	// command runs in are auto-accepted, and everything else still asks —
	// and asking, headless, is refusing.
	PermissionMode string

	// AllowedTools and DisallowedTools are the agent config's tool scope
	// (§5.2), passed through as the CLI's own flags. Empty adds nothing.
	AllowedTools    []string
	DisallowedTools []string
}

// workerPermissionMode is the permission mode a worker runs under when
// the agent config does not name one: edits inside the working tree are
// accepted without a prompt, which headless runs cannot answer anyway.
const workerPermissionMode = "acceptEdits"

var _ adapter.Adapter = (*Adapter)(nil)

// Name implements adapter.Adapter: the harness this adapter drives,
// which the runner's advertisement pairs it with (§5.1).
func (a *Adapter) Name() string { return "claude" }

// Command implements adapter.Adapter: the fresh or resume invocation of
// the CLI, running in workdir. The prompt travels on stdin — argv is
// public to anyone listing processes, and the prompt is the engine's
// product (§1.4), not the worker's neighbors' business. The environment
// is the runner's ambient set minus brigada's own share of it (§5.1):
// every BRIGADA_-prefixed variable is dropped, so no runner token or
// engine secret rides along, while the CLI's login state — untouched —
// is exactly what it needs to run.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	binary := a.Binary
	if binary == "" {
		binary = "claude"
	}
	args := []string{
		"-p", // headless print mode; the prompt itself arrives on stdin
		"--output-format", "stream-json",
		"--verbose", // print mode only streams its JSON with it
	}
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID)
	}
	mode := a.PermissionMode
	if mode == "" {
		mode = workerPermissionMode
	}
	args = append(args, "--permission-mode", mode)
	if len(a.AllowedTools) > 0 {
		args = append(args, append([]string{"--allowedTools"}, a.AllowedTools...)...)
	}
	if len(a.DisallowedTools) > 0 {
		args = append(args, append([]string{"--disallowedTools"}, a.DisallowedTools...)...)
	}
	cmd := exec.Command(binary, args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	cmd.Dir = workdir
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.Env = workerEnv(os.Environ())
	return cmd, nil
}

// envPrefix marks brigada's own variables: the runner's token, the
// engine's socket and anything else the orchestrator rides in its
// environment. None of it is the worker's to see.
const envPrefix = "BRIGADA_"

// workerEnv is the ambient environment minus everything brigada-prefixed.
// What is left is the machine's and the CLI's own — PATH, HOME, the
// vendor's tokens — which the worker needs to run and to push (§5.3).
func workerEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, envPrefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// lineJSON is one line of the CLI's stream-json output: one JSON object
// per line, whose type says what it carries. Only the fields the
// adapter reads are declared; the rest of each line is ignored where it
// stands.
type lineJSON struct {
	Type      string      `json:"type"`
	Subtype   string      `json:"subtype"`
	IsError   bool        `json:"is_error"`
	Result    string      `json:"result"`
	SessionID string      `json:"session_id"`
	Message   messageJSON `json:"message"`
}

// messageJSON is the anthropic-shaped message both assistant and user
// lines carry: a list of content blocks.
type messageJSON struct {
	Content []blockJSON `json:"content"`
}

// blockJSON is one content block. The types this adapter reads: "text"
// (assistant prose), "tool_use" (a call starting, named, with the id the
// matching tool_result ends) and "tool_result".
type blockJSON struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
}

// ParseEvent implements adapter.Adapter: one line of the CLI's
// stream-json turned into one progress event.
//
//	tool_use    → tool-call start (the name; its id pairs it with the end)
//	tool_result → tool-call end (the id of the call it ends)
//	text block  → assistant message
//	init, result session_id → session id, of which the last one wins —
//	             a resume forks the id, and the write-back (§4.2) must
//	             carry the forked one
//	error result, or a line naming a known failure → an error event
//
// The final result line is the session event: it carries the session id
// the run leaves behind with the run's final text in tow, where
// ResultText finds it. Unknown and malformed lines are ignored, never
// fatal — only the CLI's own error result, or a line that names one of
// the known failures, is an error, so a stray line on the stream cannot
// fail an otherwise good run.
func (a *Adapter) ParseEvent(line []byte) (adapter.Event, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return adapter.Event{}, false
	}
	var lj lineJSON
	if err := json.Unmarshal(trimmed, &lj); err != nil {
		// Not the CLI's JSON: a banner, prose, or the failure text the
		// CLI puts on stderr instead of an error result. The phrase
		// table decides — nothing else in prose is an event.
		return errorFromText(string(trimmed))
	}
	switch lj.Type {
	case "system":
		if lj.Subtype != "init" || lj.SessionID == "" {
			return adapter.Event{}, false
		}
		return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID}, true
	case "assistant":
		return assistantEvent(lj.Message.Content)
	case "user":
		return toolEnd(lj.Message.Content)
	case "result":
		return resultEvent(lj)
	default:
		return adapter.Event{}, false
	}
}

// assistantEvent reads one assistant message. A message that calls a
// tool is that tool call — the call is the progress it makes, and the
// prose riding beside it ("Let me look at the file") is not — and a
// message that only speaks is the message.
func assistantEvent(blocks []blockJSON) (adapter.Event, bool) {
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return adapter.Event{Kind: adapter.EvToolStart, Text: b.Name}, true
		}
	}
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return adapter.Event{Kind: adapter.EvMessage, Text: b.Text}, true
		}
	}
	return adapter.Event{}, false
}

// toolEnd reads one user line — the CLI feeding tool results back — and
// ends the tool call the result answers, by the id it names.
func toolEnd(blocks []blockJSON) (adapter.Event, bool) {
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return adapter.Event{Kind: adapter.EvToolEnd, Text: b.ToolUseID}, true
		}
	}
	return adapter.Event{}, false
}

// resultEvent reads the final line of the stream. An error result is an
// error event sorted by what its text names; a success result is the
// session event, the forked id with the final text in tow.
func resultEvent(lj lineJSON) (adapter.Event, bool) {
	if lj.IsError || strings.HasPrefix(lj.Subtype, "error") {
		text := lj.Result
		if text == "" {
			text = lj.Subtype
		}
		if ev, ok := errorFromText(text); ok {
			return ev, true
		}
		return adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: text}, true
	}
	if lj.SessionID != "" {
		return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID, Text: lj.Result}, true
	}
	return adapter.Event{Kind: adapter.EvMessage, Text: lj.Result}, true
}

// errorPhrases names, in one table, the failures the adapter sorts by:
// what their text looks like, case-insensitively, and the kind each
// sorts into. First match wins; extend the table, not the code.
var errorPhrases = []struct {
	kind   adapter.ErrorKind
	phrase string
}{
	// A resume the CLI cannot do: the session is gone (§5.6, §14), and
	// the answer is a fresh run in place, not another resume.
	{adapter.ErrResumeFailed, "no conversation found"},
	{adapter.ErrResumeFailed, "conversation not found"},
	{adapter.ErrResumeFailed, "session not found"},
	{adapter.ErrResumeFailed, "no session found"},
	// The account's quota window (§8): the account cools, the job moves.
	{adapter.ErrLimit, "usage limit"},
	{adapter.ErrLimit, "rate limit"},
	{adapter.ErrLimit, "limit reached"},
	{adapter.ErrLimit, "try again at"},
	// The CLI's login (§8): the agent is broken until a probe passes.
	{adapter.ErrAuth, "invalid api key"},
	{adapter.ErrAuth, "please run /login"},
	{adapter.ErrAuth, "not logged in"},
	{adapter.ErrAuth, "authentication"},
	{adapter.ErrAuth, "unauthorized"},
}

// errorFromText sorts one piece of failure text — an error result's, or
// a line of the CLI's stderr — by the phrase table. Text that names no
// known failure is not an event.
func errorFromText(text string) (adapter.Event, bool) {
	lower := strings.ToLower(text)
	for _, m := range errorPhrases {
		if strings.Contains(lower, m.phrase) {
			return adapter.Event{Kind: adapter.EvError, ErrorKind: m.kind, Text: text}, true
		}
	}
	return adapter.Event{}, false
}

// ResultText implements adapter.Adapter: the run's final text is the
// result line's, which its session event carries. A stream that never
// reached a result — a killed or crashed CLI — falls back to the last
// assistant message, so the record still names what the worker last
// said.
func (a *Adapter) ResultText(events []adapter.Event) string {
	result := ""
	lastMessage := ""
	for _, e := range events {
		if e.Kind == adapter.EvSession && e.Text != "" {
			result = e.Text
		}
		if e.Kind == adapter.EvMessage {
			lastMessage = e.Text
		}
	}
	if result != "" {
		return result
	}
	return lastMessage
}
