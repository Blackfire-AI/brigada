package engine

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// parkAtNeedsHelp drives the fixture's task to a needs_help park and
// returns nothing: the work ran on the author's runner, ended asking
// its question, and the task waits for the foreman.
func (e *env) parkAtNeedsHelp(author int64, question string) {
	e.t.Helper()
	e.submit(e.work())
	g := e.authorLease(author)
	if g == nil {
		e.t.Fatalf("no work job to lease; have %v", e.eventKinds())
	}
	c := Completion{
		Result:    `{"status":"needs_help","summary":"I am stuck choosing","question":"` + question + `"}`,
		SessionID: "hs-author-1",
	}
	if ans := e.complete(g, c); ans != AnsOK {
		e.t.Fatalf("complete the asking work: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		e.t.Fatalf("task state after the ask = %q, want needs_foreman", state)
	}
}

// TestAnswerResumesTheAuthorsSession covers the answer end to end (§3):
// a task parked by its worker's question is answered — the question and
// the answer kept together in questions (§11), a fix job resuming the
// author's session on the author's agent queued with the answer
// verbatim in its own delimiters and the worker's own summary nowhere
// in it, no round spent (§7), the task back to ready and the
// task_answered event naming the questions row.
func TestAnswerResumesTheAuthorsSession(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.parkAtNeedsHelp(author, "which API should the client call?")

	res, err := e.eng.Answer(e.ctx, e.project, e.task, "Use the REST API over the Unix socket.")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if res.Answer != AnsCreated || res.QuestionID == 0 || res.JobID == 0 {
		t.Fatalf("answer result = %+v, want created with a question row and a job", res)
	}

	// The questions row holds the exchange (§11): the worker's question
	// as the parking event kept it, the foreman's answer as given.
	if question := e.str("SELECT question FROM questions WHERE id = ?", res.QuestionID); question != "which API should the client call?" {
		t.Errorf("questions.question = %q, want the worker's", question)
	}
	if answer := e.str("SELECT answer FROM questions WHERE id = ?", res.QuestionID); answer != "Use the REST API over the Unix socket." {
		t.Errorf("questions.answer = %q, want the foreman's, verbatim", answer)
	}
	if e.str("SELECT answered_at FROM questions WHERE id = ?", res.QuestionID) == "" {
		t.Error("questions.answered_at is empty")
	}

	// The job: a fix job, session mode, the author's placement (§3).
	fix := res.JobID
	for _, check := range []struct {
		query string
		want  string
		what  string
	}{
		{`SELECT kind FROM jobs WHERE id = ?`, string(KindFix), "kind"},
		{`SELECT mode FROM jobs WHERE id = ?`, string(ModeSession), "mode"},
		{`SELECT harness FROM jobs WHERE id = ?`, "author", "harness"},
		{`SELECT key FROM jobs WHERE id = ?`, keyOf(e.task), "key"},
		{`SELECT round FROM jobs WHERE id = ?`, "", "round: the answer spends none"},
	} {
		if got := e.str(check.query, fix); got != check.want {
			t.Errorf("answer job %s = %q, want %q", check.what, got, check.want)
		}
	}
	if pin := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", fix); pin != e.agentOf(author) {
		t.Errorf("answer job require_agent = %d, want the author's %d", pin, e.agentOf(author))
	}
	// The prompt is the engine's own composition (§1.4, §5.3): the worker
	// template, the spec, the answer verbatim in its own delimiters,
	// introduced as the foreman's answer — and the worker's summary, which
	// the result block also carried, is no input of it at all.
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", fix),
		prompt.ComposeAnswerPrompt("the spec", "go test ./...", "Use the REST API over the Unix socket."); got != want {
		t.Errorf("answer prompt = %q\nwant the composed\n%q", got, want)
	}
	if strings.Contains(e.str("SELECT prompt FROM jobs WHERE id = ?", fix), "I am stuck choosing") {
		t.Error("the answer prompt carries the worker's summary")
	}

	// The task is ready again, and the event names the questions row and
	// no prompt (§9).
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
		t.Errorf("task state after the answer = %q, want ready", state)
	}
	if p, ok := e.eventPayloadOf(evTaskAnswered); !ok || p.QuestionID != res.QuestionID {
		t.Errorf("task_answered payload = %+v (%t), want the questions row %d", p, ok, res.QuestionID)
	}

	// The author — not anyone else — resumes, from the session the asking
	// run ended on (§4.2, §8).
	fg := e.authorLease(author)
	if fg == nil || fg.JobID != fix {
		t.Fatalf("the author's runner leased %+v, want the answer job %d", fg, fix)
	}
	if fg.Mode != ModeSession || fg.ResumeSessionID != "hs-author-1" {
		t.Errorf("answer grant mode/resume = %s/%q, want session resuming the author's", fg.Mode, fg.ResumeSessionID)
	}
}

// TestAnswerSpendsNoFixRound: the answer's resume is no fix round (§3,
// §7): a task whose fix_rounds budget is already spent can still be
// answered — the answer's job clears the cap — and the round the answer
// would otherwise have spent is still there for the next failed check,
// while the check after the answer's job finds the budget spent exactly
// by the one genuine round that ran.
func TestAnswerSpendsNoFixRound(t *testing.T) {
	e := newEnvCfg(t, nil)
	e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE id = ?", e.task)
	e.exec("UPDATE tasks SET caps = ? WHERE id = ?", `{"fix_rounds":1}`, e.task)
	author := e.agented("author", "anthropic", "implement")

	// Burn the one fix round: the work fails its check, the round runs.
	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, doneOn("task/1-1", failCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}
	// The round itself ends asking: the budget is spent and the question
	// is open at once.
	if ans := e.complete(e.authorLease(author), Completion{
		Result: `{"status":"needs_help","summary":"stuck","question":"which way now?"}`,
	}); ans != AnsOK {
		t.Fatalf("complete the round's ask: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want needs_foreman with the budget spent", state)
	}

	// The answer still queues its job: roundNone clears the caps a spent
	// fix_rounds would otherwise enforce on a fix submit.
	res, err := e.eng.Answer(e.ctx, e.project, e.task, "This way.")
	if err != nil {
		t.Fatalf("answer past the fix budget: %v", err)
	}
	if round := e.str("SELECT round FROM jobs WHERE id = ?", res.JobID); round != "" {
		t.Errorf("answer job round = %q, want none spent", round)
	}
	// The next failed check finds the budget spent by the one round that
	// ran — the answer's resume never counted against it (§6.1).
	if ans := e.complete(e.authorLease(author), doneOn("task/1-2", failCheck)); ans != AnsOK {
		t.Fatalf("complete the answer's job: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state past the spent budget = %q, want needs_foreman", state)
	}
	if _, ok := e.eventPayloadOf(evTaskOutOfRounds); !ok {
		t.Errorf("no task_out_of_rounds event; have %v", e.eventKinds())
	}
}

// TestAnswerRefusals: the answer refuses a task that is not there, a
// task of another project, a task that is not parked, a task parked by
// anything but its worker's question — answering a failed check makes
// no sense; restart it instead — and an empty answer text. Every
// refusal leaves the store untouched.
func TestAnswerRefusals(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")

	if _, err := e.eng.Answer(e.ctx, e.project, 999, "nope"); err == nil {
		t.Error("answering a task that is not there succeeded; want refusal")
	}

	// A task of another project: the project named by the call is the wall.
	other := e.addTask("{}")
	if _, err := e.eng.Answer(e.ctx, e.project+1, other, "nope"); err == nil ||
		!strings.Contains(err.Error(), "belongs to project") {
		t.Errorf("answering another project's task = %v, want the project refusal", err)
	}

	if _, err := e.eng.Answer(e.ctx, e.project, e.task, "not parked yet"); err == nil ||
		!strings.Contains(err.Error(), "not parked") {
		t.Errorf("answering a ready task = %v, want the not-parked refusal", err)
	}

	// A check-failed park: the class keeps the rules from queueing a fix
	// round, the task parks under task_check_failed — the foreman's way
	// with it is a restart, not an answer.
	e.exec("UPDATE tasks SET class = 'answer' WHERE id = ?", e.task)
	e.submit(e.work())
	g := e.leaseAny(author)
	if ans := e.complete(g, doneOn("task/1-1", failCheck)); ans != AnsOK {
		t.Fatalf("complete the failed-check work: %s", ans)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state = %q, want parked under the failed check", state)
	}
	_, err := e.eng.Answer(e.ctx, e.project, e.task, "try again")
	if err == nil || !strings.Contains(err.Error(), "restart it instead") {
		t.Errorf("answering a check-failed park = %v, want the restart-it refusal", err)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state after the refusal = %q, want still parked", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND status = 'queued'", e.task); n != 0 {
		t.Errorf("queued jobs after the refusal = %d, want none", n)
	}
	if n := e.int("SELECT COUNT(*) FROM questions"); n != 0 {
		t.Errorf("questions rows after the refusal = %d, want none", n)
	}

	if _, err := e.eng.Answer(e.ctx, e.project, e.task, ""); err == nil {
		t.Error("an empty answer succeeded; want refusal")
	}
}

// TestRestartTaskFreshAttempt covers the restart (§3, §5.6): a task
// parked out of rounds is restarted with a note — a fresh work job,
// fresh mode, flagged restart, no placement pin so any eligible agent
// may take it, the note verbatim as the worker's feedback — the task
// back to ready with a task_restarted event, and the fresh attempt's
// fix rounds counted from zero behind the restart (§6.1).
func TestRestartTaskFreshAttempt(t *testing.T) {
	e := newEnv(t)
	// Heavy tasks retain the park path; tier escalation has its own coverage.
	e.exec("UPDATE tasks SET effective_tier = 'heavy', effective_effort = 'high' WHERE id = ?", e.task)
	author := e.agented("author", "anthropic", "implement")
	stranger := e.agented("stranger", "meta", "implement")

	// Burn the fix rounds: two failed checks, two rounds, the third ask
	// parks the task out of rounds (§7).
	e.submit(e.work())
	for i := 0; i < 3; i++ {
		if ans := e.complete(e.authorLease(author), doneOn("task/1-1", failCheck)); ans != AnsOK {
			t.Fatalf("author cycle %d: %s", i, ans)
		}
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Fatalf("task state past the rounds = %q, want needs_foreman", state)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix' AND round <> ''", e.task); n != 2 {
		t.Fatalf("spent fix rounds = %d, want the two the caps allowed", n)
	}

	note := "the last attempt failed because the frobnicator was off; avoid touching it"
	res, err := e.eng.RestartTask(e.ctx, e.project, e.task, note)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if res.Answer != AnsCreated || res.JobID == 0 {
		t.Fatalf("restart result = %+v, want created with a job", res)
	}
	work := res.JobID
	if got, want := e.str("SELECT kind FROM jobs WHERE id = ?", work), string(KindWork); got != want {
		t.Errorf("restart job kind = %q, want %q", got, want)
	}
	if got, want := e.str("SELECT mode FROM jobs WHERE id = ?", work), string(ModeFresh); got != want {
		t.Errorf("restart job mode = %q, want fresh: a new attempt from the base", got)
	}
	if restart := e.int("SELECT restart FROM jobs WHERE id = ?", work); restart != 1 {
		t.Errorf("restart job restart flag = %d, want 1", restart)
	}
	if pin := e.int("SELECT COALESCE(require_agent, 0) FROM jobs WHERE id = ?", work); pin != 0 {
		t.Errorf("restart job require_agent = %d, want no pin: a restart may switch agents", pin)
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", work),
		prompt.ComposeWorkerPrompt("the spec", "go test ./...", note); got != want {
		t.Errorf("restart prompt = %q\nwant the composed\n%q", got, want)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
		t.Errorf("task state after the restart = %q, want ready", state)
	}
	if p, ok := e.eventPayloadOf(evTaskRestarted); !ok || p.Run != int64(res.Run) {
		t.Errorf("task_restarted payload = %+v (%t), want the new attempt's run %d", p, ok, res.Run)
	}

	// Any eligible agent may take the fresh attempt — the author's pin
	// died with the attempt that earned it.
	g := e.leaseAny(stranger)
	if g == nil || g.JobID != work {
		t.Fatalf("the stranger leased %+v, want the restart job %d", g, work)
	}

	// The fresh attempt's rounds are counted afresh (§6.1): the check
	// fails again and a fix round still fits, though the task spent its
	// two behind the restart.
	if ans := e.complete(g, doneOn("task/1-1", failCheck)); ans != AnsOK {
		t.Fatalf("complete the restarted work: %s", ans)
	}
	if fix := e.fixJob(); e.str("SELECT round FROM jobs WHERE id = ?", fix) != roundFix {
		t.Errorf("the fresh attempt's fix round spent nothing")
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskRunning {
		t.Errorf("task state under the fresh round = %q, want running", state)
	}
}

// TestRestartTaskWithoutNote: an empty note composes no feedback
// section — the fresh attempt's prompt is the plain worker prompt.
func TestRestartTaskWithoutNote(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.parkAtNeedsHelp(author, "sources?")

	res, err := e.eng.RestartTask(e.ctx, e.project, e.task, "")
	if err != nil {
		t.Fatalf("restart without a note: %v", err)
	}
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", res.JobID),
		prompt.ComposeWorkerPrompt("the spec", "go test ./...", ""); got != want {
		t.Errorf("restart prompt = %q, want the plain worker prompt\n%q", got, want)
	}
}

// TestRestartTaskBudgetSpent: with the restart budget spent the call
// refuses plainly and the task stays parked — no job, no event beyond
// the parking the log already holds.
func TestRestartTaskBudgetSpent(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.RoundCaps.Restart = 1 })
	author := e.agented("author", "anthropic", "implement")
	e.parkAtNeedsHelp(author, "sources?")

	// The one restart the cap allows, run to its own needs_help park.
	first, err := e.eng.RestartTask(e.ctx, e.project, e.task, "again")
	if err != nil {
		t.Fatalf("the first restart: %v", err)
	}
	if ans := e.complete(e.leaseAny(author), Completion{
		Result: `{"status":"needs_help","summary":"stuck","question":"still?"}`,
	}); ans != AnsOK {
		t.Fatalf("complete the restarted work: %s", ans)
	}

	_, err = e.eng.RestartTask(e.ctx, e.project, e.task, "once more")
	if err == nil || !strings.Contains(err.Error(), "restart budget") {
		t.Errorf("restart past the budget = %v, want the budget named", err)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state after the refusal = %q, want still parked", state)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ?", e.task); got != 2 {
		t.Errorf("task jobs after the refusal = %d, want 2 (the asking work and the one restart)", got)
	}
	if first.JobID == 0 {
		t.Error("the first restart created no job")
	}
	if _, ok := e.eventPayloadOf(evJobCapped); ok {
		t.Error("the refusal logged a capped submit; want the plain refusal")
	}
}

// TestRestartTaskRefusals: like the answer, the restart refuses a task
// that is not there, a task of another project, and a task that is not
// parked — any parker will do, but no other state.
func TestRestartTaskRefusals(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.RestartTask(e.ctx, e.project, 999, "nope"); err == nil {
		t.Error("restarting a task that is not there succeeded; want refusal")
	}
	if _, err := e.eng.RestartTask(e.ctx, e.project, e.task, "not parked"); err == nil ||
		!strings.Contains(err.Error(), "not parked") {
		t.Errorf("restarting a ready task = %v, want the not-parked refusal", err)
	}

	// A task of another project: parked or not, the project named by the
	// call is the wall.
	other := e.addTask("{}")
	if _, err := e.eng.RestartTask(e.ctx, e.project+1, other, "nope"); err == nil ||
		!strings.Contains(err.Error(), "belongs to project") {
		t.Errorf("restarting another project's task = %v, want the project refusal", err)
	}
}
