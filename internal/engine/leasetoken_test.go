package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// leaseTokenRow reads the one lease token of a job, or nil when the job
// holds none.
func (e *env) leaseTokenRow(job int64) *struct {
	hash, lease, expires string
	project              int64
} {
	e.t.Helper()
	var r struct {
		hash, lease, expires string
		project              int64
	}
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT token_hash, project_id, lease_id, expires_at
			FROM lease_tokens WHERE job_id = ?`, job).Scan(&r.hash, &r.project, &r.lease, &r.expires)
	})
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		e.t.Fatalf("read the lease token of job %d: %v", job, err)
	}
	return &r
}

// scopeOf resolves a secret through the engine's authentication path.
func (e *env) scopeOf(secret string) (project int64, ok bool) {
	e.t.Helper()
	p, _, ok, err := e.eng.LeaseTokenScope(e.ctx, secret)
	if err != nil {
		e.t.Fatalf("resolve the lease token: %v", err)
	}
	return p, ok
}

// assertSecretNowhere asserts the minted secret appears in no event
// payload and no message — the log is public history, the secret never
// (§12.1).
func (e *env) assertSecretNowhere(secret string) {
	e.t.Helper()
	for _, query := range []string{"SELECT payload FROM events", "SELECT content FROM messages"} {
		var found string
		err := e.st.View(e.ctx, func(tx *sql.Tx) error {
			rows, err := tx.Query(query)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if err := rows.Scan(&found); err != nil {
					return err
				}
				if strings.Contains(found, secret) {
					e.t.Errorf("the lease token's secret appears in %s: %s", query, found)
					return nil
				}
			}
			return rows.Err()
		})
		if err != nil {
			e.t.Fatalf("scan %q: %v", query, err)
		}
	}
}

// TestLeaseMintsForemanToken covers the mint: a foreman turn's lease
// carries a token, a task job's lease carries none, the store keeps
// only the secret's sha256 — keyed to the job, the lease and the
// project, expiring with the lease — and the secret itself reaches no
// event or message.
func TestLeaseMintsForemanToken(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner2)

	task := e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g.ForemanToken != "" {
		t.Errorf("a task job's lease carried a foreman token: %q", g.ForemanToken)
	}
	if row := e.leaseTokenRow(task.JobID); row != nil {
		t.Errorf("a task job minted a lease token: %+v", row)
	}
	e.complete(g, done())

	turn := e.submit(e.foremanTurn())
	fg := e.leaseClaude(e.runner2)
	if fg == nil || fg.JobID != turn.JobID {
		t.Fatalf("the foreman turn was not leased: %+v", fg)
	}
	secret := fg.ForemanToken
	if secret == "" || len(secret) != 2*leaseTokenEntropy {
		t.Fatalf("the turn's lease token = %q, want %d hex chars", secret, 2*leaseTokenEntropy)
	}
	if _, err := hex.DecodeString(secret); err != nil {
		t.Fatalf("the turn's lease token is not hex: %v", err)
	}

	// The store keeps the hash — never the secret — with the turn's
	// project, job and lease, expiring exactly with the lease.
	row := e.leaseTokenRow(turn.JobID)
	if row == nil {
		t.Fatal("the turn minted no lease token")
	}
	digest := sha256.Sum256([]byte(secret))
	if row.hash != hex.EncodeToString(digest[:]) {
		t.Errorf("stored hash = %s, want sha256 of the secret", row.hash)
	}
	if row.lease != fg.LeaseID || row.project != e.project {
		t.Errorf("lease token keyed to lease %s project %d, want %s and %d", row.lease, row.project, fg.LeaseID, e.project)
	}
	if want := e.str("SELECT lease_expires_at FROM jobs WHERE id = ?", turn.JobID); row.expires != want {
		t.Errorf("token expires at %s, want the lease's %s", row.expires, want)
	}

	// The scope resolves to the turn's project, and only while the
	// secret is the one minted.
	if project, ok := e.scopeOf(secret); !ok || project != e.project {
		t.Errorf("scope of the minted token = project %d, %v; want %d, true", project, ok, e.project)
	}
	if _, ok := e.scopeOf("not-the-secret"); ok {
		t.Error("an unknown secret resolved to a scope")
	}

	// Two leases of the same key mint two different secrets.
	e.complete(fg, done())
	turn2 := e.submit(e.foremanTurn())
	fg2 := e.leaseClaude(e.runner2)
	if fg2 == nil || fg2.JobID != turn2.JobID {
		t.Fatalf("the second turn was not leased: %+v", fg2)
	}
	if fg2.ForemanToken == "" || fg2.ForemanToken == secret {
		t.Errorf("the second turn's token = %q, want a fresh secret", fg2.ForemanToken)
	}
	if _, ok := e.scopeOf(secret); ok {
		t.Error("the first turn's secret still resolves after its completion")
	}

	e.assertSecretNowhere(secret)
}

// TestLeaseTokenHeartbeatExtendsIt covers the token's lifetime rule: a
// heartbeat that extends the lease extends the token to the same
// expiry, so the bridge's calls live as long as the turn does.
func TestLeaseTokenHeartbeatExtendsIt(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	turn := e.submit(e.foremanTurn())
	g := e.leaseClaude(e.runner1)

	before := e.leaseTokenRow(turn.JobID).expires
	e.clock.Advance(30 * time.Second)
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Fatalf("heartbeat = %s, want ok", ans)
	}
	after := e.leaseTokenRow(turn.JobID)
	if after.expires == before {
		t.Fatalf("the token's expiry did not move: %s", after.expires)
	}
	if want := e.str("SELECT lease_expires_at FROM jobs WHERE id = ?", turn.JobID); after.expires != want {
		t.Errorf("token expires at %s, want the lease's %s", after.expires, want)
	}

	// The extension is real authentication, not bookkeeping: past the
	// old expiry — without any sweep — the token still resolves, and
	// past the new one it does not.
	e.clock.Advance(e.eng.cfg.LeaseTTL - 15*time.Second)
	if _, ok := e.scopeOf(g.ForemanToken); !ok {
		t.Error("the token died before its extended expiry")
	}
	e.clock.Advance(30 * time.Second)
	if _, ok := e.scopeOf(g.ForemanToken); ok {
		t.Error("the token outlived its extended expiry")
	}
}

// TestLeaseTokenRevokedWhenTheTurnEnds covers the revocation: complete,
// release and cancel each take the job's lease tokens with them, in the
// same transaction as the state change, and the sweep's lease expiry
// does the same.
func TestLeaseTokenRevokedWhenTheTurnEnds(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		e := newEnv(t)
		e.offerForeman(e.runner1)
		turn := e.submit(e.foremanTurn())
		g := e.leaseClaude(e.runner1)
		if ans := e.complete(g, done()); ans != AnsOK {
			t.Fatalf("complete = %s, want ok", ans)
		}
		if row := e.leaseTokenRow(turn.JobID); row != nil {
			t.Errorf("the token survived the completion: %+v", row)
		}
		if _, ok := e.scopeOf(g.ForemanToken); ok {
			t.Error("the token still resolves after the completion")
		}
	})
	t.Run("release", func(t *testing.T) {
		e := newEnv(t)
		e.offerForeman(e.runner1)
		turn := e.submit(e.foremanTurn())
		g := e.leaseClaude(e.runner1)
		if ans := e.release(e.runner1, g, "wrong harness"); ans != AnsOK {
			t.Fatalf("release = %s, want ok", ans)
		}
		if row := e.leaseTokenRow(turn.JobID); row != nil {
			t.Errorf("the token survived the release: %+v", row)
		}
		if _, ok := e.scopeOf(g.ForemanToken); ok {
			t.Error("the token still resolves after the release")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		e := newEnv(t)
		e.offerForeman(e.runner1)
		turn := e.submit(e.foremanTurn())
		g := e.leaseClaude(e.runner1)
		if ans := e.cancel(turn.JobID); ans != AnsOK {
			t.Fatalf("cancel = %s, want ok", ans)
		}
		if row := e.leaseTokenRow(turn.JobID); row != nil {
			t.Errorf("the token survived the cancellation: %+v", row)
		}
		if _, ok := e.scopeOf(g.ForemanToken); ok {
			t.Error("the token still resolves after the cancellation")
		}
	})
	t.Run("the sweep's lease expiry", func(t *testing.T) {
		e := newEnv(t)
		e.offerForeman(e.runner1)
		turn := e.submit(e.foremanTurn())
		g := e.leaseClaude(e.runner1)
		rep := e.expireLease()
		if len(rep.Expired) != 1 || rep.Expired[0] != turn.JobID {
			t.Fatalf("the sweep expired %+v, want job %d", rep.Expired, turn.JobID)
		}
		if row := e.leaseTokenRow(turn.JobID); row != nil {
			t.Errorf("the token survived the lease's expiry: %+v", row)
		}
		if _, ok := e.scopeOf(g.ForemanToken); ok {
			t.Error("the token still resolves after the lease's expiry")
		}
	})
	t.Run("a hard cap", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) { c.HardCaps = map[Kind]time.Duration{KindForeman: time.Minute} })
		e.offerForeman(e.runner1)
		turn := e.submit(e.foremanTurn())
		g := e.leaseClaude(e.runner1)
		e.clock.Advance(2 * time.Minute)
		e.mustSweep()
		if row := e.leaseTokenRow(turn.JobID); row != nil {
			t.Errorf("the token survived the hard cap: %+v", row)
		}
		if _, ok := e.scopeOf(g.ForemanToken); ok {
			t.Error("the token still resolves after the hard cap")
		}
	})
}

// TestLeaseTokenExpiresWithoutSweep covers the rule that matters when
// the engine is quiet: the expiry is checked at authentication time, so
// a token past its lease is refused even while its row waits for the
// sweep — and the row is still there to prove the refusal was the
// check's, not the sweep's.
func TestLeaseTokenExpiresWithoutSweep(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	e.submit(e.foremanTurn())
	g := e.leaseClaude(e.runner1)

	e.clock.Advance(e.eng.cfg.LeaseTTL + time.Second)
	if _, ok := e.scopeOf(g.ForemanToken); ok {
		t.Error("the token resolved past its expiry, sweep or no sweep")
	}
	if row := e.leaseTokenRow(g.JobID); row == nil {
		t.Fatal("the row vanished without a sweep; the refusal should have been the expiry check's")
	}
}

// TestLeaseTokenDiesAtAFenceBump covers the recover path (§6.4): the
// bump fences every older-epoch lease out, and their tokens with them.
func TestLeaseTokenDiesAtAFenceBump(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	e.submit(e.foremanTurn())
	g := e.leaseClaude(e.runner1)

	epoch, err := e.eng.BumpFenceEpoch(e.ctx)
	if err != nil {
		t.Fatalf("bump the fence epoch: %v", err)
	}
	if epoch != 2 {
		t.Errorf("epoch after the bump = %d, want 2", epoch)
	}
	if n := e.int("SELECT COUNT(*) FROM lease_tokens"); n != 0 {
		t.Errorf("lease tokens after the bump = %d, want 0", n)
	}
	if _, ok := e.scopeOf(g.ForemanToken); ok {
		t.Error("the token still resolves after a fence bump")
	}
}
