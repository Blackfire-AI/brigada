package qwencode

import (
	"reflect"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func n(v int64) *int64 { return &v }

func TestUsageFixture(t *testing.T) {
	wants := []*adapter.Usage{
		{InputTokens: n(100), OutputTokens: n(20), CachedInputTokens: n(40), ModelCalls: n(3)},
		// cached above input is clamped to input
		{InputTokens: n(5), OutputTokens: n(2), CachedInputTokens: n(5), ModelCalls: n(1)},
		// per-message usage is not counted: the result line carries the run's
		nil,
	}
	lines := readLines(t, "usage.jsonl")
	if len(lines) != len(wants) {
		t.Fatalf("read %d lines, want %d", len(lines), len(wants))
	}
	for i, l := range lines {
		ev, ok := (&Adapter{}).ParseEvent([]byte(l))
		if !reflect.DeepEqual(ev.Usage, wants[i]) {
			t.Errorf("line %d usage = %+v, want %+v", i, ev.Usage, wants[i])
		}
		if wants[i] != nil && !ok {
			t.Errorf("line %d discarded", i)
		}
	}
}

func TestUsageOnlyResultIsNotProgress(t *testing.T) {
	ev, ok := (&Adapter{}).ParseEvent([]byte(`{"type":"result","usage":{"input_tokens":3,"output_tokens":1}}`))
	if !ok || ev.Kind != adapter.EvUsage || ev.IsProgress() {
		t.Fatalf("event = %+v, %v", ev, ok)
	}
}
