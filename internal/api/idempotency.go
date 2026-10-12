package api

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// timeLayout is the store's timestamp convention — fixed-width UTC text,
// so lexicographic order is chronological order — which the API's own
// rows (tokens, idempotency) are written in too.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func nowText() string {
	return time.Now().UTC().Format(timeLayout)
}

// idempotencyTTL is how long a stored response is replayed (§12.1);
// after it the key is swept and the next call acts again.
const idempotencyTTL = 24 * time.Hour

// storedResponse is one first response, as the store keeps it.
type storedResponse struct {
	requestSHA string
	statusCode int
	body       string
}

// idempotent wraps a mutating handler with the Idempotency-Key
// contract (§12.1): the callers are often LLMs whose tool calls may
// repeat. A repeat with the same key and the same body replays the
// first response with its status code; the same key with a different
// body is a 409, because what the first call did is no longer what the
// caller is asking for. The key is scoped to the authenticated caller,
// so the same key from two tokens cannot collide.
//
// Calls under the same (caller, key) are serialized in-process, so a
// concurrent repeat finds the first response stored rather than racing
// the handler. Only successes (2xx) are stored: an error says the call
// did not take effect, and the corrected retry must be allowed to act,
// not replayed for the TTL.
func (s *Server) idempotent(h handler) handler {
	return func(w http.ResponseWriter, r *http.Request, id identity) {
		key := r.Header.Get("Idempotency-Key")
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		sha := sha256.Sum256(body)
		requestSHA := hex.EncodeToString(sha[:])
		if key != "" {
			release := s.idemLocks.acquire(id.name + "\x00" + key)
			defer release()
			stored, found, err := s.lookupIdempotency(r, id.name, key)
			if err != nil {
				s.writeInternal(w, r, "idempotency lookup", err)
				return
			}
			if found {
				// The sweep in lookupIdempotency already dropped rows past
				// the TTL, so a found row is live.
				if stored.requestSHA != requestSHA {
					writeError(w, http.StatusConflict, codeIdemConflict,
						"idempotency key already used with a different body")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(stored.statusCode)
				w.Write([]byte(stored.body))
				return
			}
		}
		// The handler replays from the bytes the wrapper read, so the cap
		// and the stored digest see exactly what the handler saw.
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h(rec, r, id)
		if key != "" && rec.Code/100 == 2 {
			if err := s.storeIdempotency(r, id.name, key, requestSHA, rec.Code, rec.Body.String()); err != nil {
				s.writeInternal(w, r, "idempotency store", err)
				return
			}
		}
		for k, vv := range rec.Header() {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}
}

// keyLocks hands out one mutex per name, and forgets the mutex once
// the last holder is done: idempotency keys are minted per logical
// tool call (§12.1), so a map that only grew would leak one entry per
// call for the life of the engine.
type keyLocks struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

type keyLock struct {
	mu      sync.Mutex
	holders int
}

// acquire locks name and returns its release.
func (k *keyLocks) acquire(name string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*keyLock)
	}
	l := k.locks[name]
	if l == nil {
		l = &keyLock{}
		k.locks[name] = l
	}
	l.holders++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.holders--
		if l.holders == 0 {
			delete(k.locks, name)
		}
		k.mu.Unlock()
	}
}

// lookupIdempotency sweeps keys past the TTL and reads one key back.
// Sweeping on access keeps the table honest without a timer in the
// engine (§12.1); found=false means the caller is first.
func (s *Server) lookupIdempotency(r *http.Request, caller, key string) (storedResponse, bool, error) {
	var stored storedResponse
	found := false
	err := s.st.Update(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM idempotency WHERE created_at <= ?",
			time.Now().UTC().Add(-idempotencyTTL).Format(timeLayout)); err != nil {
			return fmt.Errorf("sweep expired keys: %w", err)
		}
		var body string
		err := tx.QueryRow("SELECT request_sha, status_code, response FROM idempotency WHERE caller = ? AND key = ?",
			caller, key).Scan(&stored.requestSHA, &stored.statusCode, &body)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read key: %w", err)
		}
		stored.body = body
		found = true
		return nil
	})
	if err != nil {
		return storedResponse{}, false, err
	}
	return stored, found, nil
}

// storeIdempotency keeps the first success for a key. The in-process
// lock already serializes same-key callers; ON CONFLICT DO NOTHING is
// the belt under it.
func (s *Server) storeIdempotency(r *http.Request, caller, key, requestSHA string, code int, body string) error {
	return s.st.Update(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO idempotency (caller, key, request_sha, status_code, response, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(caller, key) DO NOTHING`,
			caller, key, requestSHA, code, body, nowText()); err != nil {
			return fmt.Errorf("store response: %w", err)
		}
		return nil
	})
}
