package engine

import "testing"

const outputLimitText = "output limit reached: 32,000 tokens, 31,951 of them reasoning"

// A work job cut off at its output limit restarts on another agent with
// the effort unchanged: the completion carries an error, so escalation
// is skipped, and the kind is not a quota limit, so no account cools.
func TestOutputLimitWorkFailureRestartsOnAnotherAgentWithoutEffortBump(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "same-family", "implement")
	other := e.agented("other", "same-family", "implement")
	otherAgent := e.agentOf(other)
	e.exec("UPDATE agents SET account = 'other-account' WHERE id = ?", otherAgent)
	effortBefore := e.str("SELECT COALESCE(effective_effort, '') FROM tasks WHERE id = ?", e.task)
	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, Completion{Error: outputLimitText, ErrorKind: "output_limit"}); ans != AnsOK {
		t.Fatalf("complete = %s, want ok", ans)
	}
	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND restart = 1", e.task)
	if agent := e.int("SELECT require_agent FROM jobs WHERE id = ?", retry); agent != otherAgent {
		t.Errorf("retry pin = %d, want other agent %d", agent, otherAgent)
	}
	if p, ok := e.eventPayloadOf(evTaskRestarted); !ok || p.Reason != "output_limit: "+outputLimitText {
		t.Errorf("restart event = %+v (%t), want the kind and the text as reason", p, ok)
	}
	if n := e.countEvents(evTaskEffortBumped) + e.countEvents(evTaskTierBumped); n != 0 {
		t.Errorf("escalation events = %d, want none", n)
	}
	if got := e.str("SELECT COALESCE(effective_effort, '') FROM tasks WHERE id = ?", e.task); got != effortBefore {
		t.Errorf("effective effort = %q, want %q unchanged", got, effortBefore)
	}
	if _, _, cooling := e.agentState(e.agentOf(author)); cooling != "" {
		t.Errorf("the output limit cooled the account until %s, want no cooling", cooling)
	}
}

// With no other agent ready the task parks at needs_foreman, and the
// reason is the cut-off's text.
func TestOutputLimitWorkFailureParksWithItsReason(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "family", "implement")
	e.submit(e.work())
	e.complete(e.leaseAny(author), Completion{Error: outputLimitText, ErrorKind: "output_limit"})
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %s, want needs_foreman", state)
	}
	if n := e.countEvents(evTaskEffortBumped) + e.countEvents(evTaskTierBumped); n != 0 {
		t.Errorf("escalation events = %d, want none", n)
	}
	if p, ok := e.eventPayloadOf(evTaskFailed); !ok || p.Reason != outputLimitText {
		t.Errorf("task_failed = %+v (%t), want reason %q", p, ok, outputLimitText)
	}
}
