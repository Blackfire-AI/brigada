package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// pauseTestEnv wires pause and resume over an in-process API (§8.3): a
// store with one project, the API handler behind connectPause's client,
// so no engine process is spawned. It returns the state directory and
// the store, whose pauses table the assertions read.
func pauseTestEnv(t *testing.T) (string, *store.Store) {
	t.Helper()
	ctx := context.Background()
	state := testsupport.ShortStateDir(t)
	paths := lifecycle.PathsOf(state)
	st := openFreshStore(t, paths.Store)
	t.Cleanup(func() { st.Close() })
	if err := lifecycle.EnsureTokenFile(paths.Tokens); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/one.git', 'main', '{}', 'claude', 'sonnet', '2026-11-09T00:00:00Z', '2026-11-09T00:00:00Z')`)
		return err
	}); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	handler := func() http.Handler {
		srv, err := api.New(ctx, st, engine.New(st, engine.Config{}), nil, paths.Tokens)
		if err != nil {
			t.Fatal(err)
		}
		return srv.Handler()
	}()
	oldConnect := connectPause
	connectPause = func(_ context.Context, rt *runtime, role string, _ bool, stderr io.Writer) (*api.Client, int) {
		_, secret, err := lifecycle.TokenForRole(lifecycle.PathsOf(rt.stateDir).Tokens, role)
		if err != nil {
			return nil, fail(stderr, err)
		}
		return api.NewClientWithHTTPClient(&http.Client{Transport: removalHandlerTransport{handler: handler}}, secret), 0
	}
	t.Cleanup(func() { connectPause = oldConnect })
	return state, st
}

func pauseDo(t *testing.T, state string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append(append([]string{}, args...), "--state-dir", state), &out, &errb)
	return code, out.String(), errb.String()
}

// livePauseScopes reads the pauses that stand, the engine's as scope 0.
func livePauseScopes(t *testing.T, st *store.Store) map[int64]string {
	t.Helper()
	scopes := map[int64]string{}
	if err := st.View(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT COALESCE(project_id, 0), origin FROM pauses WHERE ended_at IS NULL`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var scope int64
			var origin string
			if err := rows.Scan(&scope, &origin); err != nil {
				return err
			}
			scopes[scope] = origin
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the pauses: %v", err)
	}
	return scopes
}

const pauseTestRepo = "https://example.invalid/one.git"

// TestPauseResumeEachScope: a bare pause and resume act on the engine,
// --repo on one project, and each lift takes its own scope alone
// (§8.3). An empty --repo is the engine's scope, never the working
// directory's project.
func TestPauseResumeEachScope(t *testing.T) {
	state, st := pauseTestEnv(t)
	code, out, errs := pauseDo(t, state, "pause")
	if code != 0 || out != "engine paused\n" || errs != "" {
		t.Fatalf("pause the engine: code=%d out=%q err=%q", code, out, errs)
	}
	if scopes := livePauseScopes(t, st); scopes[0] != engine.PauseOriginUser {
		t.Fatalf("the standing pauses = %v, want the engine's", scopes)
	}
	code, out, errs = pauseDo(t, state, "pause", "--repo", pauseTestRepo)
	if code != 0 || out != "paused "+pauseTestRepo+"\n" || errs != "" {
		t.Fatalf("pause the project: code=%d out=%q err=%q", code, out, errs)
	}
	if scopes := livePauseScopes(t, st); len(scopes) != 2 || scopes[0] == "" || scopes[1] == "" {
		t.Fatalf("the standing pauses = %v, want the engine's and the project's", scopes)
	}
	// Lifting the engine's takes none of the project's (§8.3).
	code, out, errs = pauseDo(t, state, "resume")
	if code != 0 || out != "engine resumed\n" || errs != "" {
		t.Fatalf("resume the engine: code=%d out=%q err=%q", code, out, errs)
	}
	if scopes := livePauseScopes(t, st); len(scopes) != 1 || scopes[1] == "" {
		t.Fatalf("the standing pauses = %v, want the project's alone", scopes)
	}
	code, out, errs = pauseDo(t, state, "resume", "--repo", pauseTestRepo)
	if code != 0 || out != "resumed "+pauseTestRepo+"\n" || errs != "" {
		t.Fatalf("resume the project: code=%d out=%q err=%q", code, out, errs)
	}
	if scopes := livePauseScopes(t, st); len(scopes) != 0 {
		t.Fatalf("the standing pauses = %v, want none", scopes)
	}
}

// TestPauseEmptyRepoIsTheEngine: `pause --repo ""` is engine-scoped —
// an empty --repo is treated as absent, never resolved to the working
// directory's origin (§8.3 line 612).
func TestPauseEmptyRepoIsTheEngine(t *testing.T) {
	state, st := pauseTestEnv(t)
	code, out, errs := pauseDo(t, state, "pause", "--repo", "")
	if code != 0 || out != "engine paused\n" || errs != "" {
		t.Fatalf("pause --repo \"\": code=%d out=%q err=%q", code, out, errs)
	}
	if scopes := livePauseScopes(t, st); scopes[0] != engine.PauseOriginUser || len(scopes) != 1 {
		t.Fatalf("the standing pauses = %v, want the engine's alone", scopes)
	}
}

// TestPauseAlreadyPausedSaysSo: pausing an already paused scope starts
// nothing and prints the pause it found; resuming an unpaused scope
// says nothing was paused.
func TestPauseAlreadyPausedSaysSo(t *testing.T) {
	state, _ := pauseTestEnv(t)
	if code, out, errs := pauseDo(t, state, "pause"); code != 0 || out != "engine paused\n" || errs != "" {
		t.Fatalf("pause: code=%d out=%q err=%q", code, out, errs)
	}
	code, out, errs := pauseDo(t, state, "pause")
	if code != 0 || out != "engine already paused (origin user)\n" || errs != "" {
		t.Fatalf("pause again: code=%d out=%q err=%q", code, out, errs)
	}
	code, out, errs = pauseDo(t, state, "resume", "--repo", pauseTestRepo)
	if code != 0 || out != pauseTestRepo+" is still paused by the engine's pause\n" || errs != "" {
		t.Fatalf("resume an unpaused project under the engine pause: code=%d out=%q err=%q", code, out, errs)
	}
	if code, out, errs = pauseDo(t, state, "resume"); code != 0 || out != "engine resumed\n" || errs != "" {
		t.Fatalf("resume: code=%d out=%q err=%q", code, out, errs)
	}
	code, out, errs = pauseDo(t, state, "resume")
	if code != 0 || out != "engine is not paused\n" || errs != "" {
		t.Fatalf("resume an unpaused engine: code=%d out=%q err=%q", code, out, errs)
	}
	code, out, errs = pauseDo(t, state, "resume", "--repo", pauseTestRepo)
	if code != 0 || out != pauseTestRepo+" is not paused\n" || errs != "" {
		t.Fatalf("resume an unpaused project: code=%d out=%q err=%q", code, out, errs)
	}
}

// TestResumeUnderEnginePause: resuming one project under the engine's
// pause lifts nothing of it and says the other scope still holds it
// (§8.3).
func TestResumeUnderEnginePause(t *testing.T) {
	state, st := pauseTestEnv(t)
	if code, out, errs := pauseDo(t, state, "pause"); code != 0 || errs != "" {
		t.Fatalf("pause the engine: code=%d out=%q err=%q", code, out, errs)
	}
	code, out, errs := pauseDo(t, state, "resume", "--repo", pauseTestRepo)
	if code != 0 || out != pauseTestRepo+" is still paused by the engine's pause\n" || errs != "" {
		t.Fatalf("resume the project under the engine pause: code=%d out=%q err=%q", code, out, errs)
	}
	if scopes := livePauseScopes(t, st); scopes[0] != engine.PauseOriginUser {
		t.Fatalf("the engine pause did not stand: %v", scopes)
	}
}

// TestPauseUnknownRepo: a repo URL no project carries is the engine's
// not-found, with the known projects named beside it.
func TestPauseUnknownRepo(t *testing.T) {
	state, _ := pauseTestEnv(t)
	code, out, errs := pauseDo(t, state, "pause", "--repo", "https://example.invalid/none.git")
	if code != 1 || out != "" || !strings.Contains(errs, "no project for repo") || !strings.Contains(errs, "known projects: "+pauseTestRepo) {
		t.Fatalf("pause an unknown repo: code=%d out=%q err=%q", code, out, errs)
	}
}

// TestPauseUsage: pause and resume take no positional arguments.
func TestPauseUsage(t *testing.T) {
	for _, verb := range []string{"pause", "resume"} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), []string{verb, "now"}, &out, &errb); code != 2 {
			t.Fatalf("%s with an argument: code=%d err=%q", verb, code, errb.String())
		}
	}
}
