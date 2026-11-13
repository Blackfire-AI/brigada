// Package qwencode drives the Qwen Code CLI (`qwen`) headless (design
// §5.2): one run per job, `qwen -p` with stream-json output, resumed by
// session id when the job carries one. The CLI's own login state — its
// tokens, its configuration — is the ambient environment's business;
// brigada adds nothing of its own to it and takes the runner's share back
// out. It can be the foreman's harness (design §4.1): a foreman turn
// carries the `brigada mcp` bridge through --mcp-config with
// --strict-mcp-config, resumes the project's session headless with
// --resume, and the attach (AttachCommand) runs the TUI forked onto the
// client's session id. Its limits: no effort
// control, and a foreman turn keeps --dangerously-skip-permissions, so
// the foreman's read-only confinement is not enforced.
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
	"time"

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

// The qwen CLI carries the attach too (§4.2 item 1): its TUI resumes a
// session forked onto the client's --session-id, so no SessionWriteback.
var _ adapter.Interactive = (*Adapter)(nil)

// DefaultModel is the agent's name for the CLI's own default model: the
// one the CLI picks when no -m names another. The adapter's answer to it
// is no -m flag at all.
const DefaultModel = "default"

// autoApproveFlag lets a headless run edit files and run commands without
// asking. `qwen --help` (1.0.0-preview.1) lists no --yolo; this is the
// auto-approve flag it does list.
const autoApproveFlag = "--dangerously-skip-permissions"

// approvalModeFlag names the approval mode a worker runs in; its values are
// qwen's own four modes (`qwen --help`, 1.0.0-preview.1). A worker whose
// agent sets no permission= keeps autoApproveFlag instead, because a
// headless run cannot answer an ask; a named mode replaces that flag.
const approvalModeFlag = "--approval-mode"

var approvalModes = map[string]bool{"plan": true, "default": true, "auto-edit": true, "yolo": true}

// approvalArgs maps a worker's permission= onto qwen's approval flags. A
// value that is not one of qwen's modes (a deprecated tools= alias among
// them, which names a tool scope qwen has no flag for) is refused with the
// value named, never dropped.
func approvalArgs(permission string) ([]string, error) {
	if permission == "" {
		return []string{autoApproveFlag}, nil
	}
	if !approvalModes[permission] {
		return nil, fmt.Errorf("qwen-code: permission %q is not one of qwen's approval modes (plan, default, auto-edit, yolo)", permission)
	}
	return []string{approvalModeFlag, permission}, nil
}

// settingSourcesFlag and settingSourcesValue keep a headless run from
// loading the repository's own .qwen/settings.json: only the user's
// settings, where the login lives, are read (as claude's --setting-sources
// user).
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
// A job that carries an MCP config is a foreman turn (§12.2): the bridge
// rides --mcp-config, and --strict-mcp-config keeps every other server
// out, as claude's headless turn does. The turn keeps auto-approve: qwen
// offers no read-only scope brigada maps the foreman's onto, so the
// foreman's confinement (§4) is not enforced by the harness.
//
// A worker or a review alike takes job.Permission as qwen's own
// --approval-mode (plan, default, auto-edit, yolo); with none set it keeps
// --dangerously-skip-permissions. A foreman turn never reads job.Permission.
// job.ReadDirs is ignored: qwen gets no brigada-authored read allowlist.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	if err := adapter.ValidateEffort("qwen-code", job.Effort); err != nil {
		return nil, fmt.Errorf("qwen-code: %w", err)
	}
	approval := []string{autoApproveFlag}
	if job.MCPConfig == "" {
		var err error
		if approval, err = approvalArgs(job.Permission); err != nil {
			return nil, err
		}
	}
	args := []string{"-p", job.Prompt, "-o", "stream-json"}
	if job.Model != "" && job.Model != DefaultModel {
		args = append(args, "-m", job.Model)
	}
	args = append(args, approval...)
	args = append(args, settingSourcesFlag, settingSourcesValue)
	if job.MCPConfig != "" {
		args = append(args, "--mcp-config", job.MCPConfig, "--strict-mcp-config")
	}
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

// AttachCommand implements adapter.Interactive (§4.2 item 1): the CLI's
// TUI — no -p, no stream to parse — with the opening prompt through -i,
// which runs it and stays interactive. The session flags are the
// write-back made possible: a resume forks the key's session onto the
// client's --session-id (--resume <old> --fork-session --session-id
// <new>), and a first session starts under the client's id alone, so
// nobody reads the CLI's own session files. The fork combination is
// checked headlessly against 1.0.0-preview.1 (TestLiveForkSession, behind
// the qwenlive tag): the init event names the minted id and the fork
// remembers the session it resumed. The TUI attach itself is not driven
// by any test.
//
// The session runs in workdir, the foreman's reading checkout. The
// bridge is added with --mcp-config but without --strict-mcp-config, so
// the user's own MCP servers load as in any session of theirs (as
// claude's attach does); the headless turn alone is strict. No
// auto-approve flag: the TUI asks the user at the terminal. The status
// bar rides --settings as inline JSON, for this session alone.
//
// The streams are the caller's to wire.
func (a *Adapter) AttachCommand(job adapter.Job, workdir, resumeSessionID, newSessionID string) (*exec.Cmd, error) {
	if newSessionID == "" {
		return nil, errors.New("qwen-code: the attach generates the new session id; an empty one cannot be named to the CLI")
	}
	if job.MCPConfig == "" {
		return nil, errors.New("qwen-code: an attached foreman runs with the bridge: the job carries no MCP config")
	}
	if err := adapter.ValidateEffort("qwen-code", job.Effort); err != nil {
		return nil, fmt.Errorf("qwen-code: %w", err)
	}
	var args []string
	if job.Prompt != "" {
		args = append(args, "-i", job.Prompt)
	}
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID, "--fork-session")
	}
	args = append(args, "--session-id", newSessionID)
	if job.Model != "" && job.Model != DefaultModel {
		args = append(args, "-m", job.Model)
	}
	args = append(args, settingSourcesFlag, settingSourcesValue, "--mcp-config", job.MCPConfig)
	if job.StatusLine != "" {
		settings, err := statusLineSettings(job.StatusLine)
		if err != nil {
			return nil, fmt.Errorf("qwen-code: %w", err)
		}
		args = append(args, "--settings", settings)
	}
	cmd := exec.Command(a.binary(), args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	cmd.Dir = workdir
	cmd.Env = workerEnv(os.Environ())
	return cmd, nil
}

// statusLineSettings renders the attach's --settings: the claude-compatible
// statusLine entry qwen renders as the bottom line, inline JSON.
func statusLineSettings(command string) (string, error) {
	type statusLine struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	data, err := json.Marshal(struct {
		StatusLine statusLine `json:"statusLine"`
	}{statusLine{Type: "command", Command: command}})
	if err != nil {
		return "", fmt.Errorf("render the status-line settings: %w", err)
	}
	return string(data), nil
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

// The stream's shape lives here and nowhere else. It is qwen's Claude
// Code-compatible stream-json, checked against a live run of 1.0.0-preview.1
// (testdata/normal.jsonl is that run, redacted): types, subtypes, block
// types and usage keys below are the ones that run printed.
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
	Usage     json.RawMessage `json:"usage"`
	NumTurns  json.RawMessage `json:"num_turns"`
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	SessionID string          `json:"session_id"`
	Message   messageJSON     `json:"message"`
	Errors    []string        `json:"errors"`
	ErrorCode string          `json:"error_code"`
}

type messageJSON struct {
	Content    []blockJSON     `json:"content"`
	StopReason json.RawMessage `json:"stop_reason"`
	Usage      json.RawMessage `json:"usage"`
}

// complete reports whether the message is a finished one: partial lines
// carry a null stop_reason and zeroed usage.
func (m messageJSON) complete() bool {
	s := bytes.TrimSpace(m.StopReason)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
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
// Tokens are counted from finished assistant messages (stop_reason set;
// partial lines carry zeroed usage) and never from the result line, whose
// usage is zeroed, so a run is not counted twice. Unknown and malformed
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
		if lj.Message.complete() {
			if u := messageUsage(lj.Message.Usage); u != nil {
				ev.Usage = u
				if !ok {
					ev.Kind, ok = adapter.EvUsage, true
				}
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
// the session event with the final text in tow. A quota result has no
// result field: its errors[] and error_code carry the failure, so they are
// read before the result and subtype fallbacks. An HTTP 402 billing_error
// carrying a future quota.reset_at is the subscription's weekly window and
// sorts as a limit until that reset; a 402 with no usable reset_at stays a
// spent balance.
func resultEvent(lj lineJSON) (adapter.Event, bool) {
	if lj.IsError || strings.HasPrefix(lj.Subtype, "error") {
		text := lj.Result
		if errs := strings.Join(lj.Errors, "; "); errs != "" {
			text = errs
		}
		if text == "" {
			text = lj.Subtype
		}
		classify := text
		if lj.ErrorCode != "" {
			classify += " " + lj.ErrorCode
		}
		ev, ok := phraseEvent(classify)
		if !ok {
			ev = adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther}
		}
		ev.Text = text
		switch ev.ErrorKind {
		case adapter.ErrBalance:
			if until := resetAt(lj.Errors); until != "" {
				if t, err := time.Parse(time.RFC3339, until); err == nil && t.After(now()) {
					ev.ErrorKind = adapter.ErrLimit
					ev.LimitUntil = until
				}
			}
		case adapter.ErrLimit:
			ev.LimitUntil = resetAt(lj.Errors)
		}
		return ev, true
	}
	if lj.SessionID != "" {
		return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID, Text: lj.Result}, true
	}
	if lj.Result != "" {
		return adapter.Event{Kind: adapter.EvMessage, Text: lj.Result}, true
	}
	return adapter.Event{}, false
}

// now is the clock resultEvent compares a reset time against; tests pin it.
var now = time.Now

// resetAt reads quota.reset_at (unix seconds) from the JSON object that
// follows the status prefix in an errors[] string ("HTTP 402: {...}") and
// returns it as RFC3339 UTC; "" when none is there or it is malformed.
func resetAt(errs []string) string {
	for _, e := range errs {
		i := strings.Index(e, "{")
		if i < 0 {
			continue
		}
		var body struct {
			Error struct {
				Quota struct {
					ResetAt float64 `json:"reset_at"`
				} `json:"quota"`
			} `json:"error"`
		}
		if json.NewDecoder(strings.NewReader(e[i:])).Decode(&body) != nil {
			continue
		}
		if at := body.Error.Quota.ResetAt; at > 0 && at < 1<<40 {
			return time.Unix(int64(at), 0).UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// errorPhrase is one row of the phrase table: what a failure's text looks
// like, case-insensitively, and the kind it sorts into. First match wins.
type errorPhrase struct {
	kind   adapter.ErrorKind
	phrase string
}

// errorPhrases is conservative on purpose: stderr also carries chatter
// ("rate limited, retrying"), so only whole failure phrases are here.
// The HTTP 402 billing_error result line (errors[] with error_code
// insufficient_quota) is verified against a real failure by the
// failure-weekly-limit fixture: with quota.reset_at it is the subscription's
// weekly window (the qwen UI showed "week limit reached" with the same reset
// time), so resultEvent sorts it as a limit until that reset. The other
// phrases are still UNVERIFIED against the real CLI, a best understanding
// and not seen output.
var errorPhrases = []errorPhrase{
	// The session --resume named is gone (§5.6): a fresh run, not another resume.
	{adapter.ErrResumeFailed, "session not found"},
	{adapter.ErrResumeFailed, "no session found"},
	{adapter.ErrResumeFailed, "no conversation found"},
	{adapter.ErrResumeFailed, "conversation not found"},
	// A 402 sorts as a spent balance (§8) here, ahead of the quota rows
	// since its text also says insufficient_quota and first match wins;
	// resultEvent turns one with a usable quota.reset_at into a limit.
	{adapter.ErrBalance, "http 402"},
	{adapter.ErrBalance, "billing_error"},
	{adapter.ErrBalance, "insufficient credits"},
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

// messageUsage reads one finished assistant message's usage, the only place
// the stream carries token counts: the result line's usage is zeroed.
// Cached input is a breakdown of input (qwen's input_tokens already
// includes cache_read_input_tokens), so it never exceeds it (the opencode
// fix, a81915b).
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

// resultUsage reads the result line: its token counts are zeroed, so only
// the turn count is taken, and the tokens come from the assistant messages.
func resultUsage(lj lineJSON) *adapter.Usage {
	var u *adapter.Usage
	var turns int64
	if len(lj.NumTurns) > 0 && json.Unmarshal(lj.NumTurns, &turns) == nil && turns >= 0 {
		if u == nil {
			u = &adapter.Usage{}
		}
		u.Add(adapter.Calls(turns))
	}
	return u
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
