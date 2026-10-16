package kimi

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// kimi --help lists no effort option, so a non-empty effort is refused.
func TestEffortIsRefused(t *testing.T) {
	stubCLI(t)
	_, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Effort: "high"}, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "effort") {
		t.Fatalf("Command error = %v, want an effort refusal", err)
	}
	if _, err := (&Adapter{}).Command(adapter.Job{Prompt: "p"}, t.TempDir(), ""); err != nil {
		t.Errorf("no effort refused: %v", err)
	}
}
