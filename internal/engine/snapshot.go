package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store snapshots (§6.4): once a day the engine copies its store into the
// snapshot directory through SQLite's backup API, so a lost store can be
// restored by `brigada recover`. The feature is opt-in: no directory, no
// snapshots.
//
// Due-ness is read from the directory itself, not from engine memory: a
// snapshot is due when the newest one is older than the interval, or
// there is none. An engine that idles out after half an hour therefore
// still snapshots daily across its restarts. The caller — the engine's
// own tick — asks SnapshotIfDue; the question is a directory listing, and
// the backup runs inline.

// Shipped snapshot pacing (§6.4): daily, the last seven kept.
const (
	defaultSnapshotEvery = 24 * time.Hour
	defaultSnapshotKeep  = 7
	snapshotRetry        = time.Hour // between attempts after a failed snapshot
)

// A snapshot file is store-<UTC stamp>.db: the stamp sorts as time does.
const (
	snapshotPrefix = "store-"
	snapshotSuffix = ".db"
	snapshotStamp  = "20060102T150405Z"
)

type snapshotState struct {
	mu      sync.Mutex
	retryAt time.Time
}

// SnapshotPath is the file a snapshot taken at t is written to.
func SnapshotPath(dir string, t time.Time) string {
	return filepath.Join(dir, snapshotPrefix+t.UTC().Format(snapshotStamp)+snapshotSuffix)
}

// ListSnapshots returns the snapshot files in dir, newest first. A
// missing directory holds none. Files that do not carry the snapshot name
// — a partial .tmp, anything else — are not snapshots.
func ListSnapshots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list snapshots in %s: %w", dir, err)
	}
	var names []string
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if _, ok := snapshotTime(ent.Name()); ok {
			names = append(names, ent.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(dir, n)
	}
	return paths, nil
}

// snapshotTime reads the moment a snapshot file name carries.
func snapshotTime(name string) (time.Time, bool) {
	stamp, ok := strings.CutPrefix(name, snapshotPrefix)
	if !ok {
		return time.Time{}, false
	}
	stamp, ok = strings.CutSuffix(stamp, snapshotSuffix)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(snapshotStamp, stamp)
	return t, err == nil
}

// SnapshotIfDue takes a snapshot when one is due and returns its path,
// or "" when none was due or snapshots are off. After a success the
// snapshots beyond the keep count — the oldest — are removed; a failure
// holds the next attempt back for an hour and is returned.
func (e *Engine) SnapshotIfDue(ctx context.Context) (string, error) {
	dir := e.cfg.SnapshotDir
	if dir == "" {
		return "", nil
	}
	now := e.cfg.Now()
	e.snap.mu.Lock()
	defer e.snap.mu.Unlock()
	if now.Before(e.snap.retryAt) {
		return "", nil
	}
	existing, err := ListSnapshots(dir)
	if err != nil {
		return "", e.snapshotFailed(now, err)
	}
	if len(existing) > 0 {
		if last, _ := snapshotTime(filepath.Base(existing[0])); now.Before(last.Add(e.cfg.SnapshotEvery)) {
			return "", nil
		}
	}
	path, err := e.snapshot(ctx, dir, now)
	if err != nil {
		return "", e.snapshotFailed(now, err)
	}
	e.pruneSnapshots(dir)
	return path, nil
}

func (e *Engine) snapshotFailed(now time.Time, err error) error {
	e.snap.retryAt = now.Add(snapshotRetry)
	return fmt.Errorf("snapshot: %w", err)
}

// snapshot writes the store to a temporary name and renames it into
// place, so a half-written file is never listed as a snapshot, then logs
// the event.
func (e *Engine) snapshot(ctx context.Context, dir string, now time.Time) (string, error) {
	if p := e.store.Path(); p != "" && p != ":memory:" {
		if sameDir(dir, filepath.Dir(p)) {
			return "", fmt.Errorf("snapshot directory %s is the store's own directory; it must be outside it", dir)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := SnapshotPath(dir, now)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s exists already", path)
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	if err := e.store.Store.Backup(ctx, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	err = e.store.Update(ctx, func(tx *sql.Tx) error {
		_, err := appendEventTx(tx, evSnapshotTaken, 0, 0, 0, eventPayload{Path: path, Bytes: info.Size()}, now)
		return err
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// pruneSnapshots removes the snapshots beyond the keep count, oldest
// first. A removal that fails is a warning: the next success tries again.
func (e *Engine) pruneSnapshots(dir string) {
	all, err := ListSnapshots(dir)
	if err != nil {
		e.cfg.Warnf("snapshots: %v", err)
		return
	}
	for _, old := range all[min(e.cfg.SnapshotKeep, len(all)):] {
		if err := os.Remove(old); err != nil {
			e.cfg.Warnf("snapshots: remove %s: %v", old, err)
		}
	}
}

func sameDir(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}
