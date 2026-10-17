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
	fs := newFlagSet("mcp",
		"brigada mcp [--foreman [URL] | --coordinator | --client | --socket PATH]",
		"Serve the foreman, coordinator, client, or headless bridge tools over stdio.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var socket string
	fs.StringVar(&socket, "socket", "", "the engine API's Unix socket path (required for the bare headless bridge; interactive modes use --state-dir and --config)")
	foreman := fs.Bool("foreman", false, "serve interactive foreman tools for this repository (or an explicit repo URL)")
	coordinator := fs.Bool("coordinator", false, "serve the coordinator tools (list_projects, ask_foreman, status, history, plan_approve, plan_reject) over every project, with the state directory's client token")
	client := fs.Bool("client", false, "serve the client tools (ask_foreman, status, history) with the state directory's client token, for any harness acting for the user")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	selectors := 0
	for _, selected := range []bool{*client, *foreman, *coordinator} {
		if selected {
			selectors++
		}
	}
	if selectors > 1 {
		return usagef(stderr, "mcp selectors --client, --foreman and --coordinator are mutually exclusive")
	}
	if *coordinator {
		if len(fs.Args()) > 0 {
			return usagef(stderr, "mcp --coordinator takes no arguments, got %q", fs.Args())
		}
		return runBridgeMCP(ctx, common, mcp.CoordinatorTools, stderr)
	}
	if *foreman {
		if len(fs.Args()) > 1 {
			return usagef(stderr, "mcp --foreman takes at most one repo URL")
		}
		repo := ""
		if len(fs.Args()) == 1 {
			repo = fs.Args()[0]
		}
		return runInteractiveMCP(ctx, common, socket, repo, stderr)
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "mcp takes no arguments, got %q", extra)
	}
	if *client {
		return runBridgeMCP(ctx, common, mcp.ClientTools, stderr)
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

// runBridgeMCP is `brigada mcp --client` and `--coordinator` (§12.2):
// the same stdio bridge for any other harness acting for the user — it
// offers the selector's tools (ClientTools, CoordinatorTools) and bears
// the client token from the state directory's token file, never a
// foreman token. The engine starts on demand like for any client
// command; its notes go to stderr, since stdout is the protocol's.
func runBridgeMCP(ctx context.Context, common *commonFlags, offer []mcp.Tool, stderr io.Writer) int {
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	c, code := connect(ctx, rt, "client", false, stderr)
	if code != 0 {
		return code
	}
	srv := &mcp.Server{
		Client: c,
		Log:    slog.New(slog.NewTextHandler(stderr, nil)),
		Tools:  offer,
	}
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		return fail(stderr, fmt.Errorf("mcp: %w", err))
	}
	return 0
}

// runInteractiveMCP uses the local client credential solely to discover the
// project and acquire a hold. BRIGADA_TOKEN is never consulted in this mode.
func runInteractiveMCP(ctx context.Context, common *commonFlags, socket, repo string, stderr io.Writer) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(stderr, err)
	}
	// Read the verified environment observation at startup, before any request.
	session := os.Getenv("CLAUDE_CODE_SESSION_ID")
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	if socket != "" {
		rt.paths.Socket = socket
	}
	c, code := connect(ctx, rt, "client", false, stderr)
	if code != 0 {
		return code
	}
	bridge := mcp.NewInteractive(c, repo, cwd, session)
	if err := bridge.StartHook(rt.stateDir, os.Getppid()); err != nil {
		// Hook failure never prevents stdio operation; expiry remains the fallback.
		mcp.LogHookFailure(rt.stateDir, err)
		fmt.Fprintf(stderr, "mcp hold hook: %v\n", err)
	}
	srv := &mcp.Server{Interactive: bridge, Log: slog.New(slog.NewTextHandler(stderr, nil))}
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		return fail(stderr, fmt.Errorf("mcp: %w", err))
	}
	return 0
}
