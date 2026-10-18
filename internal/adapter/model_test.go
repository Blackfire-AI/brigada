package adapter

import (
	"fmt"
	"strings"
	"testing"
)

func TestModelFamily(t *testing.T) {
	for _, tc := range []struct{ model, family string }{
		{"sonnet", "anthropic"}, {"anthropic/claude-opus-4", "anthropic"},
		{"openrouter/anthropic/claude-sonnet-4", "anthropic"}, {"gpt-5-codex", "openai"},
		{"openai/gpt-5", "openai"}, {"o3", "openai"}, {"kimi-k2", "moonshot"}, {"k2.5", "moonshot"},
		{"google/gemini-2.5-pro", "google"},
		{"deepseek-chat", "deepseek"}, {"qwen-3", "alibaba"}, {"llama-4", "meta"},
		{"mistral-large", "mistral"}, {"codestral", "mistral"}, {"devstral-2", "mistral"},
		{"glm-5.3", "zhipu"}, {"zai-coding-plan/glm-5.3", "zhipu"},
		{"other/provider/GLM-5.3", "zhipu"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			got, err := ModelFamily(tc.model)
			if err != nil || got != tc.family {
				t.Fatalf("got %q %v, want %q", got, err, tc.family)
			}
		})
	}
	for _, model := range []string{"", "default", "custom/unknown", "gpt-5\nSECRET", "claudeevil", "sonnet other", "glmevil"} {
		if _, err := ModelFamily(model); err == nil {
			t.Errorf("accepted unknown/invalid model %q", model)
		}
	}
}

func TestAgentFamily(t *testing.T) {
	for _, tc := range []struct {
		harness, model, family string
		wantError              bool
	}{
		{"claude", "sonnet", "anthropic", false},
		{"codex", "default", "openai", false},
		{"kimi", "default", "moonshot", false},
		{"kimi", "kimi-code/k3-256k", "moonshot", false},
		{"kimi", "k3", "moonshot", false},
		{"kimi", "kimi-code/kimi-for-coding", "moonshot", false},
		{"kimi", "custom/unknown", "moonshot", false},
		{"kimi", "provider/claude-sonnet-4", "anthropic", false},
		{"kimi", "gpt-5", "openai", false},
		{"kimi", "", "kimi", true},
		{"kimi", "k3 other", "kimi", true},
		{"kimi", "k3\nSECRET", "kimi", true},
		{"opencode", "default", "", false},
		{"opencode", "zai-coding-plan/glm-5.3", "zhipu", false},
		{"opencode", "glm-5.3", "zhipu", false},
		{"opencode", "anthropic/claude-sonnet-4", "anthropic", false},
		{"opencode", "custom/unknown", "opencode", true},
		{"claude", "default", "claude", true},
	} {
		t.Run(tc.harness+"/"+tc.model, func(t *testing.T) {
			family, err := AgentFamily(tc.harness, tc.model)
			if family != tc.family || (err != nil) != tc.wantError {
				t.Fatalf("got %q, %v; want %q, error=%v", family, err, tc.family, tc.wantError)
			}
		})
	}
}

func TestModelFamilyUnknownErrorListsPrefixes(t *testing.T) {
	var prefixes []string
	for _, group := range modelFamilyPrefixes {
		prefixes = append(prefixes, group.prefixes...)
	}
	const model = "opencode/beam-free"
	_, err := ModelFamily(model)
	want := fmt.Sprintf("cannot identify the family of model %q: brigada recognises model names starting with %s", model, strings.Join(prefixes, ", "))
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}
