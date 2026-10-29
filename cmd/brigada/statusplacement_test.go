package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestStatusShowsTheReviewPlacement: a reviewing task whose review was
// leased and placed on the author's family shows `review: same_family`
// beside its other detail; a task whose review is still waiting, and one
// placed cross_family, show nothing (issue #126).
func TestStatusShowsTheReviewPlacement(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	seedMigratedStore(t, lifecycle.PathsOf(state).Store)
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the seeded store: %v", err)
	}
	now := time.Now().UTC().Format(engine.TimeLayout)
	err = st.Update(context.Background(), func(tx *sql.Tx) error {
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(query, args...); err != nil {
				t.Fatalf("seed %q: %v", query, err)
			}
		}
		exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/placed.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, now, now)
		exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (1, 'placed', 'active', ?, ?)`, now, now)
		for i, title := range []string{"same family review", "cross family review", "waiting review"} {
			id := i + 1
			exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, branch, base_ref, caps, created_at, updated_at)
				VALUES (1, ?, 'the spec', 'go test ./...', 'implement', 'reviewing', ?, 'main', '{}', ?, ?)`,
				title, fmt.Sprintf("task/%d-1", id), now, now)
			status := "leased"
			if id == 3 {
				status = "queued"
			}
			exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, queued_at, created_at, updated_at)
				VALUES (1, ?, 'review', ?, 'fresh', 'review it', ?, ?, ?, ?)`, id, fmt.Sprintf("task:%d", id), status, now, now, now)
		}
		exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('job_agent_picked', 1, 1, 1, '{"agent":1,"placement":"same_family"}', ?)`, now)
		exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('job_agent_picked', 1, 2, 2, '{"agent":1,"placement":"cross_family"}', ?)`, now)
		return nil
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
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"status", "--state-dir", state, "--no-start"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit = %d, stderr = %q", code, stderr.String())
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		switch {
		case strings.Contains(line, "same family review") && !strings.Contains(line, "review: same_family"):
			t.Errorf("same-family task line %q lacks the placement", line)
		case (strings.Contains(line, "cross family review") || strings.Contains(line, "waiting review")) && strings.Contains(line, "review:"):
			t.Errorf("line %q names a placement", line)
		}
	}
	if !strings.Contains(stdout.String(), "review: same_family") {
		t.Errorf("status lacks the placement:\n%s", stdout.String())
	}
}
