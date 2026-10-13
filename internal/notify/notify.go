// Package notify is §10's local notifications: the OS notifier, the
// mapping from engine event kinds to a short title and one-line body,
// and the delivery loop that walks the event log once per event.
//
// The package is deliberately generic — nothing here knows how events
// are produced, only how the log's rows read. Remote channels (webhook,
// Telegram) are backlog; the Notifier interface is the seam they'd plug
// into.
package notify

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// Notifier delivers one local notification (§10). Title is the short
// name the OS banner shows bold; body is one line of detail. ctx bounds
// the attempt: shutdown and the delivery loop's per-pass deadline cancel
// it, and implementations cap it regardless, so a hung banner tool never
// stalls the engine's tick or its idle exit. An error means the banner
// did not reach the OS; the delivery loop logs it and goes on, so one
// broken notification never stalls or repeats forever.
type Notifier interface {
	Notify(ctx context.Context, title, body string) error
}

// The config file's notify values (lifecycle's FileConfig): auto picks
// the OS tool on PATH, none silences notifications. Anything else is
// refused when the config is read.
const (
	ModeAuto = "auto"
	ModeNone = "none"
)

// Resolve turns a config value into a Notifier: auto detects the OS
// tool, none (or the empty value, the default) silences. The result
// always implements Notifier, so callers never branch on the mode.
func Resolve(mode string) Notifier {
	if mode == ModeNone {
		return None{}
	}
	return Detect()
}

// Detect picks the OS's notification tool from GOOS and PATH: osascript
// on macOS, notify-send on Linux, and the silent none when neither
// exists — a host with no banner service is normal, not an error.
func Detect() Notifier {
	return detect(runtime.GOOS, exec.LookPath)
}

// detect is Detect with the platform facts injected, so tests can walk
// every branch without touching the real OS or PATH.
func detect(goos string, lookPath func(string) (string, error)) Notifier {
	switch goos {
	case "darwin":
		if _, err := lookPath("osascript"); err == nil {
			return Osascript{Tool: "osascript"}
		}
	case "linux":
		if _, err := lookPath("notify-send"); err == nil {
			return NotifySend{Tool: "notify-send"}
		}
	}
	return None{}
}

// None is the notifier for a host with no banner tool, or for
// `notify = none`: it drops every notification and reports success, so
// the delivery loop above it runs unchanged — the cursor keeps
// advancing and flipping the config to auto later never re-notifies
// ancient events.
type None struct{}

// Notify drops the notification.
func (None) Notify(context.Context, string, string) error { return nil }

// Name is none's log name.
func (None) Name() string { return ModeNone }

// namer lets the engine say which notifier it picked, without a type
// switch on the concrete tools.
type namer interface {
	Name() string
}

// NameOf is n's log name: the tool it shells out to, or none.
func NameOf(n Notifier) string {
	if nm, ok := n.(namer); ok {
		return nm.Name()
	}
	return fmt.Sprintf("%T", n)
}
