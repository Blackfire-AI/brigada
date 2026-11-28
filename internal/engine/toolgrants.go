package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ToolGrantAllowlist is the binary's whole set of per-task tool grants
// (§6.1): the Claude Code permission patterns a task's tool_grants may
// name, each the narrowest the permission syntax expresses for the forms
// that leave committing to the runner: `git merge --no-commit`,
// `git cherry-pick -n`, `git restore`, `git add`. A pattern is still a
// prefix rule, so two forms cannot be narrowed: Bash(git checkout:*) is
// one prefix for `git checkout <ref> -- <path>` and for a branch switch,
// and Bash(git merge --no-commit:*) still fast-forwards a merge that can
// (git's --no-commit stops no fast-forward). The runner's branch guard
// (§5.2, the branch_guard kind) covers both: before it commits a job's
// result it refuses a worktree off the task branch or its base, and, on a
// task with tool grants, any commit the worker made itself. On the
// default claude agent (bypassPermissions) a grant widens nothing, that
// scope already permitting these commands; its value is widening narrow
// pattern-scoped agents, the approval trigger and visibility.
var ToolGrantAllowlist = []string{
	"Bash(git merge --no-commit:*)",
	"Bash(git checkout:*)",
	"Bash(git restore:*)",
	"Bash(git cherry-pick -n:*)",
	"Bash(git add:*)",
}

// legacyToolGrants maps the prefix patterns the allowlist held before
// issue #277 onto their narrow forms. A task, plan file, plan mirror or
// approval_tool_grants value recorded with one still loads, and every
// reader sees the narrow form: a new plan_create, plan_amend or task
// submit naming one is accepted and stores the narrow form.
var legacyToolGrants = map[string]string{
	"Bash(git merge:*)":       "Bash(git merge --no-commit:*)",
	"Bash(git cherry-pick:*)": "Bash(git cherry-pick -n:*)",
}

// NarrowToolGrant is a grant's current form: a legacy pattern's narrow
// form, any other pattern itself.
func NarrowToolGrant(g string) string {
	if n, ok := legacyToolGrants[g]; ok {
		return n
	}
	return g
}

// narrowToolGrants maps a list of grants onto their current forms, in
// order, each once. nil stays nil and an empty list stays empty: a
// respec's [] clears where nil keeps.
func narrowToolGrants(grants []string) []string {
	if grants == nil {
		return nil
	}
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		if n := NarrowToolGrant(g); !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// validateToolGrants checks a task's grants as its creator names them —
// every pattern is one of the allowlist, or a legacy pattern that maps
// onto one — and returns them in their narrow forms. The refusal names
// the pattern and the allowlist.
func validateToolGrants(grants []string) ([]string, error) {
	for _, g := range grants {
		if !slices.Contains(ToolGrantAllowlist, NarrowToolGrant(g)) {
			return nil, fmt.Errorf("tool_grants: %q is not in the allowlist (%s)", g, strings.Join(ToolGrantAllowlist, ", "))
		}
	}
	return narrowToolGrants(grants), nil
}

// parseTaskToolGrants reads a tasks.tool_grants column in the grants'
// narrow forms, so a row recorded with a legacy pattern leases, shows and
// is judged as its narrow form; the empty default, or a value that is not
// a JSON array, reads as no grants.
func parseTaskToolGrants(raw string) []string {
	var grants []string
	if json.Unmarshal([]byte(raw), &grants) != nil || len(grants) == 0 {
		return nil
	}
	return narrowToolGrants(grants)
}

// taskToolGrantsJSON is the tasks.tool_grants column for a validated
// list: a JSON array, the empty default when it grants nothing.
func taskToolGrantsJSON(grants []string) string {
	if len(grants) == 0 {
		return ""
	}
	b, err := json.Marshal(narrowToolGrants(grants))
	if err != nil {
		return ""
	}
	return string(b)
}
