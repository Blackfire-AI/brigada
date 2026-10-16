package adapter

import (
	"fmt"
	"strings"
	"unicode"
)

// ModelFamily identifies a model independently of the CLI/provider serving it.
// Unknown aliases need an explicit model name in init, rather than a guessed family.
func ModelFamily(model string) (string, error) {
	for _, r := range model {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("model must be a nonempty model identifier")
		}
	}
	name := strings.ToLower(model)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, group := range []struct {
		family   string
		prefixes []string
	}{
		{"anthropic", []string{"claude", "sonnet", "opus", "haiku"}},
		{"openai", []string{"gpt", "chatgpt", "codex", "o1", "o3", "o4"}},
		{"moonshot", []string{"kimi", "k2", "moonshot"}},
		{"google", []string{"gemini"}},
		{"deepseek", []string{"deepseek"}},
		{"alibaba", []string{"qwen"}},
		{"meta", []string{"llama"}},
		{"mistral", []string{"mistral", "codestral", "devstral"}},
	} {
		for _, prefix := range group.prefixes {
			if name == prefix || strings.HasPrefix(name, prefix+"-") || strings.HasPrefix(name, prefix+".") {
				return group.family, nil
			}
		}
	}
	return "", fmt.Errorf("cannot identify the family of model %q; choose an explicit supported model (for example sonnet, gpt-5, kimi-k2 or provider/claude-sonnet-4)", model)
}
