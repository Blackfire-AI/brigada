package qwencode

import (
	"reflect"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func n(v int64) *int64 { return &v }

// usage.jsonl is lines of a real qwen 1.0.0-preview.1 run: a partial
// assistant line (stop_reason null, zeroed usage), the two final assistant
// lines that carry the counts, and the result line, whose usage object is
// all zeros. The last line is synthetic: cache reads above input.
func TestUsageFixture(t *testing.T) {
	wants := []*adapter.Usage{
		// partial message: not counted
		nil,
		{InputTokens: n(18304), OutputTokens: n(242), CachedInputTokens: n(13312)},
		{InputTokens: n(18638), OutputTokens: n(40), CachedInputTokens: n(17408)},
		// the result line's zeroed usage is not read; only its num_turns is
		{ModelCalls: n(2)},
		// cached above input is clamped to input
		{InputTokens: n(5), OutputTokens: n(2), CachedInputTokens: n(5)},
	}
	lines := readLines(t, "usage.jsonl")
	if len(lines) != len(wants) {
		t.Fatalf("read %d lines, want %d", len(lines), len(wants))
	}
	total := &adapter.Usage{}
	for i, l := range lines {
		ev, ok := (&Adapter{}).ParseEvent([]byte(l))
		if !reflect.DeepEqual(ev.Usage, wants[i]) {
			t.Errorf("line %d usage = %+v, want %+v", i, ev.Usage, wants[i])
		}
		if wants[i] != nil && !ok {
			t.Errorf("line %d discarded", i)
		}
		total.Add(ev.Usage)
		if u := ev.Usage; u != nil && u.CachedInputTokens != nil && (u.InputTokens == nil || *u.CachedInputTokens > *u.InputTokens) {
			t.Errorf("line %d: cached %d exceeds input", i, *u.CachedInputTokens)
		}
	}
	// input_tokens already includes the cache reads, so the sums keep cached <= input.
	if *total.InputTokens != 18304+18638+5 || *total.CachedInputTokens != 13312+17408+5 || *total.CachedInputTokens > *total.InputTokens {
		t.Errorf("total = %+v", total)
	}
}

// The whole recorded stream counts each model call once: two final assistant
// lines and the result's two turns.
func TestNormalStreamUsageCountedOnce(t *testing.T) {
	total := &adapter.Usage{}
	for _, l := range readLines(t, "normal.jsonl") {
		ev, _ := (&Adapter{}).ParseEvent([]byte(l))
		total.Add(ev.Usage)
	}
	if *total.InputTokens != 18304+18638 || *total.OutputTokens != 242+40 || *total.CachedInputTokens != 13312+17408 || *total.ModelCalls != 2 {
		t.Errorf("total = in %d out %d cached %d calls %d", *total.InputTokens, *total.OutputTokens, *total.CachedInputTokens, *total.ModelCalls)
	}
}

func TestUsageOnlyMessageIsNotProgress(t *testing.T) {
	ev, ok := (&Adapter{}).ParseEvent([]byte(`{"type":"assistant","message":{"stop_reason":"end_turn","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`))
	if !ok || ev.Kind != adapter.EvUsage || ev.IsProgress() {
		t.Fatalf("event = %+v, %v", ev, ok)
	}
}
