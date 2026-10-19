package engine

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/planfile"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// A pre-recovery lease is refused once recover has run (§6.4): the runner
// that kept working through the outage cannot write into the rebuilt
// store, and its lease-scoped token is gone with the epoch.
func TestPreRecoveryLeaseRefusedAfterRecover(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("nothing to lease")
	}
	epoch, err := e.eng.MarkRecovered(e.ctx)
	if err != nil {
		t.Fatalf("mark recovered: %v", err)
	}
	if epoch != 2 {
		t.Errorf("epoch after recover = %d, want 2", epoch)
	}
	if ans := e.heartbeat(g); ans != AnsStaleEpoch {
		t.Errorf("heartbeat = %s; want stale_epoch", ans)
	}
	if ans := e.complete(g, done()); ans != AnsStaleEpoch {
		t.Errorf("complete = %s; want stale_epoch", ans)
	}
	if ans := e.release(e.runner1, g, ""); ans != AnsStaleEpoch {
		t.Errorf("release = %s; want stale_epoch", ans)
	}
	kinds := strings.Join(e.eventKinds(), " ")
	if !strings.Contains(kinds, evFenceEpochBumped) || !strings.Contains(kinds, evStoreRecovered) {
		t.Errorf("events = %s; want the bump and the recovery", kinds)
	}
}

// Dispatch waits one lease TTL after a recover, on the engine's clock.
func TestDispatchHeldOneLeaseWindowAfterRecover(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.MarkRecovered(e.ctx); err != nil {
		t.Fatal(err)
	}
	r := e.submit(e.work())
	ttl := e.eng.LeaseTTL()

	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased at the moment of the recovery: %+v", g)
	}
	e.clock.Advance(ttl - time.Second)
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("leased %s after the recovery, inside the window", ttl-time.Second)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", r.JobID); status != statusQueued {
		t.Errorf("held job status = %q, want queued", status)
	}
	e.clock.Advance(time.Second)
	if g := e.leaseAny(e.runner1); g == nil || g.JobID != r.JobID {
		t.Fatalf("lease after the window = %+v, want the job", g)
	}
}

// A store never recovered holds nothing back.
func TestNoHoldWithoutRecover(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	if g := e.leaseAny(e.runner1); g == nil {
		t.Fatal("a store that was never recovered held dispatch")
	}
}

// recoverEnv is an env whose project names a local bare remote, with a
// helper to put plan files on its plans branch.
type recoverEnv struct {
	*env
	remote string
}

func newRecoverEnv(t *testing.T) *recoverEnv {
	r := &recoverEnv{env: newEnv(t), remote: testsupport.NewGitRemote(t)}
	r.exec("UPDATE projects SET repo_url = ? WHERE id = ?", r.remote, r.project)
	return r
}

func (r *recoverEnv) pushFile(p planfile.Plan) {
	r.t.Helper()
	p.Repo = r.remote
	if err := planfile.Push(context.Background(), planfile.Target{URL: r.remote, Branch: defaultPlanFileBranch}, p); err != nil {
		r.t.Fatalf("push the plan file: %v", err)
	}
}

func (r *recoverEnv) recoverPlans() []PlanRecovery {
	r.t.Helper()
	res, warns, err := r.eng.RecoverPlans(r.ctx, nil)
	if err != nil {
		r.t.Fatalf("recover plans: %v", err)
	}
	if len(warns) > 0 {
		r.t.Fatalf("warnings: %v", warns)
	}
	return res
}

func TestRecoverRebuildsPlanNewerThanSnapshotAndKeepsOlder(t *testing.T) {
	r := newRecoverEnv(t)
	stored := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) // seedTS: what the snapshot holds of r.plan

	// Plan 40: not in the store at all. Its file is rebuilt whole.
	r.pushFile(planfile.Plan{
		ID: 40, Title: "the lost plan", State: "active",
		Created: stored.Add(time.Hour), Updated: stored.Add(2 * time.Hour),
		Tasks: []planfile.Task{
			{ID: 401, Tier: "heavy", Effort: "high", Name: "api", Title: "build the api", Class: "implement", State: "done", Branch: "brigada/task-401", Rounds: 1},
			{ID: 402, Name: "ui", Title: "build the ui", Class: "implement", Deps: []string{"api"}, State: "needs_foreman", Question: "which palette?"},
			{ID: 403, Title: "midflight", Class: "implement", State: "running"},
		},
	})
	// The plan the snapshot holds, whose file is OLDER: left alone.
	r.pushFile(planfile.Plan{
		ID: r.plan, Title: "an older title", State: "cancelled",
		Created: stored.Add(-48 * time.Hour), Updated: stored.Add(-24 * time.Hour),
	})

	res := r.recoverPlans()
	actions := map[int64]string{}
	for _, p := range res {
		actions[p.PlanID] = p.Action
	}
	if actions[40] != PlanRebuilt || actions[r.plan] != PlanKept {
		t.Fatalf("actions = %v; want plan 40 rebuilt and plan %d kept", actions, r.plan)
	}

	if got := r.str("SELECT title FROM plans WHERE id = ?", r.plan); got != "a plan" {
		t.Errorf("the older file overwrote the store's plan: title %q", got)
	}
	for id, want := range map[int]string{401: "heavy|heavy|high", 402: "standard|standard|"} {
		if got := r.str("SELECT tier || '|' || effective_tier || '|' || effort FROM tasks WHERE id = ?", id); got != want {
			t.Errorf("recovered task %d settings = %q, want %q", id, got, want)
		}
	}
	if got := r.str("SELECT state FROM plans WHERE id = 40"); got != "active" {
		t.Errorf("rebuilt plan state = %q", got)
	}
	if got := r.str("SELECT title FROM plans WHERE id = 40"); got != "the lost plan" {
		t.Errorf("rebuilt plan title = %q", got)
	}
	if got := r.int("SELECT project_id FROM plans WHERE id = 40"); got != r.project {
		t.Errorf("rebuilt plan project = %d, want %d", got, r.project)
	}
	if got := r.int("SELECT COUNT(*) FROM tasks WHERE plan_id = 40"); got != 3 {
		t.Errorf("rebuilt tasks = %d, want 3", got)
	}
	if got := r.str("SELECT name || '|' || state || '|' || COALESCE(branch, '') FROM tasks WHERE id = 401"); got != "api|done|brigada/task-401" {
		t.Errorf("task 401 = %q", got)
	}
	if got := r.int("SELECT COUNT(*) FROM task_deps WHERE plan_id = 40 AND task_id = 402 AND depends_on_task_id = 401"); got != 1 {
		t.Errorf("the edge ui -> api was not rebuilt")
	}
	if got := r.str("SELECT state FROM tasks WHERE id = 403"); got != "needs_foreman" {
		t.Errorf("a mid-flight task with no job = %q, want needs_foreman", got)
	}
	// The open question is back where the plan file mirror reads it from.
	snap := r.planSnapshot(40)
	if q := taskByName(t, snap, "ui").Question; q != "which palette?" {
		t.Errorf("open question = %q", q)
	}

	// A second run changes nothing: the store is now as new as the files.
	for _, p := range r.recoverPlans() {
		if p.Action != PlanKept {
			t.Errorf("second run: plan %d %s, want kept", p.PlanID, p.Action)
		}
	}
}

func TestRecoverUpdatesPlanWhoseFileIsNewer(t *testing.T) {
	r := newRecoverEnv(t)
	stored := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	r.pushFile(planfile.Plan{
		ID: r.plan, Title: "a plan", State: "done",
		Created: stored, Updated: stored.Add(time.Hour),
		Tasks: []planfile.Task{{ID: r.task, Tier: "light", Effort: "medium", Title: "the task", Class: "implement", State: "done", Branch: "brigada/t"}},
	})
	res := r.recoverPlans()
	if len(res) != 1 || res[0].Action != PlanUpdated {
		t.Fatalf("results = %+v, want one updated", res)
	}
	if got := r.str("SELECT state FROM plans WHERE id = ?", r.plan); got != "done" {
		t.Errorf("plan state = %q, want done", got)
	}
	if got := r.str("SELECT state || '|' || branch FROM tasks WHERE id = ?", r.task); got != "done|brigada/t" {
		t.Errorf("task = %q", got)
	}
	if got := r.str("SELECT tier || '|' || effective_tier || '|' || effort FROM tasks WHERE id = ?", r.task); got != "light|light|medium" {
		t.Errorf("updated task settings = %q", got)
	}
	// The task's spec, which a file never carries, is untouched.
	if got := r.str("SELECT spec FROM tasks WHERE id = ?", r.task); got != "the spec" {
		t.Errorf("task spec = %q, want it kept", got)
	}
}

func TestRecoverSkipsAFileItCannotApply(t *testing.T) {
	r := newRecoverEnv(t)
	later := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r.pushFile(planfile.Plan{
		ID: 50, Title: "bad dep", State: "active", Created: later, Updated: later,
		Tasks: []planfile.Task{{ID: 501, Title: "t", Class: "implement", Deps: []string{"ghost"}, State: "pending"}},
	})
	res := r.recoverPlans()
	if len(res) != 1 || res[0].Action != PlanSkipped || !strings.Contains(res[0].Detail, "ghost") {
		t.Fatalf("results = %+v, want one skipped naming the dep", res)
	}
	if got := r.int("SELECT COUNT(*) FROM plans WHERE id = 50"); got != 0 {
		t.Error("a skipped plan was written")
	}
}

func (r *recoverEnv) planSnapshot(plan int64) planfile.Plan {
	r.t.Helper()
	var s planSnapshot
	err := r.st.View(r.ctx, func(tx *sql.Tx) error {
		var err error
		s, err = readPlanSnapshotTx(tx, plan)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return s.plan
}

func TestSnapshotRetentionKeepsSeven(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snaps")
	e := newEnvCfg(t, func(c *Config) { c.SnapshotDir = dir })

	var paths []string
	for i := 0; i < 10; i++ {
		path, err := e.eng.SnapshotIfDue(e.ctx)
		if err != nil || path == "" {
			t.Fatalf("snapshot %d = %q, %v; want one taken", i, path, err)
		}
		paths = append(paths, path)
		// Not due again until the interval passes.
		if again, err := e.eng.SnapshotIfDue(e.ctx); err != nil || again != "" {
			t.Fatalf("a second snapshot the same moment = %q, %v", again, err)
		}
		e.clock.Advance(24 * time.Hour)
	}
	left, err := ListSnapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 7 {
		t.Fatalf("snapshots kept = %d, want 7: %v", len(left), left)
	}
	if left[0] != paths[9] || left[6] != paths[3] {
		t.Errorf("kept %v; want the newest seven, %s down to %s", left, paths[9], paths[3])
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evSnapshotTaken); got != 10 {
		t.Errorf("snapshot events = %d, want 10", got)
	}
	if got := e.str("SELECT json_extract(payload, '$.path') FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1", evSnapshotTaken); got != paths[9] {
		t.Errorf("event path = %q, want %q", got, paths[9])
	}
	if got := e.int("SELECT json_extract(payload, '$.bytes') FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1", evSnapshotTaken); got <= 0 {
		t.Errorf("event bytes = %d, want > 0", got)
	}
}

func TestSnapshotIsOptInAndOutsideTheStore(t *testing.T) {
	e := newEnv(t)
	if path, err := e.eng.SnapshotIfDue(e.ctx); err != nil || path != "" {
		t.Errorf("snapshot with no directory = %q, %v; want nothing", path, err)
	}

	storeDir := t.TempDir()
	e = newEnvAt(t, filepath.Join(storeDir, "store.db"), func(c *Config) { c.SnapshotDir = storeDir })
	if _, err := e.eng.SnapshotIfDue(e.ctx); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("snapshot into the store's own directory = %v, want refusal", err)
	}
	entries, _ := os.ReadDir(storeDir)
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), snapshotPrefix) {
			t.Errorf("refused snapshot left %s", ent.Name())
		}
	}
}
