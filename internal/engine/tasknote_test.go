package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// setNote sets a task's pending note the way an amendment does, for
// fixtures whose task belongs to no plan.
func (e *env) setNote(task int64, note string) {
	e.t.Helper()
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		return setTaskNoteTx(tx, e.project, task, note, "foreman", e.eng.cfg.Now())
	}); err != nil {
		e.t.Fatalf("set note: %v", err)
	}
}

func (e *env) note(task int64) string {
	e.t.Helper()
	return e.str("SELECT note FROM tasks WHERE id = ?", task)
}

const noteBlockText = "<task-note>\nuse the v2 client\n</task-note>"

// TestAmendNoteSetReplaceClear: a note is set, replaced and cleared
// through plan_amend, each recorded as a task_note_set event with the
// note verbatim and the caller; status shows it; a settled task refuses
// it, naming the state, and parked and running ones take it.
func TestAmendNoteSetReplaceClear(t *testing.T) {
	e := newEnv(t)
	res := e.twoStep()
	build, ship := res.Tasks[0], res.Tasks[1]

	am := e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "ship", Note: "use the v2 client"}}, Caller: "foreman"})
	if len(am.Noted) != 1 || am.Noted[0] != ship.TaskID {
		t.Fatalf("amendment = %+v, want ship noted", am)
	}
	if got := e.note(ship.TaskID); got != "use the v2 client" {
		t.Errorf("note = %q", got)
	}
	if p, ok := e.eventPayloadOf(evTaskNoteSet); !ok || p.Note != "use the v2 client" || p.Caller != "foreman" {
		t.Errorf("task_note_set payload = %+v (%t)", p, ok)
	}
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, p := range st.Plans {
		for _, tk := range p.Tasks {
			if tk.ID == ship.TaskID && tk.Note == "use the v2 client" {
				seen = true
			}
		}
	}
	if !seen {
		t.Errorf("status does not show the pending note")
	}

	e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "ship", Note: "use the v3 client"}}, Caller: "user"})
	if got := e.note(ship.TaskID); got != "use the v3 client" {
		t.Errorf("replaced note = %q", got)
	}
	e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "ship", Note: ""}}, Caller: "user"})
	if got := e.int("SELECT note IS NULL FROM tasks WHERE id = ?", ship.TaskID); got != 1 {
		t.Errorf("cleared note is not NULL")
	}

	// Running (build has a live job) takes a note.
	e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "build", Note: "x"}}})
	for _, state := range []string{taskNeedsForeman, taskFailed} {
		e.exec("UPDATE tasks SET state = ? WHERE id = ?", state, build.TaskID)
		e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "build", Note: "while " + state}}})
	}
	if got := e.note(build.TaskID); got != "while "+taskFailed {
		t.Errorf("note = %q", got)
	}
	for _, state := range []string{taskDone, taskCancelled, taskRetired} {
		e := newEnv(t)
		res := e.twoStep()
		e.exec("UPDATE tasks SET state = ? WHERE id = ?", state, res.Tasks[0].TaskID)
		e.exec("UPDATE plans SET state = 'active' WHERE id = ?", res.PlanID)
		_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "build", Note: "late"}}})
		if err == nil || !strings.Contains(err.Error(), "is "+state) {
			t.Errorf("note on a %s task: err = %v, want the state named", state, err)
		}
		if got := e.note(res.Tasks[0].TaskID); got != "" {
			t.Errorf("a refused amendment set the note: %q", got)
		}
	}
}

// TestNoteReachesDependencyStartAndContinuation: a note set on a waiting
// task rides the work job its dependency's completion queues — the plain
// start and the continuation's own composer — and clears at its lease.
func TestNoteReachesDependencyStartAndContinuation(t *testing.T) {
	for _, cont := range []bool{false, true} {
		e := newEnv(t)
		author := e.agented("author", "claude", "implement")
		var res CreatePlanResult
		if cont {
			res = e.continuePlan("answer")
		} else {
			res = e.twoStep()
		}
		ship := res.Tasks[1].TaskID
		e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "ship", Note: "use the v2 client"}}, Caller: "foreman"})
		e.runBuild(author, "hs-build-1")
		job := e.shipJob(res)
		got := e.str("SELECT prompt FROM jobs WHERE id = ?", job)
		var want string
		if cont {
			want = prompt.ComposeContinuePrompt("ship it", "", prompt.WithTaskNote("use the v2 client"))
		} else {
			want = prompt.ComposeWorkerPrompt("ship it", "", "", prompt.WithTaskNote("use the v2 client"))
		}
		if got != want || !strings.Contains(got, noteBlockText) {
			t.Errorf("continuation=%t: ship's prompt = %q\nwant %q", cont, got, want)
		}
		if e.note(ship) == "" {
			t.Errorf("continuation=%t: the note cleared before the job leased", cont)
		}
		if g := e.lease(author, []string{"author"}, nil); g == nil || g.JobID != job {
			t.Fatalf("continuation=%t: lease = %+v, want job %d", cont, g, job)
		}
		if got := e.note(ship); got != "" {
			t.Errorf("continuation=%t: the note %q survived the lease", cont, got)
		}
	}
}

// TestNoteReachesFixRestartAndAnswer: the fix round, the restart (with
// the restart note after the pending one) and the foreman-answer job
// each carry the pending note.
func TestNoteReachesFixRestartAndAnswer(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	e.submit(e.work())
	g := e.authorLease(author)
	e.setNote(e.task, "use the v2 client")
	if ans := e.complete(g, doneOn("task/1-1", failCheck)); ans != AnsOK {
		t.Fatalf("complete: %s", ans)
	}
	fix := e.str("SELECT prompt FROM jobs WHERE id = ?", e.fixJob())
	if !strings.Contains(fix, noteBlockText) || strings.Index(fix, "<task-note>") > strings.Index(fix, "<feedback>") {
		t.Errorf("fix prompt lacks the note before the feedback:\n%s", fix)
	}

	e2 := newEnv(t)
	author = e2.agented("author", "anthropic", "implement")
	e2.parkAtNeedsHelp(author, "sources?")
	e2.setNote(e2.task, "use the v2 client")
	ans, err := e2.eng.Answer(e2.ctx, e2.project, e2.task, "the REST API")
	if err != nil {
		t.Fatal(err)
	}
	if got := e2.str("SELECT prompt FROM jobs WHERE id = ?", ans.JobID); !strings.Contains(got, noteBlockText) || !strings.Contains(got, "the REST API") {
		t.Errorf("answer prompt lacks the note:\n%s", got)
	}

	e3 := newEnv(t)
	author = e3.agented("author", "anthropic", "implement")
	e3.parkAtNeedsHelp(author, "sources?")
	e3.setNote(e3.task, "use the v2 client")
	rs, err := e3.eng.RestartTask(e3.ctx, e3.project, e3.task, "start over")
	if err != nil {
		t.Fatal(err)
	}
	got := e3.str("SELECT prompt FROM jobs WHERE id = ?", rs.JobID)
	if want := prompt.ComposeWorkerPrompt("the spec", "go test ./...", "start over", prompt.WithTaskNote("use the v2 client")); got != want {
		t.Errorf("restart prompt = %q\nwant %q", got, want)
	}
	if strings.Index(got, "<task-note>") > strings.Index(got, "<feedback>") {
		t.Errorf("the pending note comes after the restart note")
	}
}

// TestNoteQueuedJobRewrittenThenLeased: a note set while a work job is
// queued is spliced into that job's stored prompt, a replacement and a
// clear rewrite it again, and the note leaves the task at the lease. A
// queued job cancelled first leaves the note pending.
func TestNoteQueuedJobRewrittenThenLeased(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "claude", "implement")
	res := e.twoStep()
	build := res.Tasks[0]
	plain := e.str("SELECT prompt FROM jobs WHERE id = ?", build.JobID)
	meta := e.str("SELECT prompt_meta FROM jobs WHERE id = ?", build.JobID)

	e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "build", Note: "first"}}})
	if got, want := e.str("SELECT prompt FROM jobs WHERE id = ?", build.JobID), prompt.InsertTaskNote(plain, "first"); got != want || !strings.Contains(got, "<task-note>\nfirst\n</task-note>") {
		t.Fatalf("queued prompt = %q, want the note spliced in", got)
	}
	e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "build", Note: "second"}}})
	got := e.str("SELECT prompt FROM jobs WHERE id = ?", build.JobID)
	if strings.Contains(got, "first") || !strings.Contains(got, "<task-note>\nsecond\n</task-note>") || strings.Count(got, "<task-note>") != 1 {
		t.Errorf("replaced prompt = %q", got)
	}
	e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "build", Note: ""}}})
	if got := e.str("SELECT prompt FROM jobs WHERE id = ?", build.JobID); got != plain {
		t.Errorf("cleared prompt = %q, want the plain one", got)
	}
	e.amend(res.PlanID, PlanAmendment{Note: []PlanNote{{Task: "build", Note: "third"}}})
	if m := e.str("SELECT prompt_meta FROM jobs WHERE id = ?", build.JobID); m != meta {
		t.Errorf("prompt_meta changed: %q -> %q", meta, m)
	}
	g := e.lease(author, []string{"author"}, nil)
	if g == nil || g.JobID != build.JobID || !strings.Contains(g.Prompt, "<task-note>\nthird\n</task-note>") {
		t.Fatalf("grant = %+v, want the job with the note in its prompt", g)
	}
	if got := e.note(build.TaskID); got != "" {
		t.Errorf("note %q survived its first lease", got)
	}

	// Cancelled while queued: the note stays pending for the next job.
	e2 := newEnv(t)
	res2 := e2.twoStep()
	e2.amend(res2.PlanID, PlanAmendment{Note: []PlanNote{{Task: "build", Note: "keep"}}})
	e2.exec("UPDATE jobs SET status = 'cancelled' WHERE id = ?", res2.Tasks[0].JobID)
	if got := e2.note(res2.Tasks[0].TaskID); got != "keep" {
		t.Errorf("note after the queued job was dropped = %q, want it pending", got)
	}
}
