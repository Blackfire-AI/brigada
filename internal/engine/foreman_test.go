package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/foreman"
)

// wakeEvents are the judgment events a test delivers (§3): a worker's
// question and a stalled task, the two that always go to the foreman.
var wakeEvents = []string{
	"the worker of task 7 asked: should I split the CLI from the parser?",
	"task 9 stalled twice; its session is gone",
}

func TestSubmitForemanTurn(t *testing.T) {
	e := newEnv(t)

	r, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
	if err != nil {
		t.Fatalf("submit foreman turn: %v", err)
	}
	if r.Answer != AnsCreated || r.Run != 1 {
		t.Fatalf("submit = %+v; want created run 1", r)
	}
	for _, tc := range []struct{ column, want string }{
		{"kind", string(KindForeman)},
		{"key", fmt.Sprintf("foreman:%d", e.project)},
		{"mode", string(ModeSession)},
		{"harness", "claude"},
		{"status", statusQueued},
	} {
		if got := e.str("SELECT "+tc.column+" FROM jobs WHERE id = ?", r.JobID); got != tc.want {
			t.Errorf("job %s = %q, want %q", tc.column, got, tc.want)
		}
	}
	if n := e.int("SELECT COALESCE(task_id, 0) FROM jobs WHERE id = ?", r.JobID); n != 0 {
		t.Errorf("the turn's task = %d, want none: a foreman turn belongs to no task (§11)", n)
	}

	// The prompt is the instructions plus the wake template plus the
	// events verbatim (§1.4, §4.2 item 2) — composed only from the
	// versioned files and the data given.
	prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", r.JobID)
	if want := foreman.Instructions() + "\n" + foreman.ComposeTurnPrompt(wakeEvents); prompt != want {
		t.Errorf("the turn's prompt differs from instructions + wake:\n got %q\nwant %q", prompt, want)
	}
	if !strings.HasPrefix(prompt, foreman.Instructions()) {
		t.Errorf("the turn's prompt does not start with the instructions")
	}
	for _, ev := range wakeEvents {
		if !strings.Contains(prompt, "<event>\n"+ev+"\n</event>") {
			t.Errorf("an event is not verbatim in the prompt")
		}
	}
}

func TestSubmitForemanTurnNeedsHarness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		model   string
	}{
		{"no harness", "", "sonnet"},
		{"no model", "claude", ""},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.exec("UPDATE projects SET foreman_harness = ?, foreman_model = ? WHERE id = ?",
				tc.harness, tc.model, e.project)

			_, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents)
			if err == nil {
				t.Fatal("a turn for a project with no foreman succeeded; want refusal")
			}
			for _, want := range []string{
				"the project has no foreman harness",
				"`brigada project foreman`",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to name %q", err.Error(), want)
				}
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = ?", string(KindForeman)); n != 0 {
				t.Errorf("foreman jobs = %d, want 0: the refusal created nothing", n)
			}
		})
	}

	// A project that does not exist is its own error, not the harness's.
	e := newEnv(t)
	if _, err := e.eng.SubmitForemanTurn(e.ctx, e.project+1, wakeEvents); err == nil || !strings.Contains(err.Error(), "no such project") {
		t.Errorf("turn for a missing project = %v, want no such project", err)
	}
}

// TestSubmitForemanTurnCoalesces covers §4.2 item 2's coalescing: while
// a turn holds the key, a second wake creates nothing — the live turn's
// follow-up flag is set, and both wakes wait on the one job.
func TestSubmitForemanTurnCoalesces(t *testing.T) {
	e := newEnv(t)

	first, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents[:1])
	if err != nil {
		t.Fatalf("first wake: %v", err)
	}
	if first.Answer != AnsCreated {
		t.Fatalf("first wake = %s; want created", first.Answer)
	}
	g := e.lease(e.runner1, []string{"claude"}, nil)
	if g == nil || g.JobID != first.JobID {
		t.Fatal("the lease did not take the turn")
	}

	second, err := e.eng.SubmitForemanTurn(e.ctx, e.project, wakeEvents[1:])
	if err != nil {
		t.Fatalf("second wake: %v", err)
	}
	if second.Answer != AnsPending || second.JobID != first.JobID {
		t.Fatalf("second wake = %+v; want pending behind job %d", second, first.JobID)
	}
	key := fmt.Sprintf("foreman:%d", e.project)
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ?", key); n != 1 {
		t.Errorf("jobs for %s = %d, want 1: a wake behind a live turn creates nothing", key, n)
	}
	if n := e.int("SELECT followup FROM jobs WHERE id = ?", first.JobID); n != 1 {
		t.Errorf("live turn followup = %d, want 1: the second wake set the flag", n)
	}

	// The key free again, the next wake is a new turn on the same key.
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Fatalf("complete the turn: %s", ans)
	}
	third, err := e.eng.SubmitForemanTurn(e.ctx, e.project, nil)
	if err != nil {
		t.Fatalf("wake after the key freed: %v", err)
	}
	if third.Answer != AnsCreated || third.Run != 2 || third.JobID == first.JobID {
		t.Fatalf("wake after the key freed = %+v; want created run 2 on a new job", third)
	}
	if n := e.int("SELECT followup FROM jobs WHERE id = ?", third.JobID); n != 0 {
		t.Errorf("new turn followup = %d, want 0", n)
	}
}
