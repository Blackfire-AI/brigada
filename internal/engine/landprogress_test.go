package engine

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Batch progress and completion accounting (§7.1 "A lost runner resumes
// by remote evidence", acceptance at "Gate and setting acceptance").

// progressEntries encodes progress entries as the runner sends them.
func progressEntries(t *testing.T, entries ...any) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// intentEntry is a survivor's pre-push progress.
func intentEntry(task int64, commit string, pos int) landProgressEntry {
	return landProgressEntry{landMemberResult: landMemberResult{TaskID: task}, ApprovedHead: "h", BaseRef: "main",
		Commit: commit, CheckPassed: true, Intent: "target", Position: pos}
}

// TestLandProgressSurvivesALostLease: the parking, the skip and the
// pre-push intents a run reported before it lost its runner survive the
// lost lease. The next lease parks the parked member with its own failure,
// leaves the skipped dependent waiting with no landing record, and grants
// only the members still to land; a terminal lost-runner failure then
// requeues the members whose push may have published instead of parking
// them over it, and records no landing for any member.
func TestLandProgressSurvivesALostLease(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, tasks[2], tasks[1])
	entries := progressEntries(t,
		landMemberResult{TaskID: tasks[1], Outcome: landMemberParked,
			Record: &landRecord{TargetBranch: "main", Commit: "c0", ConflictFiles: []string{"README.md"}}},
		landMemberResult{TaskID: tasks[2], Outcome: landMemberDropped, BlockedBy: tasks[1]},
		intentEntry(tasks[0], "c0", 1), intentEntry(tasks[3], "c3", 2))
	if ans, err := e.eng.LandProgress(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, entries); err != nil || ans != AnsOK {
		t.Fatalf("report progress: %s, %v", ans, err)
	}
	// A repeated report, even one wording the park differently, is a no-op.
	again := progressEntries(t, landMemberResult{TaskID: tasks[1], Outcome: landMemberParked, Reason: "another word"})
	if ans, err := e.eng.LandProgress(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, again); err != nil || ans != AnsOK {
		t.Fatalf("repeat progress: %s, %v", ans, err)
	}
	if r := e.str("SELECT result FROM land_batch_progress WHERE job_id = ? AND task_id = ?", g.JobID, tasks[1]); !strings.Contains(r, "README.md") {
		t.Errorf("the parked member's progress = %s, want the first report kept", r)
	}

	e.expireLease()
	g2 := e.leaseGit(e.runner1)
	if g2 == nil || g2.JobID != g.JobID {
		t.Fatalf("leased %+v, want batch %d again", g2, g.JobID)
	}
	var granted []int64
	for _, m := range g2.LandMembers {
		granted = append(granted, m.TaskID)
	}
	if want := []int64{tasks[0], tasks[3]}; !slices.Equal(granted, want) {
		t.Errorf("the re-lease grants %v, want only %v: the parked and the skipped are not squashed again", granted, want)
	}
	if s := e.str("SELECT state FROM tasks WHERE id = ?", tasks[1]); s != taskNeedsForeman {
		t.Errorf("the parked member is %s, want needs_foreman", s)
	}
	if p, ok := e.taskEvent(tasks[1], evTaskLandFailed); !ok || !strings.Contains(p.Reason, "README.md") {
		t.Errorf("the parked member's failure = %+v (%v), want its own conflict", p, ok)
	}
	if s := e.str("SELECT state FROM tasks WHERE id = ?", tasks[2]); s != taskDone {
		t.Errorf("the skipped dependent is %s, want done, waiting", s)
	}

	// The runner is lost for good: the job fails without a record.
	turns := e.foremanJobs()
	if ans := e.complete(g2, Completion{Error: "runner lost 3 times", ErrorKind: "runner_lost"}); ans != AnsOK {
		t.Fatalf("fail the batch: %s", ans)
	}
	for _, id := range []int64{tasks[0], tasks[3]} {
		if s := e.str("SELECT state FROM tasks WHERE id = ?", id); s != taskDone {
			t.Errorf("task %d with a publication intent is %s, want requeued, done", id, s)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM landings"); n != 0 {
		t.Errorf("landing records = %d, want none: local commits are no landings", n)
	}
	if got := e.foremanJobs(); got != turns {
		t.Errorf("foreman turns = %d, want %d: no member was parked by the lost runner", got, turns)
	}
}

// TestLandProgressRefusesAForeignMember: a progress entry for a task the
// batch does not own, or one with no outcome or intent, refuses the
// report naming it, and records nothing.
func TestLandProgressRefusesAForeignMember(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	for name, entry := range map[string]any{
		"foreign": landMemberResult{TaskID: 9999, Outcome: landMemberDropped},
		"empty":   landMemberResult{TaskID: tasks[0]},
	} {
		_, err := e.eng.LandProgress(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, progressEntries(t, entry))
		if !errors.Is(err, ErrLandProgressRefused) {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
	}
	if _, err := e.eng.LandProgress(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID,
		progressEntries(t, landMemberResult{TaskID: 9999, Outcome: landMemberDropped})); err == nil || !strings.Contains(err.Error(), "task 9999") {
		t.Errorf("the refusal = %v, want it to name task 9999", err)
	}
	if n := e.int("SELECT COUNT(*) FROM land_batch_progress"); n != 0 {
		t.Errorf("progress rows = %d, want none", n)
	}
	if ans, err := e.eng.LandProgress(e.ctx, g.LeaseOwner, g.JobID, "another-lease", nil); err != nil || ans != AnsLost {
		t.Errorf("a stale lease's report = %s, %v; want lost", ans, err)
	}
}

// TestLandBatchCompletionAccountsForEveryMember: an omitted, duplicate or
// foreign member refuses the completion naming the member, nothing
// released; a dropped
// dependent gets no landing record; a repeated report writes no second
// record.
func TestLandBatchCompletionAccountsForEveryMember(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	all := []landMemberResult{landedResult(tasks[0], "c0"), landedResult(tasks[1], "c1"),
		landedResult(tasks[2], "c2"), {TaskID: tasks[3], Outcome: landMemberDropped, BlockedBy: tasks[2]}}
	for name, tc := range map[string]struct {
		c    Completion
		task int64
	}{
		"omitted":   {batchResult(t, "", all[:3]...), tasks[3]},
		"duplicate": {batchResult(t, "", append(all, all[1])...), tasks[1]},
		"foreign":   {batchResult(t, "", append(all[:3:3], landedResult(9999, "cx"), all[3])...), 9999},
	} {
		_, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, tc.c)
		if err == nil || !strings.Contains(err.Error(), "task "+itoa(tc.task)) {
			t.Errorf("%s: err = %v, want refused naming task %d", name, err, tc.task)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM tasks WHERE land_batch_job = ?", g.JobID); n != 4 {
		t.Errorf("reserved members after the refusals = %d, want all four still owned", n)
	}
	c := batchResult(t, "", all...)
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", tasks[3]); n != 0 {
		t.Errorf("the dropped dependent has %d landing records, want none", n)
	}
	e.complete(g, c)
	if n := e.int("SELECT COUNT(*) FROM landings"); n != 3 {
		t.Errorf("landing records after the repeat = %d, want the three, none twice", n)
	}
}

// TestLandBatchLostLeaseAcrossPublication (§7.1 acceptance): a lease lost
// before publication leaves no landing records for its local commits and
// the next lease of the same batch grants every remaining owned member,
// each carrying the input the lost attempt recorded; that lease's
// completion, recovering the published members from the remote, records
// each once, and a repeated report records nothing twice.
func TestLandBatchLostLeaseAcrossPublication(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	var entries []any
	for i, id := range tasks {
		entries = append(entries, intentEntry(id, "c"+itoa(id), i+1))
	}
	if ans, err := e.eng.LandProgress(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, progressEntries(t, entries...)); err != nil || ans != AnsOK {
		t.Fatalf("report the intents: %s, %v", ans, err)
	}
	e.expireLease()
	if n := e.int("SELECT COUNT(*) FROM landings"); n != 0 {
		t.Fatalf("landing records after the lost lease = %d, want none for local commits", n)
	}
	g2 := e.leaseGit(e.runner1)
	if g2 == nil || g2.JobID != g.JobID || len(g2.LandMembers) != len(tasks) {
		t.Fatalf("re-leased %+v, want batch %d with all %d members", g2, g.JobID, len(tasks))
	}
	for _, m := range g2.LandMembers {
		if m.RecordedHead != "h" {
			t.Errorf("task %d recorded head = %q, want the lost attempt's input", m.TaskID, m.RecordedHead)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM tasks WHERE land_batch_job = ?", g.JobID); n != int64(len(tasks)) {
		t.Errorf("reserved members across the lost lease = %d, want all %d kept", n, len(tasks))
	}
	var landed []landMemberResult
	for _, id := range tasks {
		landed = append(landed, landedResult(id, "c"+itoa(id)))
	}
	c := batchResult(t, "", landed...)
	if ans := e.complete(g2, c); ans != AnsOK {
		t.Fatalf("complete the recovering lease: %s", ans)
	}
	e.complete(g2, c)
	for _, id := range tasks {
		if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", id); n != 1 {
			t.Errorf("task %d landing records = %d, want exactly one", id, n)
		}
	}
}
