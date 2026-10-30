package defaults

import (
	"strings"
	"testing"
)

// TestMissingSectionRules pins the issue #129 rules in the default role
// texts: the worker stops and asks when a cited section is missing, and
// the foreman treats that question as blocking. Phrases are matched
// across line wraps.
func TestMissingSectionRules(t *testing.T) {
	cases := map[string][]string{
		"worker": {
			"cannot find that section",
			"end with `needs_help`",
			"Name the missing section and the base ref and commit you read",
			"Do not guess what the section says and never write it yourself",
		},
		"foreman": {
			"names a missing cited section is blocking",
			"Fix the base or the spec, or ask the user, before the task continues",
			`never answer "continue without it"`,
		},
	}
	for role, phrases := range cases {
		text, err := LiveRoleText(role)
		if err != nil {
			t.Fatal(err)
		}
		flat := strings.Join(strings.Fields(text), " ")
		for _, p := range phrases {
			if !strings.Contains(flat, p) {
				t.Errorf("%s.md does not carry %q", role, p)
			}
		}
	}
}
