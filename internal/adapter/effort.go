package adapter

import (
	"fmt"
	"strings"
)

// effortLevels are the effort levels brigada accepts per harness: its own
// typed interface to each CLI's reasoning-effort control, never a raw
// argument passthrough. A harness absent from the map has no effort
// control brigada can drive, and a non-empty level is refused for it.
//
// What each list rests on:
//   - claude: `claude --help` documents `--effort <level>` as low, medium,
//     high, xhigh, max.
//   - codex: `-c model_reasoning_effort="<level>"`; Codex's config
//     reference types the value as a model-dependent string, so this is
//     brigada's accepted set, not a claim that every model supports every
//     value — the CLI judges that.
//   - opencode: `opencode run --variant <v>` ("provider-specific reasoning
//     effort, e.g. high, max, minimal"); brigada maps the three the help
//     names. Real variant names depend on the provider and model, and this
//     is not an exhaustive list of them.
//   - kimi: the CLI documents no effort option; refused.
var effortLevels = map[string][]string{
	"claude":   {"low", "medium", "high", "xhigh", "max"},
	"codex":    {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
	"opencode": {"minimal", "high", "max"},
}

// EffortLevels returns the effort levels brigada accepts for harness; nil
// when the harness has no effort control.
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
