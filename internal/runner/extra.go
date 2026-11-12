package runner

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// Extra repositories (§5.8). A task may name other repositories it reads
// and never changes. The runner checks each out beside the task's worktree,
// at <worktree>.extra/<name>, with its own git credentials; the checkout has
// no remote and read-only files, and the runner never pushes it: it pushes
// the task's own branch alone. Whatever a worker with a wider tool scope than
// the default does to it is discarded with the checkout.

// extraRoot is the directory the job's extra checkouts live in: a sibling of
// the worktree, inside the runner's state directory, never in the worktree
// itself, so nothing in it can be committed by accident.
func extraRoot(stateDir string, grant *api.JobGrant) string {
	return worktreeDir(stateDir, grant) + ".extra"
}

// prepareExtras checks out the grant's extra repositories and returns the
// environment that names them, BRIGADA_REPO_<NAME>=<absolute path>, and the
// commit each resolved to. It returns a short reason, never git's output
// (which is logged), when one cannot be fetched: that is an infrastructure
// failure of the job (§5.8), delivered like a failed setup.
func (r *jobRun) prepareExtras(ctx context.Context) string {
	if len(r.grant.ExtraRepos) == 0 {
		return ""
	}
	root, err := filepath.Abs(extraRoot(r.cfg.StateDir, r.grant))
	if err != nil {
		return "setup failed: extra repositories: " + err.Error()
	}
	// A crashed run of the same job may have left its checkouts.
	removeAllWritable(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "setup failed: extra repositories: " + err.Error()
	}
	ws := &workspace{gitEnv: r.cfg.GitEnv}
	commits := map[string]string{}
	var env []string
	var dirs []string
	for _, x := range r.grant.ExtraRepos {
		// The engine validated these when the task was made; a grant from
		// an engine that did not is refused here rather than trusted.
		if !validExtraName(x.Name) || x.URL == "" || strings.HasPrefix(x.Ref, "-") {
			return fmt.Sprintf("setup failed: extra repository %q is malformed", x.Name)
		}
		dir := filepath.Join(root, x.Name)
		sha, err := ws.checkoutExtra(ctx, x, dir)
		if err != nil {
			if ctx.Err() == nil {
				r.cfg.Log.WarnContext(ctx, "extra repository not checked out",
					"job_id", r.grant.JobID, "name", x.Name, "repo", x.Repo, "err", err)
			}
			return fmt.Sprintf("setup failed: extra repository %s could not be checked out", x.Name)
		}
		commits[x.Name] = sha
		env = append(env, x.EnvVar()+"="+dir)
		dirs = append(dirs, dir)
		r.cfg.Log.InfoContext(ctx, "extra repository checked out",
			"job_id", r.grant.JobID, "name", x.Name, "repo", x.Repo, "ref", x.Ref, "commit", sha)
	}
	r.mu.Lock()
	r.extraEnv, r.extraCommits = env, commits
	r.extraReadDirs, r.extraPrompt = dirs, extraPromptNote(r.grant.ExtraRepos, dirs)
	r.mu.Unlock()
	return ""
}

// cleanupExtras takes the job's extra checkouts away; the files are
// read-only, so the directories are made writable first.
func (r *jobRun) cleanupExtras() {
	if len(r.grant.ExtraRepos) == 0 {
		return
	}
	removeAllWritable(extraRoot(r.cfg.StateDir, r.grant))
}

// extraEnvVars is the environment that names the checkouts; nil when the
// job has none.
func (r *jobRun) extraEnvVars() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.extraEnv
}

// extraDirs is the checkouts' absolute paths, in the grant's order — what
// the adapter's read grant (adapter.Job.ReadDirs) is built from; nil when
// the job has none.
func (r *jobRun) extraDirs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.extraReadDirs
}

// extraPromptOf is the fixed paragraph naming the checkouts, appended to
// the job's prompt; empty when the job has none.
func (r *jobRun) extraPromptOf() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.extraPrompt
}

// extraPromptNote is the short fixed paragraph the runner appends to a
// job's prompt when the job has extra repositories (§5.8): the worker may
// have no shell for echo or ls — claude's default worker scope is the Go
// toolchain alone — so the prompt itself must name each checkout's
// absolute path. One sentence per repository, in the grant's order, in
// the grant's own naming; no repositories, no paragraph, and a job
// without them keeps its prompt unchanged.
func extraPromptNote(repos []engine.ExtraRepo, dirs []string) string {
	if len(repos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nExtra repositories, read-only, checked out beside the worktree.\nRead them as context; never write, commit or push them:\n")
	for i := range repos {
		fmt.Fprintf(&b, "%s at %s (also in $%s).\n", repos[i].Name, dirs[i], repos[i].EnvVar())
	}
	return b.String()
}

// validExtraName is the engine's name rule, checked again where the name
// becomes a path.
func validExtraName(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// checkoutExtra clones one extra repository into dir at the entry's ref and
// leaves it read-only with no remote. It returns the commit checked out.
func (w *workspace) checkoutExtra(ctx context.Context, x engine.ExtraRepo, dir string) (string, error) {
	if out, err := w.git(ctx, "clone", "--no-checkout", "--", x.URL, dir); err != nil {
		return "", fmt.Errorf("clone %s: %w", redactURL(x.URL), trimCmdError(out, err))
	}
	// A ref is a branch, a tag or a commit. The clone holds the remote's
	// branches as origin/<name> and its tags as they are.
	var sha string
	for _, cand := range extraRefCandidates(x.Ref) {
		out, err := w.git(ctx, "-C", dir, "rev-parse", "--verify", "--quiet", cand+"^{commit}")
		if err == nil {
			sha = strings.TrimSpace(string(out))
			break
		}
	}
	if sha == "" {
		return "", fmt.Errorf("ref %q not found in %s", x.Ref, redactURL(x.URL))
	}
	if out, err := w.git(ctx, "-C", dir, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", sha); err != nil {
		return "", fmt.Errorf("check out %s: %w", sha, trimCmdError(out, err))
	}
	// No push remote: the checkout is context, never a branch the runner
	// pushes (§5.8).
	if out, err := w.git(ctx, "-C", dir, "remote", "remove", "origin"); err != nil {
		return "", fmt.Errorf("remove the remote: %w", trimCmdError(out, err))
	}
	if err := makeReadOnly(dir); err != nil {
		return "", fmt.Errorf("make the checkout read-only: %w", err)
	}
	return sha, nil
}

// extraRefCandidates are the names a ref may resolve under, in order. An
// empty ref is the clone's HEAD: the repository's default branch.
func extraRefCandidates(ref string) []string {
	if ref == "" {
		return []string{"HEAD"}
	}
	return []string{ref, "refs/remotes/origin/" + ref, "refs/tags/" + ref}
}

// makeReadOnly removes the write bits of everything under dir, directories
// included, so a stray write fails for the job's user. It is a convention,
// not a sandbox (§5.8).
func makeReadOnly(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(p, info.Mode().Perm()&^0o222)
	})
	if err != nil {
		return err
	}
	// Directories last, deepest first, so the walk above could still enter
	// them.
	for i := len(dirs) - 1; i >= 0; i-- {
		info, err := os.Stat(dirs[i])
		if err != nil {
			return err
		}
		if err := os.Chmod(dirs[i], info.Mode().Perm()&^0o222); err != nil {
			return err
		}
	}
	return nil
}

// withEnv returns env with add appended; a nil env means "inherit the
// process's", so it is materialized first rather than replaced by add alone.
func withEnv(env, add []string) []string {
	if len(add) == 0 {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	return append(env[:len(env):len(env)], add...)
}
