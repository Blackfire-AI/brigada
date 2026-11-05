package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// historyTableEnv is an engine with 30 landed tasks in five finished plans,
// 60 allowlisted events mixed with noise, behind connectClient.
type historyTableEnv struct {
	t           *testing.T
	state       string
	statusCalls *atomic.Int32
}

// historyBase is the time the fixture's events count minutes from.
var historyBase = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func (e *historyTableEnv) stamp(minutes float64) string {
	return historyBase.Add(time.Duration(minutes * float64(time.Minute))).Format(engine.TimeLayout)
}

// local is the bare time column of an event minutes into the fixture.
func (e *historyTableEnv) local(minutes float64) string {
	return historyBase.Add(time.Duration(minutes * float64(time.Minute))).Local().Format("01-02 15:04")
}

func newHistoryTableEnv(t *testing.T) *historyTableEnv {
	t.Helper()
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
	e := &historyTableEnv{t: t, state: state, statusCalls: new(atomic.Int32)}
	err = st.Update(ctx, func(tx *sql.Tx) error {
		exec := func(q string, args ...any) error {
			_, err := tx.Exec(q, args...)
			return err
		}
		if err := exec(`INSERT INTO projects (id, repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (1, 'example.invalid/org/app', 'main', '{}', 'claude', 'sonnet', ?, ?)`, e.stamp(0), e.stamp(0)); err != nil {
			return err
		}
		for p := 1; p <= 5; p++ {
			if err := exec(`INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
				VALUES (?, 1, ?, 'done', ?, ?)`, p, fmt.Sprintf("plan %d title", p), e.stamp(float64(p)), e.stamp(float64(p))); err != nil {
				return err
			}
		}
		for i := 1; i <= 30; i++ {
			plan := (i-1)%5 + 1
			if err := exec(`INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, created_at, updated_at)
				VALUES (?, ?, ?, 'Spec.', 'implement', 'done', 'main', ?, ?)`, i, plan, fmt.Sprintf("task %d title", i), e.stamp(float64(i)), e.stamp(float64(i))); err != nil {
				return err
			}
		}
		if err := exec(`INSERT INTO jobs (id, project_id, task_id, kind, key, mode, prompt, status, created_at, updated_at)
			VALUES (7, 1, 30, 'work', 'task:30', 'fresh', '', 'done', ?, ?)`, e.stamp(0), e.stamp(0)); err != nil {
			return err
		}
		event := func(kind string, task int64, job int64, payload string, at float64) error {
			var taskArg, jobArg any
			if task != 0 {
				taskArg = task
			}
			if job != 0 {
				jobArg = job
			}
			return exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at) VALUES (?, 1, ?, ?, ?, ?)`,
				kind, taskArg, jobArg, payload, e.stamp(at))
		}
		for i := 1; i <= 30; i++ {
			at := float64(4 * i)
			if err := event("task_running", int64(i), 0, `{}`, at); err != nil {
				return err
			}
			if i == 30 {
				if err := event("job_leased", 30, 7, `{"lease":"L1"}`, at+0.5); err != nil {
					return err
				}
			}
			if err := event("task_done", int64(i), 0, `{"status":"done"}`, at+1); err != nil {
				return err
			}
			if err := exec(`INSERT INTO messages (project_id, kind, author, content, created_at) VALUES (1, 'worker_result', 'worker', 'noise', ?)`, e.stamp(at+2)); err != nil {
				return err
			}
			if err := event("task_landed", int64(i), 0, fmt.Sprintf(`{"commit":"c%02d%s","pushed":"pushed"}`, i, strings.Repeat("0", 20)), at+3); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := connectClient
	connectClient = func(_ context.Context, rt *runtime, role string, _ bool, stderr io.Writer) (*api.Client, int) {
		_, token, err := lifecycle.TokenForRole(lifecycle.PathsOf(rt.stateDir).Tokens, role)
		if err != nil {
			return nil, fail(stderr, err)
		}
		return api.NewClientWithHTTPClient(&http.Client{Transport: historyTransport{handler: srv.Handler(), statusCalls: e.statusCalls}}, token), 0
	}
	t.Cleanup(func() { connectClient = old })
	return e
}

func (e *historyTableEnv) history(args ...string) string {
	e.t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"history", "--state-dir", e.state, "--no-start"}, args...)
	if code := run(context.Background(), full, &out, &errb); code != 0 {
		e.t.Fatalf("history %v: exit %d, stderr %q", args, code, errb.String())
	}
	return out.String()
}

func TestHistoryTableLastFifty(t *testing.T) {
	e := newHistoryTableEnv(t)
	check := func(args []string, first, rows int, whatCol int) {
		t.Helper()
		e.statusCalls.Store(0)
		out := e.history(args...)
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != rows {
			t.Fatalf("history %v: %d lines, want %d:\n%s", args, len(lines), rows, out)
		}
		if n := e.statusCalls.Load(); n != 1 {
			t.Errorf("history %v made %d status calls, want 1", args, n)
		}
		commitCol := -1
		for k, line := range lines {
			task := first + k/2
			what := "done"
			if k%2 == 1 {
				what = "landed"
			}
			if f := strings.Fields(line); len(f) < 5 || f[2] != "task" || f[3] != fmt.Sprintf("#%d", task) || !strings.HasPrefix(line[whatCol:], what) {
				t.Errorf("history %v line %d = %q, want task #%d %s", args, k, line, task, what)
			}
			title := fmt.Sprintf("task %d title", task)
			if !strings.Contains(line, "  "+title) {
				t.Errorf("history %v line %d lacks its title %q: %q", args, k, title, line)
			}
			if what == "landed" {
				i := strings.LastIndex(line, "  ") + 2
				if commitCol == -1 {
					commitCol = i
				}
				if i != commitCol || line[i:] != fmt.Sprintf("c%02d0000", task) {
					t.Errorf("history %v line %d commit at %d (%q), want column %d", args, k, i, line[i:], commitCol)
				}
			}
		}
	}
	// The default view: the last 50 of 60 rows, oldest first, whatever the
	// page size. The older plans' titles come from the all-plans status.
	check(nil, 6, 50, 24)
	check([]string{"--limit", "7"}, 6, 50, 24)
	check([]string{"--limit", "500"}, 6, 50, 24)
	// --all prints every row, aligned by the status call alone.
	check([]string{"--all"}, 1, 60, 26)
	check([]string{"--all", "--limit", "7"}, 1, 60, 26)

	plain := e.history()
	for _, args := range [][]string{{"--limit", "1"}, {"--limit", "500"}} {
		if got := e.history(args...); got != plain {
			t.Errorf("history %v differs from plain history", args)
		}
	}
	// A task of the oldest finished plan, which client.Status leaves out.
	if !strings.Contains(e.history("--all"), "task 1 title") {
		t.Error("history --all lacks the title of a task of an old finished plan")
	}
}

func TestHistoryDetailAndRawAreUnchanged(t *testing.T) {
	e := newHistoryTableEnv(t)
	e.statusCalls.Store(0)
	sha := "c30" + strings.Repeat("0", 20)
	detailTask := e.local(120) + " task 30 task running\n" +
		e.local(120.5) + " job 7 job leased: lease=L1\n" +
		e.local(121) + " #30 done: status=done\n" +
		e.local(123) + " #30 landed: commit=" + sha + " pushed=pushed\n"
	detailJob := e.local(120.5) + " job 7 job leased: lease=L1\n"
	rawTask := e.stamp(120) + " task_running task 30: \n" +
		e.stamp(120.5) + " job_leased job 7: lease=L1\n" +
		e.stamp(121) + " task_done task 30: status=done\n" +
		e.stamp(123) + " task_landed task 30: commit=" + sha + " pushed=pushed\n"
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--task", "30"}, detailTask},
		{[]string{"--job", "7"}, detailJob},
		{[]string{"--raw", "--task", "30"}, rawTask},
	}
	for _, c := range cases {
		if got := e.history(c.args...); got != c.want {
			t.Errorf("history %v =\n%q\nwant\n%q", c.args, got, c.want)
		}
		for _, extra := range [][]string{{"--all"}, {"--limit", "1"}} {
			if got := e.history(append(append([]string{}, c.args...), extra...)...); got != c.want {
				t.Errorf("history %v %v differs", c.args, extra)
			}
		}
	}
	raw := e.history("--raw")
	if !strings.Contains(raw, "task_landed task 30") || !strings.Contains(raw, "worker_result") {
		t.Errorf("history --raw lacks the timeline:\n%s", raw)
	}
	for _, args := range [][]string{{"--raw", "--limit", "1"}, {"--raw", "--all"}} {
		if got := e.history(args...); got != raw {
			t.Errorf("history %v differs from history --raw", args)
		}
	}
	task1 := e.history("--task", "1")
	if got := e.history("--task", "1", "--limit", "1"); got != task1 {
		t.Error("history --task 1 --limit 1 differs")
	}
	if n := e.statusCalls.Load(); n != 0 {
		t.Errorf("--task, --job and --raw made %d status calls, want none", n)
	}
}

func TestHistoryAllUsageError(t *testing.T) {
	e := newHistoryTableEnv(t)
	var out, errb bytes.Buffer
	for _, args := range [][]string{{"--al"}, {"--all=maybe"}, {"--limit", "0", "--all"}} {
		full := append([]string{"history", "--state-dir", e.state, "--no-start"}, args...)
		if code := run(context.Background(), full, &out, &errb); code != 2 {
			t.Errorf("history %v: exit %d, want 2", args, code)
		}
	}
}
