// Package lifecycle is the engine's life as a process (design §13): one
// instance per state directory behind an exclusive lock, started on
// demand by any client command, and shut down again once it has been
// idle long enough. The engine's own rules — leases, fencing, sweeps —
// are the engine's; this package owns only its coming and going.
package lifecycle
