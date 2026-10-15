package lifecycle

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// RecoverConfig is what `brigada recover` works from (§6.4).
type RecoverConfig struct {
	StateDir  string
	StorePath string // zero derives it from StateDir
	LockPath  string // zero derives it from StateDir
	// SnapshotDir is where the engine's snapshots are; empty = none, and
	// the store is rebuilt from plan files alone.
	SnapshotDir string
	// Repos name repositories, beyond the projects the snapshot holds,
	// whose plan files are applied: a project born after the last
	// snapshot, or every project when there is no snapshot.
	Repos []string
	// Now is the clock; nil is real time.
	Now func() time.Time
}

// RecoverReport says what recover did, for the command to print.
type RecoverReport struct {
	Snapshot   string // the snapshot restored; "" when there was none
	MovedAside string // where the old store file went; "" when there was none
	Plans      []engine.PlanRecovery
	Warnings   []string
	Epoch      int           // the fence epoch after the bump
	Hold       time.Duration // how long dispatch is held after the recovery
}

// Recover rebuilds the store (§6.4). Holding the engine's lock, so no
// engine runs or starts meanwhile, it
//
//  1. moves a store file that is still there aside, never deleting it;
//  2. restores the newest snapshot into a fresh store file at the same
//     path through the backup API, or starts an empty store when there is
//     none;
//  3. applies the plan files of the repos' plans branches;
//  4. bumps the fence epoch and stamps the recovery time, so late runners
//     are fenced out and dispatch waits one lease-expiry window.
//
// The tokens live beside the store, untouched.
func Recover(ctx context.Context, cfg RecoverConfig) (RecoverReport, error) {
	var rep RecoverReport
	if cfg.StateDir == "" {
		return rep, fmt.Errorf("recover: a state directory is required")
	}
	p := PathsOf(cfg.StateDir)
	if cfg.StorePath != "" {
		p.Store = cfg.StorePath
	}
	if cfg.LockPath != "" {
		p.Lock = cfg.LockPath
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return rep, fmt.Errorf("recover: create the state directory %s: %w", p.StateDir, err)
	}
	lock, err := AcquireLock(p.Lock)
	if err != nil {
		return rep, fmt.Errorf("recover: stop the engine first: %w", err)
	}
	defer lock.Release()

	if cfg.SnapshotDir != "" {
		snaps, err := engine.ListSnapshots(cfg.SnapshotDir)
		if err != nil {
			return rep, fmt.Errorf("recover: %w", err)
		}
		if len(snaps) > 0 {
			rep.Snapshot = snaps[0]
		}
	}
	if rep.Snapshot == "" && len(cfg.Repos) == 0 {
		return rep, fmt.Errorf("recover: nothing to recover from: no snapshot in %q and no --repo to read plan files of", cfg.SnapshotDir)
	}

	if rep.MovedAside, err = moveStoreAside(p.Store, cfg.Now()); err != nil {
		return rep, fmt.Errorf("recover: %w", err)
	}
	if rep.Snapshot != "" {
		if err := store.Restore(ctx, rep.Snapshot, p.Store); err != nil {
			if rep.MovedAside != "" { // put the old store back: nothing was recovered
				for _, suffix := range []string{"", "-wal", "-shm"} {
					os.Rename(rep.MovedAside+suffix, p.Store+suffix)
				}
			}
			return rep, fmt.Errorf("recover: %w", err)
		}
	}
	st, err := store.Open(ctx, p.Store) // creates the empty store when there is no snapshot; migrates a restored one
	if err != nil {
		return rep, fmt.Errorf("recover: %w", err)
	}
	defer st.Close()
	eng := engine.New(st, engine.Config{Now: cfg.Now})

	rep.Plans, rep.Warnings, err = eng.RecoverPlans(ctx, cfg.Repos)
	if err != nil {
		return rep, fmt.Errorf("recover: %w", err)
	}
	if rep.Epoch, err = eng.MarkRecovered(ctx); err != nil {
		return rep, fmt.Errorf("recover: %w", err)
	}
	rep.Hold = eng.LeaseTTL()
	return rep, nil
}

// moveStoreAside renames a store file, with its write-ahead log and
// shared-memory files, to <store>.lost-<unix time>, so recovery never
// deletes what might still be read. "" when there is no store file.
func moveStoreAside(path string, now time.Time) (string, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	aside := fmt.Sprintf("%s.lost-%d", path, now.Unix())
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(path+suffix, aside+suffix); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("move the old store aside: %w", err)
		}
	}
	return aside, nil
}
