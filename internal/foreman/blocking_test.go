package foreman

import (
	"strings"
	"testing"
)

// TestContractCarriesTheMissingSectionRule pins the issue #129 line in
// the contract, matched across line wraps.
func TestContractCarriesTheMissingSectionRule(t *testing.T) {
	flat := strings.Join(strings.Fields(Instructions()), " ")
	for _, p := range []string{
		"needs_help naming a missing cited section is blocking",
		"fix the base or the spec, or ask the user, before the task continues",
		`never answer "continue without it"`,
	} {
		if !strings.Contains(flat, p) {
			t.Errorf("the contract does not carry %q", p)
		}
	}
}
