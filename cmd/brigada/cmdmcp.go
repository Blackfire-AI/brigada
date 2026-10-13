package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/mcp"
)

// runMCP is the `brigada mcp` bridge (§12.2): a stdio MCP server the
// foreman's harness starts from the MCP config the runner wrote for the
// turn, offering brigada's tools over the engine API. It is a role of
// the one binary (§2), not a command a user types: stdin and stdout are
// the protocol's, so nothing else is printed on either — the bridge's
// own notes go to stderr.
func runMCP(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("mcp")
	var socket string
	fs.StringVar(&socket, "socket", "", "the engine API's Unix socket path")
	if err := fs.Parse(args); err != nil {
		return usagef(stderr, "%v", err)
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "mcp takes no arguments, got %q", extra)
	}
	if socket == "" {
		return usagef(stderr, "mcp needs --socket PATH: the engine API's Unix socket")
	}
	// The token comes from the environment the harness's MCP config
	// names (§12.2) — never from a flag, which anyone listing processes
	// could read.
	token := os.Getenv(mcp.EnvToken)
	if token == "" {
		return fail(stderr, fmt.Errorf("mcp: %s is not set: the harness's MCP config carries the foreman token in it", mcp.EnvToken))
	}
	srv := &mcp.Server{
		Client: api.NewClient(socket, token),
		Log:    slog.New(slog.NewTextHandler(stderr, nil)),
	}
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		return fail(stderr, fmt.Errorf("mcp: %w", err))
	}
	return 0
}
