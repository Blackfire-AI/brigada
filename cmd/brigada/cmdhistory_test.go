package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// historyTransport serves one in-process engine's handler without a
// listener, as the removal tests' transport does.
type historyTransport struct{ handler http.Handler }

func (h historyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	return w.Result(), nil
}

// TestHistoryShowsTheLandingRecord: `brigada history` shows what a
// landing left — the target branch, the resulting commit, the push
// state the gate made, gate_push never's exact push command, and an
// after_land failure, of the landing and of the push — and
// `brigada status` lists the local landing with its command in the
// needs-you queue, over a real engine and its API.
func TestHistoryShowsTheLandingRecord(t *testing.T) {
	ctx := context.Background()
	state := testsupport.ShortStateDir(t)
	paths := lifecycle.PathsOf(state)
	st := openFreshStore(t, paths.Store)
	t.Cleanup(func() { st.Close() })
	if err := lifecycle.EnsureTokenFile(paths.Tokens); err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(ctx, st, engine.New(st, engine.Config{}), nil, paths.Tokens)
	if err != nil {
		t.Fatal(err)
	}

	// One landed task under gate_push never, its after_land failed; a
	// later push phase, whose own after_land failed too. The push
	// command is the durable one the runner records: fetch the waiting
	// ref, push it to the target.
	pushCmd := "git fetch origin refs/brigada/land/main && git push origin FETCH_HEAD:refs/heads/main"
	now := time.Now().UTC().Format(engine.TimeLayout)
	stamp := []any{now, now}
	seed := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO projects (id, repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (1, 'example.invalid/org/app', 'main', '{}', 'claude', 'sonnet', ?, ?)`, stamp},
		{`INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
			VALUES (1, 1, 'ship the gizmo', 'done', ?, ?)`, stamp},
		{`INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, created_at, updated_at)
			VALUES (1, 1, 'ship the gizmo', 'Ship it.', 'implement', 'done', 'main', ?, ?)`, stamp},
		{`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, push_cmd, after_land_error, created_at)
			VALUES (1, 1, 'main', '9f21ab3c4d5e6f708192a3b4c5d6e7f890123456', 'local', ?, 'exit 3: after-land boom', ?)`,
			[]any{pushCmd, now}},
	}
	err = st.Update(ctx, func(tx *sql.Tx) error {
		for _, s := range seed {
			if _, err := tx.Exec(s.q, s.args...); err != nil {
				return err
			}
		}
		for _, ev := range []struct{ kind, payload string }{
			{"task_landed", `{"status":"done","branch":"main","commit":"9f21ab3c4d5e6f708192a3b4c5d6e7f890123456","pushed":"local","push_cmd":` + quoteJSON(pushCmd) + `,"after_land_error":"exit 3: after-land boom"}`},
			{"task_pushed", `{"status":"done","branch":"main","commit":"9f21ab3c4d5e6f708192a3b4c5d6e7f890123456","pushed":"pushed","after_land_error":"exit 1: push boom"}`},
		} {
			if _, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, payload, created_at)
				VALUES (?, 1, 1, ?, ?)`, ev.kind, ev.payload, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed the landing: %v", err)
	}

	oldConnect := connectClient
	connectClient = func(_ context.Context, rt *runtime, role string, _ bool, stderr io.Writer) (*api.Client, int) {
		_, token, err := lifecycle.TokenForRole(lifecycle.PathsOf(rt.stateDir).Tokens, role)
		if err != nil {
			return nil, fail(stderr, err)
		}
		return api.NewClientWithHTTPClient(&http.Client{Transport: historyTransport{handler: srv.Handler()}}, token), 0
	}
	t.Cleanup(func() { connectClient = oldConnect })

	var out, errb bytes.Buffer
	if code := run(ctx, []string{"history", "--state-dir", state, "--no-start"}, &out, &errb); code != 0 {
		t.Fatalf("history: exit %d, stderr %q", code, errb.String())
	}
	for _, want := range []string{
		"task_landed task 1:",
		"branch=main status=done commit=9f21ab3c4d5e6f708192a3b4c5d6e7f890123456 pushed=local",
		"push_cmd=" + pushCmd,
		"after_land_error=exit 3: after-land boom",
		"task_pushed task 1: branch=main status=done commit=9f21ab3c4d5e6f708192a3b4c5d6e7f890123456 pushed=pushed after_land_error=exit 1: push boom",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("history lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := run(ctx, []string{"status", "--state-dir", state, "--no-start"}, &out, &errb); code != 0 {
		t.Fatalf("status: exit %d, stderr %q", code, errb.String())
	}
	if want := "task #1 done: push by hand: " + pushCmd; !strings.Contains(out.String(), want) {
		t.Errorf("status lacks the local landing's command %q:\n%s", want, out.String())
	}
}

// quoteJSON is s as a JSON string literal.
func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
