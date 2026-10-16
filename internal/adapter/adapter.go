package adapter

import (
	"os/exec"
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
}

// Job is what an adapter needs to build one headless run (§5.2): the
// composed prompt and the model to run on. The prompt is the engine's
// product — a fixed, versioned template plus verbatim data (§1.4) — and
// the adapter passes it through untouched.
type Job struct {
	Prompt string
	Model  string
	// Effort is the reasoning-effort level for the run, one of the harness's
	// accepted levels (ValidateEffort); empty adds nothing. An adapter whose
	// CLI has no effort control refuses a non-empty one.
	Effort string
	// MCPConfig names the harness's MCP config for the run: a config
	// file's path, offered to the CLI through its own flag. It is where
	// the `brigada mcp` bridge (§12.2) will hand the foreman's harness
	// brigada's tool server; empty — every job today — adds nothing.
	MCPConfig string
	// ReadOnlyRepo is the foreman checkout the harness may inspect.
	// Claude adds it to its read scope; Codex reads the path from the prompt.
	ReadOnlyRepo string
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

// EventKind is the shape of one progress event (§5.6): what the runner
// tracks for heartbeats and, later, stall detection.
type EventKind string

const (
	// EvToolStart is a tool call beginning; one in flight suspends the
	// stall clock (§5.6).
	EvToolStart EventKind = "tool_start"
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
	// place, carrying the previous attempt's context in its prompt (§5.6,
	// §14) — a session cannot be rebuilt from its id alone.
	ErrResumeFailed ErrorKind = "resume_failed"
	ErrOther        ErrorKind = "other"
)

// ResumeCapableHarnesses is the set of harnesses a foreman may run on
// (design §4.1): the foreman's turns wake headless and resume the
// project's session, so choosing a foreman refuses a harness whose CLI
// cannot resume headless and carry the foreman's MCP bridge. This is
// the one place that set lives; the engine consults it at setup.
var ResumeCapableHarnesses = map[string]bool{
	"claude": true, // the Claude Code CLI: `claude -p --resume <session>` (§5.2)
	"codex":  true, // Codex CLI: `codex exec resume <id>`; -c carries the foreman's MCP bridge
	"fake":   true, // the scripted test adapter
	// kimi is absent although its CLI resumes headless (`kimi --session
	// <id>`): this set gates the foreman's harness, whose turns carry the
	// `brigada mcp` bridge (§12.2), and kimi has no MCP-config flag to
	// offer it — its adapter refuses such jobs outright. A foreman cannot
	// run on kimi until its CLI can carry the bridge.
	// opencode is also worker-only although `opencode run -s <id>`
	// resumes headless: its run path has no MCP-config flag to carry
	// the foreman bridge. Like kimi, its adapter refuses MCP-config jobs.
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
	Kind EventKind
	Time time.Time // when the adapter parsed it; the runner's clock
	Text string
	// SessionID may accompany any kind when the CLI puts its id in
	// every envelope (including an error before a session-start event).
	SessionID string
	ErrorKind ErrorKind
	// LimitUntil is a limit error's reset time, RFC3339, when the CLI's
	// text carried one machine-readably; empty when unknown (§4.4).
	LimitUntil string
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
