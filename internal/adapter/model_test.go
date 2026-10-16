package adapter

import "testing"

func TestModelFamily(t *testing.T) {
	for _, tc := range []struct{ model, family string }{
		{"sonnet", "anthropic"}, {"anthropic/claude-opus-4", "anthropic"},
		{"openrouter/anthropic/claude-sonnet-4", "anthropic"}, {"gpt-5-codex", "openai"},
		{"openai/gpt-5", "openai"}, {"o3", "openai"}, {"kimi-k2", "moonshot"}, {"k2.5", "moonshot"},
		{"google/gemini-2.5-pro", "google"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			got, err := ModelFamily(tc.model)
			if err != nil || got != tc.family {
				t.Fatalf("got %q %v, want %q", got, err, tc.family)
			}
		})
	}
	for _, model := range []string{"", "default", "custom/unknown", "gpt-5\nSECRET", "claudeevil", "sonnet other"} {
		if _, err := ModelFamily(model); err == nil {
			t.Errorf("accepted unknown/invalid model %q", model)
		}
	}
}
