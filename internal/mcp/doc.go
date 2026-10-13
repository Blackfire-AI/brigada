// Package mcp is the `brigada mcp` bridge (design §12.2): a stdio MCP
// server the foreman's harness starts, which offers brigada's tools by
// calling the engine API with the lease-scoped foreman token (§12.1).
// The foreman decides; the engine enforces (§1) — the bridge holds no
// state and no judgment of its own, it only renders what the engine
// answers.
//
// The protocol is newline-delimited JSON-RPC 2.0 over stdin and stdout:
// stdout carries protocol messages and nothing else, logs go to stderr
// alone, and one malformed line is a parse error the server answers and
// survives. The token never rides a flag — flags are public to anyone
// listing processes — but the BRIGADA_TOKEN environment variable the
// runner names in the harness's MCP config (HarnessConfig).
package mcp
