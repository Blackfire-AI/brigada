package mcp

import (
	"strings"
	"testing"
)

func TestServeDecisionNote(t *testing.T) {
	e := newEnvToken(t, foremanTokenFile, "foreman-secret-9")
	s := start(t, e)
	args := map[string]any{"name": "decision_note", "arguments": map[string]any{"text": "The user prefers a small API."}}
	first, isError := toolText(t, s.call(1, "tools/call", args))
	if isError || !strings.Contains(first, "decision note recorded") {
		t.Fatalf("note = %q error=%v", first, isError)
	}
	again, isError := toolText(t, s.call(2, "tools/call", args))
	if isError || again != first || countOf(t, e, "SELECT COUNT(*) FROM messages WHERE kind = 'decision_note'") != 1 {
		t.Fatal("repeated note was not idempotent")
	}
	if got := strOf(t, e, "SELECT author FROM messages WHERE kind = 'decision_note'"); got != "foreman-interactive" {
		t.Fatalf("author = %q", got)
	}
	for i, text := range []any{"", strings.Repeat("x", 16385), 7} {
		out, isError := toolText(t, s.call(3+i, "tools/call", map[string]any{"name": "decision_note", "arguments": map[string]any{"text": text}}))
		if !isError {
			t.Fatalf("bad argument accepted: %q", out)
		}
	}
	if _, ok := toolByName(ClientTools, "decision_note"); ok {
		t.Fatal("decision_note escaped into the client tool set")
	}
}
