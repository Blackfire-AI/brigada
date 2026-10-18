package adapter

import (
	"fmt"
	"strings"
	"unicode"
)

// AgentFamily uses the model's family, including known CLI defaults. OpenCode's
// default can serve any provider, so its family is unknown (empty, without error).
// Kimi serves Moonshot models unless the name identifies another known family.
// For other unknown aliases it returns the historical harness fallback and an error:
// standalone runners support custom aliases, while init requires known models.
func AgentFamily(harness, model string) (string, error) {
	if model == "default" {
		switch harness {
		case "codex":
			return "openai", nil
		case "kimi":
			return "moonshot", nil
		case "opencode":
			return "", nil
		}
	}
	family, err := ModelFamily(model)
	if err != nil {
		if harness == "kimi" && validModelIdentifier(model) {
			return "moonshot", nil
		}
		return harness, err
	}
	return family, nil
}

var modelFamilyPrefixes = []struct {
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
	{"zhipu", []string{"glm"}},
}

func validModelIdentifier(model string) bool {
	return model != "" && !strings.ContainsFunc(model, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// ModelFamily identifies a model independently of the CLI/provider serving it.
// Unknown aliases need an explicit model name in init, rather than a guessed family.
func ModelFamily(model string) (string, error) {
	if !validModelIdentifier(model) {
		return "", fmt.Errorf("model must be a nonempty model identifier")
	}
	name := strings.ToLower(model)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var prefixes []string
	for _, group := range modelFamilyPrefixes {
		prefixes = append(prefixes, group.prefixes...)
		for _, prefix := range group.prefixes {
			if name == prefix || strings.HasPrefix(name, prefix+"-") || strings.HasPrefix(name, prefix+".") {
				return group.family, nil
			}
		}
	}
	return "", fmt.Errorf("cannot identify the family of model %q: brigada recognises model names starting with %s", model, strings.Join(prefixes, ", "))
}
