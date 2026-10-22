package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/landing"
)

// landClient is what land asks the engine (§7.1): reads only. The engine
// runs no git for a client (product §5 rule 1).
type landClient interface {
	Task(ctx context.Context, id int64) (engine.TaskInfo, error)
	TaskLand(ctx context.Context, id int64) (engine.TaskLandInfo, error)
	ProjectSettings(ctx context.Context, repoURL string) (engine.ProjectSettings, error)
}

// landOptions are land's flags (§7.1).
type landOptions struct {
	ids     []int64
	onto    string
	repo    string
	landed  []int64
	noCheck bool
}

// Bounds of the commit message (§7.1), kept by internal/landing.
const (
	landSubjectRunes  = landing.SubjectRunes
	landSpecBytes     = landing.SpecBytes
	landSummaryBytes  = landing.SummaryBytes
	landMessageBytes  = landing.MessageBytes
	landTruncatedMark = landing.TruncatedMark
)

// idList is a repeatable integer flag.
type idList []int64

func (l *idList) String() string { return fmt.Sprint([]int64(*l)) }
func (l *idList) Set(s string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("a task id must be a positive integer, got %q", s)
	}
	*l = append(*l, id)
	return nil
}

// runLand dispatches `brigada land` (§7.1): the hand-driven squash of
// approved tasks, or the user's decision on a land-gate hold (§6.3) with
// `land approve <task>` / `land reject <task>`. A task id is always a
// positive integer, so the subcommand words never collide with it.
func runLand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "approve" || args[0] == "reject") {
		return runLandDecision(ctx, args[0], args[1:], stdout, stderr)
	}
	return runLandByHand(ctx, args, stdout, stderr)
}

// runLandByHand is `brigada land <task-id>... [--onto BRANCH] [--repo URL]
// [--landed ID]... [--no-check]` (§7.1): approved tasks squashed onto the
// checked-out branch of the user's own repository, one commit each, the
// default check rerun, and the push command printed, never run.
func runLandByHand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("land",
		"brigada land [--onto BRANCH] [--repo URL] [--landed ID]... [--no-check] [--no-start] <task-id>...",
		"Squash approved tasks onto the checked-out branch, rerun the check and print the push command.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var opts landOptions
	var landed idList
	fs.StringVar(&opts.onto, "onto", "", "the checked-out local branch to land on (default: the project's default branch)")
	fs.StringVar(&opts.repo, "repo", "", "the repository URL the working directory stands for (default: its origin's URL)")
	fs.Var(&landed, "landed", "a dependency already landed by hand or on another clone (repeatable)")
	fs.BoolVar(&opts.noCheck, "no-check", false, "do not rerun the project's default check")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	// Task ids and flags may interleave; the flag package stops at the
	// first positional, so parse again after each one.
	var rest []string
	remaining := args
	for {
		if code, stop := common.parse(stdout, stderr, remaining); stop {
			return code
		}
		left := fs.Args()
		if len(left) == 0 {
			break
		}
		rest = append(rest, left[0])
		remaining = left[1:]
	}
	if len(rest) == 0 {
		return usagef(stderr, "usage: brigada land <task-id>... [--onto BRANCH] [--repo URL] [--landed ID]... [--no-check]")
	}
	seen := map[int64]bool{}
	for _, s := range rest {
		id, ok := parseID(stderr, "task id", s)
		if !ok {
			return 2
		}
		if !seen[id] {
			seen[id] = true
			opts.ids = append(opts.ids, id)
		}
	}
	opts.landed = landed
	dir, err := os.Getwd()
	if err != nil {
		return fail(stderr, fmt.Errorf("find the working directory: %w", err))
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	return landTasks(ctx, client, dir, opts, stdout, stderr)
}

// landTask is one task to land, with what was fetched for it.
type landTask struct {
	id     int64
	info   engine.TaskInfo
	land   engine.TaskLandInfo
	base   string // private ref of its fetched base
	branch string // private ref of its fetched result branch
	head   string // the fetched head sha
	done   bool   // already on the target, by its trailer
}

// landTasks does the whole of land in dir; the exit code is the
// command's.
func landTasks(ctx context.Context, client landClient, dir string, opts landOptions, stdout, stderr io.Writer) int {
	landedByHand := map[int64]bool{}
	for _, id := range opts.landed {
		landedByHand[id] = true
	}

	// 1. Preflight, before anything is changed. Problems are gathered so
	// one call names them all; any one refuses the whole call.
	var problems []string
	refuse := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	finish := func() int {
		fmt.Fprintf(stderr, "brigada: land refused, nothing was changed:\n")
		for _, p := range problems {
			fmt.Fprintf(stderr, "  - %s\n", p)
		}
		return 1
	}

	// A dirty tree or an operation in progress refuses first: nothing
	// below is worth reading while the tree is not the user's to land on.
	if status, err := gitText(ctx, dir, "status", "--porcelain"); err != nil {
		return fail(stderr, err)
	} else if strings.TrimSpace(status) != "" {
		refuse("the working tree is dirty (tracked changes or untracked files that are not ignored); commit, stash or clean it first")
	}
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		p, err := gitText(ctx, dir, "rev-parse", "--git-path", marker)
		if err != nil {
			return fail(stderr, err)
		}
		p = strings.TrimSpace(p)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			refuse("a merge, rebase, cherry-pick or revert is in progress (%s); finish or abort it first", marker)
			break
		}
	}

	here := opts.repo
	if here == "" {
		origin, err := gitOriginURL(dir)
		if err != nil {
			return fail(stderr, err)
		}
		here = origin
	}

	// The target: the checked-out branch, which must be the project's
	// default branch or the one named with --onto.
	current, _ := gitText(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	current = strings.TrimSpace(current)
	target := opts.onto
	if target == "" {
		def, err := defaultBranch(ctx, dir)
		if err != nil {
			return fail(stderr, err)
		}
		target = def
	}
	if current != target {
		if current == "" {
			refuse("HEAD is detached; land works on a branch: git switch %s", target)
		} else {
			refuse("the checked-out branch is %s, not %s; land never switches branches: git switch %s", current, target, target)
		}
	}

	// Read every named task.
	tasks := map[int64]*landTask{}
	for _, id := range opts.ids {
		if landedByHand[id] {
			continue
		}
		info, err := client.Task(ctx, id)
		if err != nil {
			if isNotFound(err) {
				refuse("task %d: no such task", id)
				continue
			}
			return fail(stderr, err)
		}
		li, err := client.TaskLand(ctx, id)
		if err != nil {
			return fail(stderr, err)
		}
		tasks[id] = &landTask{id: id, info: info, land: li}
	}
	// A dirty tree or a wrong branch makes the rest moot, but the tasks
	// are still named so one run says everything.
	inCall := map[int64]bool{}
	for _, id := range opts.ids {
		inCall[id] = true
	}
	var warnings []string
	for _, id := range opts.ids {
		t := tasks[id]
		if t == nil {
			continue
		}
		if sameTrailer, err := landing.Landed(ctx, dir, id); err != nil {
			return fail(stderr, err)
		} else if sameTrailer {
			t.done = true
			continue
		}
		switch {
		case t.land.Class != "implement":
			refuse("task %d: class %q has no review and no branch worth squashing; only implement tasks land", id, t.land.Class)
		case t.info.State != "done" || !t.land.Approved:
			refuse("task %d: not approved (state %s); an implement task needs an approve verdict on its last review", id, t.info.State)
		case t.info.Branch == "":
			refuse("task %d: no result branch", id)
		}
		if !sameCanonicalRepo(here, t.info.RepoURL) {
			refuse("task %d belongs to %s, but this directory is %s; run land inside that repository (--repo says which URL it stands for)", id, t.info.RepoURL, here)
		}
	}
	// Dependencies: in this call, landed, or refused.
	for _, id := range opts.ids {
		t := tasks[id]
		if t == nil || t.done {
			continue
		}
		for _, dep := range t.land.Deps {
			if inCall[dep] || landedByHand[dep] {
				continue
			}
			on, err := landing.Landed(ctx, dir, dep)
			if err != nil {
				return fail(stderr, err)
			}
			if !on {
				refuse("task %d depends on task %d, which is neither landed nor in this call; land %d first (or say --landed %d)", id, dep, dep, dep)
			}
		}
	}

	// The default check: never run unchecked.
	check := ""
	if !opts.noCheck {
		repo := here
		for _, id := range opts.ids {
			if t := tasks[id]; t != nil && t.info.RepoURL != "" {
				repo = t.info.RepoURL
				break
			}
		}
		ps, err := client.ProjectSettings(ctx, repo)
		if err != nil {
			return fail(stderr, err)
		}
		for _, s := range ps.Settings {
			if s.Key == engine.SettingDefaultCheck {
				check = strings.TrimSpace(s.Value)
				// The engine renders an unset check as "none".
				// A user may also set the literal "none"; it is no check.
				if check == "none" {
					check = ""
				}
			}
		}
		if check == "" {
			refuse("the project has no default check; set one (brigada project set) or pass --no-check")
		}
	}
	if len(problems) > 0 {
		return finish()
	}

	// 2. Fetch refs only: each task's base ref and result branch, and
	// the target's remote counterpart.
	const privRefs = "refs/brigada/land"
	for _, id := range opts.ids {
		t := tasks[id]
		if t == nil || t.done {
			continue
		}
		t.base = fmt.Sprintf("%s/%d/base", privRefs, id)
		t.branch = fmt.Sprintf("%s/%d/branch", privRefs, id)
		if err := runGit(ctx, dir, nil, stderr, "fetch", "--quiet", "--no-tags", "origin",
			"+"+t.info.BaseRef+":"+t.base, "+"+t.info.Branch+":"+t.branch); err != nil {
			refuse("task %d: fetch %s and %s from origin: %v", id, t.info.BaseRef, t.info.Branch, err)
			continue
		}
		head, err := gitText(ctx, dir, "rev-parse", t.branch)
		if err != nil {
			return fail(stderr, err)
		}
		t.head = strings.TrimSpace(head)
		if want := t.land.ApprovedHead; want == "" {
			warnings = append(warnings, fmt.Sprintf("task %d: no approved head is recorded with its verdict; the head %s was not checked against what was reviewed", id, short(t.head)))
		} else if want != t.head {
			refuse("task %d: its branch head is %s, but the review approved %s; it changed after approval", id, short(t.head), short(want))
		}
	}
	targetRef := privRefs + "/target"
	if err := runGit(ctx, dir, nil, stderr, "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/"+target+":"+targetRef); err != nil {
		if opts.onto == "" {
			refuse("fetch %s from origin: %v", target, err)
		} else {
			warnings = append(warnings, fmt.Sprintf("%s has no counterpart on origin; the behind check was skipped", target))
		}
	} else if !gitSucceeds(ctx, dir, "merge-base", "--is-ancestor", targetRef, "HEAD") {
		refuse("%s is behind or has diverged from origin/%s; pull first", target, target)
	}
	if len(problems) > 0 {
		return finish()
	}

	// 3. Squash in dependency order.
	order := landOrder(opts.ids, tasks, inCall)
	start, err := gitText(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return fail(stderr, err)
	}
	start = strings.TrimSpace(start)
	for _, w := range warnings {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	var commits, empties, already []string
	var unaddressed []string
	for _, t := range order {
		if t.done {
			already = append(already, strconv.FormatInt(t.id, 10))
			continue
		}
		msg, noAddr := landMessage(t)
		if noAddr != "" {
			unaddressed = append(unaddressed, noAddr)
		}
		patch, err := landing.Diff(ctx, dir, t.base, t.branch, stderr)
		if err != nil {
			return landStopped(stdout, stderr, start, commits, t, "", fmt.Errorf("diff %s...%s: %w", t.info.BaseRef, t.info.Branch, err))
		}
		res, err := landing.Squash(ctx, dir, t.id, msg, patch, stderr)
		if err != nil {
			var conflict *landing.Conflict
			if errors.As(err, &conflict) {
				return landConflict(stdout, stderr, start, commits, t, res.MsgPath, conflict.Files, conflict.Err)
			}
			return landStopped(stdout, stderr, start, commits, t, res.MsgPath, err)
		}
		if res.Empty {
			empties = append(empties, strconv.FormatInt(t.id, 10))
			continue
		}
		commits = append(commits, fmt.Sprintf("task %d  %s  %s", t.id, res.SHA, subjectOf(msg)))
	}

	// 4. Report; the check runs once, after the last commit.
	fmt.Fprintf(stdout, "landed on %s (started at %s):\n", target, short(start))
	for _, c := range commits {
		fmt.Fprintf(stdout, "  %s\n", c)
	}
	if len(commits) == 0 {
		fmt.Fprintln(stdout, "  no commits made")
	}
	if len(empties) > 0 {
		fmt.Fprintf(stdout, "empty diff, no commit: task %s\n", strings.Join(empties, ", "))
	}
	if len(already) > 0 {
		fmt.Fprintf(stdout, "already landed (by trailer): task %s\n", strings.Join(already, ", "))
	}
	if len(opts.landed) > 0 {
		ids := make([]string, len(opts.landed))
		for i, id := range opts.landed {
			ids[i] = strconv.FormatInt(id, 10)
		}
		fmt.Fprintf(stdout, "taken as landed on your word (--landed): task %s\n", strings.Join(ids, ", "))
	}
	for _, u := range unaddressed {
		fmt.Fprintf(stdout, "no address known for the co-author %s; the trailer names the model alone\n", u)
	}
	if opts.noCheck {
		fmt.Fprintln(stdout, "check: skipped (--no-check)")
	} else {
		fmt.Fprintf(stdout, "check: %s\n", check)
		cmd := exec.CommandContext(ctx, "sh", "-c", check)
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(stdout, "check FAILED (%v); the commits stay and nothing is undone. Reset with: git reset --hard %s\n", err, start)
			return 1
		}
		fmt.Fprintln(stdout, "check: passed")
	}
	fmt.Fprintf(stdout, "to publish, when you are ready (land never pushes):\n  git push origin %s\n", target)
	return 0
}

// landOrder is the tasks in dependency order; among tasks with no order
// between them, in the order given.
func landOrder(ids []int64, tasks map[int64]*landTask, inCall map[int64]bool) []*landTask {
	var out []*landTask
	placed := map[int64]bool{}
	left := make([]int64, 0, len(ids))
	for _, id := range ids {
		if tasks[id] != nil {
			left = append(left, id)
		}
	}
	for len(left) > 0 {
		pick := -1
		for i, id := range left {
			ready := true
			for _, d := range tasks[id].land.Deps {
				if inCall[d] && tasks[d] != nil && !placed[d] {
					ready = false
					break
				}
			}
			if ready {
				pick = i
				break
			}
		}
		if pick < 0 {
			pick = 0 // a cycle cannot exist (§6.1); do not loop on one
		}
		id := left[pick]
		left = append(left[:pick], left[pick+1:]...)
		placed[id] = true
		out = append(out, tasks[id])
	}
	return out
}

// landConflict reports a stop at a conflict: files named, the tree left
// alone, the way back printed and never run.
func landConflict(stdout, stderr io.Writer, start string, commits []string, t *landTask, msgPath string, files []string, cause error) int {
	fmt.Fprintf(stderr, "brigada: task %d does not apply cleanly onto the target; stopped\n", t.id)
	if len(files) > 0 {
		fmt.Fprintf(stderr, "conflicting files:\n")
		for _, f := range files {
			fmt.Fprintf(stderr, "  %s\n", f)
		}
	} else {
		fmt.Fprintf(stderr, "git said: %v\n", cause)
	}
	landLeft(stderr, start, commits, t, msgPath)
	return 1
}

// landStopped reports a stop that is not a conflict (a diff or a hook
// that failed), the tree left as it is.
func landStopped(stdout, stderr io.Writer, start string, commits []string, t *landTask, msgPath string, cause error) int {
	fmt.Fprintf(stderr, "brigada: task %d stopped: %v\n", t.id, cause)
	landLeft(stderr, start, commits, t, msgPath)
	return 1
}

func landLeft(w io.Writer, start string, commits []string, t *landTask, msgPath string) {
	for _, c := range commits {
		fmt.Fprintf(w, "landed before the stop: %s\n", c)
	}
	fmt.Fprintf(w, "the working tree and index are left as they are; nothing was resolved\n")
	if msgPath != "" {
		fmt.Fprintf(w, "after resolving, commit with: git commit -F %s\n", msgPath)
	}
	fmt.Fprintf(w, "then run brigada land again for the rest; task %d is recognised by its Brigada-Task trailer\n", t.id)
	fmt.Fprintf(w, "to undo everything land did (not run by land): git reset --hard %s\n", start)
}

// landMessage builds the commit message from API data alone (§7.1),
// through internal/landing. The worker's own commit_message (§5.4), when
// the task's last job carries one, takes the message's subject and body.
// The second result names a known family whose address brigada lacks.
func landMessage(t *landTask) (string, string) {
	return landing.Message(landing.Task{
		ID:            t.id,
		Title:         t.land.Title,
		Branch:        t.info.Branch,
		Head:          t.head,
		Spec:          t.land.Spec,
		Summary:       t.land.Summary,
		CommitMessage: t.land.CommitMessage,
		Family:        t.land.Family,
		Model:         t.land.Model,
	})
}

func subjectOf(msg string) string {
	if i := strings.Index(msg, "\n"); i >= 0 {
		return msg[:i]
	}
	return msg
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// gitText runs git in dir and returns its stdout.
func gitText(ctx context.Context, dir string, args ...string) (string, error) {
	var out bytes.Buffer
	if err := runGit(ctx, dir, &out, io.Discard, args...); err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// gitSucceeds reports whether git exits 0.
func gitSucceeds(ctx context.Context, dir string, args ...string) bool {
	return exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Run() == nil
}

// defaultBranch is the origin's default branch: origin/HEAD when the
// clone has it, else what origin says (a read, nothing is changed).
func defaultBranch(ctx context.Context, dir string) (string, error) {
	if out, err := gitText(ctx, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if b := strings.TrimPrefix(strings.TrimSpace(out), "origin/"); b != "" {
			return b, nil
		}
	}
	out, err := gitText(ctx, dir, "ls-remote", "--symref", "origin", "HEAD")
	if err == nil {
		for _, l := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(l, "ref: refs/heads/"); ok {
				if b, _, _ := strings.Cut(rest, "\t"); b != "" {
					return b, nil
				}
			}
		}
	}
	return "", fmt.Errorf("cannot tell origin's default branch; name the branch with --onto")
}

// sameCanonicalRepo reports whether two repository URLs name one project,
// by the engine's canonical identity (§13.1), the rule diff, attach and
// ask use. A URL that does not canonicalize matches nothing.
func sameCanonicalRepo(a, b string) bool {
	ca, err := engine.Canonicalize(a)
	if err != nil {
		return false
	}
	cb, err := engine.Canonicalize(b)
	return err == nil && ca == cb
}

// landDecider is the two calls the land-gate decision commands need from
// the API client; tests stub it.
type landDecider interface {
	LandApprove(ctx context.Context, id int64, andStopAsking bool) (engine.LandDecision, error)
	LandReject(ctx context.Context, id int64) (engine.LandDecision, error)
}

// runLandDecision is the user's yes or no on a land-gate hold (§6.3):
// approve releases the hold and queues the landing, reject cancels the
// landing and leaves the branch. --and-stop-asking (approve only) also
// changes the one setting each hold reason maps to, the way
// `brigada project set` does, so the same question is not asked twice.
// Only the client token can say it: the foreman has no way to pass the
// gate.
func runLandDecision(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) int {
	description := "Release a task's land-gate hold and queue its landing."
	if verb == "reject" {
		description = "Cancel a task's landing; its branch is left as it is."
	}
	fs := newFlagSet("land "+verb, "brigada land "+verb+" [--no-start] <task>", description)
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	stopAsking := new(bool)
	if verb == "approve" {
		fs.BoolVar(stopAsking, "and-stop-asking", false, "also change the rule that held the landing, the way brigada project set does, so the same question is not asked twice")
	}
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usagef(stderr, "usage: brigada land %s <task>", verb)
	}
	id, ok := parseID(stderr, "task id", rest[0])
	if !ok {
		return 2
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	return printLandDecision(ctx, client, verb, id, *stopAsking, stdout, stderr)
}

func printLandDecision(ctx context.Context, client landDecider, verb string, id int64, stopAsking bool, stdout, stderr io.Writer) int {
	var res engine.LandDecision
	var err error
	if verb == "approve" {
		res, err = client.LandApprove(ctx, id, stopAsking)
	} else {
		res, err = client.LandReject(ctx, id)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if verb == "approve" {
		switch {
		case res.Queued:
			fmt.Fprintf(stdout, "landing for task #%d approved: land job #%d is queued\n", res.TaskID, res.JobID)
		case res.JobID != 0:
			fmt.Fprintf(stdout, "landing for task #%d approved: no land job queued, job #%d already holds the task\n", res.TaskID, res.JobID)
		default:
			fmt.Fprintf(stdout, "landing for task #%d approved: no land job queued\n", res.TaskID)
		}
		for _, s := range res.Settings {
			fmt.Fprintf(stdout, "rule changed: %s\n", s)
		}
		return 0
	}
	fmt.Fprintf(stdout, "landing for task #%d cancelled; its branch is left as it is\n", res.TaskID)
	return 0
}
