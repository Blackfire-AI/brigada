package engine

import (
	"strings"
	"testing"
	"time"
)

// TestModelRejectedBreaksTheAgentUntilReset is §8 "Health": a completion
// whose model the provider rejected breaks that agent alone, with no
// probe scheduled or accepted; the needs-you item names the model and
// the error and points at `brigada agents`; only the user's reset
// clears it.
func TestModelRejectedBreaksTheAgentUntilReset(t *testing.T) {
	e := newEnv(t)
	a := e.addAgent(e.runner1, "codex", "example-model", "work")
	sibling := e.addAgent(e.runner1, "kimi", "k2", "work")
	const msg = "The 'example-model' model is not supported when using Codex with a ChatGPT account."
	e.runPinned(e.runner1, "codex", Completion{Error: msg, ErrorKind: "model"})

	h, why, _ := e.agentState(a)
	if h != "broken" || !strings.HasPrefix(why, brokenModel) || !strings.Contains(why, msg) {
		t.Fatalf("agent: health %q reason %q, want broken(model rejected: …)", h, why)
	}
	if h, _, _ := e.agentState(sibling); h != "ready" {
		t.Errorf("sibling agent health %q, want ready", h)
	}
	if at, _ := e.agentProbe(a); at != "" {
		t.Errorf("model-broken agent has probe_at %q, want none scheduled", at)
	}

	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var item *NeedsYouItem
	for i := range info.NeedsYou {
		if info.NeedsYou[i].Kind == "agent" {
			item = &info.NeedsYou[i]
		}
	}
	if item == nil || item.Title != "codex/example-model" || !strings.Contains(item.Reason, msg) ||
		!strings.HasSuffix(item.Reason, "`brigada agents`") {
		t.Errorf("needs-you item = %+v, want the model, the error and the agents pointer", item)
	}

	e.elapse(time.Hour)
	if got := e.dueProbes(e.runner1); len(got) != 0 {
		t.Fatalf("due probes = %v, want none", got)
	}
	if err := e.eng.ReportProbe(e.ctx, e.runner1, a, true); err != nil {
		t.Fatalf("report probe: %v", err)
	}
	if h, _, _ := e.agentState(a); h != "broken" {
		t.Errorf("model-broken agent after a probe report: %q, want still broken", h)
	}

	if n, err := e.eng.ResetAgents(e.ctx, "codex", "example-model"); err != nil || n != 1 {
		t.Fatalf("reset: n = %d, err = %v; want 1", n, err)
	}
	if h, _, _ := e.agentState(a); h != "ready" {
		t.Errorf("agent after reset: %q, want ready", h)
	}
}
