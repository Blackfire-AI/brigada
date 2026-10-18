package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestStatusGolden runs `brigada status` against a real engine started
// in-process (§13), over a store seeded with plans in every listed
// state: the plans print with their tasks indented beneath them, the
// stale and pending plans stay out of the plans section, the latest
// foreman report shows beneath the plans with its time and first lines,
// and the needs-you queue names each entry with its reason — one line
// per entry, the sections exactly as the foreman's MCP tool renders
// them.
func TestStatusGolden(t *testing.T) {
	state := testsupport.ShortStateDir(t)

	// Seed the store the engine will open: the golden's rows, written
	// before the engine starts.
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the seeded store: %v", err)
	}
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(engine.TimeLayout) }
	err = st.Update(context.Background(), func(tx *sql.Tx) error {
		exec := func(query string, args ...any) sql.Result {
			t.Helper()
			res, err := tx.Exec(query, args...)
			if err != nil {
				t.Fatalf("seed %q: %v", query, err)
			}
			return res
		}
		exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/golden.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`,
			at(0), at(0))
		exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'fix the frobnicator', 'active', ?, ?)`, at(0), at(0))
		exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'stale work', 'done', ?, ?)`, at(-48*time.Hour), at(-25*time.Hour))
		exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'fresh work', 'done', ?, ?)`, at(-2*time.Hour), at(-time.Hour))
		exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'roll out v2', 'pending_approval', ?, ?)`, at(0), at(0))
		exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (1, 'the frobnicator', 'the spec', 'go test ./...', 'implement', 'ready', 'main', '{}', ?, ?)`, at(0), at(0))
		exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, branch, base_ref, caps, created_at, updated_at)
			VALUES (1, 'wire the sprocket', 'the spec', 'go test ./...', 'implement', 'needs_foreman', 'task/2-1', 'main', '{}', ?, ?)`, at(0), at(0))
		exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, branch, base_ref, caps, created_at, updated_at)
			VALUES (3, 'tidy the docs', 'the spec', NULL, 'answer', 'done', 'task/3-1', 'main', '{}', ?, ?)`, at(-2*time.Hour), at(-time.Hour))
		exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, branch, base_ref, caps, created_at, updated_at)
			VALUES (1, 'review the gadget', 'the spec', 'go test ./...', 'implement', 'reviewing', 'task/4-1', 'main', '{}', ?, ?)`, at(0), at(0))
		// One agent, of the author's family, and the task's review job
		// waiting for a reviewer of another family: nobody can take it.
		exec(`INSERT INTO runners (name, created_at) VALUES ('solo', ?)`, at(0))
		exec(`INSERT INTO agents (runner_id, adapter, harness, model, family, account, classes, created_at, updated_at)
			VALUES (1, 'claude', 'claude', 'sonnet', 'claude', 'claude', '["implement","review"]', ?, ?)`, at(0), at(0))
		exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, exclude_family, queued_at, created_at, updated_at)
			VALUES (1, 4, 'review', 'task:4', 'fresh', 'review it', 'queued', 'claude', ?, ?, ?)`, at(0), at(0), at(0))
		exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('task_needs_help', 1, 2, NULL, '{"question":"should the sprocket be blue?"}', ?)`, at(0))
		exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('foreman_report', 1, NULL, NULL, '{"report":"plan 3 done: merge task/3-1\nnothing else needs you"}', ?)`,
			at(-30*time.Minute))
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
		case err := <-done:
			if err != nil {
				t.Errorf("the engine stopped with an error: %v", err)
			}
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
	code := run(ctx, []string{"status", "--state-dir", state, "--no-start"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("status exit = %d, stderr = %q", code, stderr.String())
	}

	// The first two lines name the engine and count the jobs; their
	// values vary (a real pid, a real uptime), their shape does not.
	// The running section follows, empty: no runner holds a job.
	out := stdout.String()
	lines := strings.SplitN(out, "plans:\n", 2)
	if len(lines) != 2 {
		t.Fatalf("status printed %q, want a plans section in it", out)
	}
	head := strings.Split(strings.TrimSuffix(lines[0], "\n"), "\n")
	if len(head) != 3 || head[2] != "running: none" {
		t.Fatalf("status head = %q, want the engine line, the jobs line and an empty running section", lines[0])
	}
	if matched, _ := regexp.MatchString(`^engine pid [0-9]+ up [0-9hms]+$`, head[0]); !matched {
		t.Errorf("the engine line = %q, want `engine pid <pid> up <uptime>`", head[0])
	}
	if want := "jobs: queued 1 leased 0 running 0 done 0 failed 0 cancelled 0 stalled 0"; head[1] != want {
		t.Errorf("the jobs line = %q, want %q", head[1], want)
	}

	// The sections: the golden, exactly — the active plan with both its
	// tasks, the plan done inside the window, the stale one out, the
	// latest foreman report under the plans, the pending plan only in
	// the queue.
	want := fmt.Sprintf(`plans:
#1 active fix the frobnicator
  task #1 ready the frobnicator
  task #2 needs_foreman wire the sprocket [task/2-1]
  task #4 reviewing review the gadget [task/4-1]
#3 done fresh work
  task #3 done tidy the docs [task/3-1]
foreman report:
%s project #1: plan 3 done: merge task/3-1
  nothing else needs you
needs you:
task #2 needs_foreman: should the sprocket be blue?
task #4 reviewing: no reviewer: needs a reviewer of another family than the author (claude); offer an agent of another family to the runner (--agent harness=…) or review by hand
plan #4 pending_approval
`, at(-30*time.Minute))
	if got := "plans:\n" + lines[1]; got != want {
		t.Errorf("status sections =\n%s\nwant the golden:\n%s", got, want)
	}
}
