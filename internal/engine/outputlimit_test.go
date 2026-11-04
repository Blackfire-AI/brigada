package engine

import "testing"

const outputLimitError = "output limit reached: 32,000 tokens, 31,951 of them reasoning"

// TestOutputLimitWorkFailureRestartsOnAnotherAgentWithoutAnEffortBump: a
// run cut off at its output limit is a transient CLI failure. The task
// restarts on another agent, the error keeps the escalation ladder
// still, and the account does not cool as it does for a quota limit.
func TestOutputLimitWorkFailureRestartsOnAnotherAgentWithoutAnEffortBump(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "family", "implement")
	other := e.agented("other", "family", "implement")
	otherAgent := e.agentOf(other)
	e.submit(e.work())
	effort := e.str("SELECT effective_effort FROM tasks WHERE id = ?", e.task)
	g := e.leaseAny(author)
	if ans := e.complete(g, Completion{Error: outputLimitError, ErrorKind: "output_limit"}); ans != AnsOK {
		t.Fatalf("complete = %s, want ok", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
		t.Fatalf("task state = %s, want ready", state)
	}
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND restart = 1", e.task)
	if agent := e.int("SELECT require_agent FROM jobs WHERE id = ?", retry); agent != otherAgent {
		t.Errorf("retry pin = %d, want other agent %d", agent, otherAgent)
	}
	if got := e.str("SELECT effective_effort FROM tasks WHERE id = ?", e.task); got != effort {
		t.Errorf("effort = %s, want %s unchanged", got, effort)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE task_id = ? AND kind LIKE '%bumped%'", e.task); n != 0 {
		t.Errorf("tier or effort bumps = %d, want none", n)
	}
	if p, ok := e.eventPayloadOf(evTaskRestarted); !ok || p.Reason != "output_limit: "+outputLimitError {
		t.Errorf("restart event = %+v (%t), want the kind and text as reason", p, ok)
	}
	if _, _, cooling := e.agentState(e.agentOf(author)); cooling != "" {
		t.Errorf("agent cooling = %q, want none: this is no quota limit", cooling)
	}
}

// TestOutputLimitWorkFailureParksWithItsReason: with no other agent to
// restart on, the task parks and the needs-you entry and the task_failed
// event both carry the cut-off text.
func TestOutputLimitWorkFailureParksWithItsReason(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "family", "implement")
	e.submit(e.work())
	g := e.leaseAny(author)
	e.complete(g, Completion{Error: outputLimitError, ErrorKind: "output_limit"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %s, want needs_foreman", state)
	}
	if p, ok := e.eventPayloadOf(evTaskFailed); !ok || p.Reason != outputLimitError {
		t.Errorf("task_failed payload = %+v (%t), want the cut-off text", p, ok)
	}
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got string
	for _, it := range st.NeedsYou {
		if it.Kind == "task" && it.ID == e.task {
			got = it.Reason
		}
	}
	if got != outputLimitError {
		t.Errorf("needs-you reason = %q, want %q", got, outputLimitError)
	}
}
