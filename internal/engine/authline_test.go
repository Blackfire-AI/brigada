package engine

import (
	"strings"
	"testing"
	"time"
)

// agentLine registers a runner with one agent on harness, breaks it for
// auth and returns the agent's needs-you reason.
func (e *env) authReason(name, harness string, started time.Time) string {
	e.t.Helper()
	if _, err := e.eng.RegisterStarted(e.ctx, name, started, 0, []AgentAdvert{{
		Adapter: harness, Harness: harness, Model: "m", Classes: []string{"implement"},
	}}); err != nil {
		e.t.Fatalf("register: %v", err)
	}
	e.exec("UPDATE agents SET health = 'broken', health_reason = 'auth' WHERE harness = ?", harness)
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	for _, n := range st.NeedsYou {
		if n.Kind == "agent" && n.Title == harness+"/m" {
			return n.Reason
		}
	}
	e.t.Fatalf("no needs-you line for %s: %+v", harness, st.NeedsYou)
	return ""
}

func TestAuthLineNamesEnvVariableAndRunnerStart(t *testing.T) {
	e := newEnv(t)
	started := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	got := e.authReason("dsh-runner", "dsh", started)
	for _, want := range []string{"auth", "DEEPSEEK_API_KEY", fmtTime(started)} {
		if !strings.Contains(got, want) {
			t.Errorf("reason %q lacks %q", got, want)
		}
	}
}

func TestAuthLineSaysWhenTheRunnerReportedNoStart(t *testing.T) {
	e := newEnv(t)
	got := e.authReason("old-runner", "dsh", time.Time{})
	for _, want := range []string{"auth", "DEEPSEEK_API_KEY", "has not reported its start"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason %q lacks %q", got, want)
		}
	}
}

func TestAuthLineKeepsPlainFormForOtherHarnesses(t *testing.T) {
	e := newEnv(t)
	if got := e.authReason("claude-runner", "claude", time.Now()); got != "auth" {
		t.Errorf("reason = %q, want %q", got, "auth")
	}
}

func TestRegisterRecordsStartOnlyWhenItChanges(t *testing.T) {
	e := newEnv(t)
	first := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	reg := func(started time.Time) {
		t.Helper()
		if _, err := e.eng.RegisterStarted(e.ctx, "r", started, 0, nil); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	read := func() string { return e.str("SELECT started_at FROM runners WHERE name = 'r'") }
	reg(first)
	if got := read(); got != fmtTime(first) {
		t.Fatalf("started_at = %q, want %q", got, fmtTime(first))
	}
	reg(time.Time{}) // a register that reports no start leaves it
	if got := read(); got != fmtTime(first) {
		t.Errorf("started_at after silent register = %q", got)
	}
	reg(first) // the same process re-polling leaves it
	if got := read(); got != fmtTime(first) {
		t.Errorf("started_at after same-start register = %q", got)
	}
	reg(second)
	if got := read(); got != fmtTime(second) {
		t.Errorf("started_at after restart = %q, want %q", got, fmtTime(second))
	}
}
