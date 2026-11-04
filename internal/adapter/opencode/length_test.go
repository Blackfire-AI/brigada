package opencode

import (
	"reflect"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// TestLengthFixture: a step that finishes with reason "length" carries
// the cut-off on its usage event; the usage itself is as for any step,
// and an earlier step that ended on tool calls carries none.
func TestLengthFixture(t *testing.T) {
	a := &Adapter{}
	events := parseFixture(t, "length.jsonl", a.ParseEvent)
	var usage []adapter.Event
	for _, ev := range events {
		if ev.Kind == adapter.EvUsage {
			usage = append(usage, ev)
		}
	}
	if len(usage) != 2 {
		t.Fatalf("usage events = %d, want 2: %+v", len(usage), events)
	}
	if usage[0].OutputCut != nil {
		t.Errorf("tool-calls step OutputCut = %+v, want none", usage[0].OutputCut)
	}
	if want := (&adapter.OutputCut{Output: 49, Reasoning: 31951}); !reflect.DeepEqual(usage[1].OutputCut, want) {
		t.Errorf("length step OutputCut = %+v, want %+v", usage[1].OutputCut, want)
	}
	if usage[1].Usage == nil || *usage[1].Usage.OutputTokens != 49 || *usage[1].Usage.ReasoningTokens != 31951 {
		t.Errorf("length step usage = %+v, want the step's tokens as ever", usage[1].Usage)
	}
}
