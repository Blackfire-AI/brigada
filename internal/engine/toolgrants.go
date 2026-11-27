package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ToolGrantAllowlist is the binary's whole set of per-task tool grants
// (§6.1): the Claude Code permission patterns a task's tool_grants may
// name. They are prefix rules — Bash(git checkout:*) also matches a
// branch switch — and the allowlist is not the security boundary: the
// pipeline's structure is (§5.2), the worktree carrying no push URL and
// the runner alone committing, pushing and landing. On the default
// claude agent (bypassPermissions) a grant widens nothing, that scope
// already permitting these commands; its value is widening narrow
// pattern-scoped agents, the approval trigger and visibility.
var ToolGrantAllowlist = []string{
	"Bash(git merge:*)",
	"Bash(git checkout:*)",
	"Bash(git restore:*)",
	"Bash(git cherry-pick:*)",
	"Bash(git add:*)",
}

// validateToolGrants checks a task's grants as its creator names them:
// every pattern is one of the allowlist. The refusal names the pattern.
func validateToolGrants(grants []string) error {
	for _, g := range grants {
		if !slices.Contains(ToolGrantAllowlist, g) {
			return fmt.Errorf("tool_grants: %q is not in the allowlist (%s)", g, strings.Join(ToolGrantAllowlist, ", "))
		}
	}
	return nil
}

// parseTaskToolGrants reads a tasks.tool_grants column; the empty
// default, or a value that is not a JSON array, reads as no grants.
func parseTaskToolGrants(raw string) []string {
	var grants []string
	if json.Unmarshal([]byte(raw), &grants) != nil || len(grants) == 0 {
		return nil
	}
	return grants
}

// taskToolGrantsJSON is the tasks.tool_grants column for a validated
// list: a JSON array, the empty default when it grants nothing.
func taskToolGrantsJSON(grants []string) string {
	if len(grants) == 0 {
		return ""
	}
	b, err := json.Marshal(grants)
	if err != nil {
		return ""
	}
	return string(b)
}
