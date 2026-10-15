package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/store"
)

func TestLoopCursorSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")

	st := openStore(t, ctx, path)
	seedProject(t, ctx, st)
	// A log the engine might have written: routine events interleaved
	// with the two kinds that notify.
	appendEvent(t, ctx, st, "job_created", 0, 0, "{}")
	appendEvent(t, ctx, st, KindTaskCheckFailed, 0, 1, `{"status":"needs_foreman","reason":"exit 1"}`)
	appendEvent(t, ctx, st, "task_running", 0, 1, "{}")
	appendEvent(t, ctx, st, KindPlanDone, 0, 2, `{"status":"done"}`)

	fake := &Fake{}
	if err := NewLoop(st, fake, nil).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	notes := fake.Notes()
	if len(notes) != 2 {
		t.Fatalf("delivered %d notifications %v, want 2", len(notes), notes)
	}
	if notes[0] != (Note{"brigada", "task #1 needs you: exit 1"}) {
		t.Errorf("note 0 = %+v", notes[0])
	}
	if notes[1] != (Note{"brigada", "plan #1 done: Ship it"}) {
		t.Errorf("note 1 = %+v", notes[1])
	}
	if got := cursorOf(t, ctx, st); got != 4 {
		t.Fatalf("cursor = %d, want 4 (the last event read)", got)
	}
	st.Close()

	// A restart with the cursor already persisted: nothing old is
	// re-notified, and a new event is neither missed nor duplicated.
	st = openStore(t, ctx, path)
	defer st.Close()
	fresh := &Fake{}
	if err := NewLoop(st, fresh, nil).Flush(ctx); err != nil {
		t.Fatalf("flush after restart: %v", err)
	}
	if n := fresh.Notes(); len(n) != 0 {
		t.Fatalf("restart re-notified %v, want nothing old", n)
	}
	appendEvent(t, ctx, st, KindTaskNeedsHelp, 0, 2, `{"status":"needs_foreman","question":"which way?"}`)
	if err := NewLoop(st, fresh, nil).Flush(ctx); err != nil {
		t.Fatalf("flush the new event: %v", err)
	}
	if n := fresh.Notes(); len(n) != 1 || n[0] != (Note{"brigada", "task #2 needs you: which way?"}) {
		t.Fatalf("after restart: notes %v, want exactly the new event once", n)
	}
}

func TestLoopFlushDeliversPending(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)

	appendEvent(t, ctx, st, KindReviewChanges, 0, 1, `{"status":"needs_foreman","findings":"the retry loop"}`)
	appendEvent(t, ctx, st, KindReviewNoVerdict, 0, 1, `{"status":"needs_foreman","reason":"empty verdict"}`)

	fake := &Fake{}
	loop := NewLoop(st, fake, nil)
	if err := loop.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	notes := fake.Notes()
	if len(notes) != 2 ||
		notes[0] != (Note{"brigada", "task #1 needs you: the retry loop"}) ||
		notes[1] != (Note{"brigada", "task #1 needs you: empty verdict"}) {
		t.Fatalf("notes %v, want both pending events in order", notes)
	}
	if err := loop.Flush(ctx); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if n := fake.Notes(); len(n) != 2 {
		t.Fatalf("second flush delivered %d more: %v", len(n)-2, n)
	}
}

func TestLoopFailingNotifierDoesNotStall(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)

	for i := 0; i < 3; i++ {
		appendEvent(t, ctx, st, KindTaskCheckFailed, 0, 1,
			fmt.Sprintf(`{"status":"needs_foreman","reason":"failure %d"}`, i+1))
	}

	fake := &Fake{Err: errors.New("no banner service")}
	loop := NewLoop(st, fake, nil)
	if err := loop.Flush(ctx); err != nil {
		t.Fatalf("flush with a broken notifier = %v, want the failure logged, not fatal", err)
	}
	if n := fake.Notes(); len(n) != 3 {
		t.Fatalf("attempted %d notifications, want 3", len(n))
	}
	if got := cursorOf(t, ctx, st); got != 3 {
		t.Fatalf("cursor = %d, want 3: it advances past failed deliveries", got)
	}

	// The notifier heals; only what arrived since the cursor notifies —
	// the broken three never repeat.
	fake.Err = nil
	appendEvent(t, ctx, st, KindTaskFailed, 0, 1, `{"status":"needs_foreman","reason":"still down"}`)
	if err := loop.Flush(ctx); err != nil {
		t.Fatalf("flush after healing: %v", err)
	}
	notes := fake.Notes()
	if len(notes) != 4 || notes[3] != (Note{"brigada", "task #1 needs you: still down"}) {
		t.Fatalf("notes %v, want only the new event after the cursor", notes)
	}
}

// TestLoopDeliversForemanReport covers a report's delivery (§3, §10):
// the banner carries the foreman's name and the report's first line,
// and — unlike a parked task's kinds — no gate holds it back.
func TestLoopDeliversForemanReport(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)

	appendEvent(t, ctx, st, "job_created", 0, 0, "{}")
	appendEvent(t, ctx, st, KindForemanReport, 1, 0,
		`{"report":"plan #1 done: merge task/1-1 then task/2-1\nnothing else needs you"}`)

	fake := &Fake{}
	if err := NewLoop(st, fake, nil).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	notes := fake.Notes()
	if len(notes) != 1 || notes[0] != (Note{foremanTitle, "plan #1 done: merge task/1-1 then task/2-1"}) {
		t.Fatalf("notes %v, want the report's first line under the foreman's title", notes)
	}
}

func TestLoopNoneNotifierStillAdvances(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	appendEvent(t, ctx, st, KindTaskCheckFailed, 0, 1, `{"status":"needs_foreman","reason":"exit 9"}`)

	if err := NewLoop(st, None{}, nil).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := cursorOf(t, ctx, st); got != 1 {
		t.Fatalf("cursor = %d, want 1 even with notifications off", got)
	}
}

// cancelAfterK cancels the flush's context from inside the k-th
// notification — what a shutdown mid-pass looks like from inside the
// loop, landing between two events.
type cancelAfterK struct {
	fake   *Fake
	k      int
	seen   int
	cancel context.CancelFunc
}

func (c *cancelAfterK) Notify(ctx context.Context, title, body string) error {
	err := c.fake.Notify(ctx, title, body)
	c.seen++
	if c.seen == c.k {
		c.cancel()
	}
	return err
}

// TestLoopCursorAdvancesPerEvent stops the flush right after the third
// of five banners: the cursor must be at that third event — it advances
// one row at a time — so a crash mid-pass re-delivers at most the
// in-flight banner. The next flush delivers the remainder once: no
// duplicates, no gaps.
func TestLoopCursorAdvancesPerEvent(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, appendEvent(t, ctx, st, KindTaskCheckFailed, 0, 1,
			fmt.Sprintf(`{"status":"needs_foreman","reason":"failure %d"}`, i+1)))
	}

	flushCtx, cancel := context.WithCancel(ctx)
	fake := &Fake{}
	loop := NewLoop(st, &cancelAfterK{fake: fake, k: 3, cancel: cancel}, nil)
	if err := loop.Flush(flushCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush with a cancelled context = %v, want context.Canceled", err)
	}
	if n := fake.Notes(); len(n) != 3 {
		t.Fatalf("delivered %d notifications before the cancel, want 3", len(n))
	}
	if got := cursorOf(t, ctx, st); got != ids[2] {
		t.Fatalf("cursor = %d, want %d: it stops at the last event processed", got, ids[2])
	}

	// The unprocessed events are not lost and nothing repeats: the next
	// flush delivers exactly the remainder.
	if err := loop.Flush(ctx); err != nil {
		t.Fatalf("flush after the cancel: %v", err)
	}
	notes := fake.Notes()
	if len(notes) != 5 {
		t.Fatalf("notes %v, want all five events delivered across the two flushes", notes)
	}
	for i, note := range notes {
		if want := fmt.Sprintf("task #1 needs you: failure %d", i+1); note.Body != want {
			t.Fatalf("note %d = %q, want %q: no duplicates, no gaps", i, note.Body, want)
		}
	}
	if got := cursorOf(t, ctx, st); got != ids[4] {
		t.Fatalf("cursor = %d, want %d", got, ids[4])
	}
}

// cancelErrAfterK succeeds for the first k-1 banners, then cancels the
// flush's context and returns an error — a shutdown landing inside the
// k-th banner's exec: the tool is killed mid-flight, no banner shows,
// and the loop must not advance past that event.
type cancelErrAfterK struct {
	fake   *Fake
	k      int
	seen   int
	err    error
	cancel context.CancelFunc
}

func (c *cancelErrAfterK) Notify(ctx context.Context, title, body string) error {
	c.seen++
	if c.seen == c.k {
		c.cancel()
		return c.err
	}
	return c.fake.Notify(ctx, title, body)
}

// TestLoopCancelledBannerIsNotDropped: a shutdown that kills the second
// banner's exec must not advance the cursor past that event — no banner
// showed, so advancing would lose it. The next flush delivers it once:
// no duplicates, no gaps.
func TestLoopCancelledBannerIsNotDropped(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, appendEvent(t, ctx, st, KindTaskCheckFailed, 0, 1,
			fmt.Sprintf(`{"status":"needs_foreman","reason":"failure %d"}`, i+1)))
	}

	flushCtx, cancel := context.WithCancel(ctx)
	fake := &Fake{}
	loop := NewLoop(st, &cancelErrAfterK{fake: fake, k: 2,
		err: errors.New("killed by shutdown"), cancel: cancel}, nil)
	if err := loop.Flush(flushCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush = %v, want context.Canceled", err)
	}
	if got := cursorOf(t, ctx, st); got != ids[0] {
		t.Fatalf("cursor = %d, want %d: the killed banner's event stays past the cursor", got, ids[0])
	}

	// The killed event is not lost: the next flush delivers it and the
	// rest, exactly once each.
	if err := loop.Flush(ctx); err != nil {
		t.Fatalf("flush after the shutdown: %v", err)
	}
	notes := fake.Notes()
	if len(notes) != 3 {
		t.Fatalf("notes %v, want all three events delivered across the two flushes", notes)
	}
	for i, note := range notes {
		if want := fmt.Sprintf("task #1 needs you: failure %d", i+1); note.Body != want {
			t.Fatalf("note %d = %q, want %q: no duplicates, no gaps", i, note.Body, want)
		}
	}
	if got := cursorOf(t, ctx, st); got != ids[2] {
		t.Fatalf("cursor = %d, want %d", got, ids[2])
	}
}

func openStore(t *testing.T, ctx context.Context, path string) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// seedProject installs one project (id 1) with one plan (id 1, "Ship
// it") and two tasks (ids 1, 2), the shapes the loop's query joins to.
func seedProject(t *testing.T, ctx context.Context, st *store.Store) {
	t.Helper()
	execSQL(t, st, `INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES ('https://example.com/r.git', 'main', '{}', 'claude', 'sonnet', '2026-10-13T00:00:00Z', '2026-10-13T00:00:00Z')`)
	execSQL(t, st, `INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
		VALUES (1, 1, 'Ship it', 'done', '2026-10-13T00:00:00Z', '2026-10-13T00:00:00Z')`)
	execSQL(t, st, `INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, created_at, updated_at)
		VALUES (1, 1, 'First', 'spec', 'implement', 'needs_foreman', 'main', '2026-10-13T00:00:00Z', '2026-10-13T00:00:00Z')`)
	execSQL(t, st, `INSERT INTO tasks (id, plan_id, title, spec, class, state, base_ref, created_at, updated_at)
		VALUES (2, 1, 'Second', 'spec', 'implement', 'done', 'main', '2026-10-13T00:00:00Z', '2026-10-13T00:00:00Z')`)
}

// appendEvent adds one row to the log the way the engine writes it:
// kind, the task it names, and the payload JSON. It returns the row's
// id, for tests that pin the cursor's position.
// TestLoopPlanDoneNamesPlanFromPayload: a plan that finishes by an
// amendment or an approval has no task to resolve its plan through; the
// event's payload names it.
func TestLoopPlanDoneNamesPlanFromPayload(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	appendEvent(t, ctx, st, KindPlanDone, 0, 0, `{"status":"done","plan":1}`)

	fake := &Fake{}
	if err := NewLoop(st, fake, nil).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if notes := fake.Notes(); len(notes) != 1 || notes[0] != (Note{"brigada", "plan #1 done: Ship it"}) {
		t.Fatalf("notes = %v, want the plan named from the payload", notes)
	}
}

func appendEvent(t *testing.T, ctx context.Context, st *store.Store, kind string, project, task int64, payload string) int64 {
	t.Helper()
	var projectRef, taskRef any
	if project != 0 {
		projectRef = project
	}
	if task != 0 {
		taskRef = task
	}
	var id int64
	err := st.Update(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES (?, ?, ?, NULL, ?, '2026-10-13T00:00:00Z')`, kind, projectRef, taskRef, payload)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("append event %s: %v", kind, err)
	}
	return id
}

func cursorOf(t *testing.T, ctx context.Context, st *store.Store) int64 {
	t.Helper()
	var cursor int64
	err := st.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT COALESCE(notify_cursor, 0) FROM meta WHERE id = 1`).Scan(&cursor)
	})
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	return cursor
}

func execSQL(t *testing.T, st *store.Store, q string) {
	t.Helper()
	if err := st.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(q)
		return err
	}); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
