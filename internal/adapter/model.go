package adapter

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
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
		case "qwen-code":
			return "alibaba", nil
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
	// qwen3 also stands alone: qwen's own names glue the version to the
	// prefix (qwen3.8-max), with no separator for the prefix rule.
	{"alibaba", []string{"qwen", "qwen3"}},
	{"meta", []string{"llama"}},
	{"mistral", []string{"mistral", "codestral", "devstral"}},
	{"zhipu", []string{"glm"}},
	{"xiaomi", []string{"mimo"}},
	{"meituan", []string{"longcat"}},
	{"nvidia", []string{"nemotron"}},
}

// modelTierTable names each family's model lines by tier. A family's rows
// apply only to its own models, so a word such as "pro" means heavy only
// where the vendor uses it for its top model; a row with no family applies
// to every model. Heavy rows come first and win.
var modelTierTable = []struct {
	family   string
	tier     string
	prefixes []string // segment prefixes
	segments []string // whole segments
}{
	{"anthropic", TierHeavy, []string{"fable", "opus"}, nil},
	{"openai", TierHeavy, nil, []string{"astra", "pro"}},
	{"moonshot", TierHeavy, nil, []string{"pro"}},
	{"anthropic", TierLight, []string{"haiku"}, nil},
	{"openai", TierLight, nil, []string{"luna"}},
	// Smaller or free-tier models across vendors.
	{"", TierLight, nil, []string{"mini", "nano", "flash", "lite", "free"}},
}

// ModelTier labels a model by its name and the family the name identifies.
// Names without a matching row serve the standard tier.
func ModelTier(model string) string {
	family, _ := ModelFamily(model)
	return familyModelTier(family, model)
}

// AgentTier uses the model's table tier, with the family the harness
// serves when the name alone does not tell it. CLI defaults (codex, kimi and
// opencode) are standard because their model name is not known to brigada.
func AgentTier(harness, model string) string {
	if model == "default" {
		return TierStandard
	}
	family, err := AgentFamily(harness, model)
	if err != nil {
		family = ""
	}
	return familyModelTier(family, model)
}

func familyModelTier(family, model string) string {
	segments := strings.Split(normalizedModelName(model), "-")
	for _, row := range modelTierTable {
		if row.family != "" && row.family != family {
			continue
		}
		for _, segment := range segments {
			for _, prefix := range row.prefixes {
				if strings.HasPrefix(segment, prefix) {
					return row.tier
				}
			}
			if slices.Contains(row.segments, segment) {
				return row.tier
			}
		}
	}
	return TierStandard
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

var (
	// versionSegment is a segment that is only a version: 5, 6.1, v4.1.
	versionSegment = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)$`)
	// gluedVersion is a name glued to its version: qwen3.8, o4, k3.
	gluedVersion = regexp.MustCompile(`^([a-z]+)(\d+(?:\.\d+)*)$`)
	// dateSegment is a release date such as 20250514: a snapshot of a
	// version, not a version of its own.
	dateSegment = regexp.MustCompile(`^\d{6,}$`)
)

// modelLine splits a model name into its line — the name with its version
// numbers taken out, the provider prefix and context suffix kept — and its
// version. ok is false for a name with no version, such as an alias.
func modelLine(model string) (line string, version []int, ok bool) {
	name := strings.ToLower(model)
	prefix := ""
	if i := strings.LastIndex(name, "/"); i >= 0 {
		prefix, name = name[:i+1], name[i+1:]
	}
	suffix := ""
	if strings.HasSuffix(name, "]") {
		if i := strings.LastIndex(name, "["); i > 0 {
			name, suffix = name[:i], name[i:]
		}
	}
	numbers := func(s string) {
		for part := range strings.SplitSeq(s, ".") {
			n, _ := strconv.Atoi(part)
			version = append(version, n)
		}
	}
	var parts []string
	for _, segment := range strings.Split(name, "-") {
		switch m := gluedVersion.FindStringSubmatch(segment); {
		case dateSegment.MatchString(segment):
			continue
		case versionSegment.MatchString(segment):
			numbers(versionSegment.FindStringSubmatch(segment)[1])
			if len(parts) == 0 || parts[len(parts)-1] != "#" {
				parts = append(parts, "#")
			}
		case m != nil:
			numbers(m[2])
			parts = append(parts, m[1]+"#")
		default:
			parts = append(parts, segment)
		}
	}
	if len(version) == 0 {
		return "", nil, false
	}
	return prefix + strings.Join(parts, "-") + suffix, version, true
}

// Superseded maps each of models that a newer version of the same line,
// also in models, replaces to the newest model of that line. models is one
// CLI's listing. A name with no version is never superseded, and two
// snapshots of one version replace neither.
func Superseded(models []string) map[string]string {
	type entry struct {
		model   string
		version []int
	}
	lines := map[string][]entry{}
	var order []string
	for _, model := range models {
		line, version, ok := modelLine(model)
		if !ok {
			continue
		}
		if _, seen := lines[line]; !seen {
			order = append(order, line)
		}
		lines[line] = append(lines[line], entry{model, version})
	}
	out := map[string]string{}
	for _, line := range order {
		entries := lines[line]
		newest := entries[0]
		for _, e := range entries[1:] {
			if slices.Compare(e.version, newest.version) > 0 {
				newest = e
			}
		}
		for _, e := range entries {
			if slices.Compare(e.version, newest.version) < 0 {
				out[e.model] = newest.model
			}
		}
	}
	return out
}
