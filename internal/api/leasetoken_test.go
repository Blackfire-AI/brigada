package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// leaseTTL is the engine's default lease TTL (engine.DefaultConfig); the
// env's engine keeps it, and the tests drive the clock by it.
const leaseTTL = 90 * time.Second

// submitTurn queues a foreman turn for the project through the admin
// endpoint, returning its id.
func (e *env) submitTurn(project int64) int64 {
	e.t.Helper()
	body := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"wake up"}`,
		project, project)
	code, _, data := e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(body))
	if code != http.StatusOK {
		e.t.Fatalf("submit the project-%d turn: %d %s", project, code, data)
	}
	return decode[struct {
		JobID int64 `json:"job_id"`
	}](e.t, data).JobID
}

// registerForeman registers the runner with the one agent a seeded
// project's foreman turn needs: claude/sonnet, the pair both projects
// chose, taking every class so the runner still serves workers.
func (e *env) registerForeman(name string) int64 {
	e.t.Helper()
	code, _, data := e.do(http.MethodPost, "/v1/runner/register", name, "",
		[]byte(`{"harnesses":["claude"],"labels":["linux"],"agents":[{"adapter":"claude","harness":"claude","model":"sonnet","family":"claude","account":"claude","classes":["implement","review","answer"]}]}`))
	if code != http.StatusOK {
		e.t.Fatalf("register %s with its agent: %d %s", name, code, data)
	}
	return decode[struct {
		RunnerID int64 `json:"runner_id"`
	}](e.t, data).RunnerID
}

// leaseTurnBy registers the runner and leases the queued foreman turn,
// returning its grant.
func (e *env) leaseTurnBy(name string) *jobGrant {
	e.t.Helper()
	e.registerForeman(name)
	code, _, data := e.do(http.MethodPost, "/v1/runner/lease", name, "", []byte(`{"wait":0,"harnesses":["claude"]}`))
	if code != http.StatusOK {
		e.t.Fatalf("lease by %s: %d %s", name, code, data)
	}
	g := decode[struct {
		Job *jobGrant `json:"job"`
	}](e.t, data).Job
	if g == nil {
		e.t.Fatalf("the foreman turn was not leased: %s", data)
	}
	return g
}

// withToken performs one call bearing the minted secret, reporting the
// raw status and body.
func (e *env) withToken(secret, method, path string) (int, []byte) {
	e.t.Helper()
	code, _, data, err := e.call(method, path, "Bearer "+secret, "", nil)
	if err != nil {
		e.t.Fatalf("%s %s with a lease token: %v", method, path, err)
	}
	return code, data
}

// mintedTurn leases a foreman turn on a fresh env and returns the env
// and the grant carrying the minted secret.
func mintedTurn(t *testing.T) (*env, *jobGrant) {
	t.Helper()
	e := newEnv(t)
	e.submitTurn(e.project1)
	return e, e.leaseTurnBy("r1")
}

// TestLeaseTokenAuthenticatesAsForeman covers the mint and the scope
// (§12.1): a foreman turn's lease carries the token, a task job's does
// not, and the token authenticates as a foreman — 200 on /v1/status and
// its own project's job, 403 on another project's, the same
// mayTouchProject rule the file's foreman tokens answer to.
func TestLeaseTokenAuthenticatesAsForeman(t *testing.T) {
	e := newEnv(t)
	own := e.submitTurn(e.project1)
	other := e.submitTurn(e.project2)
	g := e.leaseTurnBy("r1")
	secret := g.ForemanToken
	if secret == "" || len(secret) != 64 {
		t.Fatalf("the turn's grant carried %q, want a 64-char hex secret", secret)
	}

	// A task job's grant carries none: both turns are already leased, so
	// the task job is all the other runner finds.
	if turn := e.leaseTurnBy("r2"); turn.JobID != other {
		t.Fatalf("the second runner leased job %d, want the other project's turn %d", turn.JobID, other)
	}
	task := e.submitWork()
	_, data := e.lease("r2", 0)
	if w := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job; w == nil || w.JobID != task {
		t.Fatalf("the task job was not leased: %s", data)
	} else if w.ForemanToken != "" {
		t.Errorf("a task job's grant carried a foreman token: %q", w.ForemanToken)
	}

	if code, data := e.withToken(secret, http.MethodGet, "/v1/status"); code != http.StatusOK {
		t.Errorf("status with the lease token = %d %s, want 200", code, data)
	}
	if code, data := e.withToken(secret, http.MethodGet, fmt.Sprintf("/v1/jobs/%d", own)); code != http.StatusOK {
		t.Errorf("the turn's own project's job = %d %s, want 200", code, data)
	}
	code, data := e.withToken(secret, http.MethodGet, fmt.Sprintf("/v1/jobs/%d", other))
	if code != http.StatusForbidden {
		t.Fatalf("another project's job with the lease token = %d %s, want 403", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeForbidden {
		t.Errorf("the scope refusal's code = %q, want %q", err.Code, codeForbidden)
	}
}

// TestLeaseTokenHeartbeatExtendsIt covers the token's lease-long life
// (§12.1): a heartbeat that extends the lease extends the token with
// it, so the bridge's calls work past the original expiry but not past
// the extended one.
func TestLeaseTokenHeartbeatExtendsIt(t *testing.T) {
	e, g := mintedTurn(t)
	e.clock.Advance(60 * time.Second)
	body := fmt.Sprintf(`{"job_id":%d,"lease_id":%q}`, g.JobID, g.LeaseID)
	code, _, data := e.do(http.MethodPost, "/v1/runner/heartbeat", "r1", "", []byte(body))
	if code != http.StatusOK || decode[struct {
		Answer string `json:"answer"`
	}](t, data).Answer != "ok" {
		t.Fatalf("heartbeat with the turn's lease = %d %s, want ok", code, data)
	}

	// Past the original expiry, before the extended one: still good.
	e.clock.Advance(60 * time.Second)
	if code, data := e.withToken(g.ForemanToken, http.MethodGet, "/v1/status"); code != http.StatusOK {
		t.Errorf("status after the heartbeat = %d %s, want 200: the token lives with the lease", code, data)
	}

	// Past the extended expiry: refused even before any sweep runs.
	e.clock.Advance(leaseTTL)
	if code, data := e.withToken(g.ForemanToken, http.MethodGet, "/v1/status"); code != http.StatusUnauthorized {
		t.Errorf("status past the extended expiry = %d %s, want 401", code, data)
	}
}

// TestLeaseTokenRefusedWhenTheTurnEnds covers the revocation through
// the API: complete, release, the sweep's lease expiry and a fence bump
// each leave the minted secret refused — 401, the token no longer
// credentials.
func TestLeaseTokenRefusedWhenTheTurnEnds(t *testing.T) {
	end := func(t *testing.T, ending func(e *env, g *jobGrant)) {
		e, g := mintedTurn(t)
		ending(e, g)
		code, data := e.withToken(g.ForemanToken, http.MethodGet, "/v1/status")
		if code != http.StatusUnauthorized {
			t.Errorf("status after the ending = %d %s, want 401", code, data)
		}
	}
	t.Run("complete", func(t *testing.T) {
		end(t, func(e *env, g *jobGrant) {
			body := fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"result":"{\"status\":\"done\",\"summary\":\"answered\"}"}`,
				g.JobID, g.LeaseID)
			code, _, data := e.do(http.MethodPost, "/v1/runner/complete", "r1", "", []byte(body))
			if code != http.StatusOK {
				e.t.Fatalf("complete the turn: %d %s", code, data)
			}
		})
	})
	t.Run("release", func(t *testing.T) {
		end(t, func(e *env, g *jobGrant) {
			body := fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"reason":"wrong harness"}`, g.JobID, g.LeaseID)
			code, _, data := e.do(http.MethodPost, "/v1/runner/release", "r1", "", []byte(body))
			if code != http.StatusOK {
				e.t.Fatalf("release the turn: %d %s", code, data)
			}
		})
	})
	t.Run("expired lease, with the sweep", func(t *testing.T) {
		end(t, func(e *env, g *jobGrant) {
			e.clock.Advance(leaseTTL + time.Second)
			if _, err := e.eng.Sweep(e.ctx); err != nil {
				e.t.Fatalf("sweep: %v", err)
			}
		})
	})
	t.Run("fence bump", func(t *testing.T) {
		end(t, func(e *env, g *jobGrant) {
			code, _, data := e.do(http.MethodPost, "/v1/admin/fence-epoch", "root", "", nil)
			if code != http.StatusOK {
				e.t.Fatalf("bump the fence epoch: %d %s", code, data)
			}
		})
	})
}

// TestLeaseTokenNeverStoredNorLogged covers the secret's hygiene: the
// store keeps only its sha256, no event payload or message carries it,
// and re-syncing the token file — the file is the authority over the
// long-lived tokens alone — leaves the engine's lease tokens whole.
func TestLeaseTokenNeverStoredNorLogged(t *testing.T) {
	e, g := mintedTurn(t)
	secret := g.ForemanToken

	digest := sha256.Sum256([]byte(secret))
	var stored string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT token_hash FROM lease_tokens WHERE job_id = ?`, g.JobID).Scan(&stored)
	}); err != nil {
		t.Fatalf("read the lease token: %v", err)
	}
	if stored != hex.EncodeToString(digest[:]) {
		t.Errorf("stored hash = %s, want sha256 of the secret", stored)
	}
	for _, query := range []string{
		"SELECT payload FROM events",
		"SELECT content FROM messages",
		`SELECT COALESCE(result, '') FROM jobs`,
	} {
		var text string
		err := e.st.View(e.ctx, func(tx *sql.Tx) error {
			rows, err := tx.Query(query)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if err := rows.Scan(&text); err != nil {
					return err
				}
				if strings.Contains(text, secret) {
					t.Errorf("the minted secret appears in %q", query)
					return nil
				}
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
	}

	// A re-sync of the token file must not touch the lease tokens: a
	// restarted engine cannot revoke a live turn's token.
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte(tokenFileBody), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if _, err := New(e.ctx, e.st, e.eng, nil, path); err != nil {
		t.Fatalf("re-sync the token file: %v", err)
	}
	if code, data := e.withToken(secret, http.MethodGet, "/v1/status"); code != http.StatusOK {
		t.Errorf("status after the re-sync = %d %s, want 200", code, data)
	}
}
