package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// seedTS is the fixed moment the seeded rows carry; the schema treats
// timestamps as opaque UTC text.
const seedTS = "2026-10-12T12:00:00.000000000Z"

// tokenFileBody seeds one token per role: two runners, a foreman scoped
// to project 1, a plain client and an admin. The secrets are test
// values; only their hashes may reach the store.
const tokenFileBody = `# test tokens
alice client alice-secret-1
root admin root-secret-1
r1 runner runner-1-secret
r2 runner runner-2-secret
foreman1 foreman:1 foreman-secret-1
`

// fakeClock is the injectable engine clock: tests move it instead of
// sleeping, so lease expiry and caps are deterministic.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// planReviewOff keeps the fixture's plan creations on the no-review path
// (§6.2); the plan review has its own tests.
var planReviewOff = false

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

// env is one API server over its own store, served on a real Unix
// socket in a temp dir, seeded with two projects (so a foreman token
// has another project to be walled off from), a plan, a task and the
// tokens above.
type env struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	eng    *engine.Engine
	api    *Server
	clock  *fakeClock
	srv    *httptest.Server
	client *http.Client

	secrets map[string]string // token name -> secret

	project1, project2, plan, task int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvTokens(t, tokenFileBody)
}

func newEnvTokens(t *testing.T, tokens string) *env {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	st := openFreshStore(t, filepath.Join(tmp, "store.db"))
	t.Cleanup(func() { st.Close() })
	e := &env{
		t: t, ctx: ctx, st: st, clock: newFakeClock(),
		secrets: map[string]string{
			"alice": "alice-secret-1", "root": "root-secret-1",
			"r1": "runner-1-secret", "r2": "runner-2-secret",
			"foreman1": "foreman-secret-1",
		},
	}
	err := st.Update(ctx, func(tx *sql.Tx) error {
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
		seedProject := func(repo string) int64 {
			return id(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
				VALUES (?, 'main', '{}', 'claude', 'sonnet', ?, ?)`, repo, seedTS, seedTS)
		}
		e.project1 = seedProject("https://example.invalid/one.git")
		e.project2 = seedProject("https://example.invalid/two.git")
		e.plan = id(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (?, 'a plan', 'active', ?, ?)`, e.project1, seedTS, seedTS)
		e.task = id(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (?, 'a task', 'the spec', 'go test ./...', 'implement', 'ready', 'main', '{}', ?, ?)`,
			e.plan, seedTS, seedTS)
		return nil
	})
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}

	tokenFile := filepath.Join(tmp, "tokens")
	if err := os.WriteFile(tokenFile, []byte(tokens), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	e.eng = engine.New(st, engine.Config{Now: e.clock.Now, PlanReview: &planReviewOff})
	api, err := New(ctx, st, e.eng, nil, tokenFile)
	if err != nil {
		t.Fatalf("build api server: %v", err)
	}
	e.api = api

	// Not under t.TempDir(): its name carries the test's, and on the
	// platforms with long temporary directories that pushes a socket
	// path past the kernel's sun_path cap.
	socket := testsupport.ShortSocketPath(t)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on the test socket: %v", err)
	}
	srv := httptest.NewUnstartedServer(api.Handler())
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	e.srv = srv
	e.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	t.Cleanup(e.client.CloseIdleConnections)
	return e
}

// call performs one request with the literal Authorization header and
// reports the response without failing the test, so background
// goroutines may use it.
func (e *env) call(method, path, authz, idemKey string, body []byte) (int, http.Header, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://brigada"+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

// bearer is the Authorization header for a known token name.
func (e *env) bearer(name string) string {
	return "Bearer " + e.secrets[name]
}

// do performs one request and fails the test on transport errors.
func (e *env) do(method, path, token, idemKey string, body []byte) (int, http.Header, []byte) {
	e.t.Helper()
	code, h, data, err := e.call(method, path, e.bearer(token), idemKey, body)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return code, h, data
}

// decode unmarshals a response body, failing the test on bad JSON.
func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return v
}

// errBody is the API's uniform error shape, which every failure must
// carry, whatever wrote it.
type errBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// submitWork queues a work job for the seeded task through the admin
// endpoint, returning its id.
func (e *env) submitWork() int64 {
	e.t.Helper()
	body := fmt.Sprintf(`{"key":"task:%d","project_id":%d,"task_id":%d,"kind":"work","mode":"fresh","prompt":"the composed prompt"}`,
		e.task, e.project1, e.task)
	code, _, data := e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(body))
	if code != http.StatusOK {
		e.t.Fatalf("submit work job: %d %s", code, data)
	}
	res := decode[struct {
		Answer string `json:"answer"`
		JobID  int64  `json:"job_id"`
	}](e.t, data)
	if res.Answer != "created" {
		e.t.Fatalf("submit work job answered %q, want created", res.Answer)
	}
	return res.JobID
}

// register registers a runner by token name and returns its runner id.
func (e *env) register(name string) int64 {
	e.t.Helper()
	code, _, data := e.do(http.MethodPost, "/v1/runner/register", name, "",
		[]byte(`{"harnesses":["claude"],"labels":["linux"]}`))
	if code != http.StatusOK {
		e.t.Fatalf("register %s: %d %s", name, code, data)
	}
	res := decode[struct {
		RunnerID int64 `json:"runner_id"`
	}](e.t, data)
	return res.RunnerID
}

// lease polls once for a job, failing the test on transport trouble.
func (e *env) lease(token string, wait float64) (int, []byte) {
	e.t.Helper()
	body := fmt.Sprintf(`{"wait":%g}`, wait)
	code, _, data := e.do(http.MethodPost, "/v1/runner/lease", token, "", []byte(body))
	if code != http.StatusOK {
		e.t.Fatalf("lease by %s: %d %s", token, code, data)
	}
	return code, data
}

func TestHealthWithoutAuth(t *testing.T) {
	e := newEnv(t)
	code, _, data := e.do(http.MethodGet, "/v1/health", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("health = %d %s, want 200", code, data)
	}
	if got := decode[struct {
		OK bool `json:"ok"`
	}](t, data); !got.OK {
		t.Errorf("health body = %s, want ok", data)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	own := e.submitWork()
	other := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"wake up"}`,
		e.project2, e.project2)
	code, _, data := e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(other))
	if code != http.StatusOK {
		t.Fatalf("submit the project-2 turn: %d %s", code, data)
	}
	otherID := decode[struct {
		JobID int64 `json:"job_id"`
	}](t, data).JobID
	jobPath := fmt.Sprintf("/v1/jobs/%d", own)
	otherPath := fmt.Sprintf("/v1/jobs/%d", otherID)

	// A missing, malformed or unknown token is 401.
	for name, authz := range map[string]string{
		"missing":    "",
		"non-bearer": "Token " + e.secrets["alice"],
		"wrong":      "Bearer not-a-secret",
		"empty":      "Bearer ",
	} {
		code, _, data, err := e.call(http.MethodGet, jobPath, authz, "", nil)
		if err != nil {
			t.Fatalf("%s token: %v", name, err)
		}
		if code != http.StatusUnauthorized {
			t.Errorf("%s token: status = %d %s, want 401", name, code, data)
			continue
		}
		if err := decode[errBody](t, data); err.Code != codeUnauthorized {
			t.Errorf("%s token: code = %q, want %q", name, err.Code, codeUnauthorized)
		}
	}

	// A known token in the wrong role is 403.
	for name, token := range map[string]string{
		"client submits":  "alice",
		"client fence":    "alice",
		"runner reads":    "r1",
		"runner cancels":  "r1",
		"foreman submits": "foreman1",
		"foreman bumps":   "foreman1",
	} {
		path, method := jobPath, http.MethodGet
		switch name {
		case "client submits", "foreman submits":
			method, path = http.MethodPost, "/v1/jobs"
		case "client fence", "foreman bumps":
			method, path = http.MethodPost, "/v1/admin/fence-epoch"
		case "runner cancels":
			method, path = http.MethodPost, otherPath+"/cancel"
		}
		code, _, data := e.do(method, path, token, "", nil)
		if code != http.StatusForbidden {
			t.Errorf("%s: status = %d %s, want 403", name, code, data)
			continue
		}
		if err := decode[errBody](t, data); err.Code != codeForbidden {
			t.Errorf("%s: code = %q, want %q", name, err.Code, codeForbidden)
		}
	}

	// The roles that belong do get in.
	if code, _, _ := e.do(http.MethodGet, jobPath, "alice", "", nil); code != http.StatusOK {
		t.Errorf("client reads a job: %d, want 200", code)
	}
	if code, _, _ := e.do(http.MethodGet, jobPath, "foreman1", "", nil); code != http.StatusOK {
		t.Errorf("foreman reads its own project's job: %d, want 200", code)
	}

	// A foreman token is walled into its own project (§12.1).
	code, _, data = e.do(http.MethodGet, otherPath, "foreman1", "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("foreman reads another project's job: %d %s, want 403", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeForbidden {
		t.Errorf("foreman scope error code = %q, want %q", err.Code, codeForbidden)
	}
	if code, _, data = e.do(http.MethodPost, otherPath+"/cancel", "foreman1", "", nil); code != http.StatusForbidden {
		t.Fatalf("foreman cancels another project's job: %d %s, want 403", code, data)
	}
	// Inside its own project the foreman may act: it cancels its
	// project's job (§4.2's "cancels it through fencing if you insist").
	code, _, data = e.do(http.MethodPost, jobPath+"/cancel", "foreman1", "", nil)
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "ok" {
		t.Fatalf("foreman cancels its own project's job = %d %s, want ok", code, data)
	}

	// The 404 is uniform JSON too.
	code, _, data = e.do(http.MethodGet, "/v1/jobs/99999", "alice", "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("read a missing job: %d %s, want 404", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeNotFound {
		t.Errorf("missing-job error code = %q, want %q", err.Code, codeNotFound)
	}
}

func TestTokenFile(t *testing.T) {
	// A file looser than 0600 is refused outright.
	e := newEnv(t)
	loose := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(loose, []byte(tokenFileBody), 0o644); err != nil {
		t.Fatalf("write loose token file: %v", err)
	}
	if _, err := New(e.ctx, e.st, e.eng, nil, loose); err == nil {
		t.Fatal("a 0644 token file was accepted; want refusal")
	} else if !strings.Contains(err.Error(), "0600") {
		t.Errorf("refusal = %v, want it to name the mode", err)
	}

	// Malformed files are refused with the line named.
	for name, body := range map[string]string{
		"two fields":       "alice client\n",
		"four fields":      "alice client secret extra\n",
		"unknown role":     "alice root secret\n",
		"unscoped foreman": "alice foreman secret\n",
		"bad scope":        "alice foreman:one secret\n",
		"duplicate name":   "alice client one\nalice runner two\n",
		"same secret":      "alice client shared-secret\nroot admin shared-secret\n",
		"empty":            "# only a comment\n",
	} {
		path := filepath.Join(t.TempDir(), "tokens")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := New(e.ctx, e.st, e.eng, nil, path); err == nil {
			t.Errorf("%s: accepted, want refusal", name)
		}
	}

	// The secrets reach the store only as sha256 hex (§6.4), with the
	// role and the foreman's project; a scope naming a project the store
	// does not know lands as NULL (projects may be created later).
	st := storeMustOpen(t)
	defer st.Close()
	if err := st.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/one.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, seedTS, seedTS)
		return err
	}); err != nil {
		t.Fatalf("seed a project: %v", err)
	}
	path := filepath.Join(t.TempDir(), "tokens")
	body := tokenFileBody + "foreman9 foreman:99 ninth-secret\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if _, err := New(context.Background(), st, engine.New(st, engine.Config{Now: newFakeClock().Now}), nil, path); err != nil {
		t.Fatalf("build api server: %v", err)
	}
	var rows []struct {
		role      string
		tokenHash string
		project   sql.NullInt64
	}
	if err := st.View(context.Background(), func(tx *sql.Tx) error {
		rowsQ, err := tx.Query(`SELECT role, token_hash, project_id FROM tokens ORDER BY role, token_hash`)
		if err != nil {
			return err
		}
		defer rowsQ.Close()
		for rowsQ.Next() {
			var r struct {
				role      string
				tokenHash string
				project   sql.NullInt64
			}
			if err := rowsQ.Scan(&r.role, &r.tokenHash, &r.project); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rowsQ.Err()
	}); err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("tokens in the store = %d, want 6", len(rows))
	}
	sha := sha256.Sum256([]byte(e.secrets["root"]))
	wantHash := hex.EncodeToString(sha[:])
	found := false
	for _, r := range rows {
		if len(r.tokenHash) != 64 || strings.ContainsRune(r.tokenHash, '-') {
			t.Errorf("token hash %q is not sha256 hex", r.tokenHash)
		}
		if r.tokenHash == wantHash {
			found = true
			if r.role != roleAdmin {
				t.Errorf("admin hash stored with role %q", r.role)
			}
		}
		if r.tokenHash == e.secrets["root"] || r.tokenHash == e.secrets["alice"] {
			t.Errorf("a plaintext secret landed in the store: %q", r.tokenHash)
		}
	}
	if !found {
		t.Errorf("no stored hash matches sha256(root's secret) = %s", wantHash)
	}
	// The foreman token scoped to project 1 carries it; the one naming
	// an unknown project stores NULL rather than failing the engine.
	var scope sql.NullInt64
	if err := st.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT project_id FROM tokens WHERE role = 'foreman' AND project_id IS NOT NULL`).Scan(&scope)
	}); err != nil {
		t.Fatalf("read the scoped foreman token: %v", err)
	}
	if !scope.Valid || scope.Int64 != 1 {
		t.Errorf("foreman scope in store = %v, want project 1", scope)
	}

	// Re-syncing replaces: the old hashes do not linger.
	if err := os.WriteFile(path, []byte("solo client solo-secret\n"), 0o600); err != nil {
		t.Fatalf("rewrite token file: %v", err)
	}
	if _, err := New(context.Background(), st, engine.New(st, engine.Config{Now: newFakeClock().Now}), nil, path); err != nil {
		t.Fatalf("rebuild api server: %v", err)
	}
	var n int
	if err := st.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&n)
	}); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if n != 1 {
		t.Errorf("tokens after re-sync = %d, want 1: the file is the authority", n)
	}
}

func storeMustOpen(t *testing.T) *store.Store {
	t.Helper()
	return openFreshStore(t, filepath.Join(t.TempDir(), "store.db"))
}
