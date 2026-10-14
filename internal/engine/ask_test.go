package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const fixtureRepo = "https://example.invalid/brigada.git"

// TestAskWakesAHeadlessTurnWithTheMessageApart: with nobody attached, a
// user message wakes a foreman turn (§4.2 item 3) whose prompt carries
// it in its own <user-message> section, never inside an <event>; the
// history keeps it with author user (§9).
func TestAskWakesAHeadlessTurnWithTheMessageApart(t *testing.T) {
	e := newEnv(t)
	const text = "Amend docs/design.md §5.4: add an optional discoveries field."
	res, err := e.eng.AskForeman(e.ctx, fixtureRepo, text, "c1")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if res.Outcome != WakeWoken || res.ProjectID != e.project || res.EventID == 0 {
		t.Fatalf("ask = %+v, want woken on project %d with its event", res, e.project)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns = %d, want the one the message woke", got)
	}
	prompt := e.str("SELECT prompt FROM jobs WHERE project_id = ? AND kind = 'foreman'", e.project)
	if !strings.Contains(prompt, "<user-message>\n1. from c1:\n"+text+"\n</user-message>") {
		t.Errorf("the turn's prompt does not carry the message in its own section:\n%s", prompt)
	}
	// The template names the event delimiters in prose; an event block
	// of its own opens on a line of its own.
	if strings.Contains(prompt, "\n<event>\n") {
		t.Errorf("the message rode as an event:\n%s", prompt)
	}
	if got := e.str("SELECT author || ':' || kind || ':' || content FROM messages WHERE event_id = ?", res.EventID); got != "user:user_message:"+text {
		t.Errorf("the history's message = %q, want it kept with author user", got)
	}
	if want := []string{fmt.Sprintf("delivered:%d", e.int("SELECT id FROM jobs WHERE kind = 'foreman'"))}; !equalStrings(e.inboxStates(), want) {
		t.Errorf("the inbox = %v, want the message delivered to the turn", e.inboxStates())
	}
}

// TestAskWhileAttachedWaitsForTheSession: an attached chat holds the
// foreman, so the message waits (§4.2 item 3): no turn wakes, the status
// counts it, and the attached foreman's events read returns it apart
// from the events and consumes it.
func TestAskWhileAttachedWaitsForTheSession(t *testing.T) {
	e := newEnv(t)
	hold := e.attach(false)
	res, err := e.eng.AskForeman(e.ctx, "", "check the flaky stall test", "c1")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if res.Outcome != WakeAttached || res.HoldID != hold.HoldID {
		t.Fatalf("ask = %+v, want attached naming hold %d", res, hold.HoldID)
	}
	if got := e.foremanJobs(); got != 0 {
		t.Fatalf("foreman turns while attached = %d, want 0", got)
	}
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil || st.ForemanMessages != 1 {
		t.Fatalf("status counts %d waiting messages (%v), want 1", st.ForemanMessages, err)
	}
	e.park("which file?")
	inbox, err := e.eng.ForemanEvents(e.ctx, e.project)
	if err != nil {
		t.Fatalf("read the inbox: %v", err)
	}
	if len(inbox.UserMessages) != 1 || inbox.UserMessages[0].Text != "check the flaky stall test" || inbox.UserMessages[0].Source != "c1" || len(inbox.Events) != 1 {
		t.Fatalf("inbox = %+v, want the message apart and the park as one event", inbox)
	}
	if st, _ = e.eng.Status(e.ctx, 0); st.ForemanMessages != 0 {
		t.Errorf("waiting messages after the read = %d, want 0", st.ForemanMessages)
	}
	e.releaseAttach(hold.HoldID, "")
}

// TestAskSaysWhyItWaits: the outcome names why a message did not wake a
// turn — a turn already running, the day's budget spent.
func TestAskSaysWhyItWaits(t *testing.T) {
	t.Run("a turn is running", func(t *testing.T) {
		e := newEnv(t)
		if res, err := e.eng.AskForeman(e.ctx, "", "first", "c1"); err != nil || res.Outcome != WakeWoken {
			t.Fatalf("first ask = %+v, %v; want woken", res, err)
		}
		res, err := e.eng.AskForeman(e.ctx, "", "second", "c1")
		if err != nil || res.Outcome != WakeTurnRunning {
			t.Fatalf("second ask = %+v, %v; want turn_running", res, err)
		}
	})
	t.Run("a user message is not charged to the budget", func(t *testing.T) {
		e := newEnvCfg(t, func(c *Config) { c.ForemanWakeBudget = 1 })
		e.park("which file?")
		e.complete(e.leaseForeman(), Completion{Result: "done"})
		e.park("and now?") // the budget is spent: this event waits
		if got := e.foremanJobs(); got != 1 {
			t.Fatalf("foreman turns with the budget spent = %d, want 1", got)
		}
		res, err := e.eng.AskForeman(e.ctx, "", "go on anyway", "c1")
		if err != nil || res.Outcome != WakeWoken {
			t.Fatalf("ask = %+v, %v; want woken past the spent budget", res, err)
		}
	})
}

// TestAskDedupesARepeat: the same words from the same token to the same
// project within the window are one message (§12.1) — a double Enter —
// answered with the first one's id; another token, or the window past,
// sends anew.
func TestAskDedupesARepeat(t *testing.T) {
	e := newEnv(t)
	first, err := e.eng.AskForeman(e.ctx, "", "run the pilot", "c1")
	if err != nil || first.Outcome != WakeWoken {
		t.Fatalf("first ask = %+v, %v", first, err)
	}
	again, err := e.eng.AskForeman(e.ctx, "", "run the pilot", "c1")
	if err != nil || again.Outcome != WakeDuplicate || again.EventID != first.EventID {
		t.Fatalf("repeat = %+v, %v; want a duplicate answering with event %d", again, err, first.EventID)
	}
	if got := e.int("SELECT COUNT(*) FROM foreman_inbox"); got != 1 {
		t.Errorf("inbox rows after a repeat = %d, want 1", got)
	}
	if other, err := e.eng.AskForeman(e.ctx, "", "run the pilot", "c2"); err != nil || other.Outcome == WakeDuplicate {
		t.Errorf("the same words from another token = %+v, %v; want a message of its own", other, err)
	}
	e.clock.Advance(userMessageDedupeWindow + time.Second)
	if later, err := e.eng.AskForeman(e.ctx, "", "run the pilot", "c1"); err != nil || later.Outcome == WakeDuplicate {
		t.Errorf("the same words after the window = %+v, %v; want a message of its own", later, err)
	}
}

// TestAskRefusals: an empty or oversized message, an unknown repo, and
// an unnamed project among several are refused with the engine's
// sentinel errors, and nothing lands in the inbox.
func TestAskRefusals(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, repo, text string
		want             error
	}{
		{"empty", "", "  \n", ErrInvalidMessage},
		{"oversized", "", strings.Repeat("x", UserMessageLimit+1), ErrInvalidMessage},
		{"unknown repo", "https://example.invalid/other.git", "hi", ErrNoProject},
	}
	for _, tc := range cases {
		if _, err := e.eng.AskForeman(e.ctx, tc.repo, tc.text, "c1"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	e.addProject("https://example.invalid/second.git")
	if _, err := e.eng.AskForeman(e.ctx, "", "which project?", "c1"); !errors.Is(err, ErrNoProject) {
		t.Errorf("unnamed project among two: err = %v, want ErrNoProject", err)
	}
	if got := e.int("SELECT COUNT(*) FROM foreman_inbox"); got != 0 {
		t.Errorf("inbox rows after refusals = %d, want 0", got)
	}
}
