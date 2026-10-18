package adapter

import (
	"fmt"
	"regexp"
	"strings"
)

// effortLevels are the effort levels brigada accepts per harness: its own
// typed interface to each CLI's reasoning-effort control, never a raw
// argument passthrough. OpenCode uses a provider-specific variant name instead
// of a fixed list. Other harnesses absent from the map refuse non-empty levels.
//
// What each list rests on:
//   - claude: `claude --help` documents `--effort <level>` as low, medium,
//     high, xhigh, max.
//   - codex: `-c model_reasoning_effort="<level>"`; Codex's config
//     reference types the value as a model-dependent string, so this is
//     brigada's accepted set, not a claim that every model supports every
//     value — the CLI judges that.
//   - opencode: `opencode run --variant <v>` accepts provider-specific names,
//     validated separately rather than restricted to a fixed list.
//   - kimi: the CLI documents no effort option; refused.
var effortLevels = map[string][]string{
	"claude": {"low", "medium", "high", "xhigh", "max"},
	"codex":  {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
}

var openCodeEffort = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

// EffortLevels returns the effort levels brigada accepts for harness; nil
// when there is no fixed list (OpenCode), or no effort control (Kimi).
func EffortLevels(harness string) []string {
	return effortLevels[harness]
}

// ValidateEffort checks level against harness's accepted set. The empty
// level (unspecified) is always valid. The error is a usage-grade message
// naming what is accepted.
func ValidateEffort(harness, level string) error {
	if level == "" {
		return nil
	}
	if harness == "opencode" {
		if openCodeEffort.MatchString(level) {
			return nil
		}
		return fmt.Errorf("effort %q is not supported for harness %q; want a variant matching [a-z0-9-]{1,16}", level, harness)
	}
	levels := effortLevels[harness]
	if levels == nil {
		return fmt.Errorf("effort is not supported for harness %q: its CLI documents no effort control", harness)
	}
	for _, l := range levels {
		if l == level {
			return nil
		}
	}
	return fmt.Errorf("effort %q is not supported for harness %q; want one of %s", level, harness, strings.Join(levels, ", "))
}
