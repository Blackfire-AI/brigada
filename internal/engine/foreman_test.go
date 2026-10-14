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

// TestForemanTurnReportsToTheUser covers the report a foreman turn
// leaves behind (§3, §10): a done turn's final message becomes the
// project's foreman_report event — the text a notification and
// `brigada status` read, bounded like any event's words and carrying no
// prompt — while an empty final message, a failed turn and a stalled
// one report nothing: their words were not an answer for the user, and
// the stalled or failed turn's events return to pending instead (§4.2).
func TestForemanTurnReportsToTheUser(t *testing.T) {
	e := newEnv(t)

	turn := func(c Completion) {
		t.Helper()
		r := e.submit(e.foremanTurn())
		g := e.lease(e.runner1, []string{"claude"}, nil)
		if g == nil || g.JobID != r.JobID {
			t.Fatalf("the foreman turn was not leased: %+v, want job %d", g, r.JobID)
		}
		if ans := e.complete(g, c); ans != AnsOK {
			t.Fatalf("complete the foreman turn: %s", ans)
		}
	}
	reports := func() int64 {
		return e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evForemanReport)
	}

	// A done turn's final message is the report, verbatim, under the
	// turn's own job and no task — a foreman turn belongs to none (§4).
	turn(Completion{Result: `{"status":"done","summary":"woke for the question of task 7; answered it\nnothing needs you"}`})
	want := `{"report":"woke for the question of task 7; answered it\nnothing needs you"}`
	if got := e.str("SELECT payload FROM events WHERE kind = ?", evForemanReport); got != want {
		t.Errorf("the report payload = %s, want the turn's final message alone:\n%s", got, want)
	}
	if job := e.int("SELECT COALESCE(job_id, 0) FROM events WHERE kind = ?", evForemanReport); job == 0 {
		t.Error("the report event names no job")
	}
	if task := e.int("SELECT COALESCE(task_id, 0) FROM events WHERE kind = ?", evForemanReport); task != 0 {
		t.Errorf("the report event names task %d, want none: a foreman turn belongs to no task", task)
	}
	if n := e.int(`SELECT COUNT(*) FROM foreman_inbox
		WHERE event_id IN (SELECT id FROM events WHERE kind = ?)`, evForemanReport); n != 0 {
		t.Error("the report landed in the foreman inbox; it is the user's to read, not the foreman's to judge")
	}

	// An empty final message, a failed turn and a stalled one report
	// nothing; only the done turn's report is in the log.
	turn(Completion{Result: `{"status":"done"}`})
	turn(Completion{Error: "the foreman CLI died"})
	turn(Completion{Result: `{"status":"stalled","summary":"a partial answer"}`, Outcome: OutcomeStalled})
	if got := reports(); got != 1 {
		t.Errorf("foreman_report events = %d, want the one done turn's alone", got)
	}

	// A report longer than any event's words is bounded where it lands.
	turn(Completion{Result: `{"status":"done","summary":"` + strings.Repeat("long ", 200) + `"}`})
	if got := reports(); got != 2 {
		t.Fatalf("foreman_report events = %d, want 2", got)
	}
	if payload := e.str("SELECT payload FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1", evForemanReport); len(payload) > 512+len(`{"report":""}`) {
		t.Errorf("the long report's payload runs %d bytes, want it bounded near 500", len(payload))
	}
}
