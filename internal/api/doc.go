// Package api exposes the engine over JSON HTTP, on a Unix socket. A remote
// TLS listener is backlog (design §12.1). Clients, the foreman's MCP tools and
// runners reach the engine only through this API, never the store directly,
// authenticating with bearer
// tokens in the roles of client, foreman, runner and admin. Mutating calls
// take idempotency keys, because the callers are often LLMs whose tool calls
// may repeat.
package api
