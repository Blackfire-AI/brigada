package engine

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
)

// TestStatusAndLeaseRecordSkills: status names a task's skills in the
// order recorded, and the lease event of its job carries the template
// version and the digests of the instruction and skill texts (§5.10, §9).
func TestStatusAndLeaseRecordSkills(t *testing.T) {
	e := newEnv(t)
	e.importSkills(Skill{Name: "bugfix", Text: "v1 bugfix"}, Skill{Name: "cli-flag", Text: "v1 flag"})
	e.setRole(RoleWorker, "v1 worker")
	res := e.createPlan([]PlanTask{
		{Name: "skilled", Spec: "fix", Skills: []string{"cli-flag", "bugfix"}},
		{Name: "plain", Spec: "no skills"},
	})
	plan := e.int("SELECT plan_id FROM tasks WHERE id = ?", res.Tasks[0].TaskID)
	var tasks []StatusTask
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		tasks, err = statusPlanTasksTx(tx, plan, e.clock.Now())
		return err
	}); err != nil {
		t.Fatalf("status tasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("status tasks = %+v, want 2", tasks)
	}
	if want := []string{"cli-flag", "bugfix"}; !reflect.DeepEqual(tasks[0].Skills, want) {
		t.Errorf("skilled task skills = %v, want %v in the order given", tasks[0].Skills, want)
	}
	if len(tasks[1].Skills) != 0 {
		t.Errorf("plain task skills = %v, want none", tasks[1].Skills)
	}

	author := e.agented("author", "anthropic", "implement")
	if g := e.leaseAny(author); g == nil {
		t.Fatal("the work job was not leased")
	}
	var leased eventPayload
	raw := e.str("SELECT payload FROM events WHERE kind = ? AND job_id = ?", evJobLeased, res.Tasks[0].JobID)
	if raw == "" {
		raw = e.str("SELECT payload FROM events WHERE kind = ? ORDER BY id LIMIT 1", evJobLeased)
	}
	if err := json.Unmarshal([]byte(raw), &leased); err != nil {
		t.Fatalf("parse the lease event %q: %v", raw, err)
	}
	if leased.Prompt == nil {
		t.Fatalf("lease event %q carries no prompt digests", raw)
	}
	if leased.Prompt.Instructions != instructionsDigest("v1 worker") {
		t.Errorf("instructions digest = %q, want the worker text's", leased.Prompt.Instructions)
	}
	if want := []string{instructionsDigest("v1 flag"), instructionsDigest("v1 bugfix")}; !reflect.DeepEqual(leased.Prompt.Skills, want) {
		t.Errorf("skill digests = %v, want %v in order", leased.Prompt.Skills, want)
	}
}

// TestStatsSplitBySkillCount: a task's end counts under the number of
// skills it carried, so one-skill and two-skill tasks keep separate
// rows while the dispatcher's pooled sum stays whole (§8).
func TestStatsSplitBySkillCount(t *testing.T) {
	e := newEnv(t)
	e.importSkills(Skill{Name: "a", Text: "a"}, Skill{Name: "b", Text: "b"})
	res := e.createPlan([]PlanTask{
		{Name: "one", Spec: "s", Skills: []string{"a"}},
		{Name: "two", Spec: "s", Skills: []string{"a", "b"}},
		{Name: "one-more", Spec: "s", Skills: []string{"b"}},
		{Name: "none", Spec: "s"},
	})
	agent := e.agentOf(e.agented("author", "anthropic", "implement"))
	for i, success := range []bool{true, false, false, true} {
		job := res.Tasks[i].JobID
		e.exec("UPDATE jobs SET agent_id = ? WHERE id = ?", agent, job)
		if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
			j, err := loadJobTx(tx, job)
			if err != nil {
				return err
			}
			return recordTaskOutcomeTx(tx, j, success, e.clock.Now())
		}); err != nil {
			t.Fatalf("record the outcome of task %d: %v", i, err)
		}
	}
	stats, err := e.eng.Stats(e.ctx, e.project)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	type counts struct{ attempts, successes int }
	got := map[int]counts{}
	for _, r := range stats[0].Rows {
		if r.AgentID == agent && r.Class == "implement" && r.Attempts > 0 {
			got[r.Skills] = counts{r.Attempts, r.Successes}
		}
	}
	want := map[int]counts{0: {1, 1}, 1: {2, 1}, 2: {1, 0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows by skill count = %v, want %v", got, want)
	}
	// The dispatcher's record is the sum over the split.
	if n := e.int("SELECT SUM(attempts) FROM outcomes WHERE agent_id = ? AND class = 'implement'", agent); n != 4 {
		t.Errorf("pooled attempts = %d, want 4", n)
	}
}
