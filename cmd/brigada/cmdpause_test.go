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

// waitForOutput waits until the buffer carries want.
func waitForOutput(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(buf.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("output %q never carried %q", buf.String(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedRunningJob seeds one project with one task whose work job is
// running: a job a drain waits on until it is cancelled.
func seedRunningJob(tx *sql.Tx) error {
	ts := time.Now().UTC().Format(engine.TimeLayout)
	for _, query := range []string{
		`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/drain.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`,
		`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'drain plan', 'active', ?, ?)`,
		`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (1, 'the task', 'the spec', 'go test ./...', 'implement', 'running', 'main', '{}', ?, ?)`,
		`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
			VALUES (1, 1, 'work', 'task:1', 'fresh', 'do it', 'running', ?, ?)`,
	} {
		if _, err := tx.Exec(query, ts, ts); err != nil {
			return err
		}
	}
	return nil
}

// startPauseEngine starts an engine in this process over a migrated store
// that seed has filled first (nil seeds nothing), and returns the engine's
// context and its state directory.
func startPauseEngine(t *testing.T, seed func(*sql.Tx) error) (context.Context, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := testsupport.ShortStateDir(t)
	seedMigratedStore(t, lifecycle.PathsOf(state).Store)
	if seed != nil {
		st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
		if err != nil {
			t.Fatalf("open the seeded store: %v", err)
		}
		err = st.Update(context.Background(), seed)
		st.Close()
		if err != nil {
			t.Fatalf("seed the store: %v", err)
		}
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
		case <-time.After(30 * time.Second):
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
	return ctx, state
}

// cmdDo runs `brigada <command> --state-dir state args...`.
func cmdDo(ctx context.Context, state, command string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	full := append([]string{command, "--state-dir", state}, args...)
	code := run(ctx, full, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// mustDo runs a command that must exit 0 and returns its stdout.
func mustDo(t *testing.T, ctx context.Context, state, command string, args ...string) string {
	t.Helper()
	code, stdout, stderr := cmdDo(ctx, state, command, args...)
	if code != 0 {
		t.Fatalf("brigada %s %v = %d, stdout %q, stderr %q", command, args, code, stdout, stderr)
	}
	return stdout
}

func wantContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to carry %q", what, got, want)
		}
	}
}

// TestPauseResumeScopes: `pause` and `resume` act on the engine bare and
// on one project with --repo, each resume lifting its own scope and no
// other, and a resume under a remaining scope says what still holds the
// project paused.
func TestPauseResumeScopes(t *testing.T) {
	ctx, state := startPauseEngine(t, nil)
	repo := "https://example.invalid/pause.git"
	mustDo(t, ctx, state, "budget", "--repo", repo, "--wakes", "5") // creates the project

	wantContains(t, "engine pause", mustDo(t, ctx, state, "pause"), "paused the engine:")
	wantContains(t, "second engine pause", mustDo(t, ctx, state, "pause"),
		"the engine was already paused (origin user, since ")
	wantContains(t, "project pause", mustDo(t, ctx, state, "pause", "--repo", repo), "paused project "+repo+":")

	// The project's resume lifts the project's pause; the engine's stands.
	out := mustDo(t, ctx, state, "resume", "--repo", repo)
	wantContains(t, "project resume", out, "resumed project "+repo+"\n",
		"project "+repo+" is still paused by the engine's pause")
	// With nothing of its own to lift, it says so and what still holds it.
	out = mustDo(t, ctx, state, "resume", "--repo", repo)
	wantContains(t, "project resume again", out, "project "+repo+" was not paused\n",
		"is still paused by the engine's pause")

	// The engine's resume leaves a project's own pause standing.
	mustDo(t, ctx, state, "pause", "--repo", repo)
	out = mustDo(t, ctx, state, "resume")
	wantContains(t, "engine resume", out, "resumed the engine\n", "is still paused by its own pause")
	wantContains(t, "engine resume again", mustDo(t, ctx, state, "resume"), "the engine was not paused\n")
	out = mustDo(t, ctx, state, "resume", "--repo", repo)
	wantContains(t, "last resume", out, "resumed project "+repo+"\n")
	if strings.Contains(out, "still paused") {
		t.Errorf("the last resume = %q, want nothing left paused", out)
	}
}

// TestPauseRepoResolution: a bare command, and one given an empty --repo,
// is engine-scoped — the working directory's origin never turns it into a
// project pause — while a given URL names its project as given, and a URL
// with no project is refused without pausing anything.
func TestPauseRepoResolution(t *testing.T) {
	ctx, state := startPauseEngine(t, nil)
	repo := "https://example.invalid/resolve.git"
	mustDo(t, ctx, state, "budget", "--repo", repo, "--wakes", "5")

	for _, args := range [][]string{{"pause", "--repo", ""}, {"pause"}} {
		out := mustDo(t, ctx, state, args[0], args[1:]...)
		if strings.Contains(out, "project") {
			t.Errorf("brigada %v = %q, want an engine pause", args, out)
		}
	}
	wantContains(t, "empty --repo resume", mustDo(t, ctx, state, "resume", "--repo", ""), "resumed the engine\n")

	code, out, errs := cmdDo(ctx, state, "pause", "--repo", "https://example.invalid/none.git")
	if code != 1 || out != "" || !strings.Contains(errs, "no project for repo") {
		t.Fatalf("pause of an unknown repo = %d, stdout %q, stderr %q, want a refusal", code, out, errs)
	}
	code, _, errs = cmdDo(ctx, state, "resume", "--repo", "https://example.invalid/none.git")
	if code != 1 || !strings.Contains(errs, "no project for repo") {
		t.Fatalf("resume of an unknown repo = %d, stderr %q, want a refusal", code, errs)
	}
	wantContains(t, "resume after the refusals", mustDo(t, ctx, state, "resume"), "the engine was not paused\n")
}

// TestPauseResumeUsageErrors: a misspelled flag and a stray argument are
// usage errors before the engine is contacted.
func TestPauseResumeUsageErrors(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	for _, command := range []string{"pause", "resume"} {
		for _, args := range [][]string{{"--rep", "x"}, {"https://example.invalid/x.git"}} {
			code, out, errs := cmdDo(context.Background(), state, command, args...)
			if code != 2 || out != "" || !strings.Contains(errs, "Run 'brigada "+command+" --help' for usage.") {
				t.Errorf("brigada %s %v = %d, stdout %q, stderr %q, want a usage error", command, args, code, out, errs)
			}
		}
	}
}
