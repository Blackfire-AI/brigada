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
	// it needs to run and to push its branch (§5.3) — but the adapter
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
// agent is broken until a probe passes), and everything else.
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

// Event is one parsed line of the CLI's event stream. Which fields
// carry data depends on Kind: tool calls fill Text (the tool name), a
// message fills Text, a session id fills SessionID, an error fills
// ErrorKind and Text.
type Event struct {
	Kind      EventKind
	Time      time.Time // when the adapter parsed it; the runner's clock
	Text      string
	SessionID string
	ErrorKind ErrorKind
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
