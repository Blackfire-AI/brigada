package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// seedTS is the fixed moment the seeded rows carry; the schema treats
// timestamps as opaque UTC text.
const seedTS = "2026-10-12T12:00:00.000000000Z"

// fakeClock is the injectable clock: tests move it instead of sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// env is one engine over a private in-memory store, seeded with the
// rows every test shares: a project, its plan, one task of that plan,
// and two runners.
type env struct {
	t     *testing.T
	ctx   context.Context
	clock *fakeClock
	eng   *Engine
	st    *store.Store

	project, plan, task, runner1, runner2 int64
}

func newEnv(t *testing.T) *env {
	return newEnvCfg(t, nil)
}

// newEnvCfg builds an env, with adjust let loose on the Config before
// the engine is built (zero fields keep their defaults, like New).
func newEnvCfg(t *testing.T, adjust func(*Config)) *env {
	return newEnvAt(t, ":memory:", adjust)
}

// newEnvAt builds an env over the store at path. ":memory:" is the
// usual single-connection store; a temp file is for the tests that need
// the pool to really hand several connections to one database.
func newEnvAt(t *testing.T, path string, adjust func(*Config)) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, ctx: ctx, clock: newFakeClock(), st: st}
	err = st.Update(ctx, func(tx *sql.Tx) error {
		id := func(query string, args ...any) int64 {
			t.Helper()
			res, err := tx.Exec(query, args...)
			if err != nil {
				t.Fatalf("seed %q: %v", query, err)
			}
			v, err := res.LastInsertId()
			if err != nil {
				t.Fatalf("seed %q: last insert id: %v", query, err)
			}
			return v
		}
		e.project = id(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/brigada.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, seedTS, seedTS)
		e.plan = id(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (?, 'a plan', 'active', ?, ?)`, e.project, seedTS, seedTS)
		e.task = e.addTaskTx(tx, "{}")
		e.runner1 = id(`INSERT INTO runners (name, created_at, last_seen_at) VALUES ('runner-1', ?, ?)`, seedTS, seedTS)
		e.runner2 = id(`INSERT INTO runners (name, created_at, last_seen_at) VALUES ('runner-2', ?, ?)`, seedTS, seedTS)
		return nil
	})
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	cfg := Config{Now: e.clock.Now}
	if adjust != nil {
		adjust(&cfg)
	}
	e.eng = New(st, cfg)
	return e
}

// addTaskTx adds another task to the plan, for the tests that need a
// second key.
func (e *env) addTaskTx(tx *sql.Tx, caps string) int64 {
	e.t.Helper()
	res, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
		VALUES (?, 'another task', 'the spec', 'go test ./...', 'implement', 'ready', 'main', ?, ?, ?)`,
		e.plan, caps, seedTS, seedTS)
	if err != nil {
		e.t.Fatalf("add task: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		e.t.Fatalf("add task: last insert id: %v", err)
	}
	return id
}

// addTask adds another task through the store.
func (e *env) addTask(caps string) int64 {
	e.t.Helper()
	var id int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		id = e.addTaskTx(tx, caps)
		return nil
	}); err != nil {
		e.t.Fatalf("add task: %v", err)
	}
	return id
}

// int reads one integer column.
func (e *env) int(query string, args ...any) int64 {
	e.t.Helper()
	var v int64
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&v)
	}); err != nil {
		e.t.Fatalf("query %q %v: %v", query, args, err)
	}
	return v
}

// str reads one text column; NULL reads as the empty string.
func (e *env) str(query string, args ...any) string {
	e.t.Helper()
	var v sql.NullString
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&v)
	}); err != nil {
		e.t.Fatalf("query %q %v: %v", query, args, err)
	}
	return v.String
}

// exec runs one statement the test needs against the raw store.
func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	}); err != nil {
		e.t.Fatalf("exec %q: %v", query, err)
	}
}

// work is the standard work-job submit for the fixture's task.
func (e *env) work() SubmitParams {
	return SubmitParams{
		Key:       fmt.Sprintf("task:%d", e.task),
		ProjectID: e.project,
		TaskID:    e.task,
		Kind:      KindWork,
		Mode:      ModeFresh,
		Prompt:    "the composed prompt",
	}
}

// foremanTurn is a wake-up turn for the fixture's project.
func (e *env) foremanTurn() SubmitParams {
	return SubmitParams{
		Key:       fmt.Sprintf("foreman:%d", e.project),
		ProjectID: e.project,
		Kind:      KindForeman,
		Mode:      ModeSession,
		Prompt:    "wake up: events are waiting",
	}
}

func (e *env) submit(p SubmitParams) SubmitResult {
	e.t.Helper()
	res, err := e.eng.Submit(e.ctx, p)
	if err != nil {
		e.t.Fatalf("submit %s: %v", p.Key, err)
	}
	return res
}

// submitTask runs one whole debug task submit (§12.1), failing the test
// on the engine's refusal.
func (e *env) submitTask(p TaskParams) TaskResult {
	e.t.Helper()
	res, err := e.eng.SubmitTask(e.ctx, p)
	if err != nil {
		e.t.Fatalf("submit task for %s: %v", p.RepoURL, err)
	}
	return res
}

func (e *env) lease(owner int64, harnesses, labels []string) *LeaseGrant {
	e.t.Helper()
	g, err := e.eng.Lease(e.ctx, owner, harnesses, labels)
	if err != nil {
		e.t.Fatalf("lease by runner %d: %v", owner, err)
	}
	return g
}

func (e *env) leaseAny(owner int64) *LeaseGrant {
	e.t.Helper()
	return e.lease(owner, nil, nil)
}

func (e *env) heartbeat(g *LeaseGrant) Answer {
	e.t.Helper()
	ans, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{})
	if err != nil {
		e.t.Fatalf("heartbeat job %d: %v", g.JobID, err)
	}
	return ans
}

func (e *env) complete(g *LeaseGrant, c Completion) Answer {
	e.t.Helper()
	ans, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, c)
	if err != nil {
		e.t.Fatalf("complete job %d: %v", g.JobID, err)
	}
	return ans
}

func (e *env) release(owner int64, g *LeaseGrant, reason string) Answer {
	e.t.Helper()
	ans, err := e.eng.Release(e.ctx, owner, g.JobID, g.LeaseID, reason)
	if err != nil {
		e.t.Fatalf("release job %d: %v", g.JobID, err)
	}
	return ans
}

func (e *env) cancel(jobID int64) Answer {
	e.t.Helper()
	ans, err := e.eng.Cancel(e.ctx, jobID)
	if err != nil {
		e.t.Fatalf("cancel job %d: %v", jobID, err)
	}
	return ans
}

func (e *env) mustSweep() SweepReport {
	e.t.Helper()
	rep, err := e.eng.Sweep(e.ctx)
	if err != nil {
		e.t.Fatalf("sweep: %v", err)
	}
	return rep
}

// expireLease moves the clock past the lease TTL and sweeps, the
// stand-in for a runner that died without another heartbeat.
func (e *env) expireLease() SweepReport {
	e.t.Helper()
	e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second)
	return e.mustSweep()
}

// done is the standard successful completion.
func done() Completion {
	return Completion{Result: `{"status":"done","summary":"finished"}`}
}
