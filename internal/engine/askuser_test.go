package engine

import (
	"errors"
	"testing"
)

// TestAskUserIsDurableWithoutAHold: a foreman question needs no hold of
// any kind — an attached chat and a headless turn ask the same way — and
// it never wakes a competing headless turn.
func TestAskUserIsDurableWithoutAHold(t *testing.T) {
	e := newEnv(t)
	id, err := e.eng.AskUser(e.ctx, e.project, "Which option should we use?")
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
	for _, text := range []string{"", "   "} {
		if _, err := e.eng.AskUser(e.ctx, e.project, text); !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("invalid question err=%v", err)
		}
	}
}
