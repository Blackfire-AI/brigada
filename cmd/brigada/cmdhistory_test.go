package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// historyTransport serves one in-process engine's handler without a
// listener, as the removal tests' transport does.
// A non-nil statusCalls counts the /v1/status requests it serves.
type historyTransport struct {
	handler     http.Handler
	statusCalls *atomic.Int32
}

func (h historyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if h.statusCalls != nil && req.URL.Path == "/v1/status" {
		h.statusCalls.Add(1)
	}
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

	// Noise the bare view hides, a worker result and a foreman report.
	report := "All landed.\nSecond line of the report."
	err = st.Update(ctx, func(tx *sql.Tx) error {
		for _, j := range []string{
			`(7, 1, 1, 'work', 'task:1', 'fresh', '', 'done', ?, ?)`,
			`(41, 1, NULL, 'foreman', 'report:41', 'fresh', '', 'done', ?, ?)`,
		} {
			if _, err := tx.Exec(`INSERT INTO jobs (id, project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
				VALUES `+j, now, now); err != nil {
				return err
			}
		}
		for _, ev := range []struct{ kind, payload string }{
			{"job_leased", `{"lease":"L1"}`},
			{"job_cap_held", `{}`},
			{"job_created", `{"branch":"task/1-1"}`},
			{"job_agent_picked", `{}`},
		} {
			if _, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
				VALUES (?, 1, 1, 7, ?, ?)`, ev.kind, ev.payload, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO events (kind, project_id, job_id, payload, created_at)
			VALUES ('foreman_report', 1, 41, ?, ?)`, `{"report":`+quoteJSON(strings.ReplaceAll(report, "\n", `\n`))+`}`, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO messages (project_id, kind, author, content, created_at)
			VALUES (1, 'worker_result', 'worker', ?, ?)`, `{"status":"done","summary":"Shipped the gizmo."}`, now)
		return err
	})
	if err != nil {
		t.Fatalf("seed the noise: %v", err)
	}

	var out, errb bytes.Buffer
	historyOut := func(args ...string) string {
		t.Helper()
		out.Reset()
		errb.Reset()
		full := append([]string{"history", "--state-dir", state, "--no-start"}, args...)
		if code := run(ctx, full, &out, &errb); code != 0 {
			t.Fatalf("history %v: exit %d, stderr %q", args, code, errb.String())
		}
		return out.String()
	}

	// The default output: a table in local time, a landing with its title,
	// its short commit and the after-land flag, nothing internal, no JSON,
	// no nanoseconds, and none of the landing record --task carries.
	bare := historyOut()
	for _, want := range []string{
		"  task    #1   landed, push waits  ship the gizmo  9f21ab3  after-land failed: --task 1\n",
		"  task    #1   pushed              ship the gizmo  9f21ab3  after-land failed: --task 1\n",
		"  report  #41\n",
	} {
		if !strings.Contains(bare, want) {
			t.Errorf("history lacks %q:\n%s", want, bare)
		}
	}
	for _, bad := range []string{"push_cmd=", "after_land_error=", "after-land boom", "push boom", "9f21ab3c4d5e", "job_leased", "job_cap_held", "job_created", "job_agent_picked", "worker_result", "Shipped", "{", "All landed", now[:19]} {
		if strings.Contains(bare, bad) {
			t.Errorf("bare history shows %q:\n%s", bad, bare)
		}
	}
	local := time.Now().Local().Format("01-02 15:04")
	if prev := time.Now().Add(-time.Minute).Local().Format("01-02 15:04"); !strings.HasPrefix(bare, local+" ") && !strings.HasPrefix(bare, prev+" ") {
		t.Errorf("history does not start with the local time %q:\n%s", local, bare)
	}

	// --task shows the worker result as words; --job shows the report whole.
	task := historyOut("--task", "1")
	if !strings.Contains(task, "job 7 job leased: lease=L1") || !strings.Contains(task, " #1 landed, push waits: ") || strings.Contains(task, `{"status"`) {
		t.Errorf("history --task:\n%s", task)
	}
	// The landing record the bare view leaves out: the target branch, the
	// full commit, the push command and the after_land failures, of the
	// landing and of the push.
	for _, want := range []string{
		" #1 landed, push waits: ",
		"branch=main",
		"commit=9f21ab3c4d5e6f708192a3b4c5d6e7f890123456",
		"push_cmd=" + pushCmd,
		"after_land_error=exit 3: after-land boom",
		" #1 pushed: ",
		"after_land_error=exit 1: push boom",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("history --task lacks %q:\n%s", want, task)
		}
	}
	if job := historyOut("--job", "41"); !strings.Contains(job, "report #41: report=All landed.\n  Second line of the report.\n") {
		t.Errorf("history --job lacks the whole report:\n%s", job)
	}

	// --raw is history.Render's output, bare and with --task.
	for _, args := range [][]string{{"--raw"}, {"--raw", "--task", "1"}} {
		var page history.Page
		query := api.HistoryQuery{Limit: 100}
		if len(args) > 1 {
			query.TaskID = 1
		}
		_, token, terr := lifecycle.TokenForRole(paths.Tokens, "client")
		if terr != nil {
			t.Fatal(terr)
		}
		client := api.NewClientWithHTTPClient(&http.Client{Transport: historyTransport{handler: srv.Handler()}}, token)
		page, err = client.History(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		var want bytes.Buffer
		if err := history.Render(&want, page.Items); err != nil {
			t.Fatal(err)
		}
		if got := historyOut(args...); got != want.String() {
			t.Errorf("history %v =\n%s\nwant history.Render's\n%s", args, got, want.String())
		}
	}

	// A misspelled flag is a usage error.
	if code := run(ctx, []string{"history", "--rw", "--state-dir", state, "--no-start"}, &out, &errb); code != 2 {
		t.Errorf("history --rw: exit %d, want 2", code)
	}

	out.Reset()
	if code := run(ctx, []string{"history", "--raw", "--state-dir", state, "--no-start"}, &out, &errb); code != 0 {
		t.Fatalf("history --raw: exit %d, stderr %q", code, errb.String())
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
