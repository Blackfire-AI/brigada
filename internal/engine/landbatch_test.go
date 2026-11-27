package engine

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// landBatchEnv drives the fixture's task and three more to done and
// approved while a stand-in batch job holds the project's land-batch key,
// so no landing forms yet (§7.1: a landing turning ready while a batch
// runs waits for the next one). It returns the four tasks, lowest id
// first, and the stand-in, which the test cancels to let formation run.
func landBatchEnv(t *testing.T) (*env, []int64, int64) {
	t.Helper()
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	tasks := []int64{e.task, e.addTask("{}"), e.addTask("{}"), e.addTask("{}")}
	blocker := e.submit(SubmitParams{Key: landBatchKey(e.project), ProjectID: e.project, TaskID: e.task,
		Kind: KindLand, Mode: ModeFresh, Labels: []string{"git"}}).JobID
	for _, id := range tasks {
		e.runTaskToDone(id, author, reviewer)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'land' AND id <> ?", blocker); n != 0 {
		t.Fatalf("land jobs beside the held key = %d, want none: every landing waits for the next batch", n)
	}
	return e, tasks, blocker
}

// formBatch runs the formation for the fixture's project, trigger 0.
func (e *env) formBatch() SubmitResult {
	e.t.Helper()
	var sr SubmitResult
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		sr, err = e.eng.formLandBatchTx(tx, e.project, 0, e.clock.Now())
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return sr
}

// reservedTasks lists the tasks the batch job reserves, lowest id first.
func (e *env) reservedTasks(job int64) []int64 {
	e.t.Helper()
	var ids []int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT id FROM tasks WHERE land_batch_job = ? ORDER BY id", job)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatal(err)
	}
	return ids
}

// TestLandBatchFormation: the formation gathers every ready landing of
// the project in dependency order, then task id lowest first, up to
// land_batch_max; the lead is the job's task, every member is reserved
// in the submit's transaction, and a landing past the bound waits.
func TestLandBatchFormation(t *testing.T) {
	e, tasks, blocker := landBatchEnv(t)
	// The second task stacks on the fourth: it lands after it.
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, tasks[1], tasks[3])
	e.setSettings(`{"land_batch_max":3}`)
	if ans, err := e.eng.Cancel(e.ctx, blocker); err != nil || ans != AnsOK {
		t.Fatalf("cancel the stand-in batch: %s, %v", ans, err)
	}

	var order []int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		cands, err := e.eng.landBatchCandidatesTx(tx, e.project, 0, e.clock.Now())
		if err != nil {
			return err
		}
		order, err = orderLandBatchTx(tx, cands, 4)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []int64{tasks[0], tasks[2], tasks[3], tasks[1]}; !slices.Equal(order, want) {
		t.Errorf("batch order = %v, want %v (dependency order, then lowest id)", order, want)
	}

	sr := e.formBatch()
	if sr.Answer != AnsCreated {
		t.Fatalf("formation answered %s, want created", sr.Answer)
	}
	if key := e.str("SELECT key FROM jobs WHERE id = ?", sr.JobID); key != landBatchKey(e.project) {
		t.Errorf("batch key = %q, want %q", key, landBatchKey(e.project))
	}
	if lead := e.int("SELECT task_id FROM jobs WHERE id = ?", sr.JobID); lead != tasks[0] {
		t.Errorf("batch lead = %d, want the first member %d", lead, tasks[0])
	}
	if got, want := e.reservedTasks(sr.JobID), []int64{tasks[0], tasks[2], tasks[3]}; !slices.Equal(got, want) {
		t.Errorf("reserved = %v, want %v: three members, the dependent past the bound", got, want)
	}
}

// TestLandBatchOneLivePerTarget: a second formation finds the key held
// and queues nothing, and the rule run again for a member or for a
// landing outside the batch never queues a second job.
func TestLandBatchOneLivePerTarget(t *testing.T) {
	e, tasks, blocker := landBatchEnv(t)
	e.setSettings(`{"land_batch_max":2}`)
	if ans, err := e.eng.Cancel(e.ctx, blocker); err != nil || ans != AnsOK {
		t.Fatalf("cancel the stand-in batch: %s, %v", ans, err)
	}
	first := e.formBatch()
	if first.Answer != AnsCreated {
		t.Fatalf("formation answered %s, want created", first.Answer)
	}
	if again := e.formBatch(); again.Answer != AnsPending || again.JobID != first.JobID {
		t.Errorf("second formation = %+v, want pending behind job %d", again, first.JobID)
	}
	for _, id := range tasks {
		e.maybeLand(id)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'land' AND status IN ('queued', 'leased', 'running')"); n != 1 {
		t.Errorf("live land jobs = %d, want the one batch", n)
	}
	// Duplicate creation coalesces on the reservation.
	var sr SubmitResult
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		sr, err = e.eng.queueLandJobTx(tx, e.project, tasks[1], false, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if sr.Answer != AnsPending || sr.JobID != first.JobID {
		t.Errorf("queue a reserved member = %+v, want pending behind batch %d", sr, first.JobID)
	}
}

// TestLandBatchReleasedOnOutcome: the batch's completion releases every
// member, and the outcome re-runs the rule, forming the next batch of the
// landings still ready.
func TestLandBatchReleasedOnOutcome(t *testing.T) {
	e, tasks, blocker := landBatchEnv(t)
	e.setSettings(`{"land_batch_max":2}`)
	if ans, err := e.eng.Cancel(e.ctx, blocker); err != nil || ans != AnsOK {
		t.Fatalf("cancel the stand-in batch: %s, %v", ans, err)
	}
	first := e.formBatch()
	g := e.leaseGit(e.runner1)
	if g == nil || g.JobID != first.JobID {
		t.Fatalf("leased %+v, want the batch %d", g, first.JobID)
	}
	if ans := e.complete(g, Completion{Result: `{"target_branch":"main","commit":"abc123","check_passed":true,"pushed":"pushed"}`}); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	if got := e.reservedTasks(first.JobID); len(got) != 0 {
		t.Errorf("still reserved by the ended batch: %v", got)
	}
	next := e.int("SELECT id FROM jobs WHERE key = ? AND status = 'queued'", landBatchKey(e.project))
	if got, want := e.reservedTasks(next), []int64{tasks[1], tasks[2]}; !slices.Equal(got, want) {
		t.Errorf("next batch reserves %v, want %v", got, want)
	}
}

// TestLandBatchReaders: every reader of live task ownership (§12.3)
// honors a reservation — a repeated approval reports the batch, the land
// retry refuses the member naming it, a reserved member's waiting landing
// counts as a released push and is neither re-released by push approve
// nor listed as a push-gate wait, and the status reads it in flight.
func TestLandBatchReaders(t *testing.T) {
	e, tasks, blocker := landBatchEnv(t)
	if ans, err := e.eng.Cancel(e.ctx, blocker); err != nil || ans != AnsOK {
		t.Fatalf("cancel the stand-in batch: %s, %v", ans, err)
	}
	batch := e.formBatch().JobID
	member := tasks[2]

	// landDecisionTx returns the reserved live batch, queueing nothing.
	e.exec("INSERT INTO land_holds (task_id, reason, created_at) VALUES (?, 'held by rule gate_land', ?)", member, seedTS)
	d, err := e.eng.LandApprove(e.ctx, e.project, member, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Queued || d.JobID != batch {
		t.Errorf("approval of a reserved member = %+v, want job %d and nothing queued", d, batch)
	}

	// liveLandJobTx reads the member's landing in flight.
	var live bool
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		live, err = liveLandJobTx(tx, member)
		return err
	}); err != nil || !live {
		t.Errorf("liveLandJobTx(reserved member) = %v, %v; want true", live, err)
	}

	// RetryLanding refuses a reserved member as live, naming its batch.
	e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskNeedsForeman, member)
	r, err := e.eng.RetryLanding(e.ctx, e.project, member)
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != LandRetryRefused || !strings.Contains(r.Reason, "live land batch") || !strings.Contains(r.Reason, "job ") {
		t.Errorf("land retry of a reserved member = %+v, want refused naming the batch", r)
	}
	e.exec("UPDATE tasks SET state = ? WHERE id = ?", taskDone, member)

	// A waiting landing beside a live reservation is a released push; a
	// squash-phase reservation alone — no record — lands nothing.
	other := tasks[1]
	e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
		VALUES (?, ?, 'main', 'c1', 'waiting', ?), (?, ?, 'main', 'c2', 'waiting', ?)`,
		member, e.project, seedTS, other, e.project, seedTS)
	e.exec("UPDATE tasks SET land_batch_job = NULL WHERE id = ?", other)
	var memberReleased, otherReleased, squashLanded bool
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		if memberReleased, err = depPushReleasedTx(tx, member); err != nil {
			return err
		}
		if otherReleased, err = depPushReleasedTx(tx, other); err != nil {
			return err
		}
		squashLanded, err = depLandedForTx(tx, tasks[0], tasks[3])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !memberReleased || otherReleased {
		t.Errorf("push released: reserved member %v, free task %v; want true, false", memberReleased, otherReleased)
	}
	if squashLanded {
		t.Error("a squash-phase reservation counted as a landed dependency")
	}

	var waits []NeedsYouItem
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		waits, err = statusPushWaitsTx(tx, e.project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, w := range waits {
		ids = append(ids, w.ID)
	}
	if !slices.Equal(ids, []int64{other}) {
		t.Errorf("push-gate waits = %v, want only the free task %d", ids, other)
	}
	res, err := e.eng.PushApprove(e.ctx, envRepo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Released != 1 {
		t.Errorf("push approve released %d, want 1: the reserved member's push is its batch's", res.Released)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND kind = 'land'", keyOf(member)); n != 0 {
		t.Errorf("push jobs on the reserved member's key = %d, want none", n)
	}
}

// TestLandNotReadyStreakPerMember: each member's not-ready streak reads
// its own ordered outcomes across land jobs, whatever their key, so
// another member's outcomes neither reset nor lengthen it; the member's
// next other outcome resets it. A member waiting on its retry time is no
// candidate.
func TestLandNotReadyStreakPerMember(t *testing.T) {
	e := newEnv(t)
	a, b := e.task, e.addTask("{}")
	notReady, err := json.Marshal(map[string]string{"result": `{"outcome":"` + OutcomeLandNotReady + `","exit":2}`})
	if err != nil {
		t.Fatal(err)
	}
	landed := `{"result":"{\"target_branch\":\"main\",\"commit\":\"abc\",\"check_passed\":true}"}`
	add := func(task int64, key, result string) {
		e.exec(`INSERT INTO jobs (project_id, task_id, kind, key, mode, prompt, status, result, created_at, updated_at)
			VALUES (?, ?, 'land', ?, 'fresh', '', 'done', ?, ?, ?)`, e.project, task, key, result, seedTS, seedTS)
	}
	add(a, landBatchKey(e.project), string(notReady))
	add(b, landBatchKey(e.project), landed)
	add(a, keyOf(a), string(notReady))
	add(b, landBatchKey(e.project), string(notReady))
	streak := func(task int64) int {
		var n int
		if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
			var err error
			n, err = landNotReadyStreakTx(tx, task)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := streak(a); got != 2 {
		t.Errorf("streak of a = %d, want 2 across its batch and its own key", got)
	}
	if got := streak(b); got != 1 {
		t.Errorf("streak of b = %d, want 1: a's outcomes do not lengthen it", got)
	}
	add(a, landBatchKey(e.project), landed)
	if got := streak(a); got != 0 {
		t.Errorf("streak of a after another outcome = %d, want 0", got)
	}

	// The retry time filters candidates before the next batch forms.
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	blocker := e.submit(SubmitParams{Key: landBatchKey(e.project), ProjectID: e.project, TaskID: a,
		Kind: KindLand, Mode: ModeFresh, Labels: []string{"git"}}).JobID
	e.runTaskToDone(b, author, reviewer)
	e.exec("UPDATE tasks SET land_retry_at = ? WHERE id = ?", fmtTime(e.clock.Now().Add(3600e9)), b)
	if ans, err := e.eng.Cancel(e.ctx, blocker); err != nil || ans != AnsOK {
		t.Fatalf("cancel the stand-in batch: %s, %v", ans, err)
	}
	if sr := e.formBatch(); sr.Answer != "" {
		t.Errorf("formation with the only landing on its backoff = %+v, want nothing formed", sr)
	}
	e.exec("UPDATE tasks SET land_retry_at = ? WHERE id = ?", fmtTime(e.clock.Now()), b)
	if sr := e.formBatch(); sr.Answer != AnsCreated {
		t.Errorf("formation once the retry time passed = %+v, want created", sr)
	}
}

// leasedBatch forms the four-member batch of landBatchEnv and leases it.
func leasedBatch(t *testing.T) (*env, []int64, *LeaseGrant) {
	t.Helper()
	e, tasks, blocker := landBatchEnv(t)
	if ans, err := e.eng.Cancel(e.ctx, blocker); err != nil || ans != AnsOK {
		t.Fatalf("cancel the stand-in batch: %s, %v", ans, err)
	}
	e.formBatch()
	g := e.leaseGit(e.runner1)
	if g == nil || !g.LandBatch {
		t.Fatalf("leased %+v, want the batch grant", g)
	}
	return e, tasks, g
}

// batchResult encodes a batch completion of the given member results.
func batchResult(t *testing.T, errMsg string, members ...landMemberResult) Completion {
	t.Helper()
	b, err := json.Marshal(landBatchRecord{Batch: true, Members: members, Error: errMsg})
	if err != nil {
		t.Fatal(err)
	}
	return Completion{Result: string(b)}
}

func landedResult(task int64, commit string) landMemberResult {
	return landMemberResult{TaskID: task, Outcome: landMemberLanded,
		Record: &landRecord{TargetBranch: "main", Commit: commit, CheckPassed: true, Pushed: "pushed"}}
}

// TestLandBatchGrantNamesEveryMember: the batch's lease grants every
// member it owns in batch order, each with its own branch, base, approved
// head, message and its dependencies inside the batch.
func TestLandBatchGrantNamesEveryMember(t *testing.T) {
	e, tasks, blocker := landBatchEnv(t)
	e.exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id) VALUES (?, ?, ?)", e.plan, tasks[1], tasks[3])
	if ans, err := e.eng.Cancel(e.ctx, blocker); err != nil || ans != AnsOK {
		t.Fatalf("cancel the stand-in batch: %s, %v", ans, err)
	}
	e.formBatch()
	g := e.leaseGit(e.runner1)
	if g == nil || !g.LandBatch || g.Push {
		t.Fatalf("leased %+v, want the batch's squash phase", g)
	}
	var order []int64
	for _, m := range g.LandMembers {
		order = append(order, m.TaskID)
		if m.Branch == "" || m.BaseRef == "" || m.Message == nil {
			t.Errorf("member %d is incomplete: %+v", m.TaskID, m)
		}
	}
	if want := []int64{tasks[0], tasks[2], tasks[3], tasks[1]}; !slices.Equal(order, want) {
		t.Errorf("member order = %v, want %v", order, want)
	}
	if deps := g.LandMembers[3].Deps; !slices.Equal(deps, []int64{tasks[3]}) {
		t.Errorf("deps of task %d = %v, want its in-batch dependency %d", tasks[1], deps, tasks[3])
	}
}

// TestLandBatchCompletionSettlesEachMember: a batch completion records
// each landed member once, parks a conflicting member alone with its
// files, leaves a dropped member done with no record, and releases every
// reservation.
func TestLandBatchCompletionSettlesEachMember(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	c := batchResult(t, "",
		landedResult(tasks[0], "c0"),
		landMemberResult{TaskID: tasks[1], Outcome: landMemberParked,
			Record: &landRecord{TargetBranch: "main", Commit: "c0", ConflictFiles: []string{"README.md"}}},
		landMemberResult{TaskID: tasks[2], Outcome: landMemberDropped, BlockedBy: tasks[1]},
		landedResult(tasks[3], "c3"))
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	for _, id := range []int64{tasks[0], tasks[3]} {
		if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", id); n != 1 {
			t.Errorf("landings of task %d = %d, want 1", id, n)
		}
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", tasks[1]); state != taskNeedsForeman {
		t.Errorf("the conflicting member is %s, want needs_foreman", state)
	}
	if p, ok := e.taskEvent(tasks[1], evTaskLandFailed); !ok || !strings.Contains(p.Reason, "README.md") {
		t.Errorf("the conflicting member's land failure = %+v (%v), want its file", p, ok)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", tasks[2]); state != taskDone {
		t.Errorf("the dropped member is %s, want done, waiting", state)
	}
	if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", tasks[2]); n != 0 {
		t.Errorf("the dropped member has %d landing records, want none", n)
	}
	if n := e.int("SELECT COUNT(*) FROM tasks WHERE land_batch_job = ?", g.JobID); n != 0 {
		t.Errorf("members still reserved by the ended batch = %d", n)
	}
	// A repeated report of the same outcome reconciles, landing nothing twice.
	if ans := e.complete(g, c); ans == AnsOK {
		if n := e.int("SELECT COUNT(*) FROM landings WHERE task_id = ?", tasks[0]); n != 1 {
			t.Errorf("landings of task %d after the repeat = %d, want 1", tasks[0], n)
		}
	}
}

// TestLandBatchCompletionRefusesABadMemberList: a completion that leaves
// a member unsettled without an error, reports one twice or reports a
// task the batch does not own is refused, nothing settled.
func TestLandBatchCompletionRefusesABadMemberList(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	all := []landMemberResult{landedResult(tasks[0], "c0"), landedResult(tasks[1], "c1"),
		landedResult(tasks[2], "c2"), landedResult(tasks[3], "c3")}
	for name, c := range map[string]Completion{
		"missing":   batchResult(t, "", all[:3]...),
		"duplicate": batchResult(t, "", append(all, all[0])...),
		"foreign":   batchResult(t, "", append(all[:3:3], landedResult(9999, "cx"), all[3])...),
	} {
		if _, err := e.eng.Complete(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, c); err == nil {
			t.Errorf("%s: the completion was accepted", name)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM landings"); n != 0 {
		t.Errorf("landing records after refused completions = %d, want none", n)
	}
	if n := e.int("SELECT COUNT(*) FROM tasks WHERE land_batch_job = ?", g.JobID); n != 4 {
		t.Errorf("reserved members = %d, want all four still owned", n)
	}
}

// TestLandBatchEnvFailure: an environment failure partway through parks
// every member the batch still owns under one cause key with one wake —
// members checked locally and members not yet attempted alike — while a
// member parked for its own content keeps that failure and its dropped
// dependent keeps waiting.
func TestLandBatchEnvFailure(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	turns := e.foremanJobs()
	c := batchResult(t, landEnvFetchErr,
		landMemberResult{TaskID: tasks[0], Outcome: landMemberParked,
			Record: &landRecord{TargetBranch: "main", Commit: "c0", ConflictFiles: []string{"README.md"}}},
		landMemberResult{TaskID: tasks[1], Outcome: landMemberDropped, BlockedBy: tasks[0]})
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	if cause := e.str("SELECT land_env_cause FROM tasks WHERE id = ?", tasks[0]); cause != "" {
		t.Errorf("the content-parked member's cause = %q, want none: it keeps its own failure", cause)
	}
	if state := e.str("SELECT state FROM tasks WHERE id = ?", tasks[1]); state != taskDone {
		t.Errorf("the dropped member is %s, want done, waiting", state)
	}
	for _, id := range tasks[2:] {
		if cause := e.str("SELECT land_env_cause FROM tasks WHERE id = ?", id); cause != landEnvNetKey {
			t.Errorf("task %d cause = %q, want %q", id, cause, landEnvNetKey)
		}
	}
	if got := e.countEvents(evLandEnvFailed); got != 1 {
		t.Errorf("land_env_failed events = %d, want the one wake", got)
	}
	if got := e.foremanJobs(); got != turns+1 {
		t.Errorf("foreman turns = %d, want one more than %d", got, turns)
	}
}

// TestLandBatchHeldHoldsEveryMember: land_ready's exit 1 before the first
// squash gives every member its own hold and its own re-check, nothing
// recorded.
func TestLandBatchHeldHoldsEveryMember(t *testing.T) {
	e, tasks, g := leasedBatch(t)
	if ans := e.complete(g, Completion{Outcome: OutcomeLandHeld, Result: `{"outcome":"held","line":"CI is red"}`}); ans != AnsOK {
		t.Fatalf("complete the batch: %s", ans)
	}
	for _, id := range tasks {
		if r := e.str("SELECT COALESCE(reason, '') FROM land_holds WHERE task_id = ? AND state = 'held'", id); !strings.Contains(r, "CI is red") {
			t.Errorf("task %d hold = %q, want the command's line", id, r)
		}
		if n := e.int("SELECT COUNT(*) FROM jobs WHERE key = ? AND status = 'queued'", keyOf(id)); n != 1 {
			t.Errorf("task %d re-checks queued = %d, want 1", id, n)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM landings"); n != 0 {
		t.Errorf("landing records = %d, want none", n)
	}
}
