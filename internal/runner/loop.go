package runner

import (
	"hash/fnv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// The loop watch (§5.6) beside the silence watch: activity alone is not
// progress either, and a worker repeating the same call is busy doing
// nothing. The watch counts tool starts by their call identity — the
// tool and its normalised arguments — and the heartbeat carries the
// signal once it trips. Nothing is killed: the foreman decides, as for
// a stall.

// loopCalls is how many near-identical tool calls — the same tool with
// the same normalised arguments — count as a loop (§5.6).
const loopCalls = 8

// loopWindow is the window the calls are counted in (§5.6).
const loopWindow = 10 * time.Minute

// loopKept bounds the watch's memory: the newest calls it keeps, each a
// hash and a time, so a runaway stream of distinct calls cannot grow it
// without end.
const loopKept = 1024

// loopCallTextLimit bounds the repeated call's line in the loop signal
// (§5.6): enough to name the call, never the whole prompt-sized
// argument blob.
const loopCallTextLimit = 160

// loopWatch is the running count of tool calls by identity (§5.6):
// plain arithmetic on injected times, like the stall policy beside it.
type loopWatch struct {
	calls []loopCall
}

// loopCall is one tool start: its identity's hash and when it happened.
// The hash, not the key: a call's arguments can run long, and the watch
// holds a window of them — the hash is fixed-size and tells identical
// keys apart as well as the keys themselves do.
type loopCall struct {
	key uint64
	at  time.Time
}

// loopKey is one call's identity (§5.6): the tool, and its arguments
// normalised — whitespace runs collapsed, nothing else. The arguments
// are what tells two calls of one tool apart, so nothing inside them is
// stripped: two different files read are two different calls. "" when
// the event carries no arguments to identify the call by: a bare tool
// name counts nothing — eight different calls of one tool would share
// it, and a loop they do not make.
func loopKey(ev adapter.Event) string {
	args := strings.Join(strings.Fields(ev.Args), " ")
	if args == "" {
		return ""
	}
	return ev.Text + "\x00" + args
}

// loopKeyHash folds a key to its fixed-size identity.
func loopKeyHash(key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

// record adds one call at at, pruning what left the window and
// bounding the memory, and reports how many calls of the key now sit
// in the window.
func (w *loopWatch) record(key uint64, at time.Time) int {
	cut := at.Add(-loopWindow)
	next := w.calls[:0]
	for _, c := range w.calls {
		if c.at.After(cut) {
			next = append(next, c)
		}
	}
	w.calls = append(next, loopCall{key: key, at: at})
	if n := len(w.calls); n > loopKept {
		w.calls = append([]loopCall(nil), w.calls[n-loopKept:]...)
	}
	return w.count(key, at)
}

// count is how many calls of key sit in the window at now.
func (w *loopWatch) count(key uint64, now time.Time) int {
	cut := now.Add(-loopWindow)
	n := 0
	for _, c := range w.calls {
		if c.key == key && !c.at.Before(cut) {
			n++
		}
	}
	return n
}

// loopCallText renders the repeated call for the loop signal's report
// (§5.6): the tool and its arguments, one bounded line.
func loopCallText(ev adapter.Event) string {
	line := ev.Text
	if args := strings.Join(strings.Fields(ev.Args), " "); args != "" {
		line += " " + args
	}
	return cutRuneSafe(line, loopCallTextLimit)
}

// cutRuneSafe keeps s within max bytes, the cut backing off to a
// complete rune so it never lands inside one.
func cutRuneSafe(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 {
		if b := s[len(s)-1]; b&0xC0 == 0x80 {
			s = s[:len(s)-1] // a continuation byte: the rune's start is further back
			continue
		}
		break
	}
	return s
}
