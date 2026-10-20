package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/version"
)

// protocolVersion is the MCP revision the bridge speaks: the version
// string initialize answers, one dated revision of the protocol.
const protocolVersion = "2025-06-18"

// ServerName is the bridge's name as the harness's config offers it
// (§12.2) and initialize reports it: the one MCP server a foreman turn
// carries, `brigada`.
const ServerName = "brigada"

// The JSON-RPC 2.0 error codes the bridge answers with (§12.2): a line
// that is not the protocol's JSON, a method the server has not, and
// arguments a method cannot take.
const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Server is the bridge (§12.2): the engine's API client — bearing the
// turn's foreman token — behind the MCP protocol. The zero value is not
// usable; Client is required, Log is optional and writes to stderr.
type Server struct {
	// Client reaches the engine (§12.1); its token is the lease-scoped
	// foreman token, so every call is already scoped to the turn's
	// project and dies with the turn's lease.
	Client *api.Client
	// Log is where the bridge's own notes go: stderr, never stdout —
	// stdout is the protocol's alone. Nil discards them.
	Log *slog.Logger
	// Tools is the offer: nil is the foreman's tools (§12.2); the client
	// bridge passes ClientTools.
	Tools []Tool
}

// offer is the tool list this server serves.
func (s *Server) offer() []Tool {
	if s.Tools != nil {
		return s.Tools
	}
	return tools
}

// request is one line of the protocol as it arrives: a JSON-RPC 2.0
// request, or a notification — the same shape without the id.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is one line of the protocol as it leaves: a result or an
// error, never both, under the id the request carried. A notification
// produces none at all.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the error half of a response: the code and the message,
// nothing more.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolResult is a tools/call answer (§12.2): the text the tool produced,
// or — the engine refused, the arguments were wrong — the failure's
// message with isError set. A tool error is an answer, not a crash: the
// foreman reads it and decides.
type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// toolContent is one block of a tool result's content. The bridge speaks
// text alone — its tools render, they do not stream structures.
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Serve reads newline-delimited JSON-RPC 2.0 from in and writes the
// answers to out, one JSON object per line, until in ends or ctx does.
// It never writes anything but protocol messages to out; its own notes
// go to the logger, which the caller points at stderr.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s.Log != nil {
		// One note, for the transcript a turn keeps of its stderr: what
		// this process was, and when it started serving.
		s.Log.Info("brigada mcp bridge serving on stdio")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	lines := make(chan []byte)
	scanDone := make(chan error, 1)
	go func() {
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		scanDone <- sc.Err()
	}()
	for {
		var line []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-scanDone:
			return err
		case line = <-lines:
		}
		resp := s.handleLine(ctx, line)
		if resp != nil {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
	}
}

// handleLine answers one line: nil for a line that earns no answer — a
// notification, a blank line — and a response for every request, the
// method's result or the protocol's own refusal of it.
func (s *Server) handleLine(ctx context.Context, line []byte) *response {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		// A malformed line has no id to answer to; the parse error goes
		// out under a null one and the server carries on (§12.2).
		return &response{
			JSONRPC: "2.0",
			Error:   &rpcError{codeParseError, "parse error: " + err.Error()},
		}
	}
	if string(req.ID) == "" || string(req.ID) == "null" {
		// A notification: no id, no answer — notifications/initialized
		// among them, the client's own note that it is ready.
		return nil
	}
	resp := &response{JSONRPC: "2.0", ID: req.ID}
	switch {
	case req.Method == "initialize":
		resp.Result = newInitializeResult()
	case req.Method == "ping":
		resp.Result = struct{}{}
	case req.Method == "tools/list":
		resp.Result = toolsListResult{Tools: toolInfos(s.offer())}
	case req.Method == "tools/call":
		resp.Result, resp.Error = s.callTool(ctx, req.Params)
	default:
		resp.Error = &rpcError{codeMethodNotFound, "method not found: " + req.Method}
	}
	return resp
}

// initializeResult is the initialize answer (§12.2): the protocol
// version the bridge speaks, the tools capability — the one it has — and
// the bridge's own name and version.
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
}

// newInitializeResult fills the answer: the bridge's protocol version,
// its tools capability, and its name and version.
func newInitializeResult() initializeResult {
	return initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{"tools": map[string]any{}},
		ServerInfo:      serverInfo{Name: ServerName, Version: version.Version},
	}
}

// serverInfo names the server: `brigada`, at the binary's version.
type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// callTool runs one tools/call (§12.2). A tool the bridge has not is the
// protocol's refusal; everything that goes wrong inside a tool it has —
// arguments it cannot take, an engine that refuses or does not answer —
// is that tool's answer, isError with the message, so a refused call
// reaches the foreman as words to read, never as a dead bridge.
func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{codeInvalidParams, "tools/call params: " + err.Error()}
	}
	tool, ok := toolByName(s.offer(), p.Name)
	if !ok {
		return nil, &rpcError{codeInvalidParams, "unknown tool: " + p.Name}
	}
	if s.Client == nil {
		return toolResult{
			Content: textContent("the bridge has no engine client to call"),
			IsError: true,
		}, nil
	}
	text, err := tool.Call(ctx, s.Client, p.Arguments)
	if err != nil {
		return toolResult{Content: textContent(errorText(err)), IsError: true}, nil
	}
	return toolResult{Content: textContent(text)}, nil
}

// errorText renders an API refusal for a tool's answer: the engine's own
// message with its status in tow, so the foreman sees what the engine
// said and not only that it said no.
func errorText(err error) string {
	var se *api.StatusError
	if errors.As(err, &se) {
		return se.APICode + ": " + se.Message + " (" + http.StatusText(se.Status) + ")"
	}
	return err.Error()
}

// textContent wraps one text as a tool result's content.
func textContent(text string) []toolContent {
	return []toolContent{{Type: "text", Text: text}}
}
