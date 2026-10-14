package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// startClient runs the client bridge (§12.2, `brigada mcp --client`)
// over pipes, bearing the env's client token.
func startClient(t *testing.T, e *env) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := &Server{Client: e.client, Tools: ClientTools}
	go func() { _ = srv.Serve(context.Background(), inR, outW) }()
	t.Cleanup(func() {
		inW.Close()
		outW.Close()
	})
	return &session{t: t, w: inW, r: bufio.NewReader(outR)}
}

// TestClientBridge: the client bridge offers ask_foreman, status and
// history and nothing a foreman does; ask_foreman sends the user's
// message, which wakes the seeded project's foreman, and the answer
// says so; a message without text is the tool's own error.
func TestClientBridge(t *testing.T) {
	s := startClient(t, newEnv(t))
	a := s.call(1, "tools/list", nil)
	var res struct {
		Tools []toolInfo `json:"tools"`
	}
	if a.Error != nil || json.Unmarshal(a.Result, &res) != nil {
		t.Fatalf("tools/list = %+v", a)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "ask_foreman,status,history" {
		t.Fatalf("client tools = %s, want ask_foreman,status,history", got)
	}
	a = s.call(2, "tools/call", map[string]any{"name": "plan_create", "arguments": map[string]any{}})
	if a.Error == nil || a.Error.Code != codeInvalidParams {
		t.Errorf("plan_create on the client bridge = %+v, want an unknown tool", a.Error)
	}
	a = s.call(3, "tools/call", map[string]any{"name": "ask_foreman", "arguments": map[string]any{"text": "amend §5.4"}})
	if text, isErr := toolText(t, a); isErr || !strings.Contains(text, "waking up") {
		t.Errorf("ask_foreman = %q (error %v), want the foreman waking", text, isErr)
	}
	a = s.call(4, "tools/call", map[string]any{"name": "ask_foreman", "arguments": map[string]any{"text": " "}})
	if text, isErr := toolText(t, a); !isErr || !strings.Contains(text, "text is required") {
		t.Errorf("ask_foreman without text = %q (error %v), want the tool's own error", text, isErr)
	}
}
