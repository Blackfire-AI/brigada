// Package adapter bridges brigada and each agent CLI. One adapter per CLI: it
// builds the headless command (fresh or resume), parses the CLI's event stream
// into progress events, detects limit-hit and auth errors, and finds the
// session id needed to resume a session. It never modifies the CLI and never
// holds credentials itself; the user signs in through the vendor's own flow.
//
// Every adapter implements the interface whole, both parse methods
// included: ParseEvent reads the CLI's stdout and ParseErrEvent its
// stderr, because some failures print only there — a resume that cannot
// be found before any JSON, a limit or an auth message — and must still
// become error events. A line with nothing to act on returns false from
// either method; only the CLI's own errors are errors.
package adapter
