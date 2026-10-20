// Package claude drives the Claude Code CLI headless (design §5.2): one
// run per job, `claude -p` with stream-json output, resumed by session
// id when the job carries one. The CLI's own login state — its tokens,
// its configuration — is the ambient environment's business; brigada
// adds nothing of its own to it and takes the runner's share back out.
package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/mcp"
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
	// (§5.2), passed through as the CLI's own flags. They scope a code
	// worker only: a foreman turn or attach always runs under the
	// foreman's own scope and never reads them. A nil AllowedTools is
	// "no explicit scope" and the worker gets the Go default; a non-nil
	// empty one is explicitly none — the file tools alone, no shell.
	AllowedTools    []string
	DisallowedTools []string
}

// workerPermissionMode is the permission mode a worker runs under when
// the agent config does not name one: edits inside the working tree are
// accepted without a prompt, which headless runs cannot answer anyway.
const workerPermissionMode = "acceptEdits"

// foremanToolScope is the foreman's tool scope (§4): brigada's MCP tools
// — the bridge's whole offer, in the mcp__<server>__<tool> spelling the
// CLI names them by — plus the read-only file tools. The deny side
// names the shell and every editing tool the CLI carries, so no
// permission mode can let one through: the foreman never edits code.
func foremanToolScope() (allowed, disallowed []string) {
	allowed = []string{"Read", "Grep", "Glob"}
	for _, name := range mcp.ToolNames() {
		allowed = append(allowed, "mcp__"+mcp.ServerName+"__"+name)
	}
	return allowed, []string{"Bash", "Edit", "NotebookEdit", "Write"}
}

// workerToolScope is the code worker's default tool scope (§5.2): the Go
// toolchain's build, test, vet and format commands, and nothing else of
// the shell. The worker verifies its own work as it goes; the runner
// still runs the check on the same tree, so no new code reaches
// execution — the check already runs what tests the worker wrote. The
// agent config's explicit scope replaces this default.
func workerToolScope() (allowed, disallowed []string) {
	return DefaultWorkerTools(), nil
}

// DefaultWorkerTools is the Go default scope a worker runs under when
// the agent names none, as a fresh list.
func DefaultWorkerTools() []string {
	return []string{"Bash(go build:*)", "Bash(go test:*)", "Bash(go vet:*)", "Bash(gofmt:*)"}
}

// ScopeText renders a worker scope as the agent registration records it
// and the runner prints it: the patterns joined by "+", the --agent
// tools= spelling, and "none" for the explicitly empty scope. A nil
// scope is the default and renders as the Go default's patterns.
func ScopeText(tools []string) string {
	if tools == nil {
		tools = DefaultWorkerTools()
	}
	if len(tools) == 0 {
		return "none"
	}
	return strings.Join(tools, "+")
}

var _ adapter.Adapter = (*Adapter)(nil)

// The claude CLI carries the attach too (§4.2 item 1): interactive
// resume on a forked session id.
var _ adapter.Interactive = (*Adapter)(nil)

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
	if err := adapter.ValidateEffort("claude", job.Effort); err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	if job.Effort != "" {
		args = append(args, "--effort", job.Effort)
	}
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID)
	}
	mode := a.PermissionMode
	if mode == "" {
		mode = workerPermissionMode
	}
	args = append(args, "--permission-mode", mode)
	allowed, disallowed := a.AllowedTools, a.DisallowedTools
	if job.MCPConfig != "" {
		// A job that carries an MCP config is a foreman turn (§12.2): the
		// bridge is the turn's own, and the CLI offers no other server
		// beside it. The foreman's tool scope is brigada's too (§4), not
		// the agent config's — it decides through the bridge and reads
		// the repo, and never edits code.
		args = append(args, "--mcp-config", job.MCPConfig, "--strict-mcp-config")
		allowed, disallowed = foremanToolScope()
	} else if allowed == nil {
		// A code worker with no explicit scope gets the default (§5.2):
		// the Go toolchain, so it can build and test as it goes.
		allowed, disallowed = workerToolScope()
	}
	if len(allowed) > 0 {
		args = append(args, append([]string{"--allowedTools"}, allowed...)...)
	}
	if len(disallowed) > 0 {
		args = append(args, append([]string{"--disallowedTools"}, disallowed...)...)
	}
	// The extra repositories' checkouts (§5.8) sit outside the worktree
	// the command runs in: one --add-dir per directory lets the file
	// tools read them, and their read-only files refuse writes anyway.
	for _, dir := range job.ReadDirs {
		args = append(args, "--add-dir", dir)
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

// statusLineRefreshSeconds is how often the attached session's status
// bar re-runs its command while nothing else updates it (§10).
const statusLineRefreshSeconds = 3

// AttachCommand implements adapter.Interactive (§4.2 item 1): the CLI
// interactive — no -p, no stream to parse — with the opening prompt as
// its initial prompt, carrying the bridge and the foreman's tool scope
// exactly as a headless turn does. The session flags are the write-back
// made possible (§4.2): a resume forks the CLI's session ids, so the
// attach resumes the key's session and forks it onto the client's
// --session-id, which the client generated and will release with —
// nobody has to read the CLI's own session files. A first session
// starts under the client's id alone.
//
// The streams are the caller's to wire: an interactive session belongs
// to the terminal, and the adapter leaves stdin, stdout and stderr
// untouched for the client to connect.
//
// The session runs in workdir, the foreman's reading checkout, so the
// repository's own CLAUDE.md, .claude/ and the project memory load as in
// any clone. The bridge is added with --mcp-config but without
// --strict-mcp-config, so the user's own user-scope MCP servers load as
// in any session of theirs; the headless turn alone is strict. A
// repository whose .mcp.json still carries an older init's `brigada`
// entry (`brigada mcp --foreman`) must have it removed (`brigada
// remove`, or rerun `brigada init` once the init change lands): two
// servers named `brigada` would clash.
func (a *Adapter) AttachCommand(job adapter.Job, workdir, resumeSessionID, newSessionID string) (*exec.Cmd, error) {
	if newSessionID == "" {
		return nil, fmt.Errorf("the attach generates the new session id; an empty one cannot be named to the CLI")
	}
	if job.MCPConfig == "" {
		return nil, fmt.Errorf("an attached foreman runs with the bridge: the job carries no MCP config")
	}
	binary := a.Binary
	if binary == "" {
		binary = "claude"
	}
	args := make([]string, 0, 24)
	// The opening prompt is the CLI's initial prompt, its one positional
	// argument — and it leads the argv, before every flag: the tool-scope
	// flags take a variadic list (<tools...>), so a positional after them
	// would parse as one more tool name and the session would start with
	// no prompt at all. First, nothing can capture it. (The headless
	// command has no such care to take: its prompt rides on stdin.)
	if job.Prompt != "" {
		args = append(args, job.Prompt)
	}
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID, "--fork-session")
	}
	args = append(args, "--session-id", newSessionID)
	if job.Model != "" {
		args = append(args, "--model", job.Model)
	}
	if err := adapter.ValidateEffort("claude", job.Effort); err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	if job.Effort != "" {
		args = append(args, "--effort", job.Effort)
	}
	args = append(args, "--mcp-config", job.MCPConfig)
	if job.StatusLine != "" {
		// The CLI's status bar runs the command on a timer as well as on
		// the session's own events, so it stays current while the chat
		// is idle and the workers are not. The settings ride on the flag
		// for this session alone; the user's own settings files are
		// never touched.
		settings, err := json.Marshal(map[string]any{
			"statusLine": map[string]any{
				"type":            "command",
				"command":         job.StatusLine,
				"refreshInterval": statusLineRefreshSeconds,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("encode the status line settings: %w", err)
		}
		args = append(args, "--settings", string(settings))
	}
	allowed, disallowed := foremanToolScope()
	args = append(args, append([]string{"--allowedTools"}, allowed...)...)
	args = append(args, append([]string{"--disallowedTools"}, disallowed...)...)
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
// per line, whose type says what it carries. Only the fields the
// adapter reads are declared; the rest of each line is ignored where it
// stands.
type lineJSON struct {
	Usage     json.RawMessage `json:"usage"`
	NumTurns  json.RawMessage `json:"num_turns"`
	Cost      json.RawMessage `json:"total_cost_usd"`
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	SessionID string          `json:"session_id"`
	Message   messageJSON     `json:"message"`
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
		// Not the CLI's JSON: a banner, or prose on its stdout. The
		// broad table decides — nothing else in prose is an event.
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
		ev, ok := resultEvent(lj)
		ev.Usage = resultUsage(lj)
		return ev, ok
	default:
		return adapter.Event{}, false
	}
}

// ParseErrEvent implements adapter.Adapter: one line of the CLI's
// stderr, the strictest stream the adapter reads. The failures that
// print only there — a resume the CLI cannot do, before any JSON; a
// spent account; a broken login — are plain text named in full, and the
// strict table sorts them, because stderr also carries the CLI's
// chatter: an MCP server that "requires authentication", a "rate
// limited, retrying" note, and a warning must not fail a run that ended
// clean. The CLI's JSON there is an error result, sorted as on stdout
// by the broad table, or an error object, sorted like plain stderr
// text; anything else — a banner, a warning, a progress note — is not
// the run's business.
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return adapter.Event{}, false
	}
	var lj lineJSON
	if err := json.Unmarshal(trimmed, &lj); err == nil && lj.Type != "" {
		// The CLI's JSON on the wrong stream: its error result sorts as
		// it would on stdout, its error object like plain stderr text;
		// every other event belongs to stdout's stream, not here.
		switch lj.Type {
		case "result":
			if ev, ok := resultEvent(lj); ok && ev.Kind == adapter.EvError {
				ev.Usage = resultUsage(lj)
				return ev, true
			}
		case "error":
			return phraseEvent(stderrPhrases, string(trimmed))
		}
		return adapter.Event{}, false
	}
	return phraseEvent(stderrPhrases, string(trimmed))
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

// errorPhrase is one row of a phrase table: what a failure's text looks
// like, case-insensitively, and the kind each sorts into. First match
// wins; extend the table, not the code.
type errorPhrase struct {
	kind   adapter.ErrorKind
	phrase string
}

// errorPhrases is the broad table an error result's text sorts by: an
// error result is the CLI's own verdict on the whole run, so its text
// can afford loose phrases.
var errorPhrases = []errorPhrase{
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
	{adapter.ErrLimit, "hit your session limit"},
	// The CLI's login (§8): the agent is broken until a probe passes.
	{adapter.ErrAuth, "invalid api key"},
	{adapter.ErrAuth, "please run /login"},
	{adapter.ErrAuth, "not logged in"},
	{adapter.ErrAuth, "authentication"},
	{adapter.ErrAuth, "unauthorized"},
}

// stderrPhrases is the strict table stderr sorts by: only the failure
// messages the CLI itself prints there, named in full. Stderr also
// carries the CLI's chatter — an MCP server that "requires
// authentication", a "rate limited, retrying" note — and a warning must
// not fail a run that ended clean, so the broad substrings an error
// result's text can afford earn no place here.
var stderrPhrases = []errorPhrase{
	{adapter.ErrResumeFailed, "no conversation found"},
	{adapter.ErrResumeFailed, "conversation not found"},
	{adapter.ErrResumeFailed, "session not found"},
	{adapter.ErrResumeFailed, "no session found"},
	{adapter.ErrLimit, "usage limit reached"},
	{adapter.ErrLimit, "hit your session limit"},
	{adapter.ErrAuth, "invalid api key"},
	{adapter.ErrAuth, "please run /login"},
	{adapter.ErrAuth, "not logged in"},
}

// phraseEvent sorts one piece of text by one table. Text that names
// nothing in the table is not an event.
func phraseEvent(table []errorPhrase, text string) (adapter.Event, bool) {
	lower := strings.ToLower(text)
	for _, m := range table {
		if strings.Contains(lower, m.phrase) {
			ev := adapter.Event{Kind: adapter.EvError, ErrorKind: m.kind, Text: text}
			if m.kind == adapter.ErrLimit {
				ev.LimitUntil = limitReset(text, now())
			}
			return ev, true
		}
	}
	return adapter.Event{}, false
}

// now is the clock limitReset reads; tests replace it.
var now = time.Now

// resetRe finds the CLI's reset note: "resets 6:40pm (Europe/Moscow)".
var resetRe = regexp.MustCompile(`(?i)resets\s+(\d{1,2})(?::(\d{2}))?\s*([ap])m\s*\(([^)]+)\)`)

// limitReset reads the reset time out of a limit message, as an RFC3339
// instant: the next occurrence of that wall-clock time in the named zone.
// Best effort — anything that does not parse is "" (unknown), never an
// error.
func limitReset(text string, at time.Time) string {
	m := resetRe.FindStringSubmatch(text)
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

// errorFromText sorts an error result's text by the broad table.
func errorFromText(text string) (adapter.Event, bool) {
	return phraseEvent(errorPhrases, text)
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

// Result usage and total_cost_usd are the CLI result envelope's fields:
// https://code.claude.com/docs/en/agent-sdk/cost-tracking
// Only the result is counted, never its per-message usage as well.
func resultUsage(lj lineJSON) *adapter.Usage {
	u := adapter.ParseUsage(lj.Usage, "input_tokens", "output_tokens", "cache_read_input_tokens", "", "")
	cost := adapter.ParseUsage(json.RawMessage(`{"cost":`+string(lj.Cost)+`}`), "", "", "", "", "cost")
	if u == nil {
		u = cost
	} else {
		u.Add(cost)
	}
	// Anthropic's input_tokens excludes cache reads and cache creation.
	creation := adapter.ParseUsage(lj.Usage, "cache_creation_input_tokens", "", "", "", "")
	if creation != nil {
		if u == nil {
			u = &adapter.Usage{}
		}
		u.Add(creation)
	}
	if u != nil && u.CachedInputTokens != nil {
		n := *u.CachedInputTokens
		u.Add(&adapter.Usage{InputTokens: &n})
	}
	// num_turns is the CLI's own count of model requests; the assistant
	// lines split one request into a line per content block.
	var turns int64
	if len(lj.NumTurns) > 0 && json.Unmarshal(lj.NumTurns, &turns) == nil && turns >= 0 {
		if u == nil {
			u = &adapter.Usage{}
		}
		u.Add(adapter.Calls(turns))
	}
	return u
}
