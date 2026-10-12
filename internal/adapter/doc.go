// Package adapter bridges brigada and each agent CLI. One adapter per CLI: it
// builds the headless command (fresh or resume), parses the CLI's event stream
// into progress events, detects limit-hit and auth errors, and finds the
// session id needed to resume a session. It never modifies the CLI and never
// holds credentials itself; the user signs in through the vendor's own flow.
package adapter
