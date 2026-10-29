package engine

import "fmt"

// CheckBaseFetchable reports why a task's base ref cannot be fetched by
// the land job (§7.1): the job fetches each ref by branch name
// (+refs/heads/<ref>), so a bare commit SHA, which names no branch, is
// never fetchable however reachable the commit is. Task submit, the plan
// tools and the land job all call it, so they cannot disagree.
func CheckBaseFetchable(ref string) error {
	if looksLikeCommitSHA(baseBranchOf(ref)) {
		return fmt.Errorf("base %q is a commit SHA, which the land job cannot fetch: it fetches branches by name (refs/heads/<name>); name a branch that contains the commit", ref)
	}
	return nil
}

// looksLikeCommitSHA is true for a full SHA-1 (40) or SHA-256 (64) hex id.
func looksLikeCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
