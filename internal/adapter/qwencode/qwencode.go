// Package qwencode drives the Qwen Code CLI (`qwen`) headless (design
// §5.2): one run per job, `qwen -p` with stream-json output, resumed by
// session id when the job carries one. The CLI's own login state — its
// tokens, its configuration — is the ambient environment's business;
// brigada adds nothing of its own to it and takes the runner's share back
// out. The adapter is worker-only: no review policy, no attach.
package qwencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Adapter drives the qwen CLI (1.0.0-preview.1 and its line) in headless
// prompt mode. The zero value runs `qwen` from PATH; the fields carry what
// the agent's configuration passes through.
type Adapter struct {
	// Binary is the CLI to run; "qwen" when empty.
	Binary string
}

var _ adapter.Adapter = (*Adapter)(nil)

// DefaultModel is the agent's name for the CLI's own default model: the
// one the CLI picks when no -m names another. The adapter's answer to it
// is no -m flag at all.
const DefaultModel = "default"

// autoApproveFlag lets a headless run edit files: `qwen --help`
// (1.0.0-preview.1) lists it as "Bypass all permission checks", and a live
// run reports permissionMode "bypassPermissions". Without it a headless
// worker could not write. The older --yolo is not in --help and is not used.
const autoApproveFlag = "--dangerously-skip-permissions"

// settingSourcesFlag and settingSourcesValue keep the workspace's own
// settings (.qwen/settings.json: the "project" and "local" sources) out of a
// headless run: only the user's settings, which hold the login, load. qwen
// has no flag that skips context files such as QWEN.md; those are still
// read from the workspace.
const (
	settingSourcesFlag  = "--setting-sources"
	settingSourcesValue = "user"
)

// Name implements adapter.Adapter: the harness this adapter drives.
func (a *Adapter) Name() string { return "qwen-code" }

func (a *Adapter) binary() string {
	if a.Binary == "" {
		return "qwen"
	}
	return a.Binary
}

// ProbeCommand implements adapter.Adapter: `qwen --version`, bound to ctx
// so a hung CLI is killed and reads as a failing probe.
func (a *Adapter) ProbeCommand(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, a.binary(), "--version")
}

// Command implements adapter.Adapter: the fresh or resume invocation of
// the CLI, running in workdir. The environment is the runner's ambient set
// minus every BRIGADA_-prefixed variable (§5.1).
//
// job.ReadDirs and job.Tools are ignored for now: reading extra
// directories and mapping a tool scope onto qwen's permission rules are
// follow-ups.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	if job.MCPConfig != "" {
		return nil, errors.New("qwen-code: the adapter wires no MCP-config flag; a job carrying an MCP config cannot run on it")
	}
	if job.Review {
		return nil, errors.New("qwen-code: no read-only review policy yet; review jobs cannot run on it")
	}
	if err := adapter.ValidateEffort("qwen-code", job.Effort); err != nil {
		return nil, fmt.Errorf("qwen-code: %w", err)
	}
	args := []string{"-p", job.Prompt, "-o", "stream-json", settingSourcesFlag, settingSourcesValue}
	if job.Model != "" && job.Model != DefaultModel {
		args = append(args, "-m", job.Model)
	}
	args = append(args, autoApproveFlag)
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID)
	}
	cmd := exec.Command(a.binary(), args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	cmd.Dir = workdir
	cmd.Env = workerEnv(os.Environ())
	return cmd, nil
}

// envPrefix marks brigada's own variables, none of which is the worker's
// to see.
const envPrefix = "BRIGADA_"

// workerEnv is the ambient environment minus everything brigada-prefixed.
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

// The stream's shape lives here and nowhere else: qwen's Claude Code-style
// stream-json, as recorded from qwen 1.0.0-preview.1 (testdata/normal.jsonl).
const (
	typeSystem    = "system"
	typeAssistant = "assistant"
	typeUser      = "user"
	typeResult    = "result"
	typeError     = "error"

	subtypeInit = "init"

	blockText       = "text"
	blockToolUse    = "tool_use"
	blockToolResult = "tool_result"

	usageInput  = "input_tokens"
	usageOutput = "output_tokens"
	usageCached = "cache_read_input_tokens"
)

// lineJSON is one line of the stream: one JSON object per line, whose
// type says what it carries. Only the fields the adapter reads are
// declared.
type lineJSON struct {
	NumTurns  json.RawMessage `json:"num_turns"`
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	SessionID string          `json:"session_id"`
	Message   messageJSON     `json:"message"`
}

// messageJSON is an assistant or user message. qwen streams an assistant
// message twice: first as a partial line with stop_reason null and zeroed
// usage, then as a final line whose stop_reason is set and whose usage holds
// the message's real counts.
type messageJSON struct {
	StopReason *string         `json:"stop_reason"`
	Usage      json.RawMessage `json:"usage"`
	Content    []blockJSON     `json:"content"`
}

type blockJSON struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// ParseEvent implements adapter.Adapter: one line of stream-json turned
// into one progress event.
//
//	tool_use    → tool-call start (the name)
//	tool_result → tool-call end (the id of the call it ends)
//	text block  → assistant message
//	init, result session_id → session id; the result's carries the final text
//	error result, or a plain line naming a known failure → an error event
//
// An assistant line that both speaks and calls a tool is its tool call.
// Token usage is counted from the final assistant lines (stop_reason set):
// the result line's own usage object is zeroed in qwen's stream, so only its
// num_turns (the model calls) is read there. Nothing is counted twice. Unknown and malformed
// lines are ignored, never fatal.
func (a *Adapter) ParseEvent(line []byte) (adapter.Event, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return adapter.Event{}, false
	}
	var lj lineJSON
	if err := json.Unmarshal(trimmed, &lj); err != nil {
		return phraseEvent(string(trimmed))
	}
	switch lj.Type {
	case typeSystem:
		if lj.Subtype != subtypeInit || lj.SessionID == "" {
			return adapter.Event{}, false
		}
		return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID}, true
	case typeAssistant:
		ev, ok := assistantEvent(lj.Message.Content)
		if lj.Message.StopReason != nil {
			ev.Usage = messageUsage(lj.Message.Usage)
			if !ok && ev.Usage != nil {
				ev.Kind, ok = adapter.EvUsage, true
			}
		}
		return ev, ok
	case typeUser:
		return toolEnd(lj.Message.Content)
	case typeResult:
		ev, ok := resultEvent(lj)
		ev.Usage = resultUsage(lj)
		if !ok && ev.Usage != nil {
			ev.Kind, ok = adapter.EvUsage, true
		}
		return ev, ok
	default:
		return adapter.Event{}, false
	}
}

// ParseErrEvent implements adapter.Adapter: one line of stderr. Plain
// text sorts by the phrase table; the CLI's JSON there is an error result
// or an error object. Anything else — a banner, a warning, a progress
// note — is not the run's business.
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return adapter.Event{}, false
	}
	var lj lineJSON
	if err := json.Unmarshal(trimmed, &lj); err == nil && lj.Type != "" {
		switch lj.Type {
		case typeResult:
			if ev, ok := resultEvent(lj); ok && ev.Kind == adapter.EvError {
				ev.Usage = resultUsage(lj)
				return ev, true
			}
		case typeError:
			return phraseEvent(string(trimmed))
		}
		return adapter.Event{}, false
	}
	return phraseEvent(string(trimmed))
}

func assistantEvent(blocks []blockJSON) (adapter.Event, bool) {
	for _, b := range blocks {
		if b.Type == blockToolUse {
			return adapter.Event{Kind: adapter.EvToolStart, Text: b.Name, Args: adapter.ToolArgs(b.Input)}, true
		}
	}
	for _, b := range blocks {
		if b.Type == blockText && b.Text != "" {
			return adapter.Event{Kind: adapter.EvMessage, Text: b.Text}, true
		}
	}
	return adapter.Event{}, false
}

func toolEnd(blocks []blockJSON) (adapter.Event, bool) {
	for _, b := range blocks {
		if b.Type == blockToolResult {
			return adapter.Event{Kind: adapter.EvToolEnd, Text: b.ToolUseID}, true
		}
	}
	return adapter.Event{}, false
}

// resultEvent reads the final line. An error result is an error event
// sorted by its text (unknown text stays ErrOther); a success result is
// the session event with the final text in tow.
func resultEvent(lj lineJSON) (adapter.Event, bool) {
	if lj.IsError || strings.HasPrefix(lj.Subtype, "error") {
		text := lj.Result
		if text == "" {
			text = lj.Subtype
		}
		if ev, ok := phraseEvent(text); ok {
			return ev, true
		}
		return adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: text}, true
	}
	if lj.SessionID != "" {
		return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID, Text: lj.Result}, true
	}
	if lj.Result != "" {
		return adapter.Event{Kind: adapter.EvMessage, Text: lj.Result}, true
	}
	return adapter.Event{}, false
}

// errorPhrase is one row of the phrase table: what a failure's text looks
// like, case-insensitively, and the kind it sorts into. First match wins.
type errorPhrase struct {
	kind   adapter.ErrorKind
	phrase string
}

// errorPhrases is conservative on purpose: stderr also carries chatter
// ("rate limited, retrying"), so only whole failure phrases are here.
// The live run (1.0.0-preview.1) hit no failure, so these are generic
// failure phrases, not text observed from the CLI.
var errorPhrases = []errorPhrase{
	// The session --resume named is gone (§5.6): a fresh run, not another resume.
	{adapter.ErrResumeFailed, "session not found"},
	{adapter.ErrResumeFailed, "no session found"},
	{adapter.ErrResumeFailed, "no conversation found"},
	{adapter.ErrResumeFailed, "conversation not found"},
	// The account's quota window (§8).
	{adapter.ErrLimit, "usage limit"},
	{adapter.ErrLimit, "rate limit exceeded"},
	{adapter.ErrLimit, "rate limit reached"},
	{adapter.ErrLimit, "quota exceeded"},
	{adapter.ErrLimit, "exceeded your quota"},
	{adapter.ErrLimit, "insufficient_quota"},
	{adapter.ErrLimit, "too many requests"},
	// The CLI's login (§8).
	{adapter.ErrAuth, "invalid api key"},
	{adapter.ErrAuth, "not logged in"},
	{adapter.ErrAuth, "authentication failed"},
	{adapter.ErrAuth, "authentication required"},
	{adapter.ErrAuth, "unauthorized"},
	{adapter.ErrAuth, "token expired"},
}

// phraseEvent sorts one piece of text. Text that names nothing is not an
// event; callers that know the text is a failure fall back to ErrOther.
func phraseEvent(text string) (adapter.Event, bool) {
	if adapter.IsModelRejectedText(text) {
		return adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrModel, Text: text}, true
	}
	lower := strings.ToLower(text)
	for _, m := range errorPhrases {
		if strings.Contains(lower, m.phrase) {
			return adapter.Event{Kind: adapter.EvError, ErrorKind: m.kind, Text: text}, true
		}
	}
	if adapter.IsInfraText(text) {
		return adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrInfra, Text: text}, true
	}
	return adapter.Event{}, false
}

// messageUsage reads a final assistant message's usage. qwen's input_tokens
// is the whole prompt, cache reads included (the line's context_usage_ratio
// is input_tokens over the window; cache_read_input_tokens is a part of it),
// so cached input never exceeds input (the opencode fix, a81915b).
func messageUsage(raw json.RawMessage) *adapter.Usage {
	u := adapter.ParseUsage(raw, usageInput, usageOutput, usageCached, "", "")
	if u != nil && u.CachedInputTokens != nil {
		if u.InputTokens == nil {
			u.CachedInputTokens = nil
		} else if *u.CachedInputTokens > *u.InputTokens {
			n := *u.InputTokens
			u.CachedInputTokens = &n
		}
	}
	return u
}

// resultUsage reads the result line's model-call count (num_turns). Its
// usage object is not read: qwen leaves it zeroed, and the tokens are
// counted from the assistant lines.
func resultUsage(lj lineJSON) *adapter.Usage {
	var turns int64
	if len(lj.NumTurns) > 0 && json.Unmarshal(lj.NumTurns, &turns) == nil && turns >= 0 {
		return adapter.Calls(turns)
	}
	return nil
}

// ResultText implements adapter.Adapter: the final text is the result
// line's, which its session event carries. A stream that never reached a
// result falls back to the last assistant message.
func (a *Adapter) ResultText(events []adapter.Event) string {
	result, lastMessage := "", ""
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
