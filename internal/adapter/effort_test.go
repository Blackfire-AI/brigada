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
		{"claude", "max", true},
		{"claude", "ultra", false},
		{"claude", "none", false},
		{"codex", "minimal", true},
		{"codex", "ultra", true},
		{"codex", "turbo", false},
		{"opencode", "minimal", true},
		{"opencode", "high", true},
		{"opencode", "max", true},
		{"opencode", "low", false},
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
