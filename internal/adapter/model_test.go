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
		{"deepseek-chat", "deepseek"}, {"qwen-3", "alibaba"}, {"qwen3.8-max", "alibaba"},
		{"qwen-subscription/qwen3.8-max", "alibaba"}, {"qwen-subscription/qwen3.8-flash", "alibaba"},
		{"llama-4", "meta"},
		{"mistral-large", "mistral"}, {"codestral", "mistral"}, {"devstral-2", "mistral"},
		{"glm-5.3", "zhipu"}, {"zai-coding-plan/glm-5.3", "zhipu"},
		{"other/provider/GLM-5.3", "zhipu"},
		{"opencode/mimo-v2-flash-free", "xiaomi"}, {"mimo", "xiaomi"},
		{"opencode/longcat-flash-chat", "meituan"},
		{"opencode/nemotron-3-super-free", "nvidia"},
		{"fable", "anthropic"}, {"opus[1m]", "anthropic"}, {"claude-fable-5-1[1m]", "anthropic"},
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
		{"dsh", "deepseek-flash", "deepseek", false},
		{"dsh", "deepseek-v4-pro", "deepseek", false},
		{"qwen-code", "default", "alibaba", false},
		{"qwen-code", "qwen-subscription/qwen3.8-max", "alibaba", false},
		{"qwen-code", "qwen3.8-flash", "alibaba", false},
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

var modelTierExamples = []struct {
	tier   string
	models []string
}{
	{TierHeavy, []string{"fable", "claude-fable-5-1[1m]", "provider/CLAUDE-FABLE-5-1[1M]", "gpt-6-astra", "gpt-6-astra-mini", "opus", "claude-opus-4", "anthropic/claude-opus-4[1m]", "o3-pro", "gpt-5-pro", "provider/CLAUDE-OPUS-4[1M]", "kimi-k3.1-pro"}},
	{TierLight, []string{"gpt-6-luna", "gpt-5.6-luna", "provider/GPT-6-LUNA[1M]", "haiku", "claude-haiku-4", "gpt-5.4-mini", "o4-mini", "gemini-2.5-flash", "deepseek-flash", "opencode/space-bunny-free", "qwen-3-lite", "qwen-subscription/qwen3.8-flash", "gpt-5-nano", "provider/HAIKU[1m]", "zai-coding-plan/glm-5.3-flash", "mini-opus"}},
	{TierStandard, []string{"gpt-6-sol", "gpt-5.6-terra", "gpt-6-astral", "gpt-6-lunar", "astra/provider/gpt-6-sol", "luna/provider/gpt-6-sol", "sonnet", "claude-sonnet-4[1m]", "gpt-6.1-sol", "gpt-5", "kimi-code/k3", "kimi-code/k3-256k", "zai-coding-plan/glm-5.3", "deepseek-v3", "default", "maximal-coder", "freedom-7", "pro/provider/gpt-5", "flash/provider/sonnet", "professional", "minify", "",
		// No family row makes these heavy or light: pro is heavy only for
		// openai and moonshot, and max, ultra and highspeed carry no tier.
		"gemini-2.5-pro", "deepseek-v4-pro", "qwen-subscription/qwen3.8-max", "grok-4-max", "gpt-5-ultra",
		"glm-5.3-highspeed", "zai-coding-plan/glm-5.3-highspeed", "kimi-code/kimi-for-coding-highspeed",
		// A family's line word does not reach a name of unknown family.
		"opusnext", "fablenext", "haikunext"}},
}

func TestModelTier(t *testing.T) {
	for _, group := range modelTierExamples {
		for _, model := range group.models {
			t.Run(model, func(t *testing.T) {
				if got := ModelTier(model); got != group.tier {
					t.Errorf("ModelTier(%q) = %q, want %q", model, got, group.tier)
				}
			})
		}
	}
}

func TestAgentTier(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "kimi", "opencode", "dsh", "qwen-code"} {
		for _, group := range modelTierExamples {
			for _, model := range group.models {
				t.Run(harness+"/"+model, func(t *testing.T) {
					if got := AgentTier(harness, model); got != group.tier {
						t.Errorf("AgentTier(%q, %q) = %q, want %q", harness, model, got, group.tier)
					}
				})
			}
		}
	}
}

func TestAgentTierUsesHarnessFamily(t *testing.T) {
	// The name alone does not identify kimi-code/k3.1-pro as Moonshot's;
	// the kimi harness does, and Moonshot's pro is heavy.
	if got := ModelTier("kimi-code/k3.1-pro"); got != TierStandard {
		t.Errorf("ModelTier = %q, want standard", got)
	}
	if got := AgentTier("kimi", "kimi-code/k3.1-pro"); got != TierHeavy {
		t.Errorf("AgentTier(kimi) = %q, want heavy", got)
	}
	if got := AgentTier("opencode", "deepseek/deepseek-v4-pro"); got != TierStandard {
		t.Errorf("AgentTier(opencode, deepseek-v4-pro) = %q, want standard", got)
	}
}

func TestSuperseded(t *testing.T) {
	got := Superseded([]string{
		"gpt-6.1-sol", "gpt-6-sol", "gpt-5.6-sol", "gpt-6.1-terra", "gpt-5.6-terra",
		"gpt-6-luna", "gpt-5.6-luna", "gpt-6-astra", "gpt-5", "gpt-5.5",
		"claude-fable-5-1[1m]", "claude-fable-5-2[1m]", "claude-fable-5-1",
		"claude-opus-4-20250514", "claude-opus-4-1-20250805", "claude-sonnet-4-20250514", "claude-sonnet-4-20250601",
		"opus", "sonnet", "default",
		"qwen-subscription/qwen3.7-max", "qwen-subscription/qwen3.8-max", "qwen3.6-max",
		"opencode/deepseek-v4.1-flash-free", "opencode/deepseek-v4-flash-free", "deepseek/deepseek-v4-pro", "deepseek/deepseek-flash",
		"opencode/nemotron-3.5-super-vl-free", "opencode/nemotron-3-super-vl-free", "opencode/nemotron-3.5-lightning-free",
		"o3", "o4-mini", "o3-mini",
	})
	want := map[string]string{
		"gpt-6-sol":                         "gpt-6.1-sol",
		"gpt-5.6-sol":                       "gpt-6.1-sol",
		"gpt-5.6-terra":                     "gpt-6.1-terra",
		"gpt-5.6-luna":                      "gpt-6-luna",
		"gpt-5":                             "gpt-5.5",
		"claude-fable-5-1[1m]":              "claude-fable-5-2[1m]",
		"claude-opus-4-20250514":            "claude-opus-4-1-20250805",
		"qwen-subscription/qwen3.7-max":     "qwen-subscription/qwen3.8-max",
		"opencode/deepseek-v4-flash-free":   "opencode/deepseek-v4.1-flash-free",
		"opencode/nemotron-3-super-vl-free": "opencode/nemotron-3.5-super-vl-free",
		"o3-mini":                           "o4-mini",
	}
	if len(got) != len(want) {
		t.Errorf("got %d superseded, want %d: %v", len(got), len(want), got)
	}
	for model, newer := range want {
		if got[model] != newer {
			t.Errorf("Superseded[%q] = %q, want %q", model, got[model], newer)
		}
	}
}
