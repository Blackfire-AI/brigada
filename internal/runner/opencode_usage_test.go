package runner

import (
	"fmt"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/opencode"
)

// opencodeStep is a step_finish line as opencode v1.19.0 emits it, with the
// given part.tokens object.
func opencodeStep(tokens string) []byte {
	return []byte(fmt.Sprintf(`{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","tokens":%s}}`, tokens))
}

// TestOpencodeUsageKeepsCachedWithinInput pins the §8 invariant end to end on
// the runner side: adapter ParseEvent, then sumUsage. Cached input is part of
// input for every tokens shape, per step and in the job total.
func TestOpencodeUsageKeepsCachedWithinInput(t *testing.T) {
	type counts struct{ input, cached, output, reasoning, calls int64 }
	cases := []struct {
		name   string
		tokens []string
		want   counts
		noCach bool // cached input stays unknown
	}{
		{"input and cache read", []string{`{"input":100,"output":20,"reasoning":0,"cache":{"read":40,"write":0}}`}, counts{140, 40, 20, 0, 1}, false},
		{"cache read without input", []string{`{"output":20,"cache":{"read":40,"write":0}}`}, counts{40, 40, 20, 0, 1}, false},
		{"cache read with null input", []string{`{"input":null,"output":2,"cache":{"read":40}}`}, counts{40, 40, 2, 0, 1}, false},
		{"cache read with cache write", []string{`{"input":100,"output":20,"reasoning":0,"cache":{"read":40,"write":10}}`}, counts{140, 40, 20, 0, 1}, false},
		{"reasoning present", []string{`{"input":10,"output":3,"reasoning":7,"cache":{"read":5,"write":0}}`}, counts{15, 5, 3, 7, 1}, false},
		{"several steps", []string{
			`{"input":100,"output":20,"cache":{"read":0,"write":50}}`,
			`{"input":5,"output":10,"cache":{"read":150,"write":0}}`,
			`{"output":1,"cache":{"read":160}}`,
		}, counts{415, 310, 31, 0, 3}, false},
		{"tokens without cache", []string{`{"input":10,"output":3,"reasoning":0}`}, counts{10, 0, 3, 0, 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []adapter.Event
			for i, tokens := range tc.tokens {
				ev, ok := (&opencode.Adapter{}).ParseEvent(opencodeStep(tokens))
				if !ok || ev.Usage == nil {
					t.Fatalf("step %d: no usage event", i)
				}
				if u := ev.Usage; u.CachedInputTokens != nil && (u.InputTokens == nil || *u.CachedInputTokens > *u.InputTokens) {
					t.Fatalf("step %d: cached %d above input %v", i, *u.CachedInputTokens, u.InputTokens)
				}
				events = append(events, ev)
			}
			got := sumUsage(events)
			if got == nil || got.InputTokens == nil || got.OutputTokens == nil || got.ModelCalls == nil {
				t.Fatalf("usage = %+v", got)
			}
			if tc.noCach {
				if got.CachedInputTokens != nil {
					t.Fatalf("cached = %d, want unknown", *got.CachedInputTokens)
				}
			} else if got.CachedInputTokens == nil || *got.CachedInputTokens > *got.InputTokens || *got.CachedInputTokens != tc.want.cached {
				t.Fatalf("cached = %v, input = %d, want cached %d <= input", got.CachedInputTokens, *got.InputTokens, tc.want.cached)
			}
			if *got.InputTokens != tc.want.input || *got.OutputTokens != tc.want.output || *got.ModelCalls != tc.want.calls {
				t.Fatalf("input %d output %d calls %d, want %+v", *got.InputTokens, *got.OutputTokens, *got.ModelCalls, tc.want)
			}
			if tc.want.reasoning > 0 && (got.ReasoningTokens == nil || *got.ReasoningTokens != tc.want.reasoning) {
				t.Fatalf("reasoning = %v, want %d", got.ReasoningTokens, tc.want.reasoning)
			}
		})
	}
}
