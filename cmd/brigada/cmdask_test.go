package main

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/mcp"
)

// TestAskOutcomeLine: every wake outcome reads as one line that says
// what happened to the message — never a silent queue (§4.2 item 3).
func TestAskOutcomeLine(t *testing.T) {
	want := map[engine.WakeOutcome]string{
		engine.WakeWoken:       "waking up",
		engine.WakeAttached:    "a chat is attached",
		engine.WakeTurnRunning: "a foreman turn is running",
		engine.WakeNoForeman:   "no foreman yet",
		engine.WakeStalled:     "stalled",
		engine.WakeBudgetSpent: "budget is spent",
		engine.WakeDuplicate:   "not queued again",
	}
	for outcome, says := range want {
		if got := mcp.AskOutcomeLine(engine.AskResult{ProjectID: 1, Outcome: outcome}); !strings.Contains(got, says) || !strings.Contains(got, "project 1") {
			t.Errorf("%s: %q, want it to say %q and name the project", outcome, got, says)
		}
	}
}
