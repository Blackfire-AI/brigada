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
	{"anthropic", []string{"claude", "sonnet", "opus", "haiku", "fable"}},
	{"openai", []string{"gpt", "chatgpt", "codex", "o1", "o3", "o4"}},
	{"moonshot", []string{"kimi", "k2", "moonshot"}},
	{"google", []string{"gemini"}},
	{"deepseek", []string{"deepseek"}},
	{"alibaba", []string{"qwen"}},
	{"meta", []string{"llama"}},
	{"mistral", []string{"mistral", "codestral", "devstral"}},
	{"zhipu", []string{"glm"}},
	{"xiaomi", []string{"mimo"}},
	{"meituan", []string{"longcat"}},
	{"nvidia", []string{"nemotron"}},
}

var modelTierPatterns = []struct {
	tier     string
	prefixes []string
	segments []string
}{
	// Heavy: Opus and Fable variants and explicit high-capability model segments.
	{TierHeavy, []string{"opus", "fable"}, []string{"pro", "max", "ultra", "astra"}},
	// Light: Haiku variants and explicit smaller, faster or free segments.
	{TierLight, []string{"haiku"}, []string{"mini", "nano", "flash", "lite", "highspeed", "free", "luna"}},
}

// ModelTier labels a model by its name. Heavy matches take precedence;
// names without a matching segment serve the standard tier.
func ModelTier(model string) string {
	segments := strings.Split(normalizedModelName(model), "-")
	for _, group := range modelTierPatterns {
		for _, segment := range segments {
			for _, prefix := range group.prefixes {
				if strings.HasPrefix(segment, prefix) {
					return group.tier
				}
			}
			for _, word := range group.segments {
				if segment == word {
					return group.tier
				}
			}
		}
	}
	return TierStandard
}

// AgentTier uses the model's table tier. CLI defaults (codex, kimi and
// opencode) are standard because their model name is not known to brigada.
func AgentTier(harness, model string) string {
	if model == "default" {
		return TierStandard
	}
	return ModelTier(model)
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
	name := normalizedModelName(model)
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

func normalizedModelName(model string) string {
	name := strings.ToLower(model)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	// A context suffix such as "[1m]" changes neither family nor tier.
	if strings.HasSuffix(name, "]") {
		if i := strings.LastIndex(name, "["); i > 0 {
			name = name[:i]
		}
	}
	return name
}
