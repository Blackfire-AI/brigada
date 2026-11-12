package qwencode

import (
	"reflect"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func n(v int64) *int64 { return &v }

func TestUsageFixture(t *testing.T) {
	wants := []*adapter.Usage{
		{InputTokens: n(100), OutputTokens: n(20), CachedInputTokens: n(40)},
		// cached above input is clamped to input
		{InputTokens: n(5), OutputTokens: n(2), CachedInputTokens: n(5)},
		// a partial message (stop_reason null) carries zeroed usage: not counted
		nil,
		// the result line's tokens are zeroed: only its turn count is taken
		{ModelCalls: n(3)},
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

// The captured live stream (normal.jsonl): qwen's input_tokens already
// includes cache_read_input_tokens, so cached input never exceeds input, and
// the run's tokens are the finished assistant messages' sum.
func TestUsageLiveStream(t *testing.T) {
	var total adapter.Usage
	for _, l := range readLines(t, "normal.jsonl") {
		ev, _ := (&Adapter{}).ParseEvent([]byte(l))
		if u := ev.Usage; u != nil {
			if u.InputTokens != nil && u.CachedInputTokens != nil && *u.CachedInputTokens > *u.InputTokens {
				t.Errorf("cached %d exceeds input %d", *u.CachedInputTokens, *u.InputTokens)
			}
			total.Add(u)
		}
	}
	want := adapter.Usage{InputTokens: n(18293 + 18677 + 18811), OutputTokens: n(292 + 117 + 24), CachedInputTokens: n(17408 + 17408 + 18432), ModelCalls: n(3)}
	if !reflect.DeepEqual(total, want) {
		t.Errorf("total = %+v, want %+v", total, want)
	}
}

func TestUsageOnlyMessageIsNotProgress(t *testing.T) {
	ev, ok := (&Adapter{}).ParseEvent([]byte(`{"type":"assistant","message":{"stop_reason":"end_turn","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`))
	if !ok || ev.Kind != adapter.EvUsage || ev.IsProgress() {
		t.Fatalf("event = %+v, %v", ev, ok)
	}
}
