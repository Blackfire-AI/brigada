package runner

import "time"

// stallPolicy is where stalled-worker detection hooks in (§5.6,
// BP8a-4b): the loop below already tracks everything it needs — the
// time of the last progress event, and whether a tool call is in
// flight — and consults the policy on every beat. The policy is off
// until detection lands; when it does, StallAfter is the configured
// stall_minutes and ToolCap the per-class tool_call_cap_minutes.
type stallPolicy struct {
	// StallAfter is how long without any activity a worker is stalled;
	// 0 disables detection (BP8a-4b turns it on).
	StallAfter time.Duration
	// ToolCap bounds how long one in-flight tool call suspends the
	// stall clock (§5.6); 0 means no suspension once detection is on.
	ToolCap time.Duration
}

// stalled reports whether the worker counts as stalled at now. A tool
// call in flight suspends the clock, bounded by ToolCap; a worker with
// no progress yet — one that never reported anything since it started
// — is measured from the run's start, which the caller passes as
// lastProgress in that case.
func (p stallPolicy) stalled(now, lastProgress time.Time, toolInFlight bool) bool {
	if p.StallAfter <= 0 || lastProgress.IsZero() {
		return false
	}
	idle := now.Sub(lastProgress)
	if toolInFlight && p.ToolCap > 0 {
		idle = max(0, idle-p.ToolCap)
	}
	return idle > p.StallAfter
}
