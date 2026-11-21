package lifecycle_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestRunQueuesTaskBranchSweepAtStartup proves the inline startup work stops
// at computing and queueing: an old terminal task with a timestamp written
// without fractional seconds yields one task-less internal job, and the engine
// starts and idles out without contacting the unreachable example remote.
func TestRunQueuesTaskBranchSweepAtStartup(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	p := lifecycle.PathsOf(state)
	ctx := context.Background()
	st, err := store.Open(ctx, p.Store)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Update(ctx, func(tx *sql.Tx) error {
		const ts = "2026-10-13T00:00:00Z"
		if _, err := tx.Exec(`INSERT INTO projects (id, repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (1, 'https://example.invalid/r.git', 'main', '{"branch_retention_days":0}', 'claude', 'sonnet', ?, ?)`, ts, ts); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
			VALUES (1, 1, 'Done', 'done', ?, ?)`, ts, ts); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, settled_at, created_at, updated_at)
			VALUES (1, 1, 'Old', 'spec', 'implement', 'cancelled', 'main', ?, ?, ?)`, ts, ts, ts)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(ctx, p.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- lifecycle.Run(runCtx, lifecycle.Config{StateDir: state, Notify: "none", Idle: 300 * time.Millisecond, IdleTick: 50 * time.Millisecond})
	}()
	var jobs int
	deadline := time.Now().Add(10 * time.Second)
	for jobs == 0 {
		if err := st.View(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE kind = 'sweep' AND status = 'queued'").Scan(&jobs)
		}); err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		if jobs == 0 {
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("the startup sweep never queued; engine exit: %v", <-done)
			}
			time.Sleep(15 * time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("stop the engine after its startup queue: %v", err)
	}
	var task sql.NullInt64
	if err := st.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT task_id FROM jobs WHERE kind = 'sweep'").Scan(&task)
	}); err != nil {
		t.Fatal(err)
	}
	if task.Valid {
		t.Error("the startup sweep has a task_id; want a task-less internal job")
	}
}
