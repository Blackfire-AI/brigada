package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorded is one POST the test relay received: the URL path and the
// decoded body.
type recorded struct {
	Path string
	Body webhookBody
	Raw  string
}

// relay is a local httptest server that records every POST it receives.
type relay struct {
	srv   *httptest.Server
	mu    sync.Mutex
	posts []recorded
	// code answers the first fail POSTs with 500, the rest with 200; 0
	// answers everything with 200.
	fail int32
	// delay holds each handler before answering, to play a relay that
	// never answers in time.
	delay time.Duration
}

func newRelay(t *testing.T) *relay {
	t.Helper()
	r := &relay{}
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, req *http.Request) {
		var body webhookBody
		buf, _ := io.ReadAll(req.Body)
		json.Unmarshal(buf, &body)
		r.mu.Lock()
		r.posts = append(r.posts, recorded{Path: req.URL.Path, Body: body, Raw: string(buf)})
		fail := atomic.LoadInt32(&r.fail)
		if fail > 0 {
			atomic.AddInt32(&r.fail, -1)
		}
		r.mu.Unlock()
		// The delay sits after the recording: a relay that answers too
		// late still saw the request, and the client's timeout does not
		// undo it.
		if r.delay > 0 {
			time.Sleep(r.delay)
		}
		if fail > 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

// hook is r's webhook, pointed at the relay's URL.
func (r *relay) hook() *Webhook {
	return &Webhook{URL: r.srv.URL + "/hook"}
}

// received returns the POSTs recorded so far.
func (r *relay) received() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.posts...)
}

// TestWebhookPayloadForEachKind walks one event of every kind that
// notifies through a flush with the webhook on: each delivered
// notification is one POST whose fields are the event's — the project
// repo URL, the kind, the banner's own one-line text, the task and plan
// ids, the log's time — and nothing else is posted.
func TestWebhookPayloadForEachKind(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)

	kinds := []struct {
		kind string
		task int64
		plan int64
		body string
		text string
	}{
		{KindPlanAwaitingApproval, 0, 1, `{"plan":1,"reason":"2 tasks at once"}`,
			"plan #1 needs your approval: 2 tasks at once: `brigada plan approve 1` or `brigada plan reject 1`"},
		{KindPlanDone, 0, 1, `{"plan":1,"status":"done"}`, "plan #1 done: Ship it"},
		{KindTaskCheckFailed, 1, 1, `{"status":"needs_foreman","reason":"exit 1"}`, "task #1 needs you: exit 1"},
		{KindTaskNeedsHelp, 1, 1, `{"status":"needs_foreman","question":"which way?"}`, "task #1 needs you: which way?"},
		{KindTaskFailed, 1, 1, `{"status":"needs_foreman","reason":"boom"}`, "task #1 needs you: boom"},
		{KindReviewChanges, 1, 1, `{"status":"needs_foreman","findings":"the retry loop"}`, "task #1 needs you: the retry loop"},
		{KindReviewNoVerdict, 1, 1, `{"status":"needs_foreman","reason":"empty verdict"}`, "task #1 needs you: empty verdict"},
		{KindTaskOutOfRounds, 1, 1, `{"status":"needs_foreman","reason":"rounds spent"}`, "task #1 needs you: rounds spent"},
		{KindReviewUnservable, 1, 1, `{"reason":"no agent of another family"}`, "task #1 reviewing: no agent of another family"},
		{KindForemanReport, 0, 0, `{"report":"plan #1 done\nnothing else needs you"}`, "plan #1 done"},
		{KindForemanLimited, 0, 0, `{"until":"2026-10-21T18:00:00Z"}`,
			"the foreman hit its limit (until 2026-10-21T18:00:00Z): wait, or switch it with `brigada project foreman`"},
		{KindForemanWakeBudgetSpent, 0, 0, `{"count":20,"until":"2026-10-22T00:00:00Z"}`,
			"the foreman's wake budget is spent (20/20 today, resets 2026-10-22T00:00:00Z); raise it with `brigada budget --wakes N` or send `brigada ask`"},
		{KindAgentBroken, 0, 0, `{"harness":"claude","model":"sonnet","reason":"login expired"}`,
			"agent broken, claude/sonnet: login expired: fix it, then `brigada agent reset <harness>/<model>`"},
	}
	for _, k := range kinds {
		appendEvent(t, ctx, st, k.kind, 1, k.task, k.body)
	}
	// A routine event between them: never posted.
	appendEvent(t, ctx, st, "job_created", 1, 1, "{}")

	r := newRelay(t)
	loop := NewLoop(st, None{}, nil).WithWebhook(r.hook())
	if err := loop.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// The posts leave the flush on the worker: wait for their arrival.
	waitFor(t, "one post per notifying kind", func() bool {
		return len(r.received()) >= len(kinds)
	})

	posts := r.received()
	if len(posts) != len(kinds) {
		t.Fatalf("%d posts, want one per notifying kind (%d)", len(posts), len(kinds))
	}
	for i, k := range kinds {
		p := posts[i]
		if p.Path != "/hook" {
			t.Errorf("post %d path %q, want the webhook's", i, p.Path)
		}
		if p.Body.Project != "https://example.com/r.git" {
			t.Errorf("kind %s: project %q, want the project's repo URL", k.kind, p.Body.Project)
		}
		if p.Body.Kind != k.kind {
			t.Errorf("post %d kind %q, want %q", i, p.Body.Kind, k.kind)
		}
		if p.Body.Task != k.task {
			t.Errorf("kind %s: task %d, want %d", k.kind, p.Body.Task, k.task)
		}
		if p.Body.Plan != k.plan {
			t.Errorf("kind %s: plan %d, want %d", k.kind, p.Body.Plan, k.plan)
		}
		if p.Body.Time != "2026-10-13T00:00:00Z" {
			t.Errorf("kind %s: time %q, want the log's stamp", k.kind, p.Body.Time)
		}
		if p.Body.Text != k.text {
			t.Errorf("kind %s: text %q, want the banner's body %q", k.kind, p.Body.Text, k.text)
		}
	}
}

// TestWebhookRetriesOnceOnFailure: a POST that answers 500 is retried
// exactly once, the same body again; the retry's success is delivery,
// the cursor advances and the flush reports nothing.
func TestWebhookRetriesOnceOnFailure(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	id := appendEvent(t, ctx, st, KindTaskCheckFailed, 1, 1, `{"status":"needs_foreman","reason":"exit 1"}`)

	r := newRelay(t)
	atomic.StoreInt32(&r.fail, 1) // the first attempt answers 500
	if err := NewLoop(st, None{}, nil).WithWebhook(r.hook()).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	waitFor(t, "the attempt and its one retry", func() bool {
		return len(r.received()) >= 2
	})
	posts := r.received()
	if len(posts) != 2 {
		t.Fatalf("%d posts, want the attempt and its one retry", len(posts))
	}
	if posts[0].Raw != posts[1].Raw {
		t.Errorf("retry body %q, want the same %q", posts[1].Raw, posts[0].Raw)
	}
	if got := cursorOf(t, ctx, st); got != id {
		t.Fatalf("cursor = %d, want %d: a delivered webhook advances it", got, id)
	}
}

// TestWebhookExhaustedRetriesNeverBlock: a relay that fails both
// attempts is a logged failure and nothing more — the flush returns,
// the cursor advances past the event, the engine never waits on it.
func TestWebhookExhaustedRetriesNeverBlock(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	id := appendEvent(t, ctx, st, KindTaskCheckFailed, 1, 1, `{"status":"needs_foreman","reason":"exit 1"}`)

	r := newRelay(t)
	atomic.StoreInt32(&r.fail, 2) // both attempts answer 500
	if err := NewLoop(st, None{}, nil).WithWebhook(r.hook()).Flush(ctx); err != nil {
		t.Fatalf("flush: %v, want the failure logged, not fatal", err)
	}
	waitFor(t, "the two failed attempts", func() bool {
		return len(r.received()) >= 2
	})
	if posts := r.received(); len(posts) != 2 {
		t.Fatalf("%d posts, want exactly two attempts", len(posts))
	}
	if got := cursorOf(t, ctx, st); got != id {
		t.Fatalf("cursor = %d, want %d: a failing webhook never holds it back", got, id)
	}
}

// TestWebhookTimeoutDoesNotBlock: a relay that never answers in time
// costs its worker one short timeout per attempt — two with the retry —
// and the flush nothing: it returns at once, the posts arrive when
// their attempts time out.
func TestWebhookTimeoutDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	appendEvent(t, ctx, st, KindTaskCheckFailed, 1, 1, `{"status":"needs_foreman","reason":"exit 1"}`)

	r := newRelay(t)
	r.delay = 500 * time.Millisecond
	hook := r.hook()
	hook.Timeout = 20 * time.Millisecond

	start := time.Now()
	if err := NewLoop(st, None{}, nil).WithWebhook(hook).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("flush took %v, want it back at once with the post queued", elapsed)
	}
	waitFor(t, "the attempt and its one retry", func() bool {
		return len(r.received()) >= 2
	})
	if got := cursorOf(t, ctx, st); got == 0 {
		t.Fatal("cursor did not advance past the timed-out event")
	}
}

// TestWebhookHungRelayDoesNotDelayTheTick: the posts leave the engine's
// tick, so a relay that hangs on every request slows only its own
// worker. A flush over a batch of events returns in milliseconds, the
// cursor lands at the log's end without waiting for one answer, and the
// posts trickle in as the worker's timeouts let them go.
func TestWebhookHungRelayDoesNotDelayTheTick(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	const events = 6
	var last int64
	for i := 0; i < events; i++ {
		last = appendEvent(t, ctx, st, KindTaskCheckFailed, 1, 1,
			fmt.Sprintf(`{"status":"needs_foreman","reason":"failure %d"}`, i+1))
	}

	r := newRelay(t)
	r.delay = 300 * time.Millisecond // every answer outlives the attempt
	hook := r.hook()
	hook.Timeout = 50 * time.Millisecond

	start := time.Now()
	if err := NewLoop(st, None{}, nil).WithWebhook(hook).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	elapsed := time.Since(start)
	// The worker alone needs events × (attempt + retry) × timeout to
	// walk the queue — well past a second here; the flush must not wait
	// for any of it.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("flush took %v with a hung relay, want the tick free of webhook I/O", elapsed)
	}
	if got := cursorOf(t, ctx, st); got != last {
		t.Fatalf("cursor = %d, want %d: the cursor advances without one answer", got, last)
	}
	waitFor(t, "every post through the hung relay", func() bool {
		return len(r.received()) >= 2*events // each attempt, the retry included
	})
	seen := map[string]bool{}
	for _, p := range r.received() {
		seen[p.Body.Text] = true
	}
	for i := 0; i < events; i++ {
		if want := fmt.Sprintf("task #1 needs you: failure %d", i+1); !seen[want] {
			t.Errorf("no post carried %q", want)
		}
	}
}

// TestWebhookFailureLoggedWithoutURL pins the URL's one place: a
// logged webhook failure names the event and the failure, never the
// URL — which may carry a token — in whole or in host.
func TestWebhookFailureLoggedWithoutURL(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	appendEvent(t, ctx, st, KindTaskCheckFailed, 1, 1, `{"status":"needs_foreman","reason":"exit 1"}`)

	r := newRelay(t)
	atomic.StoreInt32(&r.fail, 2)
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if err := NewLoop(st, None{}, log).WithWebhook(r.hook()).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	waitFor(t, "the failure to reach the log", func() bool {
		return strings.Contains(buf.String(), "webhook post failed")
	})
	written := buf.String()
	if !strings.Contains(written, "webhook post failed") {
		t.Fatalf("log %q, want the webhook's failure named", written)
	}
	if strings.Contains(written, r.srv.URL) || strings.Contains(written, strings.TrimPrefix(r.srv.URL, "http://")) {
		t.Fatalf("log %q, want no trace of the webhook URL", written)
	}
}

// TestWebhookErrorStripsURL feeds the net stack's own transport error —
// a *url.Error quoting the full URL — through Post: what comes back
// names the cause alone.
func TestWebhookErrorStripsURL(t *testing.T) {
	r := newRelay(t)
	url := r.srv.URL + "/hook"
	r.srv.Close() // the address now refuses connections

	err := r.hook().Post(context.Background(), Event{Kind: KindPlanDone, At: "2026-10-13T00:00:00Z"}, "plan #1 done")
	if err == nil {
		t.Fatal("post to a dead relay succeeded; want a transport error")
	}
	if strings.Contains(err.Error(), url) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error %q, want the URL stripped", err)
	}
	if strings.Contains(err.Error(), "webhook: webhook") {
		t.Errorf("error %q, want the name prefixed once, not per layer", err)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// syncBuffer is a bytes.Buffer safe for the handler's concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
