package api

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// holdCall performs one request with a mode A credential and, when
// fence is non-zero, its fence header, plus an optional idempotency key.
func (e *env) holdCall(method, path, credential string, fence int64, idemKey string, body []byte) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://brigada"+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	if fence != 0 {
		req.Header.Set(holdFenceHeader, strconv.FormatInt(fence, 10))
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, data
}

// acquireHold takes project 1's slot as a client token.
func (e *env) acquireHold(token, bridge string) AcquireResponse {
	e.t.Helper()
	code, _, data := e.do(http.MethodPost, "/v1/foreman/hold/acquire", token, "",
		attachBody(map[string]any{"repo_url": repo1, "bridge_instance": bridge, "label": bridge + " label"}))
	if code != http.StatusOK {
		e.t.Fatalf("acquire = %d %s, want 200", code, data)
	}
	return decode[AcquireResponse](e.t, data)
}

func planBody(title string) []byte {
	return attachBody(map[string]any{"title": title, "tasks": []map[string]any{{"spec": "do it"}}})
}

func (e *env) countRows(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error { return tx.QueryRow(query, args...).Scan(&n) }); err != nil {
		e.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestHoldAcquireRoutes: only client and admin tokens acquire; the
// credential and fence come back once with the digest; a foreman cannot
// mint itself a hold.
func TestHoldAcquireRoutes(t *testing.T) {
	e := newEnv(t)
	for _, tok := range []string{"foreman1", "r1"} {
		code, _, _ := e.do(http.MethodPost, "/v1/foreman/hold/acquire", tok, "",
			attachBody(map[string]any{"repo_url": repo1, "bridge_instance": "b"}))
		if code != http.StatusForbidden {
			t.Errorf("acquire as %s = %d, want 403", tok, code)
		}
	}
	for body, want := range map[string]int{
		`{"bridge_instance":"b"}`:                                            http.StatusBadRequest,
		`{"repo_url":"` + repo1 + `"}`:                                       http.StatusBadRequest,
		`{"repo_url":"https://example.invalid/x.git","bridge_instance":"b"}`: http.StatusNotFound,
	} {
		if code, _, data := e.do(http.MethodPost, "/v1/foreman/hold/acquire", "alice", "", []byte(body)); code != want {
			t.Errorf("acquire %s = %d %s, want %d", body, code, data, want)
		}
	}
	h := e.acquireHold("alice", "bridge-a")
	if h.Credential == "" || h.Fence == 0 || h.HoldID == 0 || h.Digest == nil || len(h.Digest.Plans) != 1 {
		t.Fatalf("acquire = %+v, want credential, fence, hold and digest", h)
	}
	if !h.IdleExpires.Before(h.MaxAt) || !h.LeaseExpires.Before(h.IdleExpires) {
		t.Errorf("bounds lease %s, idle %s, max %s: want them in that order", h.LeaseExpires, h.IdleExpires, h.MaxAt)
	}
	if n := e.countRows("SELECT COUNT(*) FROM interactive_holds WHERE credential_hash = ?", h.Credential); n != 0 {
		t.Error("the credential itself is stored")
	}
}

// TestHoldRefusalsOverTheWire: a turn, an attach and a second bridge each
// refuse an acquire, naming the holder, its label and since.
func TestHoldRefusalsOverTheWire(t *testing.T) {
	type refusal struct {
		Error  string `json:"error"`
		Code   string `json:"code"`
		Holder struct {
			State    string   `json:"state"`
			HolderID int64    `json:"holder_id"`
			Label    string   `json:"label"`
			Since    string   `json:"since"`
			Events   []string `json:"events"`
		} `json:"holder"`
	}
	acquire := func(e *env, bridge string) refusal {
		code, _, data := e.do(http.MethodPost, "/v1/foreman/hold/acquire", "alice", "",
			attachBody(map[string]any{"repo_url": repo1, "bridge_instance": bridge, "label": "x"}))
		if code != http.StatusConflict {
			t.Fatalf("acquire = %d %s, want 409", code, data)
		}
		return decode[refusal](t, data)
	}

	t.Run("a headless turn is foreman_busy", func(t *testing.T) {
		e := newEnv(t)
		turn := e.submitTurn(e.project1)
		r := acquire(e, "b")
		if r.Code != "foreman_busy" || r.Holder.State != "headless turn" || r.Holder.HolderID != turn || r.Holder.Since == "" {
			t.Errorf("refusal = %+v, want foreman_busy naming turn %d and its start", r, turn)
		}
		if !strings.Contains(r.Error, "headless turn") {
			t.Errorf("message = %q, want the holder named", r.Error)
		}
	})
	t.Run("an attach is hold_busy", func(t *testing.T) {
		e := newEnv(t)
		code, data := e.attachAs("alice", repo1, false)
		if code != http.StatusOK {
			t.Fatalf("attach = %d %s", code, data)
		}
		r := acquire(e, "b")
		if r.Code != "hold_busy" || r.Holder.State != "attach" || r.Holder.Label == "" || r.Holder.Since == "" {
			t.Errorf("refusal = %+v, want hold_busy naming the attach", r)
		}
	})
	t.Run("a second bridge is hold_busy with the first's label", func(t *testing.T) {
		e := newEnv(t)
		first := e.acquireHold("alice", "bridge-a")
		r := acquire(e, "bridge-b")
		if r.Code != "hold_busy" || r.Holder.State != "interactive hold" || r.Holder.HolderID != first.HoldID ||
			r.Holder.Label != "bridge-a label" || r.Holder.Since == "" {
			t.Errorf("refusal = %+v, want hold_busy naming hold %d and its label", r, first.HoldID)
		}
	})
	t.Run("an attach is refused while the session holds", func(t *testing.T) {
		e := newEnv(t)
		first := e.acquireHold("alice", "bridge-a")
		code, data := e.attachAs("alice", repo1, true)
		if code != http.StatusConflict || !strings.Contains(string(data), fmt.Sprintf(`"interactive_hold_id":%d`, first.HoldID)) {
			t.Errorf("attach under a hold = %d %s, want 409 naming the hold", code, data)
		}
	})
}

// TestStaleHoldRejectsWholeAndRecordsNothing is §4.6's stale rule and
// §12.1's idempotency rule together: an old fence and a revoked
// credential are 409 stale_hold, apply nothing, and leave no idempotency
// record — a retry under a new hold acts, it is not replayed or refused.
func TestStaleHoldRejectsWholeAndRecordsNothing(t *testing.T) {
	e := newEnv(t)
	h := e.acquireHold("alice", "bridge-a")

	// The live hold with its fence works, and is recorded.
	code, data := e.holdCall(http.MethodPost, "/v1/plans", h.Credential, h.Fence, "k-ok", planBody("first"))
	if code != http.StatusOK {
		t.Fatalf("plan_create with the fence = %d %s, want 200", code, data)
	}
	plans := e.countRows("SELECT COUNT(*) FROM plans")

	assertStale := func(what string, code int, data []byte) {
		t.Helper()
		if code != http.StatusConflict || decode[errBody](t, data).Code != "stale_hold" {
			t.Errorf("%s = %d %s, want 409 stale_hold", what, code, data)
		}
		if n := e.countRows("SELECT COUNT(*) FROM plans"); n != plans {
			t.Errorf("%s: plans went %d -> %d; a stale call applies nothing", what, plans, n)
		}
	}

	// Old fence on a live credential, and no fence at all.
	code, data = e.holdCall(http.MethodPost, "/v1/plans", h.Credential, h.Fence-1, "k-old", planBody("old fence"))
	assertStale("old fence", code, data)
	code, data = e.holdCall(http.MethodPost, "/v1/plans", h.Credential, 0, "k-nofence", planBody("no fence"))
	assertStale("missing fence", code, data)
	// Every mutation is behind the fence, not just plan_create.
	for _, path := range []string{fmt.Sprintf("/v1/tasks/%d/answer", e.task), fmt.Sprintf("/v1/tasks/%d/restart", e.task),
		fmt.Sprintf("/v1/plans/%d/amend", e.plan), "/v1/events"} {
		code, data = e.holdCall(http.MethodPost, path, h.Credential, h.Fence-1, "", []byte(`{}`))
		assertStale("old fence on "+path, code, data)
	}

	// Revoked credential: yield first, then every route says stale_hold.
	code, data = e.holdCall(http.MethodPost, "/v1/foreman/hold/yield", h.Credential, h.Fence, "", nil)
	if code != http.StatusOK {
		t.Fatalf("yield = %d %s, want 200", code, data)
	}
	code, data = e.holdCall(http.MethodPost, "/v1/plans", h.Credential, h.Fence, "k-revoked", planBody("revoked"))
	assertStale("revoked credential", code, data)
	// ...even a retry of the call that had succeeded under that hold: it is
	// not replayed, because the session must decide again.
	code, data = e.holdCall(http.MethodPost, "/v1/plans", h.Credential, h.Fence, "k-ok", planBody("first"))
	assertStale("replay of a recorded call after revocation", code, data)
	code, data = e.holdCall(http.MethodGet, "/v1/status", h.Credential, 0, "", nil)
	if code != http.StatusConflict {
		t.Errorf("a read with a revoked credential = %d %s, want 409 stale_hold", code, data)
	}
	code, data = e.holdCall(http.MethodPost, "/v1/foreman/hold/heartbeat", h.Credential, h.Fence, "", nil)
	assertStale("heartbeat after the hold ended", code, data)

	// No idempotency record for any rejection; the one success has its own.
	for _, key := range []string{"k-old", "k-nofence", "k-revoked"} {
		if n := e.countRows("SELECT COUNT(*) FROM idempotency WHERE key = ?", key); n != 0 {
			t.Errorf("idempotency key %s was recorded for a stale call", key)
		}
	}
	if n := e.countRows("SELECT COUNT(*) FROM idempotency WHERE key = 'k-ok'"); n != 1 {
		t.Errorf("the successful call has %d idempotency records, want 1", n)
	}

	// The new hold acts with the same key a stale call used.
	h2 := e.acquireHold("alice", "bridge-a")
	code, data = e.holdCall(http.MethodPost, "/v1/plans", h2.Credential, h2.Fence, "k-old", planBody("old fence"))
	if code != http.StatusOK {
		t.Errorf("the same key under a new hold = %d %s, want 200", code, data)
	}
}

// TestHoldActivityOverTheWire: a mutation and an `events` call that
// consumed something are activity; an empty poll, status, history and
// capacity are not.
func TestHoldActivityOverTheWire(t *testing.T) {
	e := newEnv(t)
	h := e.acquireHold("alice", "bridge-a")
	idleAt := func() string {
		var s string
		if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT activity_at FROM interactive_holds WHERE id = ?", h.HoldID).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	start := idleAt()
	// A live bridge beats inside its lease; the beats are not activity.
	beat := func() {
		t.Helper()
		e.clock.Advance(30 * time.Second)
		if code, data := e.holdCall(http.MethodPost, "/v1/foreman/hold/heartbeat", h.Credential, h.Fence, "", nil); code != http.StatusOK {
			t.Fatalf("heartbeat = %d %s", code, data)
		}
	}
	beat()

	for _, path := range []string{"/v1/status", "/v1/history", "/v1/capacity"} {
		if code, data := e.holdCall(http.MethodGet, path, h.Credential, h.Fence, "", nil); code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, code, data)
		}
	}
	if code, data := e.holdCall(http.MethodPost, "/v1/events", h.Credential, h.Fence, "", attachBody(map[string]any{"wait_seconds": 0})); code != http.StatusOK {
		t.Fatalf("empty events = %d %s", code, data)
	}
	beat()
	if got := idleAt(); got != start {
		t.Errorf("activity moved on reads, an empty poll and a heartbeat: %s -> %s", start, got)
	}

	if code, data := e.holdCall(http.MethodPost, "/v1/plans", h.Credential, h.Fence, "", planBody("a plan")); code != http.StatusOK {
		t.Fatalf("plan_create = %d %s", code, data)
	}
	afterMutation := idleAt()
	if afterMutation == start {
		t.Error("a mutation did not count as activity")
	}

	beat()
	if code, _, data := e.do(http.MethodPost, "/v1/foreman/messages", "alice", "",
		attachBody(map[string]any{"repo_url": repo1, "text": "hello"})); code != http.StatusOK {
		t.Fatalf("ask = %d %s", code, data)
	}
	code, data := e.holdCall(http.MethodPost, "/v1/events", h.Credential, h.Fence, "", attachBody(map[string]any{"wait_seconds": 0}))
	if code != http.StatusOK || !strings.Contains(string(data), "hello") {
		t.Fatalf("events with a message = %d %s", code, data)
	}
	if idleAt() == afterMutation {
		t.Error("an events call that consumed a message did not count as activity")
	}
}

// TestAdminForemanSubmitRefusedWhileHeld: the admin job path cannot start
// a headless turn under an interactive hold (§4.6).
func TestAdminForemanSubmitRefusedWhileHeld(t *testing.T) {
	e := newEnv(t)
	e.acquireHold("alice", "bridge-a")
	body := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"wake up"}`,
		e.project1, e.project1)
	code, _, data := e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(body))
	if code != http.StatusConflict || decode[errBody](t, data).Code != "hold_busy" {
		t.Errorf("admin foreman submit under a hold = %d %s, want 409 hold_busy", code, data)
	}
	if n := e.countRows("SELECT COUNT(*) FROM jobs WHERE kind = 'foreman'"); n != 0 {
		t.Errorf("%d foreman jobs under a hold", n)
	}
}

// TestForcedReleaseOverTheWire: client authority, through fencing.
func TestForcedReleaseOverTheWire(t *testing.T) {
	e := newEnv(t)
	h := e.acquireHold("alice", "bridge-a")
	for _, tok := range []string{"foreman1", "r1"} {
		if code, _, _ := e.do(http.MethodPost, "/v1/foreman/hold/release", tok, "", attachBody(map[string]any{"repo_url": repo1})); code != http.StatusForbidden {
			t.Errorf("release as %s = %d, want 403", tok, code)
		}
	}
	// The hold's own credential is not client authority.
	if code, _ := e.holdCall(http.MethodPost, "/v1/foreman/hold/release", h.Credential, h.Fence, "", attachBody(map[string]any{"repo_url": repo1})); code != http.StatusForbidden {
		t.Errorf("release with the hold's credential = %d, want 403", code)
	}
	if code, _, data := e.do(http.MethodPost, "/v1/foreman/hold/release", "alice", "", attachBody(map[string]any{"repo_url": repo1})); code != http.StatusOK ||
		decode[answerBody](t, data).Answer != "ok" {
		t.Fatalf("release = %d %s, want ok", code, data)
	}
	code, data := e.holdCall(http.MethodPost, "/v1/plans", h.Credential, h.Fence, "", planBody("after"))
	if code != http.StatusConflict || decode[errBody](t, data).Code != "stale_hold" {
		t.Errorf("a call after the forced release = %d %s, want 409 stale_hold", code, data)
	}
	if _, _, data := e.do(http.MethodPost, "/v1/foreman/hold/release", "alice", "", attachBody(map[string]any{"repo_url": repo1})); decode[answerBody](t, data).Answer != "gone" {
		t.Errorf("a second release = %s, want gone", data)
	}
}

// TestStatusShowsTheSlot: `brigada status` names the slot's state and
// holder.
func TestStatusShowsTheSlot(t *testing.T) {
	e := newEnv(t)
	slotOf := func() Status {
		code, _, data := e.do(http.MethodGet, "/v1/status", "alice", "", nil)
		if code != http.StatusOK {
			t.Fatalf("status = %d %s", code, data)
		}
		return decode[Status](t, data)
	}
	if text := StatusSections(slotOf()); strings.Contains(text, "foreman slot") {
		t.Errorf("a free slot is listed:\n%s", text)
	}
	h := e.acquireHold("alice", "bridge-a")
	st := slotOf()
	text := StatusSections(st)
	if !strings.Contains(text, "foreman slot (project #1): interactive hold \"bridge-a label\"") ||
		!strings.Contains(text, fmt.Sprintf("#%d since", h.HoldID)) {
		t.Errorf("status under a hold:\n%s", text)
	}
	e.do(http.MethodPost, "/v1/foreman/hold/release", "alice", "", attachBody(map[string]any{"repo_url": repo1}))
	turn := e.submitTurn(e.project1)
	text = StatusSections(slotOf())
	if !strings.Contains(text, "headless turn") || !strings.Contains(text, fmt.Sprintf("#%d since", turn)) {
		t.Errorf("status under a turn:\n%s", text)
	}
}
