package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// ErrInvalidReview marks a standalone review the engine will not take:
// one that names no branch.
var ErrInvalidReview = errors.New("invalid review")

// ErrNoReviewer marks a standalone review nobody can take (§7): no
// agent is ready, out of cooling, offering the review class and of a
// family other than the author's — or the author's family cannot be
// told and no default reviewer is configured.
var ErrNoReviewer = errors.New("no eligible reviewer")

// Where a standalone review learned the author's family from.
const (
	reviewSourceJob     = "job"     // the agent of the branch's last work job
	reviewSourceTrailer = "trailer" // a Co-Authored-By trailer on the branch's commits
	reviewSourceDefault = "default" // neither: the config's default reviewer
)

// ReviewBranchResult is what ReviewBranch did: the review job created —
// or the live one a repeat coalesced behind — and who it excludes and
// why.
type ReviewBranchResult struct {
	Answer    Answer `json:"answer"`
	ProjectID int64  `json:"project_id"`
	JobID     int64  `json:"job_id"`
	Run       int    `json:"run,omitempty"`
	// Family is the author's family the reviewer must differ from; empty
	// when it could not be told and the default reviewer was taken.
	Family string `json:"family,omitempty"`
	// Source is where the family came from: job, trailer or default.
	Source string `json:"source"`
}

// ReviewBranch queues a review of a branch with no plan, no task and no
// foreman behind it (§7, `brigada review --branch`). The author's
// family is the family of the agent of the branch's last work job; a
// branch with no such job, or one whose agent is gone or has no family,
// falls back to the Co-Authored-By trailers of its commits, and then to
// the config's default reviewer, which takes the review with no family
// rule. The review is placed by the review policy's cross-family rule
// among the agents that are ready, out of cooling and offer the review
// class — and, as for any review, the lease picks among them by the
// dispatcher's ranking. No such agent is ErrNoReviewer. The project is
// the one with repoURL, or the engine's only one.
func (e *Engine) ReviewBranch(ctx context.Context, repoURL, branch string) (ReviewBranchResult, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ReviewBranchResult{}, fmt.Errorf("%w: a branch is required", ErrInvalidReview)
	}
	var in struct {
		project int64
		repo    string
		base    string
		family  string
	}
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		project, err := askProjectTx(tx, repoURL)
		if err != nil {
			return err
		}
		in.project = project
		if err := tx.QueryRow("SELECT COALESCE(NULLIF(transport_url, ''), repo_url), default_branch FROM projects WHERE id = ?", project).
			Scan(&in.repo, &in.base); err != nil {
			return fmt.Errorf("read project %d: %w", project, err)
		}
		// The job record is the family's first witness (§7): the agent
		// of the branch's newest work job.
		agent, err := reviewAuthorTx(tx, &jobRow{ProjectID: project, Kind: KindReview, Branch: branch})
		if err != nil {
			return err
		}
		in.family, err = agentFamilyTx(tx, agent)
		return err
	})
	if err != nil {
		return ReviewBranchResult{}, err
	}
	// The trailers come from git, not from the store: read outside any
	// transaction. A repo that cannot be read leaves no trailers.
	var trailers string
	if in.family == "" {
		read := e.cfg.BranchTrailers
		if read == nil {
			read = gitBranchTrailers
		}
		if text, terr := read(ctx, in.repo, in.base, branch); terr == nil {
			trailers = text
		} else if e.cfg.Warnf != nil {
			e.cfg.Warnf("read the Co-Authored-By trailers of %s: %v", branch, terr)
		}
	}
	var res ReviewBranchResult
	err = e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		review := &jobRow{ProjectID: in.project, Kind: KindReview, Branch: branch}
		family, source, harness := in.family, reviewSourceJob, ""
		if family == "" {
			var err error
			family, err = trailerFamilyTx(tx, trailers)
			if err != nil {
				return err
			}
			source = reviewSourceTrailer
		}
		if family == "" {
			source, harness = reviewSourceDefault, e.cfg.DefaultReviewer
			if harness == "" {
				onlyAuthor, err := onlyAuthorReviewerTx(tx, review)
				if err != nil {
					return err
				}
				if onlyAuthor {
					return fmt.Errorf("%w: %s", ErrNoReviewer, noReviewerAuthor)
				}
				return fmt.Errorf("%w: the author's family of %s is unknown (no work job of the branch, no known Co-Authored-By trailer) and no default_reviewer is configured", ErrNoReviewer, branch)
			}
		}
		review.ExcludeFamily, review.Harness = family, harness
		cands, err := eligibleAgentsTx(tx, e.cfg.Caps, 0, review,
			string(KindReview), fmtTime(now))
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			onlyAuthor, err := onlyAuthorReviewerTx(tx, review)
			if err != nil {
				return err
			}
			if onlyAuthor {
				return fmt.Errorf("%w: %s", ErrNoReviewer, noReviewerAuthor)
			}
			if family != "" {
				return fmt.Errorf("%w: no agent of a family other than %q is ready and offers the review class", ErrNoReviewer, family)
			}
			return fmt.Errorf("%w: no ready agent of the default reviewer's harness %q offers the review class", ErrNoReviewer, harness)
		}
		instructions, _, err := projectInstructionsTx(tx, in.project)
		if err != nil {
			return err
		}
		sr, err := e.submitTx(tx, SubmitParams{
			Key:           fmt.Sprintf("review:%d:%s", in.project, branch),
			ProjectID:     in.project,
			Kind:          KindReview,
			Mode:          ModeFresh,
			Prompt:        prompt.ComposeStandaloneReviewPrompt(in.base, branch, prompt.WithInstructions(instructions)),
			Harness:       harness,
			ExcludeFamily: family,
			Branch:        branch,
		}, now)
		if err != nil {
			return fmt.Errorf("submit the review of %s: %w", branch, err)
		}
		res = ReviewBranchResult{Answer: sr.Answer, ProjectID: in.project, JobID: sr.JobID, Run: sr.Run, Family: family, Source: source}
		return nil
	})
	return res, err
}

// trailerFamilyTx maps Co-Authored-By trailer values to a family (§7):
// the family of the registered agent whose family, harness or model the
// trailers name — the longest such name wins, so "claude-sonnet" beats
// "claude". Empty when the trailers name none, or hold nothing.
func trailerFamilyTx(tx *sql.Tx, trailers string) (string, error) {
	text := strings.ToLower(trailers)
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	rows, err := tx.Query("SELECT family, harness, model FROM agents ORDER BY id")
	if err != nil {
		return "", fmt.Errorf("list the agents: %w", err)
	}
	defer rows.Close()
	best, bestLen := "", 0
	for rows.Next() {
		var family, harness, model string
		if err := rows.Scan(&family, &harness, &model); err != nil {
			return "", fmt.Errorf("list the agents: %w", err)
		}
		if family == "" {
			continue
		}
		for _, name := range []string{family, harness, model} {
			name = strings.ToLower(name)
			if len(name) > bestLen && strings.Contains(text, name) {
				best, bestLen = family, len(name)
			}
		}
	}
	return best, rows.Err()
}

// gitBranchTrailers is the default BranchTrailers: it fetches the base
// and the branch from the remote into a scratch repository, blobs left
// out, and prints the Co-Authored-By trailer values of the commits the
// branch holds beyond the base.
func gitBranchTrailers(ctx context.Context, repoURL, base, branch string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "brigada-review-")
	if err != nil {
		return "", fmt.Errorf("scratch repository: %w", err)
	}
	defer os.RemoveAll(dir)
	if _, err := reviewGit(ctx, dir, "init", "-q", "--bare"); err != nil {
		return "", err
	}
	if _, err := reviewGit(ctx, dir, "fetch", "-q", "--no-tags", "--filter=blob:none", repoURL,
		"+refs/heads/"+base+":refs/heads/base", "+refs/heads/"+branch+":refs/heads/branch"); err != nil {
		return "", err
	}
	return reviewGit(ctx, dir, "log", "--format=%(trailers:key=Co-Authored-By,valueonly,unfold)", "base..branch")
}

// reviewGit runs one git command in dir with prompts off — a remote that
// wants credentials the engine lacks fails instead of hanging — and
// returns its stdout; the error carries git's own stderr.
func reviewGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// standaloneReviewCompletedTx applies the end of a review that has no
// task (§7): the verdict block read as a task's review reads it, the
// substantive approve counted on the outcome record the same way, and
// one review_verdict event — on the project's history with the job and
// its branch — saying what came of it. There is no task to move: a
// changes verdict starts no fix round, and a review with no usable
// verdict is not retried; the event says so and the person asking runs
// the review again.
func (e *Engine) standaloneReviewCompletedTx(tx *sql.Tx, job *jobRow, c Completion, now time.Time) error {
	var v verdictBlock
	haveVerdict := c.Error == "" && json.Unmarshal([]byte(c.Result), &v) == nil &&
		(v.Verdict == verdictApprove || v.Verdict == verdictChanges)
	p := eventPayload{Branch: job.Branch}
	switch {
	case haveVerdict && v.Verdict == verdictApprove && len(v.Findings) >= minSubstantiveFindings:
		if err := recordOutcomeTx(tx, job, now); err != nil {
			return err
		}
		p.Status, p.Findings = verdictApprove, boundNote(v.Findings)
	case haveVerdict && v.Verdict == verdictChanges:
		p.Status, p.Findings = verdictChanges, boundNote(v.Findings)
	default:
		p.Status = "no_verdict"
		switch {
		case c.Error != "":
			p.Reason = boundNote(c.Error)
		case haveVerdict:
			p.Reason = "approve without substantive findings"
		default:
			p.Reason = "no verdict in the review result"
		}
	}
	_, err := appendEventTx(tx, evReviewVerdict, job.ProjectID, 0, job.ID, p, now)
	return err
}
