package engine

import (
	"errors"
	"testing"
)

func TestInteractiveAskUserIsDurableAndFenced(t *testing.T) {
	e := newEnv(t)
	h := e.acquire("interactive-bridge", "working session")
	ctx := WithHoldGuard(e.ctx, &HoldGuard{Hold: h.HoldID, Fence: h.Fence})
	id, err := e.eng.AskUser(ctx, e.project, "Which option should we use?")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.str("SELECT question FROM questions WHERE id=?", id); got != "Which option should we use?" {
		t.Fatal(got)
	}
	if got := e.str("SELECT author FROM messages WHERE kind='foreman_question'"); got != "foreman" {
		t.Fatal(got)
	}
	if e.foremanJobs() != 0 {
		t.Fatal("a question must not create a competing headless wake")
	}
	if err := e.eng.YieldHold(e.ctx, h.HoldID, h.Fence); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.AskUser(ctx, e.project, "stale question"); !errors.Is(err, ErrStaleHold) {
		t.Fatalf("stale question err=%v", err)
	}
	if n := e.str("SELECT COUNT(*) FROM questions"); n != "1" {
		t.Fatalf("stale question applied: %s", n)
	}
	for _, text := range []string{"", "   "} {
		if _, err := e.eng.AskUser(e.ctx, e.project, text); !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("invalid question err=%v", err)
		}
	}
}
