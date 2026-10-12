package runner

import (
	"fmt"
	"strings"
	"time"
)

// The stall policy (§5.6, BP8a-4b): a worker that is alive but making
// no progress is caught by the heartbeat loop, which consults the
// policy on every beat with the run's timing. The policy is plain
// arithmetic on injected times, so its table test needs no sleeps.

// stallPolicy is the watch on one running worker (§5.6): how long
// without any activity a run counts as stalled, the bound on the
// suspension a tool call in flight buys, and the wall-time cap the
// job's class puts on the whole run.
type stallPolicy struct {
	// StallAfter is how long without any worker activity — no tool
	// call, no message — a run counts as stalled; <= 0 disables the
	// silence watch.
	StallAfter time.Duration
	// ToolCap bounds how long one tool call may stay open: while a
	// call is in flight the stall clock is suspended (a long build or
	// test run is work, not silence), and the call itself is bounded
	// here. <= 0 means an open call suspends the clock unbounded.
	ToolCap time.Duration
	// TimeCap is the class's wall-time cap on the whole run, measured
	// from StartedAt; <= 0 disables it. The default sits under the
	// engine's hard cap for the class, so the runner stops first and
	// completes the job with a useful result.
	TimeCap   time.Duration
	StartedAt time.Time
}

// runTiming is the worker's state the policy reads: when it was last
// active (its start, until the first progress event), and whether — and
// since when — a tool call is in flight.
type runTiming struct {
	LastActivity time.Time
	ToolOpenedAt time.Time
	ToolInFlight bool
}

// stop is why the runner cut a run short itself (§5.6): the outcome the
// job completes with — stalled or time_cap — and the reason naming it.
type stop struct {
	Outcome string
	Reason  string
}

// check reports whether — and why — the run is over at now, or nil when
// the worker is fine. The class time cap comes first: it bounds
// everything, stall or no stall, and when both trip at once the cap is
// the truer name for what happened.
func (p stallPolicy) check(now time.Time, t runTiming) *stop {
	if p.TimeCap > 0 && now.Sub(p.StartedAt) > p.TimeCap {
		return &stop{
			Outcome: statusTimeCap,
			Reason:  fmt.Sprintf("the run passed its class time cap of %s", durText(p.TimeCap)),
		}
	}
	if t.ToolInFlight {
		if p.ToolCap > 0 && now.Sub(t.ToolOpenedAt) > p.ToolCap {
			return &stop{
				Outcome: statusStalled,
				Reason:  fmt.Sprintf("one tool call has been open for over %s", durText(p.ToolCap)),
			}
		}
		return nil // the call suspends the stall clock
	}
	if p.StallAfter > 0 && !t.LastActivity.IsZero() && now.Sub(t.LastActivity) > p.StallAfter {
		return &stop{
			Outcome: statusStalled,
			Reason:  fmt.Sprintf("no activity for over %s", durText(p.StallAfter)),
		}
	}
	return nil
}

// durText renders a duration for a reason that names it: "10m", not
// "10m0s" — the same shape the engine's cap errors carry.
func durText(d time.Duration) string {
	return strings.TrimSuffix(d.String(), "0s")
}
