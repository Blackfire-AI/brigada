package runner

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestRunnerProbesAndClearsABrokenAgent exercises the runner's probe
// loop end to end (§8 "Health"): a failure-broken agent whose probe is
// due is fetched, probed through its adapter, and a passing probe
// clears it through the engine.
func TestRunnerProbesAndClearsABrokenAgent(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}
	e := newTestEnv(t, engine.Config{ProbeDelay: 200 * time.Millisecond, ProbeMaxDelay: time.Second})
	p := e.startRunner("", 50*time.Millisecond, func(cfg *Config) {
		cfg.ProbeEvery = 50 * time.Millisecond
		cfg.Adapters[0].(*fake.Adapter).ProbeScript = probe
	})
	defer p.stop(t)

	// The runner registers its fake agent before the probe can reach it.
	e.waitUntil("the runner registers its agent", func() bool {
		return e.agentHealth() != ""
	})
	// Break the agent with a probe already due.
	if err := e.breakAgent("repeated failures", "2000-01-01T00:00:00.000000000Z", 60); err != nil {
		t.Fatalf("break the agent: %v", err)
	}
	e.waitUntil("a passing probe clears the broken agent", func() bool {
		return e.agentHealth() == "ready"
	})
	if why := e.agentReason(); why != "" {
		t.Errorf("health_reason after the passing probe = %q, want empty", why)
	}
}

// TestRunnerProbesAndReportsAFailure exercises the failing side of the
// loop: a failing probe is reported as such, so the agent stays broken
// and the engine backs the next probe off (the delay, in seconds, grows).
func TestRunnerProbesAndReportsAFailure(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}
	e := newTestEnv(t, engine.Config{ProbeDelay: 100 * time.Millisecond, ProbeMaxDelay: 10 * time.Second})
	p := e.startRunner("", 50*time.Millisecond, func(cfg *Config) {
		cfg.ProbeEvery = 50 * time.Millisecond
		cfg.Adapters[0].(*fake.Adapter).ProbeScript = probe
	})
	defer p.stop(t)

	e.waitUntil("the runner registers its agent", func() bool {
		return e.agentHealth() != ""
	})
	if err := e.breakAgent("repeated failures", "2000-01-01T00:00:00.000000000Z", 1); err != nil {
		t.Fatalf("break the agent: %v", err)
	}
	// The failing probe keeps the agent broken and doubles the delay to
	// the next probe, from 1 second to 2.
	e.waitUntil("a failing probe backs the next probe off", func() bool {
		return e.agentProbeDelay() > 1
	})
	if h := e.agentHealth(); h != "broken" {
		t.Errorf("agent health after a failing probe = %q, want broken", h)
	}
}

// agentHealth reads the fake agent's health, "" when it has none yet.
func (e *testEnv) agentHealth() string {
	var h string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT health FROM agents WHERE adapter = 'fake'").Scan(&h)
	}); err != nil {
		return ""
	}
	return h
}

// agentReason reads the fake agent's health_reason.
func (e *testEnv) agentReason() string {
	var r string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT health_reason FROM agents WHERE adapter = 'fake'").Scan(&r)
	}); err != nil {
		return ""
	}
	return r
}

// agentProbeDelay reads the fake agent's probe_delay.
func (e *testEnv) agentProbeDelay() int64 {
	var d int64
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT probe_delay FROM agents WHERE adapter = 'fake'").Scan(&d)
	}); err != nil {
		return 0
	}
	return d
}

// breakAgent marks the fake agent broken for reason with a probe due at
// probeAt and the given delay.
func (e *testEnv) breakAgent(reason, probeAt string, delay int64) error {
	return e.update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE agents SET health = 'broken', health_reason = ?, probe_at = ?, probe_delay = ? WHERE adapter = 'fake'`,
			reason, probeAt, delay)
		return err
	})
}
