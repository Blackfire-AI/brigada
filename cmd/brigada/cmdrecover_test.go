package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/planfile"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// recoverFixture is a state directory whose store has a project on a
// local remote and one plan, snapshotted into a snapshot directory.
type recoverFixture struct {
	state, snaps, config, remote string
}

func newRecoverFixture(t *testing.T, snapshot bool) recoverFixture {
	t.Helper()
	ctx := context.Background()
	f := recoverFixture{
		state:  testsupport.ShortStateDir(t),
		snaps:  filepath.Join(t.TempDir(), "snaps"),
		config: filepath.Join(t.TempDir(), "config"),
		remote: testsupport.NewGitRemote(t),
	}
	if err := os.WriteFile(f.config, []byte("snapshot_dir = "+f.snaps+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(f.state, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seed := "2026-10-12T12:00:00.000000000Z"
	err = st.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects (id, repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (1, ?, 'main', '{}', 'claude', 'sonnet', ?, ?)`, f.remote, seed, seed); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO plans (id, project_id, title, state, created_at, updated_at) VALUES (1, 1, 'kept plan', 'active', ?, ?)`, seed, seed)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot {
		eng := engine.New(st, engine.Config{SnapshotDir: f.snaps})
		if path, err := eng.SnapshotIfDue(ctx); err != nil || path == "" {
			t.Fatalf("snapshot = %q, %v", path, err)
		}
	}
	return f
}

func (f recoverFixture) pushPlan(t *testing.T, p planfile.Plan) {
	t.Helper()
	p.Repo = f.remote
	if err := planfile.Push(context.Background(), planfile.Target{URL: f.remote, Branch: "brigada/plans"}, p); err != nil {
		t.Fatal(err)
	}
}

func (f recoverFixture) recover(t *testing.T, extra ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	args := append([]string{"recover", "--state-dir", f.state, "--config", f.config}, extra...)
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func (f recoverFixture) query(t *testing.T, query string) string {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(f.state, "store.db"))
	if err != nil {
		t.Fatalf("open the recovered store: %v", err)
	}
	defer st.Close()
	var got string
	if err := st.View(context.Background(), func(tx *sql.Tx) error { return tx.QueryRow(query).Scan(&got) }); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return got
}

func TestRecoverFromSnapshotAndPlanFiles(t *testing.T) {
	f := newRecoverFixture(t, true)
	// A plan the snapshot never saw, and the old file of the plan it did.
	later := time.Now().UTC().Add(time.Hour)
	f.pushPlan(t, planfile.Plan{ID: 9, Title: "born after the snapshot", State: "active", Created: later, Updated: later,
		Tasks: []planfile.Task{{ID: 90, Name: "a", Title: "t", Class: "implement", State: "ready"}}})
	f.pushPlan(t, planfile.Plan{ID: 1, Title: "stale file", State: "cancelled",
		Created: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), Updated: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	// The store is lost.
	if err := os.Remove(filepath.Join(f.state, "store.db")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(f.state, "store.db-wal"))
	os.Remove(filepath.Join(f.state, "store.db-shm"))

	code, out, errOut := f.recover(t)
	if code != 0 {
		t.Fatalf("recover exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{
		"restored the store from the snapshot",
		"plan #9 (" + f.remote + "): rebuilt",
		"plan #1 (" + f.remote + "): kept",
		"fence epoch is now 2",
		"dispatch is held for 1m30s",
		"tokens live outside the store",
		"runners re-register on their next poll",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT restored without a snapshot") {
		t.Errorf("a recover with a snapshot warned about having none:\n%s", out)
	}
	if got := f.query(t, "SELECT title FROM plans WHERE id = 1"); got != "kept plan" {
		t.Errorf("plan 1 title = %q, want the snapshot's", got)
	}
	if got := f.query(t, "SELECT title FROM plans WHERE id = 9"); got != "born after the snapshot" {
		t.Errorf("plan 9 title = %q", got)
	}
	if got := f.query(t, "SELECT fence_epoch FROM meta"); got != "2" {
		t.Errorf("fence_epoch = %s", got)
	}
}

func TestRecoverWithoutSnapshotSaysWhatIsNotRestored(t *testing.T) {
	f := newRecoverFixture(t, false)
	later := time.Now().UTC().Add(time.Hour)
	f.pushPlan(t, planfile.Plan{ID: 5, Title: "from the file", State: "active", Created: later, Updated: later})
	if err := os.Remove(filepath.Join(f.state, "store.db")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(f.state, "store.db-wal"))
	os.Remove(filepath.Join(f.state, "store.db-shm"))

	code, out, errOut := f.recover(t, "--repo", f.remote)
	if code != 0 {
		t.Fatalf("recover exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{
		"no snapshot to restore from",
		"NOT restored without a snapshot: messages, transcripts and outcomes",
		"the dispatcher's scores restart from defaults",
		"plan #5 (" + f.remote + "): rebuilt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := f.query(t, "SELECT title FROM plans WHERE id = 5"); got != "from the file" {
		t.Errorf("plan 5 title = %q", got)
	}
}

func TestRecoverMovesAnExistingStoreAsideAndNeedsASource(t *testing.T) {
	f := newRecoverFixture(t, false)
	// No snapshot and no repo: nothing to recover from, and the store stays.
	if code, _, errOut := f.recover(t); code != 1 || !strings.Contains(errOut, "nothing to recover from") {
		t.Fatalf("recover with no source = %d, %q; want 1 and the reason", code, errOut)
	}
	if got := f.query(t, "SELECT title FROM plans WHERE id = 1"); got != "kept plan" {
		t.Errorf("a refused recover touched the store: %q", got)
	}
	if code, out, _ := f.recover(t, "--repo", f.remote); code != 0 || !strings.Contains(out, ".lost-") {
		t.Errorf("recover over a live store = %d:\n%s\nwant it moved aside, not deleted", code, out)
	}
}

func TestRecoverUsageListed(t *testing.T) {
	var out bytes.Buffer
	run(context.Background(), []string{"help"}, &out, &out)
	if !strings.Contains(out.String(), "recover") {
		t.Errorf("usage does not list recover:\n%s", out.String())
	}
}
