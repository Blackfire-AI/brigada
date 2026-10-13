package notify

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// execTimeout caps one tool invocation when the caller's context names
// no sooner deadline: a notifier that hangs must not stall the delivery
// loop or the engine's idle exit.
const execTimeout = 10 * time.Second

// Osascript is the macOS notifier: it asks osascript to display a
// notification. Title and body reach the script only as the run
// handler's argv — after `--`, as whole arguments — never interpolated
// into the script source, so no text in a plan or task title can
// become AppleScript (TestOsascriptArgsGolden in notify_test.go pins
// the argv golden).
type Osascript struct {
	// Tool is the osascript path; empty keeps "osascript" (PATH lookup).
	Tool string
}

// Name is the log name: the tool it shells out to.
func (o Osascript) Name() string { return "osascript" }

// Notify displays one banner through osascript. The attempt is bounded
// by the caller's context, capped at execTimeout — a shutdown never
// waits on the exec past the hook's grace.
func (o Osascript) Notify(ctx context.Context, title, body string) error {
	tool := o.Tool
	if tool == "" {
		tool = "osascript"
	}
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, osascriptArgs(title, body)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, oneLine(string(out), 200))
	}
	return nil
}

// osascriptArgs builds the argv for one banner: the script is fixed
// source — three `-e` lines — and the title and body follow `--` as
// the run handler's arguments. Whatever the title and body contain —
// quotes, newlines, AppleScript-looking text — they stay data.
func osascriptArgs(title, body string) []string {
	return []string{
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"--", title, body,
	}
}

// NotifySend is the Linux notifier: it calls notify-send, which takes
// the title and body as plain arguments — no shell, no interpolation.
type NotifySend struct {
	// Tool is the notify-send path; empty keeps "notify-send" (PATH lookup).
	Tool string
}

// Name is the log name: the tool it shells out to.
func (n NotifySend) Name() string { return "notify-send" }

// Notify displays one banner through notify-send. The attempt is
// bounded by the caller's context, capped at execTimeout.
func (n NotifySend) Notify(ctx context.Context, title, body string) error {
	tool := n.Tool
	if tool == "" {
		tool = "notify-send"
	}
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, notifySendArgs(title, body)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("notify-send: %w: %s", err, oneLine(string(out), 200))
	}
	return nil
}

// notifySendArgs builds the argv for one banner: the title and body
// are whole arguments, never joined into a command line.
func notifySendArgs(title, body string) []string {
	return []string{title, body}
}
