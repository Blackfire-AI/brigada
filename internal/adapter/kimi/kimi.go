// Package kimi drives the Kimi Code CLI headless (design §5.2): one run
// per job, `kimi -p` with stream-json output, resumed by session id
// when the job carries one. The CLI's own login state — its tokens, its
// configuration — is the ambient environment's business; brigada adds
// nothing of its own to it and takes the runner's share back out.
package kimi

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

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

	// ReadOnlyAgentFile is the runner-owned definition used for review jobs,
	// whether or not AgentFile is set. Work and fix jobs keep the CLI's normal agent.
	ReadOnlyAgentFile string

	// ExtraArgs are further flags the agent's configuration passes
	// through, appended verbatim after the adapter's own. Empty adds
	// nothing.
	ExtraArgs []string
}

var _ adapter.Adapter = (*Adapter)(nil)

// DefaultModel is the agent's name for the CLI's own default model: the
// one the CLI picks when no -m names another. The runner offers it when
// the agent's configuration sets no --model, and the adapter's answer
// to it is no -m flag at all.
const DefaultModel = "default"

// ReadOnlyAgentDefinition uses Kimi Code's Markdown agent format:
// https://moonshotai.github.io/kimi-code/en/customization/agents
// The frontmatter includes its required description and an exact tool allowlist.
//
//go:embed readonly.md
var ReadOnlyAgentDefinition string

// Name implements adapter.Adapter: the harness this adapter drives,
// which the runner's advertisement pairs it with (§5.1).
func (a *Adapter) Name() string { return "kimi" }

// ProbeCommand implements adapter.Adapter: `kimi --version`, a cheap
// command that answers when the CLI is well (§8 "Health"), bound to ctx
// so a hung CLI is killed and reads as a failing probe.
func (a *Adapter) ProbeCommand(ctx context.Context) *exec.Cmd {
	binary := a.Binary
	if binary == "" {
		binary = "kimi"
	}
	return exec.CommandContext(ctx, binary, "--version")
}

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
	if err := adapter.ValidateEffort("kimi", job.Effort); err != nil {
		return nil, fmt.Errorf("kimi: %w", err)
	}
	binary := a.Binary
	if binary == "" {
		binary = "kimi"
	}
	args := []string{
		"-p", job.Prompt,
		"--output-format", "stream-json",
	}
	if job.Model != "" && job.Model != DefaultModel {
		args = append(args, "-m", job.Model)
	}
	agentFile := a.AgentFile
	if job.Review && a.ReadOnlyAgentFile != "" {
		// A reviewer is read-only whatever agent the configuration names.
		agentFile = a.ReadOnlyAgentFile
	}
	if resumeSessionID != "" {
		args = append(args, "--session", resumeSessionID)
	} else if agentFile != "" {
		args = append(args, "--agent-file", agentFile)
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
	Usage      json.RawMessage `json:"usage"`
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
		ev, ok := assistantEvent(lj)
		ev.Usage = adapter.ParseUsage(lj.Usage, "input_tokens", "output_tokens", "cached_input_tokens", "reasoning_tokens", "")
		// Each assistant line that reports usage is one model request.
		if ev.Usage != nil {
			ev.Usage.Add(adapter.Calls(1))
		}
		if !ok && ev.Usage != nil {
			ev.Kind, ok = adapter.EvUsage, true
		}
		return ev, ok
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
		return adapter.Event{Kind: adapter.EvToolStart, Text: lj.ToolCalls[0].Function.Name,
			Args: lj.ToolCalls[0].Function.Arguments}, true
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
// must not read a gone session's id as its kind. The phrases that name a
// limit come next, ahead of the auth ones: the CLI prefixes a quota
// message with the provider's error code (`provider.auth_error: 403
// You've reached your 5-hour usage limit…`), and the code must not
// outvote what the message says.
var errorPhrases = []errorPhrase{
	// A resume that cannot happen: the session --session named is gone
	// (§5.6), and the answer is a fresh run in place, not another
	// resume.
	{adapter.ErrResumeFailed, `" not found`},
	// The account's quota window (§8): the account cools, the job moves.
	// "usage limit" and "5-hour" are from a real capture (kimi 2.1.1);
	// the rest is the vocabulary other limit messages are expected to
	// carry.
	{adapter.ErrLimit, "rate limit"},
	{adapter.ErrLimit, "usage limit"},
	{adapter.ErrLimit, "quota"},
	{adapter.ErrLimit, "5-hour"},
	{adapter.ErrLimit, "reset window"},
	// The CLI's login (§8): the agent is broken until a probe passes.
	{adapter.ErrAuth, "authorization grant is invalid"},
	{adapter.ErrAuth, "no credential configured"},
	{adapter.ErrAuth, "unauthorized"},
	{adapter.ErrAuth, "401"},
	// Bare status codes: a line that names no limit keeps the kind its
	// other words give it, and the status code outvotes the generic
	// provider-error words below it.
	{adapter.ErrLimit, "429"},
	{adapter.ErrLimit, "too many requests"},
	{adapter.ErrAuth, "auth_error"},
	{adapter.ErrAuth, "invalid api key"},
}

// now is the clock limitReset reads; tests replace it.
var now = time.Now

// resetInstantRe finds a reset given as an instant, anywhere after the
// word "reset": "resets at 2026-10-15T18:40:00+03:00". The one real
// limit message captured gives no time at all, so this is the shape a
// message that does is expected to take.
var resetInstantRe = regexp.MustCompile(`(?i)reset.*?(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))`)

// resetClockRe finds a reset given as a wall-clock time in a named zone,
// the claude CLI's convention: "resets 6:40pm (Europe/Moscow)".
var resetClockRe = regexp.MustCompile(`(?i)resets\s+(\d{1,2})(?::(\d{2}))?\s*([ap])m\s*\(([^)]+)\)`)

// limitReset reads the reset time out of a limit message, as an RFC3339
// instant. Best effort: a message that gives no time, or one already
// past at, is "" (unknown) and the default cooling applies. A window
// length like "5-hour" is never a reset.
func limitReset(text string, at time.Time) string {
	if m := resetInstantRe.FindStringSubmatch(text); m != nil {
		t, err := time.Parse(time.RFC3339, m[1])
		if err != nil || !t.After(at) {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	m := resetClockRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	hour, _ := strconv.Atoi(m[1])
	min := 0
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	if hour < 1 || hour > 12 || min > 59 {
		return ""
	}
	hour %= 12
	if strings.EqualFold(m[3], "p") {
		hour += 12
	}
	loc, err := time.LoadLocation(strings.TrimSpace(m[4]))
	if err != nil {
		return ""
	}
	local := at.In(loc)
	t := time.Date(local.Year(), local.Month(), local.Day(), hour, min, 0, 0, loc)
	if !t.After(at) {
		t = time.Date(local.Year(), local.Month(), local.Day()+1, hour, min, 0, 0, loc)
	}
	return t.Format(time.RFC3339)
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
	if kind == adapter.ErrOther && adapter.IsInfraText(text) {
		kind = adapter.ErrInfra
	}
	ev := adapter.Event{Kind: adapter.EvError, ErrorKind: kind, Text: text}
	if kind == adapter.ErrLimit {
		ev.LimitUntil = limitReset(text, now())
	}
	return ev, true
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
