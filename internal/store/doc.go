// Package store persists the engine's authoritative state in one SQLite file:
// projects, plans and their task graphs, jobs with their leases and fencing
// tokens, sessions, runners, agents, outcomes, messages, hashed tokens, the
// pending requests of the approval authority (approval_requests: the act,
// its challenge, expiry and state; package approval reads and answers them)
// and the append-only event log. No state lives only in a session, so everything
// needed to continue after a crash or a lost engine is here; the plan file in
// the repo is a mirror of this store, never the other way around.
package store
