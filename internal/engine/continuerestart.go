package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// The continue-from-stopped-run restart (§5.6): task_restart with the
// continue option on a task a time-capped or stalled run parked. The
// fresh attempt is seeded from what that run pushed — the result branch
// its stalled completion names, or, for a run the branch guard refused,
// the salvage branch the runner pushed its work to — through the same
// seed a stop-running restart uses. Never automatic: the foreman asks.

// RestartOptions are task_restart's options (§5.6).
type RestartOptions struct {
	// Continue starts the fresh attempt from the stopped run's pushed
	// work instead of the task's base. Valid only for a task parked by a
	// time-capped or stalled run that pushed work; refused otherwise.
	Continue bool
}

// StoppedRunText marks a branch-guard refusal of a run the runner's own
// policy had stopped (§5.6): the guard's error goes on to name why the
// run was stopped. The runner writes it; the engine reads it to tell a
// refused stopped run from a refused done one.
const StoppedRunText = "the run was stopped: "

// SalvageBranch is the branch the runner pushes a refused or failed
// run's work to (§5.2): the job's branch, -salvage-, the job's id.
func SalvageBranch(branch string, job int64) string {
	return fmt.Sprintf("%s-salvage-%d", branch, job)
}

// SalvagedText is the words a completion's error names its salvage
// branch with.
func SalvagedText(branch string) string {
	return "edits saved on branch " + branch
}

// namesSalvage reports whether a completion's error names branch as the
// branch its work was saved on: the words, ending there.
func namesSalvage(text, branch string) bool {
	words := SalvagedText(branch)
	for i := strings.Index(text, words); i >= 0; {
		end := i + len(words)
		if end == len(text) || strings.ContainsRune(" ;,(\n", rune(text[end])) {
			return true
		}
		next := strings.Index(text[end:], words)
		if next < 0 {
			break
		}
		i = end + next
	}
	return false
}

// diffstatFields is the size of a stopped run's pushed branch against
// the task's base, as the runner's result block carries it.
type diffstatFields struct {
	Files   int64 `json:"files"`
	Added   int64 `json:"added"`
	Removed int64 `json:"removed"`
}

// stoppedRunWork is the branch a time-capped or stalled run's work sits
// on and its size, for the task_failed payload (§5.6): the result branch
// a stopped completion names, with the diffstat the runner computed; or,
// for a stopped run the branch guard refused, the salvage branch its
// error names — the size is in the error's words then. Empty for any
// other completion, and for a stopped run that pushed nothing.
func stoppedRunWork(job *jobRow, c Completion) (string, diffstatFields) {
	if job.Kind != KindWork && job.Kind != KindFix {
		return "", diffstatFields{}
	}
	if isStopOutcome(c.Outcome) {
		var wr struct {
			Branch   string          `json:"branch"`
			Diffstat *diffstatFields `json:"diffstat"`
		}
		if json.Unmarshal([]byte(c.Result), &wr) != nil || wr.Branch == "" {
			return "", diffstatFields{}
		}
		if wr.Diffstat == nil {
			return wr.Branch, diffstatFields{}
		}
		return wr.Branch, *wr.Diffstat
	}
	if salvage := stoppedSalvage(job, c.ErrorKind, c.Error); salvage != "" {
		return salvage, diffstatFields{}
	}
	return "", diffstatFields{}
}

// stoppedSalvage is the salvage branch of a stopped run the branch guard
// refused: derived from the job (SalvageBranch), and only when the
// error is a stopped run's refusal and names that branch. Empty for
// anything else, a refused run that salvaged nothing included.
func stoppedSalvage(job *jobRow, kind, text string) string {
	if kind != ErrorKindBranchGuard || job.Branch == "" || !strings.Contains(text, StoppedRunText) {
		return ""
	}
	branch := SalvageBranch(job.Branch, job.ID)
	if !namesSalvage(text, branch) {
		return ""
	}
	return branch
}

// continueSeed is what a continue restart seeds from: the branch, and
// whether it is a salvage branch the remote must be asked about first,
// with the repository to ask.
type continueSeed struct {
	Branch  string
	Salvage bool
	Repo    string
}

// continueSeedTx finds the branch a continue restart of the task seeds
// from (§5.6). The task must be parked, and parked by a task_failed
// whose job is a work or fix run the runner stopped — time-capped or
// stalled — and that left work: the branch its stalled completion
// pushed, or the salvage branch a branch-guard refusal of it names.
// Anything else refuses with the reason.
func continueSeedTx(tx *sql.Tx, project, taskID int64) (continueSeed, error) {
	if err := parkedTaskTx(tx, project, taskID); err != nil {
		return continueSeed{}, err
	}
	var kind string
	var jobID int64
	kinds := "'" + strings.Join(parkingEventKinds, "', '") + "'"
	err := tx.QueryRow(`SELECT kind, COALESCE(job_id, 0) FROM events
		WHERE task_id = ? AND kind IN (`+kinds+`)
		ORDER BY id DESC LIMIT 1`, taskID).Scan(&kind, &jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return continueSeed{}, fmt.Errorf("task %d is parked with no parking event", taskID)
	}
	if err != nil {
		return continueSeed{}, fmt.Errorf("read what parked task %d: %w", taskID, err)
	}
	notStopped := fmt.Errorf("task %d is parked by %s, not by a time-capped or stalled run: there is no stopped run to continue; restart it without continue", taskID, kind)
	if kind != evTaskFailed || jobID == 0 {
		return continueSeed{}, notStopped
	}
	job, err := loadJobTx(tx, jobID)
	if err != nil {
		return continueSeed{}, err
	}
	if job == nil || (job.Kind != KindWork && job.Kind != KindFix) {
		return continueSeed{}, notStopped
	}
	noWork := fmt.Errorf("task %d's stopped run (job %d) pushed no work to continue from; restart it without continue", taskID, jobID)
	switch job.Status {
	case statusStalled:
		wr, ok := parseWorkerResult(jobResultText(job.Result))
		if !ok || wr.Branch == "" {
			return continueSeed{}, noWork
		}
		return continueSeed{Branch: wr.Branch}, nil
	case statusFailed:
		var errKind string
		if err := tx.QueryRow(`SELECT COALESCE(error_kind, '') FROM jobs WHERE id = ?`, jobID).Scan(&errKind); err != nil {
			return continueSeed{}, fmt.Errorf("read job %d: %w", jobID, err)
		}
		var stored struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(job.Result), &stored)
		if errKind != ErrorKindBranchGuard || !strings.Contains(stored.Error, StoppedRunText) {
			return continueSeed{}, fmt.Errorf("task %d is parked by a failed run (job %d), not a time-capped or stalled one: there is no stopped run to continue; restart it without continue", taskID, jobID)
		}
		branch := stoppedSalvage(job, errKind, stored.Error)
		if branch == "" {
			return continueSeed{}, noWork
		}
		var repo string
		if err := tx.QueryRow(`SELECT COALESCE(NULLIF(transport_url, ''), repo_url) FROM projects WHERE id = ?`,
			job.ProjectID).Scan(&repo); err != nil {
			return continueSeed{}, fmt.Errorf("read the repository of project %d: %w", job.ProjectID, err)
		}
		return continueSeed{Branch: branch, Salvage: true, Repo: repo}, nil
	}
	return continueSeed{}, notStopped
}

// confirmSalvage asks the remote whether the salvage branch a continue
// restart would seed from is there (§5.6): an absent one is a run with
// no pushed work, and a remote that cannot be asked refuses too rather
// than seed from a branch nobody saw.
func (e *Engine) confirmSalvage(ctx context.Context, cs continueSeed) error {
	exists := e.cfg.RemoteBranchExists
	if exists == nil {
		exists = gitRemoteBranchExists
	}
	ok, err := exists(ctx, cs.Repo, cs.Branch)
	if err != nil {
		return fmt.Errorf("could not confirm the salvage branch %s on the remote: %w", cs.Branch, err)
	}
	if !ok {
		return fmt.Errorf("the stopped run's salvage branch %s is not on the remote: no pushed work to continue from; restart it without continue", cs.Branch)
	}
	return nil
}

// gitRemoteBranchExists is the default RemoteBranchExists: git ls-remote
// for the one branch.
func gitRemoteBranchExists(ctx context.Context, repoURL, branch string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := reviewGit(ctx, os.TempDir(), "ls-remote", repoURL, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(out)
	return len(fields) >= 2 && fields[1] == "refs/heads/"+branch, nil
}
