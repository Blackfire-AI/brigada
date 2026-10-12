package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestIdempotency(t *testing.T) {
	e := newEnv(t)
	bodyA := fmt.Sprintf(`{"key":"task:%d","project_id":%d,"task_id":%d,"kind":"work","mode":"fresh","prompt":"first"}`,
		e.task, e.project1, e.task)
	bodyB := fmt.Sprintf(`{"key":"task:%d","project_id":%d,"task_id":%d,"kind":"work","mode":"fresh","prompt":"second"}`,
		e.task, e.project1, e.task)

	// The first call acts and its response is stored.
	code1, _, first := e.do(http.MethodPost, "/v1/jobs", "root", "key-1", []byte(bodyA))
	if code1 != http.StatusOK {
		t.Fatalf("first submit: %d %s", code1, first)
	}
	// A repeat with the same key and body replays it, body and status,
	// and nothing new happens in the store.
	code2, _, replay := e.do(http.MethodPost, "/v1/jobs", "root", "key-1", []byte(bodyA))
	if code2 != code1 || string(replay) != string(first) {
		t.Fatalf("replay = %d %s, want %d %s", code2, replay, code1, first)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM jobs"); n != 1 {
		t.Errorf("jobs after the repeat = %d, want 1: the repeat must not act", n)
	}
	// The same key with a different body is a conflict.
	code, _, data := e.do(http.MethodPost, "/v1/jobs", "root", "key-1", []byte(bodyB))
	if code != http.StatusConflict {
		t.Fatalf("same key, different body = %d %s, want 409", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeIdemConflict {
		t.Errorf("conflict code = %q, want %q", err.Code, codeIdemConflict)
	}
	// Without a key, the same body coalesces behind the live job
	// (§12.1): a different answer than the replay above.
	code, _, data = e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(bodyA))
	if code != http.StatusOK || decode[struct {
		Answer string `json:"answer"`
	}](t, data).Answer != "pending" {
		t.Errorf("unkeyed repeat = %d %s, want the engine's pending answer", code, data)
	}

	// Failures are not stored: a rejected body under a key leaves the
	// key fresh, so the corrected retry acts instead of replaying the
	// rejection (or conflicting with it).
	badBody := `{"key":"","kind":"work"}`
	code, _, _ = e.do(http.MethodPost, "/v1/jobs", "root", "fixup", []byte(badBody))
	if code != http.StatusBadRequest {
		t.Fatalf("rejected submit under a key = %d, want 400", code)
	}
	code, _, data = e.do(http.MethodPost, "/v1/jobs", "root", "fixup", []byte(bodyB))
	if code != http.StatusOK {
		t.Fatalf("corrected retry under the same key = %d %s, want it to act", code, data)
	}
	if res := decode[struct {
		Answer string `json:"answer"`
	}](t, data); res.Answer != "pending" && res.Answer != "created" {
		t.Errorf("corrected retry answered %q, want it to have acted", res.Answer)
	}

	// Cancel under a key: the replay answers as the first call did,
	// while the engine alone would say already.
	var jobID int64
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT id FROM jobs ORDER BY id LIMIT 1").Scan(&jobID)
	}); err != nil {
		t.Fatalf("read the job: %v", err)
	}
	path := fmt.Sprintf("/v1/jobs/%d/cancel", jobID)
	code1, _, first = e.do(http.MethodPost, path, "alice", "cancel-1", nil)
	if code1 != http.StatusOK || decode[answerBody](t, first).Answer != "ok" {
		t.Fatalf("cancel = %d %s, want ok", code1, first)
	}
	code2, _, replay = e.do(http.MethodPost, path, "alice", "cancel-1", nil)
	if code2 != code1 || string(replay) != string(first) {
		t.Fatalf("cancel replay = %d %s, want %d %s", code2, replay, code1, first)
	}
	code, _, data = e.do(http.MethodPost, path, "alice", "", nil)
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "already" {
		t.Errorf("unkeyed repeat cancel = %d %s, want already", code, data)
	}
}

// TestIdempotencyExpiry: keys past the 24 h TTL are swept on access, so
// the next call with them acts again (§12.1).
func TestIdempotencyExpiry(t *testing.T) {
	e := newEnv(t)
	body := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"wake"}`,
		e.project1, e.project1)
	sha := sha256.Sum256([]byte(body))

	old := time.Now().UTC().Add(-25 * time.Hour).Format(timeLayout)
	seed := func(key string) {
		t.Helper()
		if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO idempotency (caller, key, request_sha, status_code, response, created_at)
				VALUES ('root', ?, ?, 200, '{"answer":"sentinel"}', ?)`,
				key, hex.EncodeToString(sha[:]), old)
			return err
		}); err != nil {
			t.Fatalf("seed key %s: %v", key, err)
		}
	}
	seed("stale")
	seed("swept-too")

	// The stale key's stored response is gone; the call acts, and its
	// fresh response is stored in turn.
	code, _, data := e.do(http.MethodPost, "/v1/jobs", "root", "stale", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("submit under the stale key: %d %s", code, data)
	}
	if string(data) == `{"answer":"sentinel"}` {
		t.Fatalf("the expired response was replayed: %s", data)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM idempotency WHERE key = 'swept-too'"); n != 0 {
		t.Errorf("rows for swept-too after access = %d, want 0: the sweep is on access", n)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM idempotency WHERE key = 'stale'"); n != 1 {
		t.Fatalf("rows for stale = %d, want the fresh one", n)
	}
	// And the fresh row replays.
	code2, _, replay := e.do(http.MethodPost, "/v1/jobs", "root", "stale", []byte(body))
	if code2 != code || string(replay) != string(data) {
		t.Errorf("repeat under the refreshed key = %d %s, want the fresh first response", code2, replay)
	}
}

// TestIdempotencyConcurrent: same-key callers are serialized, so of a
// burst of identical requests exactly one reaches the handler and the
// rest replay its response.
func TestIdempotencyConcurrent(t *testing.T) {
	e := newEnv(t)
	body := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"wake"}`,
		e.project2, e.project2)

	const callers = 8
	type reply struct {
		code int
		data []byte
		err  error
	}
	replies := make(chan reply, callers)
	for i := 0; i < callers; i++ {
		go func() {
			code, _, data, err := e.call(http.MethodPost, "/v1/jobs", e.bearer("root"), "race", []byte(body))
			replies <- reply{code, data, err}
		}()
	}
	var first reply
	for i := 0; i < callers; i++ {
		r := <-replies
		if r.err != nil {
			t.Fatalf("request %d failed: %v", i, r.err)
		}
		if r.code != http.StatusOK {
			t.Fatalf("concurrent same-key request = %d %s, want 200", r.code, r.data)
		}
		if i == 0 {
			first = r
			continue
		}
		if string(r.data) != string(first.data) {
			t.Errorf("response %d = %s, want the first response %s replayed verbatim", i, r.data, first.data)
		}
	}
	if n := e.count(t, "SELECT COUNT(*) FROM jobs"); n != 1 {
		t.Errorf("jobs after the burst = %d, want 1: only one request may act", n)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM idempotency WHERE key = 'race'"); n != 1 {
		t.Errorf("stored responses for the key = %d, want 1", n)
	}
}

// count reads one integer from the store.
func (e *env) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(query).Scan(&n)
	}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}
