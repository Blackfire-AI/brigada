package lifecycle_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestRunFlushesNotificationsBeforeIdleExit runs the engine role
// in-process (§13): the §10 hook is the whole notification story so far,
// and an idle exit must call it before the socket and the lock go.
func TestRunFlushesNotificationsBeforeIdleExit(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	p := lifecycle.PathsOf(state)
	flushed := make(chan struct{}, 1)
	err := lifecycle.Run(context.Background(), lifecycle.Config{
		StateDir: state,
		Idle:     400 * time.Millisecond,
		IdleTick: 50 * time.Millisecond,
		FlushNotifications: func(context.Context) error {
			flushed <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run the engine: %v", err)
	}
	select {
	case <-flushed:
	default:
		t.Error("the idle exit did not flush the notifications")
	}
	if _, err := os.Stat(p.Socket); !os.IsNotExist(err) {
		t.Errorf("the socket survived the idle exit: %v", err)
	}
	if pid := lifecycle.EnginePid(p.Lock); pid != 0 {
		t.Errorf("the lock survived the idle exit, held by pid %d", pid)
	}
	if _, err := os.Stat(p.Tokens); err != nil {
		t.Errorf("the token file was not created: %v", err)
	}
}

// TestRunDeliveriesAdvanceTheCursor runs the engine role in-process over
// a store that already holds one parking event (§10): the delivery loop
// the run wires must walk the log — here with notifications off, so the
// proof is the cursor in meta standing past the seeded event once the
// run idles out. It pins the wiring, not the notifier: the notifier's
// own behavior is the notify package's tests.
func TestRunDeliveriesAdvanceTheCursor(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	p := lifecycle.PathsOf(state)

	ctx := context.Background()
	st, err := store.Open(ctx, p.Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	seedNotifyEvent(t, ctx, st)
	st.Close()

	err = lifecycle.Run(ctx, lifecycle.Config{
		StateDir: state,
		Notify:   "none",
		Idle:     300 * time.Millisecond,
		IdleTick: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run the engine: %v", err)
	}

	st, err = store.Open(ctx, p.Store)
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	defer st.Close()
	var cursor, lastEvent int64
	if err := st.View(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT COALESCE(notify_cursor, -1) FROM meta WHERE id = 1`).Scan(&cursor); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&lastEvent)
	}); err != nil {
		t.Fatalf("read the cursor: %v", err)
	}
	if cursor != lastEvent {
		t.Errorf("the delivery loop left the cursor at %d, want it advanced past the seeded event %d", cursor, lastEvent)
	}
}

// seedNotifyEvent installs one project and one task_check_failed event,
// the minimal log rows the delivery loop's joins resolve.
func seedNotifyEvent(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	err := st.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.com/r.git', 'main', '{}', 'claude', 'sonnet', '2026-10-13T00:00:00Z', '2026-10-13T00:00:00Z')`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
			VALUES (1, 1, 'Ship it', 'active', '2026-10-13T00:00:00Z', '2026-10-13T00:00:00Z')`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, created_at, updated_at)
			VALUES (1, 1, 'First', 'spec', 'implement', 'needs_foreman', 'main', '2026-10-13T00:00:00Z', '2026-10-13T00:00:00Z')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('task_check_failed', 1, 1, NULL, '{"reason":"exit 1"}', '2026-10-13T00:00:00Z')`)
		return err
	})
	if err != nil {
		t.Fatalf("seed the event log: %v", err)
	}
}

// TestRunStuckFlushDoesNotHoldTheExit runs the engine role with a §10
// hook that never returns: the idle exit's flush must be abandoned
// after the grace and the exit must come within it, not wait the hook
// out (a flush that ignores its context cannot hold the engine
// hostage).
func TestRunStuckFlushDoesNotHoldTheExit(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	block := make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		runDone <- lifecycle.Run(context.Background(), lifecycle.Config{
			StateDir:   state,
			Notify:     "none",
			Idle:       400 * time.Millisecond,
			IdleTick:   50 * time.Millisecond,
			FlushGrace: 300 * time.Millisecond,
			FlushNotifications: func(context.Context) error {
				<-block
				return nil
			},
		})
	}()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run the engine: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the idle exit never returned: a never-returning flush holds the engine hostage")
	}
	close(block)
}

// TestRunCtxCancelShutdownBounded cancels the engine's context while
// its flush hook never returns: the shutdown must complete within the
// grace plus slack — SIGTERM stops the engine whatever the notifier or
// the hook is doing.
func TestRunCtxCancelShutdownBounded(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	block := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- lifecycle.Run(ctx, lifecycle.Config{
			StateDir:   state,
			Notify:     "none",
			Idle:       time.Hour,
			IdleTick:   50 * time.Millisecond,
			FlushGrace: 300 * time.Millisecond,
			FlushNotifications: func(context.Context) error {
				<-block
				return nil
			},
		})
	}()
	time.Sleep(600 * time.Millisecond) // let it come up
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run the engine: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ctx-cancel shutdown never returned: a never-returning flush holds the engine hostage")
	}
	close(block)
}

// bannerToolScript is the hostile banner CLI: it stays alive until the
// context's kill lands on it, and a background child it spawns inherits
// the output pipes and holds them open — so the killed exec still has
// no EOF and the delivery stays in flight for far longer than a tick.
// The script leaves its own pid and the holder's in markers for the
// test's cleanup, so no orphan process outlives it.
func bannerToolScript(markers string) string {
	return fmt.Sprintf(`
echo $$ > "%s/tool"
(sleep 600) &
echo $! > "%s/holder"
echo banner up
while :; do sleep 1; done
`, markers, markers)
}

// TestRunDeliveryOffTheMainLoop wires the real notifier to a scripted
// banner tool whose exec never ends on its own, over a store holding
// one parking event: the first tick's delivery stays in flight, and the
// main loop must keep moving — the ctx-cancel shutdown is served while
// the banner exec still runs, instead of waiting it out.
func TestRunDeliveryOffTheMainLoop(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	p := lifecycle.PathsOf(state)

	markers := t.TempDir()
	tool := testsupport.WriteScript(t, bannerToolScript(markers))
	t.Setenv("PATH", filepath.Dir(tool)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		var pids []int
		for _, name := range []string{"tool", "holder"} {
			if pid := testsupport.ReadPid(filepath.Join(markers, name)); pid > 0 {
				pids = append(pids, pid)
			}
		}
		testsupport.KillPids(t, pids...)
	})

	// One parking event, so the first tick's flush has a banner to show.
	ctx := context.Background()
	st, err := store.Open(ctx, p.Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	seedNotifyEvent(t, ctx, st)
	st.Close()

	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- lifecycle.Run(runCtx, lifecycle.Config{
			StateDir:   state,
			Idle:       time.Hour,
			IdleTick:   50 * time.Millisecond,
			FlushGrace: 500 * time.Millisecond,
		})
	}()
	time.Sleep(500 * time.Millisecond) // the delivery is in flight by now
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run the engine: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ctx-cancel shutdown never returned: a stuck banner exec holds the main loop")
	}
}
