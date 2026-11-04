package runner

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/opencode"
)

func TestOutputLimitText(t *testing.T) {
	got := outputLimitText(adapter.OutputCut{Output: 49, Reasoning: 31951})
	if want := "output limit reached: 32,000 tokens, 31,951 of them reasoning"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if got := groupDigits(1234567); got != "1,234,567" {
		t.Errorf("groupDigits = %q", got)
	}
	if got := groupDigits(999); got != "999" {
		t.Errorf("groupDigits = %q", got)
	}
}

// TestOutputLimitIsTheLastStepsAlone: a cut-off step is kept apart from
// the CLI errors, and a later step that ends otherwise clears it.
func TestOutputLimitIsTheLastStepsAlone(t *testing.T) {
	ad := &opencode.Adapter{}
	const cut = `{"type":"step_finish","sessionID":"s","part":{"reason":"length","type":"step-finish","tokens":{"input":10,"output":49,"reasoning":31951}}}`
	const stop = `{"type":"step_finish","sessionID":"s","part":{"reason":"stop","type":"step-finish","tokens":{"input":10,"output":5,"reasoning":0}}}`
	r := &jobRun{cfg: &Config{Clock: realClock()}}
	feedCodex(r, ad, cut)
	if r.cliError != nil {
		t.Errorf("cliError = %+v, want none: a cut-off is no CLI error", r.cliError)
	}
	if r.outputCut == nil || r.outputCut.Output != 49 || r.outputCut.Reasoning != 31951 {
		t.Fatalf("outputCut = %+v, want 49 output and 31951 reasoning", r.outputCut)
	}
	feedCodex(r, ad, stop)
	if r.outputCut != nil {
		t.Errorf("outputCut = %+v, want cleared by the later step", r.outputCut)
	}
}
