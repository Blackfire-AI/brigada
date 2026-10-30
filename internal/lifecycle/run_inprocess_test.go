package lifecycle_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
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

// TestRunReopensStrandedPlansAtStartup runs the engine role in-process
// over a store holding the pre-#133 strand of issue #137: a plan closed
// done while its task sat parked at needs_foreman. Start-up runs the
// reopen pass before the API serves (§13), so the plan is active again,
// one plan_reopened event names it and the parked task, and the log says
// the plan ids at info level.
func TestRunReopensStrandedPlansAtStartup(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	p := lifecycle.PathsOf(state)

	ctx := context.Background()
	st, err := store.Open(ctx, p.Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	err = st.Update(ctx, func(tx *sql.Tx) error {
		const ts = "2026-10-13T00:00:00.000000000Z"
		if _, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.com/r.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, ts, ts); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
			VALUES (1, 1, 'Stranded', 'done', ?, ?)`, ts, ts); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, created_at, updated_at)
			VALUES (1, 1, 'Parked', 'spec', 'implement', 'needs_foreman', 'main', ?, ?)`, ts, ts)
		return err
	})
	if err != nil {
		t.Fatalf("seed the stranded plan: %v", err)
	}
	st.Close()

	var logs bytes.Buffer
	err = lifecycle.Run(ctx, lifecycle.Config{
		StateDir: state,
		Notify:   "none",
		Idle:     300 * time.Millisecond,
		IdleTick: 50 * time.Millisecond,
		Log:      slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("run the engine: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "reopened plans that closed done while still holding live work") || !strings.Contains(out, "[1]") {
		t.Errorf("the start-up log = %q, want the reopened plan ids at info level", out)
	}

	st, err = store.Open(ctx, p.Store)
	if err != nil {
		t.Fatalf("reopen the store: %v", err)
	}
	defer st.Close()
	var planState string
	var events int
	if err := st.View(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT state FROM plans WHERE id = 1`).Scan(&planState); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = 'plan_reopened'`).Scan(&events)
	}); err != nil {
		t.Fatalf("read the reopened plan: %v", err)
	}
	if planState != "active" {
		t.Errorf("plan state after start-up = %q, want active", planState)
	}
	if events != 1 {
		t.Errorf("plan_reopened events = %d, want 1", events)
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
			VALUES ('task_check_failed', 1, 1, NULL, '{"status":"needs_foreman","reason":"exit 1"}', '2026-10-13T00:00:00Z')`)
		return err
	})
	if err != nil {
		t.Fatalf("seed the event log: %v", err)
	}
}
