package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Adapter is what one CLI adapter provides (design §5.2): it builds the
// headless command for a job, parses the CLI's event stream into
// progress events, and finds the final message and the session id in
// what the CLI printed. An adapter never modifies the CLI and never
// holds credentials itself.
type Adapter interface {
	// Name is the adapter's identifier, one per CLI ("claude", "kimi",
	// "fake"): the name a runner's advertisement pairs it with (§5.1)
	// and the agents table keys on (§11).
	Name() string

	// Command builds the headless command for job, to run in workdir.
	// resumeSessionID is empty for a fresh run; otherwise the command
	// resumes that harness session with its context kept (§5.3).
	//
	// The environment is the adapter's to build, under one policy: the
	// worker's own login state is its business — the ambient
	// environment, the vendor CLI's tokens and configuration, are what
	// it needs to run (§5.3) — but the adapter
	// must not add brigada's own secrets to it. The runner enforces the
	// line from its side: whatever variables its git credentials ride
	// (Config.GitEnv) are stripped from the command's environment
	// before it starts, whether the adapter put them there or they came
	// from the ambient set.
	Command(job Job, workdir, resumeSessionID string) (*exec.Cmd, error)

	// ParseEvent parses one line of the CLI's JSON event stream. false
	// means the line is not an event the runner tracks — a banner, a
	// blank line, anything the CLI prints that is not one of its
	// events. Only the CLI's own error event is an error: a stray line
	// on its stdout must not fail an otherwise good run.
	ParseEvent(line []byte) (Event, bool)

	// ParseErrEvent parses one line of the CLI's stderr, which rides
	// the same event pipeline as stdout: some failures print only there
	// — a resume-not-found before any JSON, a limit or an auth message
	// — and must become error events all the same. false means the line
	// carries nothing to act on, like a progress note or a warning.
	ParseErrEvent(line []byte) (Event, bool)

	// ResultText returns the worker's final message from the events it
	// parsed: the text the result contract (§5.4) is read from.
	ResultText(events []Event) string

	// ProbeCommand builds the cheap probe of the CLI for the
	// broken-agent recovery (§8 "Health"): a command bound to ctx — a
	// version or status check — that exits 0 when the CLI answers. The
	// runner runs it against an agent broken by repeated failures with a
	// short deadline; a passing probe clears the agent, a failing one
	// backs the next probe off. The command is built with ctx so the
	// deadline kills it: a hung CLI reads as a failing probe, never a
	// stuck runner.
	ProbeCommand(ctx context.Context) *exec.Cmd
}

// Job is what an adapter needs to build one headless run (§5.2): the
// composed prompt and the model to run on. The prompt is the engine's
// product — a fixed, versioned template plus verbatim data (§1.4) — and
// the adapter passes it through untouched.
type Job struct {
	Prompt string
	Model  string
	// Review selects the harness's read-only review policy, where supported.
	Review bool
	// Effort is the reasoning-effort level for the run, one of the harness's
	// accepted levels (ValidateEffort); empty adds nothing. An adapter whose
	// CLI has no effort control refuses a non-empty one.
	Effort string
	// MCPConfig names the harness's MCP config for the run: a config
	// file's path, offered to the CLI through its own flag. It is where
	// the `brigada mcp` bridge (§12.2) will hand the foreman's harness
	// brigada's tool server; empty — every job today — adds nothing.
	MCPConfig string
	// ReadDirs names absolute directories outside the worktree the
	// worker must be able to read but never write: the extra
	// repositories' checkouts (§5.8). The adapter opens them to the
	// worker as its CLI allows, and the checkouts' read-only files
	// refuse writes all the same; codex's and dsh's sandboxes already
	// allow reads outside the workspace, and kimi has no directory
	// flag, so those adapters ignore it. Empty adds nothing.
	ReadDirs []string
	// Tools is the worker's tool scope in Claude's spelling
	// (`Bash(go test:*)`), for a harness that maps it onto its own
	// permission rules; nil is the Go default scope. Claude takes its
	// scope from its adapter's config instead and ignores this.
	Tools []string
	// StatusLine is a shell command whose one line of output an
	// interactive session shows in its status bar, re-run every few
	// seconds (§10): the attach sets it to `brigada status --line`, so
	// the user sees what the workers are doing while they chat with the
	// foreman. Only AttachCommand reads it, and an adapter whose CLI has
	// no status bar ignores it; empty adds nothing.
	StatusLine string
}

// Interactive is the optional adapter interface for the attach (§4.2
// item 1): an adapter whose CLI can carry an interactive terminal
// session — the user's own chat with the foreman, not a headless run —
// implements it beside Adapter. The client asserts it when it attaches
// and builds the command through it; a harness whose adapter does not
// implement it cannot be attached to, and the client says so rather
// than falling back to a headless run, which is no chat at all.
type Interactive interface {
	// AttachCommand builds the interactive command for job, to run in
	// workdir on the caller's terminal: the adapter wires none of the
	// process's streams — stdin is the user's, and the caller connects
	// it. The opening prompt (job.Prompt) reaches the CLI the way it
	// takes one interactively; the MCP config and the model ride as in
	// Command. resumeSessionID is the key's current harness session,
	// empty when the foreman has never run; newSessionID is the id the
	// client generated for the session this chat forks into — the CLI
	// names it itself, so the client knows what to write back on detach
	// (§4.2) without reading the CLI's own files.
	AttachCommand(job Job, workdir, resumeSessionID, newSessionID string) (*exec.Cmd, error)
}

// SessionWriteback is the optional adapter interface for a CLI whose
// interactive resume keeps its own session id — one the client cannot
// mint for it the way claude's --session-id takes a caller's id: the
// release of the attach writes back the id this returns instead of the
// id the client minted. resumeSessionID is the id the chat resumed —
// returned unchanged, for the CLI continues under it — and "" means the
// chat started fresh, its id discovered from the CLI's own session list
// in workdir, which is why the caller runs this after the CLI exits. An
// error is the caller's to warn about and answer with the minted id,
// which the CLI does not know: its next headless turn then refuses the
// resume and starts fresh in place (§5.6).
type SessionWriteback interface {
	// WritebackSessionID names the session id an ended attach leaves the
	// key on, bound to ctx for the discovery a fresh chat needs.
	WritebackSessionID(ctx context.Context, workdir, resumeSessionID string) (string, error)
}

// EventKind is the shape of one progress event (§5.6): what the runner
// tracks for heartbeats and, later, stall detection.
type EventKind string

const (
	// EvToolStart is a tool call beginning; one in flight suspends the
	// stall clock (§5.6).
	EvToolStart EventKind = "tool_start"
	// EvUsage reports counters without advancing the progress clock.
	EvUsage EventKind = "usage"
	// EvToolEnd is a tool call finishing.
	EvToolEnd EventKind = "tool_end"
	// EvMessage is an assistant message.
	EvMessage EventKind = "message"
	// EvSession carries the harness's own session id, written back on
	// complete so the next session-mode job resumes it (§4.2).
	EvSession EventKind = "session"
	// EvError is the CLI reporting a failure whose kind sorts what the
	// engine does with it (§8: quota, auth, the rest).
	EvError EventKind = "error"
	// EvWarning is a CLI notice that is not a failure: the run goes on,
	// and the runner does not take it for the run's error.
	EvWarning EventKind = "warning"
)

// ErrorKind sorts an error event: a quota limit hit (the account cools
// and the job moves to another agent), an authentication failure (the
// agent is broken until a probe passes), a resume that could not happen
// (the session is gone or forked; §5.6 restarts fresh in place), and
// everything else.
type ErrorKind string

const (
	ErrLimit ErrorKind = "limit"
	ErrAuth  ErrorKind = "auth"
	// ErrResumeFailed is a resume that could not happen: the session the
	// command was told to resume is gone or its conversation cannot be
	// continued. What answers it is not another resume but a fresh run in
	// place, carrying the previous attempt's context in its prompt (§5.6) — a session cannot be rebuilt from its id alone.
	ErrResumeFailed ErrorKind = "resume_failed"
	ErrOther        ErrorKind = "other"
	// ErrInfra is the CLI failing to reach its provider at all: the
	// connection or the name lookup failed before the run produced
	// anything. The environment's failure, never the agent's: the job is
	// retried as a transient error and no health strike is charged.
	ErrInfra ErrorKind = "infra"
	// ErrModel is the provider rejecting the model itself: the account
	// has no access to it, or it does not exist (a 400
	// invalid_request_error). The agent cannot work until the user
	// fixes its model or the account, so it stays out like an auth break
	// and no probe clears it.
	ErrModel ErrorKind = "model"
)

// modelRejectedRe matches a provider refusing the model by name: "The
// 'x' model is not supported when using Codex with a ChatGPT account",
// "The model `x` does not exist", "Model not found: x", "There's an
// issue with the selected model". The word "model" must come before the
// refusal on the same sentence, so a session or tool that is "not found"
// is not taken for it.
var modelRejectedRe = regexp.MustCompile(`(?i)\bmodels?\b[^.\n]{0,100}?(not exist|is not supported|not supported|not found|is unsupported|is not available)|invalid model|issue with the selected model`)

// IsModelRejectedText reports whether a failure's text says the provider
// rejected the model: unsupported for the account, or nonexistent.
func IsModelRejectedText(text string) bool {
	// "Model metadata for x not found" is a codex notice that the run
	// goes on past, not a refusal.
	if strings.Contains(strings.ToLower(text), "metadata") {
		return false
	}
	return modelRejectedRe.MatchString(text)
}

// infraPhrases are the words a CLI's connection or DNS failure carries,
// lowercase: what a network that is down or not yet up looks like.
var infraPhrases = []string{
	"cannot connect to api",
	"could not resolve host",
	"connection refused",
	"connection reset",
	"getaddrinfo",
}

// IsInfraText reports whether a failure's text names a connection or DNS
// failure. The words alone prove nothing about when they were said: the
// runner keeps the infra kind only for a run that produced no output
// before it.
func IsInfraText(text string) bool {
	lower := strings.ToLower(text)
	for _, p := range infraPhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// ResumeCapableHarnesses is the set of harnesses a foreman may run on
// (design §4.1): the foreman's turns wake headless and resume the
// project's session, so choosing a foreman refuses a harness whose CLI
// cannot resume headless and carry the foreman's MCP bridge. This is
// the one place that set lives; the engine consults it at setup.
var ResumeCapableHarnesses = map[string]bool{
	"claude":   true, // the Claude Code CLI: `claude -p --resume <session>` (§5.2)
	"codex":    true, // Codex CLI: `codex exec resume <id>`; -c carries the foreman's MCP bridge
	"opencode": true, // OpenCode CLI: `opencode run -s <id>` resumes headless; the
	// bridge rides an adapter-written config in a directory OPENCODE_CONFIG_DIR
	// names, the attach the TUI's own `-s <id>` (its session-id write-back
	// through SessionWriteback).
	"fake": true, // the scripted test adapter
	// kimi is absent although its CLI resumes headless (`kimi --session
	// <id>`): this set gates the foreman's harness, whose turns carry the
	// `brigada mcp` bridge (§12.2), and kimi has no MCP-config flag to
	// offer it — its adapter refuses such jobs outright. A foreman cannot
	// run on kimi until its CLI can carry the bridge.
	// dsh is worker-only too: `dsh --session-id <id>` resumes headless,
	// but no MCP-config flag is known to carry the bridge, so its adapter
	// refuses MCP-config jobs.
}

// CredentialEnv names the environment variable a harness's credential
// comes from, or "" when the harness logs in some other way (a stored
// login, an OAuth session). A runner started without the variable cannot
// authenticate that harness, which the needs-you line for an auth break
// says.
func CredentialEnv(harness string) string {
	if harness == "dsh" {
		return "DEEPSEEK_API_KEY"
	}
	return ""
}

// ResumeCapable reports whether harness's CLI can resume a session
// headless (§4.1), which the foreman's harness must.
func ResumeCapable(harness string) bool {
	return ResumeCapableHarnesses[harness]
}

// Event is one parsed line of the CLI's event stream. Which fields
// carry data depends on Kind: tool calls fill Text (the tool name), a
// message fills Text, a session id fills SessionID, an error fills
// ErrorKind and Text.
type Event struct {
	Usage *Usage
	Kind  EventKind
	Time  time.Time // when the adapter parsed it; the runner's clock
	Text  string
	// Args is a tool call's arguments as the CLI's stream carried them,
	// empty for a CLI that prints none (or a kind that has none): with
	// Text it names the call, so the runner's loop watch (§5.6) tells
	// two calls of one tool apart.
	Args string
	// SessionID may accompany any kind when the CLI puts its id in
	// every envelope (including an error before a session-start event).
	SessionID string
	ErrorKind ErrorKind
	// LimitUntil is a limit error's reset time, RFC3339, when the CLI's
	// text carried one machine-readably; empty when unknown (§4.4).
	LimitUntil string
	// Final marks an error event as the CLI's closing verdict on the run
	// (codex's turn.failed): it names the failure over any earlier error
	// event of the same stream.
	Final bool
	// Notice marks an error event that is only a warning while the
	// turn has not started (codex's startup notices): the runner, which
	// tracks each job's turn through TurnMarker, downgrades it to a
	// warning until then.
	Notice bool
}

// TurnMarker is the optional adapter interface for a CLI whose stream
// opens the turn with a line of its own. The adapter itself is shared by
// concurrent jobs and keeps no stream state; the runner asks it of each
// stdout line and keeps the job's turn.
type TurnMarker interface {
	// StartsTurn reports whether the stdout line opens the turn.
	StartsTurn(line []byte) bool
}

// IsProgress reports whether the event is progress in the sense of
// §5.6: a tool call starting or ending, or a message. Sessions and
// errors are not progress.
func (e Event) IsProgress() bool {
	return e.Kind == EvToolStart || e.Kind == EvToolEnd || e.Kind == EvMessage
}

// ProgressEvents renders the last few progress events for a heartbeat:
// one line each, newest last, so the engine's record of a run names
// what the worker was doing, not just when.
func ProgressEvents(events []Event) []string {
	var out []string
	for _, e := range events {
		if !e.IsProgress() {
			continue
		}
		line := string(e.Kind)
		if e.Text != "" {
			line += " " + e.Text
		}
		out = append(out, line)
	}
	return out
}

// ToolArgs renders a tool call's arguments for an Event's Args: the
// raw JSON the CLI's stream carried, compacted so two identical calls
// read identical whatever whitespace the stream put between them.
// Empty input, or input that is not JSON, renders empty — no arguments
// identifiable, nothing to compare.
func ToolArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return ""
	}
	return buf.String()
}
