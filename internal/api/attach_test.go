package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// repo1 and repo2 are the fixture projects' URLs.
const (
	repo1 = "https://example.invalid/one.git"
	repo2 = "https://example.invalid/two.git"
)

// doToken performs one request bearing an engine-minted bearer secret —
// a hold token the token file does not know — failing the test on
// transport trouble.
func (e *env) doToken(method, path, secret string, body []byte) (int, []byte) {
	e.t.Helper()
	code, _, data, err := e.call(method, path, "Bearer "+secret, "", body)
	if err != nil {
		e.t.Fatalf("%s %s with a hold token: %v", method, path, err)
	}
	return code, data
}

// attachBody renders one attach/heartbeat/release/events body.
func attachBody(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// attachAs attaches to repo as a token name, reporting the raw status
// and body so the caller can branch on a busy 409.
func (e *env) attachAs(token, repo string, force bool) (int, []byte) {
	e.t.Helper()
	code, _, data := e.do(http.MethodPost, "/v1/foreman/attach", token, "",
		attachBody(map[string]any{"repo_url": repo, "force": force}))
	return code, data
}

// parkNeedsHelp drives the fixture's task to a needs_help park through
// the runner protocol, the judgment event the wake and the events read
// answer to.
func (e *env) parkNeedsHelp(question string) int64 {
	e.t.Helper()
	job := e.submitWork()
	e.register("r1")
	_, data := e.lease("r1", 0)
	g := decode[struct {
		Job *jobGrant `json:"job"`
	}](e.t, data).Job
	if g == nil || g.JobID != job {
		e.t.Fatalf("the work job was not leased: %s", data)
	}
	body := fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"result":"{\"status\":\"needs_help\",\"question\":\"%s\"}"}`,
		g.JobID, g.LeaseID, question)
	code, _, data := e.do(http.MethodPost, "/v1/runner/complete", "r1", "", []byte(body))
	if code != http.StatusOK {
		e.t.Fatalf("complete the work job: %d %s", code, data)
	}
	return job
}

// TestAttachRoutes covers the attach API (§4.2 item 1, §12.1): client
// and admin attach by repo URL and get the hold, the token, the
// foreman's harness and model, the key's session id, the opening prompt
// and the expiry; a second attach is 409; other roles, a missing repo
// URL and an unknown repo are refused; heartbeat extends the hold and
// release writes the session id back, after which the foreman is
// attachable again.
func TestAttachRoutes(t *testing.T) {
	e := newEnv(t)
	code, data := e.attachAs("alice", repo1, false)
	if code != http.StatusOK {
		t.Fatalf("attach as the client = %d %s, want 200", code, data)
	}
	res := decode[attachResponse](t, data)
	if res.Answer != "created" || res.HoldID == 0 {
		t.Fatalf("attach = %+v, want created with a hold", res)
	}
	if len(res.ForemanToken) != 64 {
		t.Errorf("the hold token = %q, want a 64-char hex secret", res.ForemanToken)
	}
	if res.Harness != "claude" || res.Model != "sonnet" {
		t.Errorf("the foreman = %s/%s, want claude/sonnet", res.Harness, res.Model)
	}
	if res.SessionID != "" {
		t.Errorf("the key's session id = %q, want empty before any run", res.SessionID)
	}
	if !strings.Contains(res.Prompt, "You are waking up for one headless turn") {
		t.Errorf("the opening prompt = %q, want the wake template in it", res.Prompt)
	}
	if res.ExpiresAt.IsZero() {
		t.Error("the attach named no expiry")
	}

	// A second live attach is 409, named.
	code, data = e.attachAs("root", repo1, false)
	if code != http.StatusConflict {
		t.Fatalf("the second attach = %d %s, want 409", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeConflict ||
		!strings.Contains(err.Error, "busy") || !strings.Contains(err.Error, fmt.Sprint(res.HoldID)) {
		t.Errorf("the busy refusal = %+v, want a conflict naming the hold", err)
	}

	// The roles that do not attach, and the projects that do not exist.
	if code, data = e.attachAs("r1", repo1, false); code != http.StatusForbidden {
		t.Errorf("attach as a runner = %d %s, want 403", code, data)
	}
	if code, data = e.attachAs("foreman1", repo1, false); code != http.StatusForbidden {
		t.Errorf("attach as the foreman = %d %s, want 403", code, data)
	}
	code, _, data = e.do(http.MethodPost, "/v1/foreman/attach", "alice", "", attachBody(map[string]any{"force": true}))
	if code != http.StatusBadRequest {
		t.Errorf("attach without repo_url = %d %s, want 400", code, data)
	}
	if code, data = e.attachAs("alice", "https://example.invalid/none.git", false); code != http.StatusNotFound {
		t.Errorf("attach for an unknown repo = %d %s, want 404", code, data)
	}

	// The heartbeat extends the hold, the release ends it.
	e.clock.Advance(60 * time.Second)
	code, _, data = e.do(http.MethodPost, "/v1/foreman/heartbeat", "alice", "",
		attachBody(map[string]any{"hold_id": res.HoldID}))
	if code != http.StatusOK {
		t.Fatalf("heartbeat the hold = %d %s, want 200", code, data)
	}
	beat := decode[struct {
		Answer    string     `json:"answer"`
		ExpiresAt *time.Time `json:"expires_at"`
	}](t, data)
	if beat.Answer != "ok" || beat.ExpiresAt == nil || !beat.ExpiresAt.After(res.ExpiresAt) {
		t.Errorf("heartbeat = %+v, want ok with the expiry moved out", beat)
	}

	code, _, data = e.do(http.MethodPost, "/v1/foreman/release", "alice", "",
		attachBody(map[string]any{"hold_id": res.HoldID, "session_id": "hs-9"}))
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "ok" {
		t.Fatalf("release the hold = %d %s, want ok", code, data)
	}
	var sessionID string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT harness_session_id FROM sessions WHERE key = 'foreman:1'").Scan(&sessionID)
	}); err != nil {
		t.Fatalf("read the key's session: %v", err)
	}
	if sessionID != "hs-9" {
		t.Errorf("the key's session id = %q, want hs-9 written back", sessionID)
	}
	code, _, data = e.do(http.MethodPost, "/v1/foreman/heartbeat", "alice", "",
		attachBody(map[string]any{"hold_id": res.HoldID}))
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "gone" {
		t.Errorf("heartbeat after the release = %d %s, want gone", code, data)
	}
	if code, data = e.attachAs("root", repo1, false); code != http.StatusOK {
		t.Errorf("attach after the release = %d %s, want 200: the foreman is free again", code, data)
	}

	// The events route is the foreman role's alone.
	code, _, data = e.do(http.MethodPost, "/v1/events", "alice", "", attachBody(map[string]any{"wait_seconds": 0}))
	if code != http.StatusForbidden {
		t.Errorf("events as the client = %d %s, want 403", code, data)
	}
}

// TestAttachBusyTurnAndForce is the busy rule through the API (§4.2 item
// 1): a live foreman turn makes attach 409 naming the turn, and force
// cancels it through the cancel path so the attach takes the foreman.
func TestAttachBusyTurnAndForce(t *testing.T) {
	e := newEnv(t)
	turn := e.submitTurn(e.project1)
	code, data := e.attachAs("alice", repo1, false)
	if code != http.StatusConflict {
		t.Fatalf("attach with a live turn = %d %s, want 409", code, data)
	}
	if err := decode[errBody](t, data); !strings.Contains(err.Error, fmt.Sprint(turn)) {
		t.Errorf("the busy refusal = %q, want it naming turn %d", err.Error, turn)
	}
	code, data = e.attachAs("alice", repo1, true)
	if code != http.StatusOK {
		t.Fatalf("the forced attach = %d %s, want 200", code, data)
	}
	job, err := e.eng.Job(e.ctx, turn)
	if err != nil {
		t.Fatalf("read the cancelled turn: %v", err)
	}
	if job.Status != "cancelled" {
		t.Errorf("the live turn = %s, want cancelled by the forced attach", job.Status)
	}
}

// TestHoldTokenWorksForTheToolsAndDiesAtRelease covers the hold token's
// life through the API (§12.1, §4.2 item 1): minted at attach, it
// authenticates as a foreman scoped to its project — the MCP tools' reads
// and plan_create work in it, another project's rows are 403 — and the
// release revokes it: from there it credentials nothing.
func TestHoldTokenWorksForTheToolsAndDiesAtRelease(t *testing.T) {
	e := newEnv(t)
	work := e.submitWork()
	other := e.submitTurn(e.project2)
	code, data := e.attachAs("root", repo1, false)
	if code != http.StatusOK {
		t.Fatalf("attach = %d %s, want 200", code, data)
	}
	res := decode[attachResponse](t, data)
	secret := res.ForemanToken

	if code, data := e.withToken(secret, http.MethodGet, "/v1/status"); code != http.StatusOK {
		t.Errorf("status with the hold token = %d %s, want 200", code, data)
	}
	if code, data := e.withToken(secret, http.MethodGet, fmt.Sprintf("/v1/jobs/%d", work)); code != http.StatusOK {
		t.Errorf("the own project's job with the hold token = %d %s, want 200", code, data)
	}
	if code, data := e.withToken(secret, http.MethodGet, fmt.Sprintf("/v1/jobs/%d", other)); code != http.StatusForbidden {
		t.Errorf("another project's job with the hold token = %d %s, want 403", code, data)
	}
	code, data = e.doToken(http.MethodPost, "/v1/plans", secret,
		attachBody(map[string]any{"title": "a plan", "tasks": []map[string]any{{"spec": "do it"}}}))
	if code != http.StatusOK {
		t.Errorf("plan_create with the hold token = %d %s, want 200", code, data)
	}
	code, data = e.doToken(http.MethodPost, "/v1/events", secret, attachBody(map[string]any{"wait_seconds": 0}))
	if code != http.StatusOK {
		t.Fatalf("events with the hold token = %d %s, want 200", code, data)
	}
	if events := decode[eventsResponse](t, data).Events; len(events) != 0 {
		t.Errorf("events with nothing pending = %v, want empty", events)
	}

	code, _, data = e.do(http.MethodPost, "/v1/foreman/release", "root", "",
		attachBody(map[string]any{"hold_id": res.HoldID, "session_id": "hs-7"}))
	if code != http.StatusOK {
		t.Fatalf("release the hold = %d %s, want 200", code, data)
	}
	if code, data := e.withToken(secret, http.MethodGet, "/v1/status"); code != http.StatusUnauthorized {
		t.Errorf("status after the release = %d %s, want 401: the token died with the hold", code, data)
	}
}

// TestEventsLongPollReturnsWhenAnEventArrives is the events tool's wait
// (§4.2): the call holds until a judgment event lands — a park while the
// hold lives wakes no turn — and returns that event at once, consuming
// it, rendered as a wake prompt carries it.
func TestEventsLongPollReturnsWhenAnEventArrives(t *testing.T) {
	e := newEnv(t)
	code, data := e.attachAs("root", repo1, false)
	if code != http.StatusOK {
		t.Fatalf("attach = %d %s, want 200", code, data)
	}
	secret := decode[attachResponse](t, data).ForemanToken

	type result struct {
		code    int
		data    []byte
		err     error
		elapsed time.Duration
	}
	ch := make(chan result, 1)
	go func() {
		start := time.Now()
		code, _, data, err := e.call(http.MethodPost, "/v1/events", "Bearer "+secret, "",
			attachBody(map[string]any{"wait_seconds": 10}))
		ch <- result{code, data, err, time.Since(start)}
	}()
	time.Sleep(400 * time.Millisecond) // let the long-poll take hold
	e.parkNeedsHelp("which file?")

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("the events call: %v", r.err)
		}
		if r.code != http.StatusOK {
			t.Fatalf("the events call = %d %s, want 200", r.code, r.data)
		}
		// A generous margin: the poll re-asks every 200 ms, so anything
		// under the wait's whole span still proves it saw the event as
		// arrived rather than at the deadline — without failing on a
		// loaded machine running -race.
		if r.elapsed > 8*time.Second {
			t.Errorf("the long-poll took %s to see the event, want a prompt return", r.elapsed)
		}
		events := decode[eventsResponse](t, r.data).Events
		if len(events) != 1 || !strings.Contains(events[0], "task_needs_help task "+fmt.Sprint(e.task)) ||
			!strings.Contains(events[0], "which file?") {
			t.Errorf("the events = %v, want the park's line verbatim", events)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the long-poll did not return when the event arrived")
	}

	// The read consumed the event; a fresh call says nothing new, and no
	// turn was woken past the hold.
	code, data = e.doToken(http.MethodPost, "/v1/events", secret, attachBody(map[string]any{"wait_seconds": 0}))
	if code != http.StatusOK {
		t.Fatalf("the events call after the read = %d %s, want 200", code, data)
	}
	if events := decode[eventsResponse](t, data).Events; len(events) != 0 {
		t.Errorf("the events after the read = %v, want empty: the read consumed them", events)
	}
	var turns int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = 'foreman'", e.project1).Scan(&turns)
	}); err != nil {
		t.Fatalf("count the foreman turns: %v", err)
	}
	if turns != 0 {
		t.Errorf("foreman turns while the hold lives = %d, want 0: no wake past an attach", turns)
	}
}

// TestReleaseAfterExpiryKeepsTheSessionID is the late release through
// the API (§4.2 item 1): a detach that arrives after the hold lapsed —
// the laptop slept through the expiry — answers expired, and the session
// id the chat ended on is still written back, for the next wake resumes
// what the chat reached.
func TestReleaseAfterExpiryKeepsTheSessionID(t *testing.T) {
	e := newEnv(t)
	code, data := e.attachAs("alice", repo1, false)
	if code != http.StatusOK {
		t.Fatalf("attach = %d %s, want 200", code, data)
	}
	res := decode[attachResponse](t, data)
	e.clock.Advance(2 * time.Minute)
	code, _, data = e.do(http.MethodPost, "/v1/foreman/release", "alice", "",
		attachBody(map[string]any{"hold_id": res.HoldID, "session_id": "hs-4"}))
	if code != http.StatusOK {
		t.Fatalf("the late release = %d %s, want 200", code, data)
	}
	if ans := decode[answerBody](t, data).Answer; ans != "expired" {
		t.Fatalf("the late release = %s, want expired", ans)
	}
	var sessionID string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT harness_session_id FROM sessions WHERE key = 'foreman:1'").Scan(&sessionID)
	}); err != nil {
		t.Fatalf("read the key's session: %v", err)
	}
	if sessionID != "hs-4" {
		t.Errorf("the key's session id after the late release = %q, want hs-4 written back", sessionID)
	}
}

// TestEventsCallThatGoesAwayConsumesNothing is the departed-caller rule
// (§4.2): a long-poll whose caller cancels mid-wait consumes nothing —
// the check under each wait refuses to read on a dead request context,
// so an event that lands after the caller went away stays pending for
// the next read.
func TestEventsCallThatGoesAwayConsumesNothing(t *testing.T) {
	e := newEnv(t)
	code, data := e.attachAs("root", repo1, false)
	if code != http.StatusOK {
		t.Fatalf("attach = %d %s, want 200", code, data)
	}
	secret := decode[attachResponse](t, data).ForemanToken

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://brigada/v1/events",
		bytes.NewReader(attachBody(map[string]any{"wait_seconds": 10})))
	if err != nil {
		t.Fatalf("build the events request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	done := make(chan error, 1)
	go func() {
		resp, err := e.client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the long-poll take hold
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the cancelled call was answered, want the connection broken")
	}

	// The event lands after the caller went away; the hold is live, so no
	// turn wakes, and the departed call consumed nothing.
	e.parkNeedsHelp("which file?")
	var state string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT state FROM foreman_inbox WHERE project_id = ?", e.project1).Scan(&state)
	}); err != nil {
		t.Fatalf("read the inbox: %v", err)
	}
	if state != "pending" {
		t.Fatalf("the inbox after the caller went away = %s, want pending: unread events are not consumed", state)
	}
	// The next read still gets it.
	code, data = e.doToken(http.MethodPost, "/v1/events", secret, attachBody(map[string]any{"wait_seconds": 0}))
	if code != http.StatusOK {
		t.Fatalf("the events call after the departure = %d %s, want 200", code, data)
	}
	if events := decode[eventsResponse](t, data).Events; len(events) != 1 {
		t.Errorf("the events after the departure = %v, want the parked event still there", events)
	}
}

// TestEventsLongPollTimesOutEmpty is the bounded wait (§4.2): with
// nothing pending the call waits out its client-given wait and answers
// an empty list — "no new events" — inside the cap.
func TestEventsLongPollTimesOutEmpty(t *testing.T) {
	e := newEnv(t)
	code, data := e.attachAs("root", repo1, false)
	if code != http.StatusOK {
		t.Fatalf("attach = %d %s, want 200", code, data)
	}
	secret := decode[attachResponse](t, data).ForemanToken
	start := time.Now()
	code, data = e.doToken(http.MethodPost, "/v1/events", secret, attachBody(map[string]any{"wait_seconds": 0.3}))
	if code != http.StatusOK {
		t.Fatalf("the events call = %d %s, want 200", code, data)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the empty wait took %s, want it bounded by the client-given wait", elapsed)
	}
	events := decode[eventsResponse](t, data).Events
	if events == nil || len(events) != 0 {
		t.Errorf("the events = %v, want an empty (non-null) list", events)
	}
}
