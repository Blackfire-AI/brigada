// Package kimi drives the Kimi Code CLI headless (design §5.2): one run
// per job, `kimi -p` with stream-json output, resumed by session id
// when the job carries one. The CLI's own login state — its tokens, its
// configuration — is the ambient environment's business; brigada adds
// nothing of its own to it and takes the runner's share back out.
package kimi

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Adapter drives the kimi CLI (2.1.x) in its headless prompt mode: -p
// runs non-interactively without approval prompts, which is how a
// production fleet runs it. The zero value runs `kimi` from PATH; the
// fields carry what the agent's configuration passes through.
type Adapter struct {
	// Binary is the CLI to run; "kimi" when empty.
	Binary string

	// AgentFile is --agent-file for a fresh run: the agent definition
	// the session is created under. A resumed session keeps the agent it
	// was created with — the CLI refuses --agent-file beside --session —
	// so the field is read only when the run is fresh.
	AgentFile string

	// ExtraArgs are further flags the agent's configuration passes
	// through, appended verbatim after the adapter's own. Empty adds
	// nothing.
	ExtraArgs []string
}

var _ adapter.Adapter = (*Adapter)(nil)

// Name implements adapter.Adapter: the harness this adapter drives,
// which the runner's advertisement pairs it with (§5.1).
func (a *Adapter) Name() string { return "kimi" }

// Command implements adapter.Adapter: the fresh or resume invocation of
// the CLI, running in workdir. The prompt is -p's argument, verbatim —
// the engine's product (§1.4), not the adapter's to edit. The
// environment is the runner's ambient set minus brigada's own share of
// it (§5.1): every BRIGADA_-prefixed variable is dropped, so no runner
// token or engine secret rides along, while the CLI's login state —
// untouched — is exactly what it needs to run.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	// The CLI has no MCP-config flag, so a job that carries one — a
	// foreman turn with the `brigada mcp` bridge (§12.2) — cannot run
	// here: refusing beats silently dropping the bridge the turn needs.
	if job.MCPConfig != "" {
		return nil, errors.New("kimi: the CLI has no MCP-config flag; a job carrying an MCP config cannot run on it")
	}
	binary := a.Binary
	if binary == "" {
		binary = "kimi"
	}
	args := []string{
		"-p", job.Prompt,
		"--output-format", "stream-json",
	}
	if job.Model != "" {
		args = append(args, "-m", job.Model)
	}
	if resumeSessionID != "" {
		args = append(args, "--session", resumeSessionID)
	} else if a.AgentFile != "" {
		args = append(args, "--agent-file", a.AgentFile)
	}
	args = append(args, a.ExtraArgs...)
	cmd := exec.Command(binary, args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	cmd.Dir = workdir
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
// per line, whose role says what it carries. Only the fields the
// adapter reads are declared; the rest of each line is ignored where it
// stands.
type lineJSON struct {
	Role       string          `json:"role"`
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []toolCallJSON  `json:"tool_calls"`
}

// toolCallJSON is one call of an assistant line's tool_calls. The
// function's arguments are a JSON string the adapter never needs to
// open: the call's name is the progress it makes.
type toolCallJSON struct {
	Type     string       `json:"type"`
	ID       string       `json:"id"`
	Function functionJSON `json:"function"`
}

// functionJSON names the called function.
type functionJSON struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// partJSON is one part of a list-shaped content. The types this adapter
// reads: "text"; other part types carry nothing it reads.
type partJSON struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// contentText reads one line's content, which the CLI prints either as
// one string or as a list of {"type":"text","text":…} parts. Text parts
// join with newlines; a content that is neither shape reads as empty.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []partJSON
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// ParseEvent implements adapter.Adapter: one line of the CLI's
// stream-json turned into one progress event.
//
//	assistant tool_calls → tool-call start (the first call's function
//	                      name; the runner tracks progress, not the call
//	                      graph, so parallel calls collapse to their first)
//	role "tool"          → tool-call end (the call id it answers)
//	assistant text only  → assistant message
//	meta resume_hint     → session id, of which the last one wins
//	error: failed to run prompt: … → an error event, sorted by its reason
//
// An assistant line that both speaks and calls tools is its tool call:
// the prose riding beside a call ("Let me look at the tests") is
// mid-turn narration, never the final answer — the answer is a line
// that only speaks after the last tool call, which is where ResultText
// reads. Unknown and malformed lines are ignored, never fatal — the
// version banner, a blank line, a log hint — so a stray line cannot
// fail an otherwise good run.
func (a *Adapter) ParseEvent(line []byte) (adapter.Event, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return adapter.Event{}, false
	}
	var lj lineJSON
	if err := json.Unmarshal(trimmed, &lj); err != nil {
		// Not the CLI's JSON: a failure line, or noise. Only the
		// failure-line shape is an event; noise names nothing to act on.
		return errorFromText(string(trimmed))
	}
	switch {
	case lj.Role == "assistant":
		return assistantEvent(lj)
	case lj.Role == "tool":
		return adapter.Event{Kind: adapter.EvToolEnd, Text: lj.ToolCallID}, true
	case lj.Role == "meta" && lj.Type == "session.resume_hint":
		if lj.SessionID == "" {
			return adapter.Event{}, false
		}
		return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID}, true
	default:
		return adapter.Event{}, false
	}
}

// ParseErrEvent implements adapter.Adapter: one line of the CLI's
// stderr, parsed exactly as a stdout line. The CLI prints its failures
// to either stream in the same shapes — a plain `error: failed to run
// prompt: …` line, then a log hint — and its noise is the same on both:
// the `See log:` hint and a telemetry note carry nothing to act on
// wherever they appear.
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	return a.ParseEvent(line)
}

// assistantEvent reads one assistant line: its first tool call when it
// makes one, else its text. A line that only speaks and says nothing is
// not an event.
func assistantEvent(lj lineJSON) (adapter.Event, bool) {
	if len(lj.ToolCalls) > 0 {
		return adapter.Event{Kind: adapter.EvToolStart, Text: lj.ToolCalls[0].Function.Name}, true
	}
	if text := contentText(lj.Content); text != "" {
		return adapter.Event{Kind: adapter.EvMessage, Text: text}, true
	}
	return adapter.Event{}, false
}

// errorLine is how every failure the CLI prints begins, on stdout and
// stderr alike: a plain line, not JSON, and the process exits. The
// reason that follows is what the phrase table sorts.
const errorLine = "error: failed to run prompt:"

// errorPhrase is one row of the phrase table: what a failure's reason
// looks like, case-insensitively, and the kind each sorts into. First
// match wins; extend the table, not the code.
type errorPhrase struct {
	kind   adapter.ErrorKind
	phrase string
}

// errorPhrases sorts a failure line's reason. Resume sorts first: a
// session id is hex digits, and the numeric code phrases ("401", "429")
// must not read a gone session's id as its kind.
var errorPhrases = []errorPhrase{
	// A resume that cannot happen: the session --session named is gone
	// (§5.6, §14), and the answer is a fresh run in place, not another
	// resume.
	{adapter.ErrResumeFailed, `" not found`},
	// The CLI's login (§8): the agent is broken until a probe passes.
	{adapter.ErrAuth, "authorization grant is invalid"},
	{adapter.ErrAuth, "no credential configured"},
	{adapter.ErrAuth, "unauthorized"},
	{adapter.ErrAuth, "401"},
	// The account's quota window (§8): the account cools, the job moves.
	// UNVERIFIED: no real kimi limit message has been captured yet —
	// these phrases are the vocabulary such a message is expected to
	// carry, checked against nothing.
	{adapter.ErrLimit, "rate limit"},
	{adapter.ErrLimit, "usage limit"},
	{adapter.ErrLimit, "quota"},
	{adapter.ErrLimit, "429"},
	{adapter.ErrLimit, "too many requests"},
}

// errorFromText reads one plain line: a failure line is an error event
// sorted by what its reason names; any other line is not an event.
func errorFromText(text string) (adapter.Event, bool) {
	lower := strings.ToLower(text)
	if !strings.HasPrefix(lower, errorLine) {
		return adapter.Event{}, false
	}
	kind := adapter.ErrOther
	for _, m := range errorPhrases {
		if strings.Contains(lower, m.phrase) {
			kind = m.kind
			break
		}
	}
	return adapter.Event{Kind: adapter.EvError, ErrorKind: kind, Text: text}, true
}

// ResultText implements adapter.Adapter: the run's final answer is the
// last assistant message that comes after the final tool call. Text
// from before a later tool call is mid-turn — "let me check…" is not
// the answer — so a tool event retires every message before it, and a
// stream that never spoke after its last tool call, or never spoke at
// all, has no answer.
func (a *Adapter) ResultText(events []adapter.Event) string {
	result := ""
	for _, e := range events {
		switch e.Kind {
		case adapter.EvToolStart, adapter.EvToolEnd:
			result = ""
		case adapter.EvMessage:
			if e.Text != "" {
				result = e.Text
			}
		}
	}
	return result
}
