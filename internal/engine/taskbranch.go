package engine

import (
	"regexp"
	"strconv"
	"strings"
)

// The task-branch family (§13.2): the reserved refs under brigada/ a
// task's jobs push — brigada/task-<id> for the first attempt,
// brigada/task-<id>-a<n> for a re-leased attempt (the runner's fallback
// naming when a grant names no branch), brigada/task-<id>-r<n> for the
// task's n-th restart round (jobBranchTx), and
// <branch>-salvage-<jobid> for the edits a failed run left.
// ParseTaskBranchRef is the family's one parser: every place that
// prunes refs — the land-time deletion, the engine-start sweep —
// filters the remote's branches through it and makes no name test of
// its own, so the land target, the plans branch, the default branch
// and a user's branch are never matched.

// taskBranchPrefix starts every branch of the family; jobBranchTx names
// them with it.
const taskBranchPrefix = "brigada/task-"

// taskBranchSuffix is all that may follow the task id in a family name:
// an attempt or restart counter, a salvage, or a counter's salvage.
var taskBranchSuffix = regexp.MustCompile(`^(-[ar][0-9]+)?(-salvage-[0-9]+)?$`)

// ParseTaskBranchRef maps a branch name of the task-branch family to
// its task id and suffix (§13.2): "" for a first attempt, else "-a<n>",
// "-r<n>", "-salvage-<jobid>" or a counter's salvage. ok is false for
// every other name — and the id cut is exact, so a task id that merely
// shares a numeric prefix (brigada/task-12 against task 1) parses as
// its own task, never the shorter one's.
func ParseTaskBranchRef(ref string) (taskID int64, suffix string, ok bool) {
	rest, found := strings.CutPrefix(ref, taskBranchPrefix)
	if !found {
		return 0, "", false
	}
	digits := rest
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		digits, suffix = rest[:i], rest[i:]
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id <= 0 || !taskBranchSuffix.MatchString(suffix) {
		return 0, "", false
	}
	return id, suffix, true
}
