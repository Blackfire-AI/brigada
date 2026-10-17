package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

// repoList is a repeatable --repo flag.
type repoList []string

func (r *repoList) String() string     { return strings.Join(*r, ",") }
func (r *repoList) Set(v string) error { *r = append(*r, v); return nil }

// runRecover rebuilds a lost store (§6.4): the newest snapshot of the
// configured snapshot directory is restored into a fresh store file, then
// the plan files on the repos' plans branches are applied. It works on
// the state directory itself, so the engine must be stopped; the engine's
// lock refuses it otherwise. The tokens live in the state directory's
// token file, outside the store, and stay as they are.
func runRecover(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("recover",
		"brigada recover [--snapshot-dir DIR] [--repo URL]...",
		"Rebuild a lost store from the newest snapshot and repository plan files.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var repos repoList
	var snapshotDir string
	fs.Var(&repos, "repo", "a repository URL whose plan files to apply, beyond the snapshot's projects (repeatable)")
	fs.StringVar(&snapshotDir, "snapshot-dir", "", "the snapshot directory (default: the config file's snapshot_dir)")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "recover takes no positional arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	if snapshotDir == "" {
		snapshotDir = rt.file.SnapshotDir
	}
	rep, err := lifecycle.Recover(ctx, lifecycle.RecoverConfig{
		StateDir:    rt.stateDir,
		SnapshotDir: snapshotDir,
		Repos:       repos,
	})
	if err != nil {
		return fail(stderr, err)
	}

	if rep.MovedAside != "" {
		fmt.Fprintf(stdout, "the old store was moved aside to %s\n", rep.MovedAside)
	}
	if rep.Snapshot != "" {
		fmt.Fprintf(stdout, "restored the store from the snapshot %s\n", rep.Snapshot)
	} else {
		fmt.Fprintf(stdout, "no snapshot to restore from (snapshot_dir %q): the store was started empty and rebuilt from plan files alone\n", snapshotDir)
		fmt.Fprintln(stdout, "NOT restored without a snapshot: messages, transcripts and outcomes — the dispatcher's scores restart from defaults")
	}
	for _, p := range rep.Plans {
		line := fmt.Sprintf("plan #%d (%s): %s", p.PlanID, p.Repo, p.Action)
		if p.Detail != "" {
			line += " — " + p.Detail
		}
		fmt.Fprintln(stdout, line)
	}
	if len(rep.Plans) == 0 {
		fmt.Fprintln(stdout, "no plan files were applied")
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(stderr, "brigada: warning: %s\n", w)
	}
	if rep.Snapshot != "" {
		fmt.Fprintln(stdout, "what the snapshot did not hold is not restored: anything since it was taken, other than plans rebuilt from their files")
	}
	fmt.Fprintf(stdout, "fence epoch is now %d: a lease from before the recovery is refused\n", rep.Epoch)
	fmt.Fprintf(stdout, "dispatch is held for %s (one lease window), so runners that lost their heartbeats have stopped their workers\n", rep.Hold)
	fmt.Fprintln(stdout, "tokens live outside the store and still work: clients, runners and the foreman authenticate as before; runners re-register on their next poll")
	return 0
}
