package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The environment step (§5.7). The runner runs the project's recorded setup
// command in the worktree, as itself and not as the worker, before the
// worker starts and again before the check. Its cache is a convenience of
// this runner: entries under the state directory, per repository mirror,
// keyed by the files the project names, and handed to a job by copy.

// Bounds on the cache (§5.7 "Invalidation"; the defaults are open in §16
// item 8): an entry unused for setupCacheMaxAge, or beyond the
// setupCacheMaxEntries most recently used of its repository, is removed.
const (
	setupCacheMaxAge     = 30 * 24 * time.Hour
	setupCacheMaxEntries = 3
	// setupStaleTemp is how old an interrupted promotion's leftovers must
	// be before the sweep takes them.
	setupStaleTemp = time.Hour
)

// cacheEnv is the variable that names the job's private cache copy to the
// setup command.
const cacheEnv = "BRIGADA_CACHE_DIR"

// setupCacheMu orders the readers of an entry (a seeding copy) against its
// replacement. One runner owns a state directory (lock.go), so a process
// lock is enough.
var setupCacheMu sync.RWMutex

// setupDirs are the two roots under the state directory the step uses.
func setupCacheRoot(stateDir string) string { return filepath.Join(stateDir, "setup-cache") }
func setupJobsRoot(stateDir string) string  { return filepath.Join(stateDir, "setup-jobs") }

// setupJobDir is the job's private area: its cache copy lives under it.
func (r *jobRun) setupJobDir() string {
	return filepath.Join(setupJobsRoot(r.cfg.StateDir), fmt.Sprintf("job-%d-a%d", r.grant.JobID, r.grant.Attempt))
}

// cleanupSetup takes the job's private cache copy away.
func (r *jobRun) cleanupSetup() {
	if r.grant.Setup == nil {
		return
	}
	removeAllWritable(r.setupJobDir())
}

// setupRepoDir is the cache directory of the job's repository mirror: the
// mirror's own name, so an entry is never shared across repositories.
func (r *jobRun) setupRepoDir() string {
	name := strings.TrimSuffix(filepath.Base(mirrorPath(r.cfg.StateDir, r.grant.RepoURL)), ".git")
	return filepath.Join(setupCacheRoot(r.cfg.StateDir), name)
}

// setupTimeout is the bound of one run: the recorded one, unless the
// runner's config overrides it (tests do).
func (r *jobRun) setupTimeout(spec *engine.SetupSpec) time.Duration {
	if r.cfg.SetupTimeout > 0 {
		return r.cfg.SetupTimeout
	}
	m := spec.TimeoutMinutes
	if m <= 0 {
		m = 15
	}
	return time.Duration(m) * time.Minute
}

// runSetup runs the recorded setup in dir. first is the run before the
// worker, the only one allowed to replace the cache entry. It returns ""
// when there is nothing to do or the setup succeeded, and otherwise the
// reason it failed — a short sentence with the exit code or the timeout,
// never the command's output, which is data and lives in the log artifact.
// A run cut short because the job is no longer ours returns "" too: the
// caller asks r.gone.
func (r *jobRun) runSetup(ctx context.Context, dir string, first bool) string {
	spec := r.grant.Setup
	if spec == nil || strings.TrimSpace(spec.Command) == "" {
		return ""
	}
	step := 2
	if first {
		step = 1
	}

	key, cacheable, err := setupCacheKey(dir, spec)
	if err != nil {
		r.saveSetupLog(ctx, step, []byte("setup failed before it ran: "+err.Error()+"\n"))
		return "setup failed: " + err.Error()
	}
	jobDir := r.setupJobDir()
	cache := filepath.Join(jobDir, "cache")
	removeAllWritable(cache)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return "setup failed: " + err.Error()
	}
	entry := filepath.Join(r.setupRepoDir(), key)
	hit := false
	if cacheable {
		setupCacheMu.RLock()
		if st, err := os.Stat(entry); err == nil && st.IsDir() {
			if err := copyTree(entry, cache); err == nil {
				hit = true
				now := time.Now()
				_ = os.Chtimes(entry, now, now)
			} else {
				removeAllWritable(cache)
			}
		}
		setupCacheMu.RUnlock()
	}
	if !hit {
		if err := os.MkdirAll(cache, 0o755); err != nil {
			return "setup failed: " + err.Error()
		}
	}

	cctx, cancel := context.WithTimeout(ctx, r.setupTimeout(spec))
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", spec.Command)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), cacheEnv+"="+cache), r.extraEnvVars()...)
	cmd.SysProcAttr = processGroup()
	// A timeout or an abort kills the whole tree, not just sh; WaitDelay
	// bounds the wait for pipes a straggler holds open (as for the check).
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = checkKillGrace
	out, runErr := cmd.CombinedOutput()
	timedOut := cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil

	state := "miss"
	if hit {
		state = "hit"
	}
	header := fmt.Sprintf("# setup run %d (cache %s)\n", step, state)
	r.saveSetupLog(ctx, step, append([]byte(header), out...))

	switch {
	case ctx.Err() != nil || r.takeDropped():
		return ""
	case timedOut:
		return fmt.Sprintf("setup failed: timed out after %s", r.setupTimeout(spec))
	case runErr != nil:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return fmt.Sprintf("setup failed: exit code %d", ee.ExitCode())
		}
		return "setup failed: " + runErr.Error()
	}
	// Only a work job's first run may store: a fix or review worktree sits
	// on a branch a worker already wrote (§5.7).
	if first && cacheable && r.grant.Kind == string(engine.KindWork) {
		if err := r.promoteCache(cache, entry); err != nil {
			// A cache that cannot be stored costs the next job a cold
			// install, not this one its setup.
			r.cfg.Log.WarnContext(ctx, "store the setup cache", "job_id", r.grant.JobID, "err", err)
		}
	}
	return ""
}

// saveSetupLog uploads one run's log as the job's setup log artifact (§5.7).
func (r *jobRun) saveSetupLog(ctx context.Context, step int, data []byte) {
	name := fmt.Sprintf("job-%d-setup-%d.log", r.grant.JobID, step)
	if err := r.saveRecord(ctx, "setup_log", name, data); err != nil {
		r.cfg.Log.WarnContext(ctx, "save setup log", "job_id", r.grant.JobID, "err", err)
	}
}

// setupCacheKey is the key of a run (§5.7): a hash of the contents of the
// files the project names — a missing one hashes as absent — of the command
// text, and of the runner's OS and architecture. cacheable is false when
// the project named no file: no cache, a cold run every time.
func setupCacheKey(dir string, spec *engine.SetupSpec) (key string, cacheable bool, err error) {
	h := sha256.New()
	fmt.Fprintf(h, "command\x00%s\x00os\x00%s\x00arch\x00%s\x00", spec.Command, runtime.GOOS, runtime.GOARCH)
	files := append([]string(nil), spec.CacheKeyFiles...)
	sort.Strings(files)
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		if !filepath.IsLocal(f) {
			return "", false, fmt.Errorf("cache key file %q is not inside the repository", f)
		}
		data, rerr := os.ReadFile(filepath.Join(dir, f))
		switch {
		case errors.Is(rerr, fs.ErrNotExist):
			fmt.Fprintf(h, "file\x00%s\x00absent\x00", f)
		case rerr != nil:
			// A directory or an unreadable file: its content cannot key.
			fmt.Fprintf(h, "file\x00%s\x00unreadable\x00", f)
		default:
			sum := sha256.Sum256(data)
			fmt.Fprintf(h, "file\x00%s\x00%s\x00", f, hex.EncodeToString(sum[:]))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32], len(seen) > 0, nil
}

// promoteCache replaces the repository's entry for key with the job's copy,
// by moving it into place (§5.7): the copy is renamed beside the entry, and
// the entry swapped for it, so a reader sees the old entry or the new one.
func (r *jobRun) promoteCache(cache, entry string) error {
	repoDir := filepath.Dir(entry)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return err
	}
	tag := fmt.Sprintf("%d-a%d", r.grant.JobID, r.grant.Attempt)
	fresh := entry + ".new-" + tag
	removeAllWritable(fresh)
	if err := os.Rename(cache, fresh); err != nil {
		// Not on one filesystem: copy it across instead.
		if cerr := copyTree(cache, fresh); cerr != nil {
			removeAllWritable(fresh)
			return cerr
		}
		removeAllWritable(cache)
	}
	old := entry + ".old-" + tag
	setupCacheMu.Lock()
	removeAllWritable(old)
	if _, err := os.Stat(entry); err == nil {
		if err := os.Rename(entry, old); err != nil {
			setupCacheMu.Unlock()
			removeAllWritable(fresh)
			return err
		}
	}
	err := os.Rename(fresh, entry)
	setupCacheMu.Unlock()
	removeAllWritable(old)
	if err != nil {
		removeAllWritable(fresh)
		return err
	}
	pruneSetupCache(repoDir, time.Now())
	return nil
}

// pruneSetupCache applies the bounds (§5.7): entries unused for too long go,
// then the least recently used beyond the count, and the leftovers of an
// interrupted promotion once they are stale.
func pruneSetupCache(repoDir string, now time.Time) {
	ents, err := os.ReadDir(repoDir)
	if err != nil {
		return
	}
	type entry struct {
		path string
		mod  time.Time
	}
	var live []entry
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || !e.IsDir() {
			continue
		}
		p := filepath.Join(repoDir, e.Name())
		if strings.Contains(e.Name(), ".new-") || strings.Contains(e.Name(), ".old-") {
			if now.Sub(info.ModTime()) > setupStaleTemp {
				removeAllWritable(p)
			}
			continue
		}
		live = append(live, entry{p, info.ModTime()})
	}
	sort.Slice(live, func(i, j int) bool { return live[i].mod.After(live[j].mod) })
	setupCacheMu.Lock()
	defer setupCacheMu.Unlock()
	for i, e := range live {
		if i >= setupCacheMaxEntries || now.Sub(e.mod) > setupCacheMaxAge {
			removeAllWritable(e.path)
		}
	}
}

// copyTree copies the directory src to dst, keeping modes and symlinks. It
// makes every directory writable for its owner, so a copy of a read-only
// module cache can be extended and removed.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		default:
			return nil // sockets, devices: nothing a cache holds
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

// removeAllWritable removes a tree a setup command may have made read-only
// (a Go module cache is): directories are made writable first.
func removeAllWritable(path string) { _ = removeAllWritableErr(path) }

func removeAllWritableErr(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}
