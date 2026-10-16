// Package codex drives the Codex CLI headless (design §5.2): one run per
// job, `codex exec --json`, resumed by thread id when the job carries
// one. The CLI's own login state — its subscription tokens, its
// configuration — is the ambient environment's business; brigada adds
// nothing of its own to it and takes the runner's share back out.
package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Adapter drives the codex CLI (0.162.x) in its headless exec mode. The
// zero value runs `codex` from PATH; the fields carry what the agent's
// configuration passes through.
//
// It can also be a foreman harness: codex's config overrides carry the
// runner-written MCP bridge into `codex exec`.
type Adapter struct {
	// Binary is the CLI to run; "codex" when empty.
	Binary string

	// ExtraArgs are further flags the agent's configuration passes
	// through, appended verbatim after the adapter's own, before the
	// prompt. Empty adds nothing.
	ExtraArgs []string
}

var _ adapter.Adapter = (*Adapter)(nil)
var _ adapter.Interactive = (*Adapter)(nil)

// DefaultModel is the agent's name for the CLI's own default model: the
// one the CLI picks when no -m names another. The runner offers it when
// the agent's configuration sets no --model, and the adapter's answer
// to it is no -m flag at all.
const DefaultModel = "default"

// Name implements adapter.Adapter: the harness this adapter drives,
// which the runner's advertisement pairs it with (§5.1).
func (a *Adapter) Name() string { return "codex" }

// Command implements adapter.Adapter: the fresh or resume invocation of
// the CLI, running in workdir:
//
// A worker runs `codex exec --json -s workspace-write`; a foreman runs
// `codex exec --json -s read-only` with MCP and approval overrides below.
// The worker's sandbox lets it edit inside its worktree without approval,
// while the CLI's default keeps the network off. No --skip-git-repo-check:
// a worker's directory is a worktree, which is a git repo. The prompt is
// the positional after `--`, argv
// and no shell, verbatim — the engine's product (§1.4), not the
// adapter's to edit, and a prompt that starts with a dash stays one.
// For a foreman, use the read-only sandbox and independent approval settings.
// --approve-for-me selects workspace-write and conflicts with -s. Automatic
// review remains available to MCP; sandbox and permission escalations are
// rejected, so policy alone cannot turn the reading checkout writable.
// The environment is the runner's ambient set minus brigada's own share
// of it (§5.1): every BRIGADA_-prefixed variable is dropped.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	binary := a.Binary
	if binary == "" {
		binary = "codex"
	}
	args := []string{"exec", "--json"}
	if job.MCPConfig == "" {
		args = append(args, "-s", "workspace-write")
	} else {
		configArgs, err := mcpConfigArgs(job.MCPConfig)
		if err != nil {
			return nil, err
		}
		args = append(args, foremanPermissions()...)
		args = append(args, configArgs...)
		// A foreman turn runs in the runner's scratch directory, which
		// is deliberately not a git repo; codex refuses such directories
		// without this flag. It skips only the repo check, nothing else.
		args = append(args, "--skip-git-repo-check")
	}
	if job.Model != "" && job.Model != DefaultModel {
		args = append(args, "-m", job.Model)
	}
	effortArgs, err := effortArgs(job.Effort)
	if err != nil {
		return nil, err
	}
	args = append(args, effortArgs...)
	args = append(args, a.ExtraArgs...)
	if resumeSessionID != "" {
		args = append(args, "resume", resumeSessionID)
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

// foremanPermissions keeps MCP approval review separate from filesystem
// approval. CLI 0.162.1 accepts these settings; the --approve-for-me alias
// would instead force workspace-write. No sandbox escape is auto-approved.
func foremanPermissions() []string {
	return []string{
		"-s", "read-only",
		"-c", `approvals_reviewer="auto_review"`,
		"-c", `approval_policy={granular={sandbox_approval=false,rules=false,mcp_elicitations=true,request_permissions=false,skill_approval=false}}`,
	}
}

// effortArgs is the fixed -c override that carries the job's effort: only
// the model_reasoning_effort key, its value a quoted TOML string. Empty
// effort adds nothing; a level outside brigada's accepted set is refused.
func effortArgs(effort string) ([]string, error) {
	if err := adapter.ValidateEffort("codex", effort); err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	if effort == "" {
		return nil, nil
	}
	return []string{"-c", "model_reasoning_effort=" + strconv.Quote(effort)}, nil
}

type mcpConfig struct {
	Servers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Command *string            `json:"command"`
	Args    *[]string          `json:"args"`
	Env     *map[string]string `json:"env"`
}

func mcpConfigArgs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("codex: read MCP config %q: %w", path, err)
	}
	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("codex: decode MCP config %q: %w", path, err)
	}
	if len(cfg.Servers) != 1 {
		return nil, fmt.Errorf("codex: MCP config must name exactly one server, got %d", len(cfg.Servers))
	}
	var name string
	var srv mcpServer
	for name, srv = range cfg.Servers {
		break
	}
	if name == "" || srv.Command == nil || srv.Args == nil || srv.Env == nil || *srv.Command == "" {
		return nil, errors.New("codex: MCP config server is missing command, args, or env")
	}
	quotedArgs := make([]string, len(*srv.Args))
	for i, value := range *srv.Args {
		quotedArgs[i] = strconv.Quote(value)
	}
	keys := make([]string, 0, len(*srv.Env))
	for key := range *srv.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	quotedEnv := make([]string, 0, len(keys))
	for _, key := range keys {
		quotedEnv = append(quotedEnv, key+"="+strconv.Quote((*srv.Env)[key]))
	}
	prefix := "mcp_servers." + name
	return []string{
		"-c", prefix + ".command=" + strconv.Quote(*srv.Command),
		"-c", prefix + ".args=[" + strings.Join(quotedArgs, ",") + "]",
		"-c", prefix + ".env={" + strings.Join(quotedEnv, ",") + "}",
	}, nil
}

// AttachCommand implements adapter.Interactive: Codex's own terminal UI is
// started directly, with the opening prompt as its positional argument. A
// saved exec thread is resumed with `codex resume <thread-id>`; without one,
// plain `codex` starts a fresh conversation. The client-generated session id
// is intentionally unused: Codex does not expose Claude's fork/session-id
// pair. On detach the client nevertheless writes its minted id back as the
// key's session, so a later headless or interactive resume targets a thread
// Codex never created and falls back to a fresh run; this is the documented
// limitation until Codex exposes a fork/session-id facility.
//
// A foreman reading checkout gets the same read-only sandbox and separate
// approval settings as a headless turn, without --json or approval bypass.
// Codex has no equivalent of Claude's statusLine, so StatusLine is ignored.
// MCPConfig is likewise not translated: Codex has no adapter-supported MCP
// config flag; the terminal remains the CLI's own interactive session.
func (a *Adapter) AttachCommand(job adapter.Job, workdir, resumeSessionID, _ string) (*exec.Cmd, error) {
	binary := a.Binary
	if binary == "" {
		binary = "codex"
	}
	args := make([]string, 0, 6+len(a.ExtraArgs))
	if job.ReadOnlyRepo != "" {
		args = append(args, foremanPermissions()...)
	}
	if resumeSessionID != "" {
		args = append(args, "resume", resumeSessionID)
	}
	if job.Model != "" && job.Model != DefaultModel {
		args = append(args, "-m", job.Model)
	}
	effortArgs, err := effortArgs(job.Effort)
	if err != nil {
		return nil, err
	}
	args = append(args, effortArgs...)
	args = append(args, a.ExtraArgs...)
	if job.Prompt != "" {
		args = append(args, job.Prompt)
	}
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

// lineJSON is one line of the CLI's --json output: one event per line.
// Only the fields the adapter reads are declared.
type lineJSON struct {
	Type     string    `json:"type"`
	ThreadID string    `json:"thread_id"`
	Message  string    `json:"message"`
	Item     *itemJSON `json:"item"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// itemJSON is the item of an item.started / item.completed event. The
// types this adapter reads: agent_message, command_execution, mcp_tool_call
// and error;
// every other type (reasoning, file changes, …) is ignored.
type itemJSON struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Message string          `json:"message"`
	Command string          `json:"command"`
	Server  string          `json:"server"`
	Tool    string          `json:"tool"`
	Status  string          `json:"status"`
	Error   json.RawMessage `json:"error"`
}

// maxToolText bounds a command's text in a tool event: a heredoc can
// run to kilobytes, and the event is a progress line.
const maxToolText = 200

// firstLine is the command's first line, cut to maxToolText runes.
func firstLine(command string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(command), "\n")
	if r := []rune(line); len(r) > maxToolText {
		line = string(r[:maxToolText])
	}
	return line
}

// ParseEvent implements adapter.Adapter: one line of the CLI's --json
// stream turned into one progress event.
//
//	thread.started                    → session id (the thread id)
//	item.completed agent_message      → assistant message
//	item.started command_execution    → tool start (the command's first line)
//	item.completed command_execution  → tool end (the same text)
//	item.started mcp_tool_call       → tool start (server/tool)
//	item.completed mcp_tool_call     → tool end, naming the error when the call failed
//	error, turn.failed, error item    → an error event, sorted by its text
//	turn.started, turn.completed      → ignored: the usage counters it
//	                                    carries are no progress
//
// Unknown event and item types and malformed lines are ignored, never
// fatal, so a stray line cannot fail an otherwise good run.
func (a *Adapter) ParseEvent(line []byte) (adapter.Event, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return adapter.Event{}, false
	}
	var lj lineJSON
	if err := json.Unmarshal(trimmed, &lj); err != nil {
		// Not the CLI's JSON: noise, or a failure printed as prose.
		// Prose sorts by the strict table only.
		return phraseEvent(stderrPhrases, string(trimmed))
	}
	switch lj.Type {
	case "thread.started":
		if lj.ThreadID == "" {
			return adapter.Event{}, false
		}
		return adapter.Event{Kind: adapter.EvSession, SessionID: lj.ThreadID}, true
	case "item.started", "item.completed":
		return itemEvent(lj)
	case "error":
		return errorEvent(lj.Message)
	case "turn.failed":
		if lj.Error == nil {
			return errorEvent("")
		}
		return errorEvent(lj.Error.Message)
	default:
		return adapter.Event{}, false
	}
}

// itemEvent reads one item event.
func itemEvent(lj lineJSON) (adapter.Event, bool) {
	if lj.Item == nil {
		return adapter.Event{}, false
	}
	started := lj.Type == "item.started"
	switch lj.Item.Type {
	case "agent_message":
		if started || lj.Item.Text == "" {
			return adapter.Event{}, false
		}
		return adapter.Event{Kind: adapter.EvMessage, Text: lj.Item.Text}, true
	case "command_execution":
		kind := adapter.EvToolEnd
		if started {
			kind = adapter.EvToolStart
		}
		return adapter.Event{Kind: kind, Text: firstLine(lj.Item.Command)}, true
	case "mcp_tool_call":
		text := firstLine(lj.Item.Server + "/" + lj.Item.Tool)
		if started {
			return adapter.Event{Kind: adapter.EvToolStart, Text: text}, true
		}
		// A failed call is the tool's outcome, not the CLI's: the model
		// reads the failure and answers around it, and the run may still
		// end clean (turn.completed). It maps to the tool's end with the
		// error named, never to the stream's fatal error — only
		// turn.failed and the CLI's own error events are that (the first
		// live codex foreman turn was failed by a null-error misread
		// here, 2026-10-15).
		if lj.Item.Status != "" && lj.Item.Status != "completed" && lj.Item.Status != "success" {
			if why := mcpErrorText(lj.Item); why != "" {
				text = firstLine(text + ": " + why)
			}
		}
		return adapter.Event{Kind: adapter.EvToolEnd, Text: text}, true
	case "error":
		if started {
			return adapter.Event{}, false
		}
		return errorEvent(lj.Item.Message)
	default:
		return adapter.Event{}, false
	}
}

func mcpErrorText(item *itemJSON) string {
	// json.RawMessage keeps JSON null as its four bytes; that is no
	// error text at all.
	if raw := strings.TrimSpace(string(item.Error)); raw != "" && raw != "null" {
		var message string
		if json.Unmarshal(item.Error, &message) == nil && message != "" {
			return message
		}
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(item.Error, &detail) == nil && detail.Message != "" {
			return detail.Message
		}
		return string(item.Error)
	}
	if item.Message != "" {
		return item.Message
	}
	return item.Status
}

// ParseErrEvent implements adapter.Adapter: one line of the CLI's
// stderr, the strictest stream the adapter reads. Stderr carries the
// CLI's chatter — retry notes, warnings, MCP noise — so prose there
// sorts by the strict table only: a failure message named in full, or
// nothing, and noise cannot fail a run that ended clean. The CLI's JSON
// there is an error event, sorted as on stdout.
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return adapter.Event{}, false
	}
	var lj lineJSON
	if err := json.Unmarshal(trimmed, &lj); err == nil && lj.Type != "" {
		switch lj.Type {
		case "error", "turn.failed":
			return a.ParseEvent(trimmed)
		}
		return adapter.Event{}, false
	}
	return phraseEvent(stderrPhrases, string(trimmed))
}

// errorPhrase is one row of a phrase table: what a failure's text looks
// like, case-insensitively, and the kind each sorts into. First match
// wins; extend the table, not the code.
type errorPhrase struct {
	kind   adapter.ErrorKind
	phrase string
}

// errorPhrases is the broad table a JSON error's text sorts by: an
// error event is the CLI's own verdict, so its text can afford loose
// phrases.
var errorPhrases = []errorPhrase{
	// A resume the CLI cannot do: the thread is gone (§5.6, §14), and
	// the answer is a fresh run in place, not another resume.
	{adapter.ErrResumeFailed, "no saved session found"},
	{adapter.ErrResumeFailed, "session not found"},
	{adapter.ErrResumeFailed, "no session found"},
	// The account's quota window (§8): the account cools, the job moves.
	{adapter.ErrLimit, "usage limit"},
	{adapter.ErrLimit, "rate limit"},
	// The CLI's login (§8): the agent is broken until a probe passes.
	{adapter.ErrAuth, "log in"},
	{adapter.ErrAuth, "unauthorized"},
	{adapter.ErrAuth, "invalid api key"},
}

// stderrPhrases is the strict table prose sorts by: only the failure
// messages the CLI itself prints, named in full. The loose "rate limit"
// and "unauthorized" earn no place here — a "rate limited, retrying"
// note or an MCP server's 401 must not fail a run that ended clean.
var stderrPhrases = []errorPhrase{
	{adapter.ErrResumeFailed, "no saved session found"},
	{adapter.ErrResumeFailed, "session not found"},
	{adapter.ErrLimit, "hit your usage limit"},
	{adapter.ErrLimit, "usage limit reached"},
	{adapter.ErrAuth, "not logged in"},
	{adapter.ErrAuth, "please log in"},
	{adapter.ErrAuth, "invalid api key"},
}

// errorEvent sorts a JSON error's text by the broad table; text that
// names nothing is still a failure, of kind other.
func errorEvent(text string) (adapter.Event, bool) {
	if ev, ok := phraseEvent(errorPhrases, text); ok {
		return ev, true
	}
	return adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: text}, true
}

// phraseEvent sorts one piece of text by one table. Text that names
// nothing in the table is not an event.
func phraseEvent(table []errorPhrase, text string) (adapter.Event, bool) {
	lower := strings.ToLower(text)
	for _, m := range table {
		if strings.Contains(lower, m.phrase) {
			return adapter.Event{Kind: adapter.EvError, ErrorKind: m.kind, Text: text}, true
		}
	}
	return adapter.Event{}, false
}

// ResultText implements adapter.Adapter: the run's final answer is the
// last agent_message of the run (§5.4); a run that never spoke has none.
func (a *Adapter) ResultText(events []adapter.Event) string {
	result := ""
	for _, e := range events {
		if e.Kind == adapter.EvMessage && e.Text != "" {
			result = e.Text
		}
	}
	return result
}
