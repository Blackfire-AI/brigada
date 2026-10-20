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
//   - dsh: the patch file's reasoningEffort takes off, low, high, max (no
//     medium); the adapter writes it into a per-job patch file.
//   - kimi: the CLI documents no effort option; refused.
var effortLevels = map[string][]string{
	"claude": {"low", "medium", "high", "xhigh", "max"},
	"codex":  {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
	"dsh":    {"off", "low", "high", "max"},
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

// Task tiers describe how heavy a model a task needs.
const (
	TierLight    = "light"
	TierStandard = "standard"
	TierHeavy    = "heavy"
)

var Tiers = []string{TierLight, TierStandard, TierHeavy}

// TaskEffortLevels is brigada's task vocabulary, independent of harness levels.
var TaskEffortLevels = []string{"low", "medium", "high", "max"}

// ValidateTier accepts an unset tier, which means standard.
func ValidateTier(tier string) error {
	if tier == "" {
		return nil
	}
	for _, word := range Tiers {
		if tier == word {
			return nil
		}
	}
	return fmt.Errorf("tier %q is invalid; want one of %s", tier, strings.Join(Tiers, ", "))
}

// ValidateTaskEffort accepts an unset effort, which uses the agent's level.
func ValidateTaskEffort(level string) error {
	if level == "" {
		return nil
	}
	for _, word := range TaskEffortLevels {
		if level == word {
			return nil
		}
	}
	return fmt.Errorf("task effort %q is invalid; want one of %s", level, strings.Join(TaskEffortLevels, ", "))
}

// EffortForHarness maps a task's brigada effort level to the CLI's control.
// An empty result leaves the CLI variant or effort unspecified.
func EffortForHarness(harness, taskEffort string) string {
	switch harness {
	case "claude", "codex":
		return taskEffort
	case "opencode":
		switch taskEffort {
		case "low":
			return "minimal"
		case "high", "max":
			return taskEffort
		}
	case "dsh":
		// dsh has no medium: omitting the key is the provider default.
		switch taskEffort {
		case "low", "high", "max":
			return taskEffort
		}
	}
	return ""
}
