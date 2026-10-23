package api

import (
	"strings"
	"testing"
)

// TestClientRegisterRecordsProcessStartAndRendersAuthLine runs the wire
// path: Client.Register sends the process start, the engine stores it, and
// the rendered needs-you line for an auth-broken dsh agent names the
// variable and that start.
func TestClientRegisterRecordsProcessStartAndRendersAuthLine(t *testing.T) {
	e := newEnv(t)
	c := NewClient(e.srv.Listener.Addr().String(), e.secrets["r1"])
	agents := []Agent{{Adapter: "dsh", Harness: "dsh", Model: "m", Classes: []string{"implement"}}}
	if err := c.Register(e.ctx, agents, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	want := processStart.UTC().Format("2006-01-02T15:04:05Z")
	got := e.str("SELECT started_at FROM runners")
	if got == "" || !strings.HasPrefix(got, want[:10]) {
		t.Fatalf("started_at = %q, want the process start (%s)", got, want)
	}
	// A re-register of the same process leaves the stored start alone.
	if err := c.Register(e.ctx, agents, 0); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if again := e.str("SELECT started_at FROM runners"); again != got {
		t.Errorf("started_at after re-register = %q, want %q", again, got)
	}

	e.exec("UPDATE agents SET health = 'broken', health_reason = 'auth'")
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	out := StatusSections(Status{NeedsYou: st.NeedsYou})
	line := "agent dsh/m broken: auth: DEEPSEEK_API_KEY is read from the runner's environment; the runner started " + got
	if !strings.Contains(out, line) {
		t.Errorf("rendered status lacks %q:\n%s", line, out)
	}

	// Another harness keeps the bare line.
	e.exec("UPDATE agents SET harness = 'claude'")
	st, err = e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	out = StatusSections(Status{NeedsYou: st.NeedsYou})
	if !strings.Contains(out, "agent claude/m broken: auth\n") && !strings.HasSuffix(strings.TrimSpace(out), "agent claude/m broken: auth") {
		t.Errorf("claude line changed:\n%s", out)
	}
}
