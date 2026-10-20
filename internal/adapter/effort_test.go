package adapter

import (
	"strings"
	"testing"
)

func TestValidateEffort(t *testing.T) {
	cases := []struct {
		harness, level string
		ok             bool
	}{
		{"claude", "", true},
		{"claude", "low", true},
		{"claude", "medium", true},
		{"claude", "high", true},
		{"claude", "xhigh", true},
		{"claude", "max", true},
		{"claude", "ultra", false},
		{"claude", "none", false},
		{"codex", "minimal", true},
		{"codex", "ultra", true},
		{"codex", "turbo", false},
		{"opencode", "minimal", true},
		{"opencode", "high", true},
		{"opencode", "max", true},
		{"opencode", "low", true},
		{"opencode", "medium", true},
		{"opencode", "xhigh", true},
		{"opencode", "turbo", true},
		{"opencode", "a1-custom", true},
		{"opencode", "a", true},
		{"opencode", "1234567890123456", true},
		{"opencode", "12345678901234567", false},
		{"opencode", "bad level", false},
		{"opencode", "HIGH", false},
		{"opencode", "bad_level", false},
		{"opencode", "high\n", false},
		{"dsh", "", true},
		{"dsh", "off", true},
		{"dsh", "low", true},
		{"dsh", "high", true},
		{"dsh", "max", true},
		{"dsh", "medium", false},
		{"dsh", "xhigh", false},
		{"kimi", "", true},
		{"kimi", "high", false},
		{"fake", "high", false},
	}
	for _, tc := range cases {
		err := ValidateEffort(tc.harness, tc.level)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateEffort(%q, %q) = %v, want ok=%v", tc.harness, tc.level, err, tc.ok)
		}
	}
	if err := ValidateEffort("kimi", "high"); err == nil || !strings.Contains(err.Error(), "kimi") {
		t.Errorf("kimi refusal = %v, want it to name the harness", err)
	}
}

func TestTaskTierAndEffortVocabulary(t *testing.T) {
	for _, tier := range append([]string{""}, Tiers...) {
		if err := ValidateTier(tier); err != nil {
			t.Fatal(err)
		}
	}
	for _, effort := range append([]string{""}, TaskEffortLevels...) {
		if err := ValidateTaskEffort(effort); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateTier("big"); err == nil || !strings.Contains(err.Error(), "light, standard, heavy") {
		t.Fatalf("bad tier: %v", err)
	}
	for _, effort := range []string{"ultra", "xhigh", "minimal", "none"} {
		if err := ValidateTaskEffort(effort); err == nil || !strings.Contains(err.Error(), "low, medium, high, max") {
			t.Fatalf("bad task effort %s: %v", effort, err)
		}
	}
	// Harness-only levels remain available on agents.
	if err := ValidateEffort("codex", "ultra"); err != nil {
		t.Fatal(err)
	}
}

func TestEffortForHarness(t *testing.T) {
	for _, tc := range []struct{ harness, effort, want string }{
		{"claude", "high", "high"}, {"codex", "max", "max"},
		{"opencode", "low", "minimal"}, {"opencode", "medium", ""},
		{"opencode", "high", "high"}, {"opencode", "max", "max"},
		{"dsh", "low", "low"}, {"dsh", "medium", ""}, {"dsh", "high", "high"}, {"dsh", "max", "max"},
		{"kimi", "high", ""}, {"unknown", "max", ""},
		{"claude", "", ""}, {"codex", "", ""}, {"opencode", "", ""}, {"dsh", "", ""}, {"kimi", "", ""},
	} {
		if got := EffortForHarness(tc.harness, tc.effort); got != tc.want {
			t.Errorf("%s/%s = %q, want %q", tc.harness, tc.effort, got, tc.want)
		}
	}
}
