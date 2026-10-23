package runner

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// The same connection words after real output are not infra: only a run
// that said or did nothing yet failed because the network was not there.
func TestProducedOutput(t *testing.T) {
	tests := []struct {
		name   string
		events []adapter.Event
		want   bool
	}{
		{"no events", nil, false},
		{"startup only", []adapter.Event{{Kind: adapter.EvSession}, {Kind: adapter.EvUsage}}, false},
		{"message", []adapter.Event{{Kind: adapter.EvSession}, {Kind: adapter.EvMessage, Text: "working"}}, true},
		{"tool call", []adapter.Event{{Kind: adapter.EvToolStart}}, true},
		{"tool result", []adapter.Event{{Kind: adapter.EvToolEnd}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &jobRun{events: tt.events}
			if got := r.producedOutput(); got != tt.want {
				t.Fatalf("producedOutput() = %v, want %v", got, tt.want)
			}
		})
	}
}
