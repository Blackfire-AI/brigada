package main

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestOutputLimitReasonShowsInStatusAndHistory runs `brigada status` and
// `brigada history` against a real in-process engine whose store holds a
// task parked after a run cut off at its output limit: both name the
// cut-off's numbers as the reason, not the worker's last message.
func TestOutputLimitReasonShowsInStatusAndHistory(t *testing.T) {
	const reason = "output limit reached: 32,000 tokens, 31,951 of them reasoning"
	state := testsupport.ShortStateDir(t)
	seedMigratedStore(t, lifecycle.PathsOf(state).Store)
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the seeded store: %v", err)
	}
	now := time.Now().UTC().Format(engine.TimeLayout)
	err = st.Update(context.Background(), func(tx *sql.Tx) error {
		for _, q := range []string{
			`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
				VALUES ('https://example.invalid/cut.git', 'main', '{}', 'opencode', 'm', ?, ?)`,
			`INSERT INTO plans (project_id, title, state, created_at, updated_at)
				VALUES (1, 'long thinking', 'active', ?, ?)`,
			`INSERT INTO tasks (plan_id, title, spec, class, state, branch, base_ref, caps, created_at, updated_at)
				VALUES (1, 'the long task', 'the spec', 'implement', 'needs_foreman', 'task/1-1', 'main', '{}', ?, ?)`,
		} {
			if _, err := tx.Exec(q, now, now); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('task_failed', 1, 1, NULL, ?, ?)`, `{"reason":"`+reason+`"}`, now)
		return err
	})
	st.Close()
	if err != nil {
		t.Fatalf("seed the store: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the engine stopped with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, lifecycle.PathsOf(state).Socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"status", []string{"status"}, "task #1 needs_foreman: " + reason},
		{"history", []string{"history", "--task", "1"}, "reason=" + reason},
	} {
		var stdout, stderr bytes.Buffer
		args := append(tc.args, "--state-dir", state, "--no-start")
		if code := run(ctx, args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", tc.name, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), tc.want) {
			t.Errorf("%s lacks %q:\n%s", tc.name, tc.want, stdout.String())
		}
	}
}
