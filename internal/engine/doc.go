// Package engine enforces the mechanics so the foreman can stick to judgment:
// it is the mailbox every message passes through, it turns ready tasks into
// jobs, applies the routine rules (fix rounds, capped retries, dependency
// readiness, account cooling), dispatches each job to the best eligible agent
// by measured outcomes, and wakes the foreman only for judgment events. It
// also owns the engine's lifecycle: a single instance locked on its store,
// started on demand and shut down when idle.
package engine
