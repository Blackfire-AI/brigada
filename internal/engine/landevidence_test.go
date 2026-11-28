package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// The checked head and suspects (§7.1, acceptance "Gate and setting
// acceptance"): attribution is conditional — tasks are named only from a
// known baseline through a complete recorded publication chain.

// shippedGate is the recognized shipped script's stable evidence at head
// for the target main, with the exit given.
func shippedGate(head string, exit int) *landGateEvidence {
	return &landGateEvidence{Command: "scripts/land-ready.sh", Digest: "d1", Recognized: true, Remote: "r",
		Ref: "main", Attributed: true, Before: head, After: head, Exit: exit, Stable: true}
}

// seedPublication records one publication of the target main.
func (e *env) seedPublication(pred, final string, tasks ...int64) {
	e.t.Helper()
	var members []landPublicationMember
	for _, id := range tasks {
		members = append(members, landPublicationMember{TaskID: id, Commit: final})
	}
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		return recordLandPublicationTx(tx, e.project, "main", 0, landPublication{Predecessor: pred, Final: final, Members: members}, e.clock.Now())
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) noteBaseline(ev *landGateEvidence) {
	e.t.Helper()
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		return noteLandBaselineTx(tx, e.project, "main", ev, e.clock.Now())
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) suspects(ev *landGateEvidence) string {
	e.t.Helper()
	var s string
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		s, err = landSuspectsTx(tx, e.project, "main", ev)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) baselineHead() string {
	e.t.Helper()
	return e.str("SELECT COALESCE(MAX(head), '') FROM land_baselines WHERE project_id = ?", e.project)
}

// TestLandBaselineOnlyFromVerifiedEvidence: only the recognized script's
// stable exit 0, for an existing head brigada published, is a green
// baseline; an absent branch, another command's 0, an unstable run or a
// head brigada did not publish establishes none.
func TestLandBaselineOnlyFromVerifiedEvidence(t *testing.T) {
	e := newEnv(t)
	e.seedPublication("a", "b", e.task)
	absent := shippedGate("absent", 0)
	other := shippedGate("b", 0)
	other.Recognized, other.Digest = false, "d2"
	unstable := shippedGate("b", 0)
	unstable.Before = "a"
	unattributed := shippedGate("b", 0)
	unattributed.Attributed = false
	for name, ev := range map[string]*landGateEvidence{
		"absent branch": absent, "arbitrary command": other, "moved head": unstable,
		"another target": unattributed, "unpublished head": shippedGate("z", 0), "exit 1": shippedGate("b", 1),
	} {
		e.noteBaseline(ev)
		if got := e.baselineHead(); got != "" {
			t.Fatalf("%s: baseline = %q, want none", name, got)
		}
	}
	e.noteBaseline(shippedGate("b", 0))
	if got := e.baselineHead(); got != "b" {
		t.Errorf("baseline = %q, want the published head b", got)
	}
}

// TestLandSuspectsConditional: with a known baseline and a complete
// recorded chain the hold names only the chain's members after the
// baseline, in landing order, each once, the baseline's members excluded;
// at the baseline itself the list is explicitly empty; without a
// baseline, at a head brigada did not publish, across a gap, for another
// command identity or an unrecognized command the suspects are unknown and
// no task is named; and a head the records show the target left and came
// back to invalidates the evidence.
func TestLandSuspectsConditional(t *testing.T) {
	e := newEnv(t)
	t1, t2, t3, t4 := e.task, e.addTask("{}"), e.addTask("{}"), e.addTask("{}")
	e.seedPublication("a", "b", t1)
	if got := e.suspects(shippedGate("b", 1)); got != suspectsUnknown {
		t.Errorf("no baseline: %q, want %q", got, suspectsUnknown)
	}
	e.noteBaseline(shippedGate("b", 0))
	if got := e.suspects(shippedGate("b", 1)); !strings.HasPrefix(got, "suspects: none, no recorded landings since the baseline b") {
		t.Errorf("at the baseline: %q, want the explicit empty list", got)
	}
	e.seedPublication("b", "c", t2, t3)
	e.seedPublication("c", "d", t1, t4, t2)
	got := e.suspects(shippedGate("d", 1))
	if want := "suspects since the baseline b: task " + itoa(t2) + ", task " + itoa(t3) + ", task " + itoa(t4); got != want {
		t.Errorf("complete chain: %q, want %q", got, want)
	}

	other := shippedGate("d", 1)
	other.Digest = "changed"
	unrecognized := shippedGate("d", 1)
	unrecognized.Recognized = false
	for name, ev := range map[string]*landGateEvidence{
		"unpublished head": shippedGate("x", 1), "changed command": other, "unrecognized command": unrecognized,
		"moved away and back": shippedGate("c", 1),
	} {
		if got := e.suspects(ev); got != suspectsUnknown {
			t.Errorf("%s: %q, want %q and no task named", name, got, suspectsUnknown)
		}
	}
	// A gap: a head published from one brigada did not publish.
	e.seedPublication("q", "r", t3)
	if got := e.suspects(shippedGate("r", 1)); got != suspectsUnknown {
		t.Errorf("gap in the chain: %q, want %q", got, suspectsUnknown)
	}
	// The baseline is not renewed at a head the target left and came back to.
	e.noteBaseline(shippedGate("c", 0))
	if got := e.baselineHead(); got != "b" {
		t.Errorf("baseline after a moved-back pass = %q, want b kept", got)
	}
}

// TestLandBatchHoldNamesSuspects: a batch held before its first squash
// gives every member the command's line then the same suspect line, worded
// from the batch's evidence; a batch whose pass verifies a baseline and
// whose push moves the target records both.
func TestLandBatchHoldNamesSuspects(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	e.seedPublication("a", "b", 9001)
	e.noteBaseline(shippedGate("b", 0))
	e.seedPublication("b", "c", tasks[0])
	held, err := json.Marshal(landHeldRecord{Outcome: OutcomeLandHeld, Line: "PR #7 conflicts with main: CI will not run", Gate: shippedGate("c", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if ans := e.complete(g, Completion{Outcome: OutcomeLandHeld, Result: string(held)}); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	want := "held by rule land_ready: PR #7 conflicts with main: CI will not run; suspects since the baseline b: task " + itoa(tasks[0])
	for _, id := range tasks[1:] {
		if r := e.str("SELECT COALESCE(reason, '') FROM land_holds WHERE task_id = ? AND state = 'held'", id); r != want {
			t.Errorf("task %d hold = %q, want %q", id, r, want)
		}
	}
}

// TestLandBatchRecordsBaselineAndPublication: a batch completion whose
// gate passed at a published head verifies the baseline, and its push to
// the target records one publication of its members in landing order.
func TestLandBatchRecordsBaselineAndPublication(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	e.seedPublication("a", "b", 9001)
	b, err := json.Marshal(landBatchRecord{Batch: true, Gate: shippedGate("b", 0),
		Publication: &landPublication{Predecessor: "b", Final: "c3",
			Members: []landPublicationMember{{tasks[0], "c0"}, {tasks[1], "c1"}, {tasks[2], "c2"}, {tasks[3], "c3"}}},
		Members: []landMemberResult{landedResult(tasks[0], "c0"), landedResult(tasks[1], "c1"),
			landedResult(tasks[2], "c2"), landedResult(tasks[3], "c3")}})
	if err != nil {
		t.Fatal(err)
	}
	if ans := e.complete(g, Completion{Result: string(b)}); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	if got := e.baselineHead(); got != "b" {
		t.Errorf("baseline = %q, want b", got)
	}
	if got := e.str("SELECT predecessor_head || '>' || final_head FROM land_publications WHERE job_id = ?", g.JobID); got != "b>c3" {
		t.Errorf("the batch's publication = %q, want b>c3", got)
	}
	if got := e.suspects(shippedGate("c3", 1)); !strings.Contains(got, itoa(tasks[3])) || strings.Contains(got, "9001") {
		t.Errorf("suspects at the batch's head = %q, want its members, not the baseline's", got)
	}
}

// TestLandPushRecordsItsReleasedPrefix: a push phase that moved the target
// records one publication of the members it actually released — the
// waiting landings beneath its commit, then its own.
func TestLandPushRecordsItsReleasedPrefix(t *testing.T) {
	e, stack := waitingStack(t)
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := e.eng.queuePushJobTx(tx, e.project, stack[1], false, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pg := e.leaseGit(e.runner1)
	if pg == nil || !pg.Push || pg.TaskID != stack[1] {
		t.Fatalf("leased %+v, want the middle push", pg)
	}
	if ans := e.complete(pg, Completion{Result: `{"target_branch":"main","commit":"c1","pushed":"pushed","predecessor":"p"}`}); ans != AnsOK {
		t.Fatalf("complete the push: %s", ans)
	}
	var members []landPublicationMember
	raw := e.str("SELECT members FROM land_publications WHERE predecessor_head = 'p' AND final_head = 'c1'")
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		t.Fatalf("publication members %q: %v", raw, err)
	}
	if len(members) != 2 || members[0].TaskID != stack[0] || members[1].TaskID != stack[1] {
		t.Errorf("published members = %+v, want tasks %d then %d", members, stack[0], stack[1])
	}
}
