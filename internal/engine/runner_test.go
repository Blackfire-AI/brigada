package engine

import (
	"errors"
	"testing"
	"time"
)

func TestRegisterUpsertsByName(t *testing.T) {
	e := newEnv(t)

	first, err := e.eng.Register(e.ctx, "runner-a", 0, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if first == 0 {
		t.Fatal("register returned id 0")
	}
	if got := e.int("SELECT COUNT(*) FROM runners WHERE name = 'runner-a'"); got != 1 {
		t.Fatalf("rows for runner-a = %d, want 1", got)
	}
	again, err := e.eng.Register(e.ctx, "runner-a", 0, nil)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if again != first {
		t.Errorf("re-register id = %d, want %d: one row per name", again, first)
	}
	other, err := e.eng.Register(e.ctx, "runner-b", 0, nil)
	if err != nil {
		t.Fatalf("register second name: %v", err)
	}
	if other == first {
		t.Errorf("second name id = %d, want a fresh row", other)
	}

	if got, err := e.eng.RunnerID(e.ctx, "runner-a"); err != nil || got != first {
		t.Errorf("RunnerID(runner-a) = %d, %v; want %d", got, err, first)
	}
	if got, err := e.eng.RunnerID(e.ctx, "nobody"); err != nil || got != 0 {
		t.Errorf("RunnerID(nobody) = %d, %v; want 0", got, err)
	}
}

// TestRegisterPersistsAdvertisement covers §5.1: the agents a register
// carries replace the runner's set, keyed by runner, adapter and model,
// and a kept agent keeps its row — the outcomes referencing it survive a
// restart.
func TestRegisterPersistsAdvertisement(t *testing.T) {
	e := newEnv(t)
	first := []AgentAdvert{
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Family: "anthropic", Account: "work", Classes: []string{"implement", "review"}},
		{Adapter: "kimi", Harness: "kimi", Model: "k2", Family: "moonshot", Account: "work", Classes: []string{"implement"}},
	}
	id, err := e.eng.Register(e.ctx, "runner-a", 0, first)
	if err != nil {
		t.Fatalf("register with agents: %v", err)
	}
	if got := e.int("SELECT COUNT(*) FROM agents WHERE runner_id = ?", id); got != 2 {
		t.Fatalf("agents after register = %d, want 2", got)
	}
	if got := e.str("SELECT classes FROM agents WHERE runner_id = ? AND adapter = 'claude' AND model = 'sonnet'", id); got != `["implement","review"]` {
		t.Errorf("classes = %s, want the advertised classes", got)
	}
	kept := e.int("SELECT id FROM agents WHERE runner_id = ? AND adapter = 'claude' AND model = 'sonnet'", id)

	// Re-registering replaces the set: kimi is gone, claude keeps its
	// row (updated), a new codex agent arrives.
	second := []AgentAdvert{
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Family: "anthropic", Account: "personal", Classes: []string{"implement"}},
		{Adapter: "codex", Harness: "codex", Model: "gpt", Family: "openai", Account: "work", Classes: []string{"review"}},
	}
	if _, err := e.eng.Register(e.ctx, "runner-a", 0, second); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := e.int("SELECT COUNT(*) FROM agents WHERE runner_id = ?", id); got != 2 {
		t.Fatalf("agents after re-register = %d, want 2: the set is replaced", got)
	}
	if got := e.int("SELECT COUNT(*) FROM agents WHERE runner_id = ? AND adapter = 'kimi'", id); got != 0 {
		t.Error("the dropped agent is still there")
	}
	if got := e.str("SELECT account FROM agents WHERE id = ?", kept); got != "personal" {
		t.Errorf("kept agent account = %q, want the updated advertisement", got)
	}
	if got := e.str("SELECT model FROM agents WHERE runner_id = ? AND adapter = 'codex'", id); got != "gpt" {
		t.Errorf("the new agent = %q, want it inserted", got)
	}

	// Registering with no agents clears the set.
	if _, err := e.eng.Register(e.ctx, "runner-a", 0, nil); err != nil {
		t.Fatalf("register with no agents: %v", err)
	}
	if got := e.int("SELECT COUNT(*) FROM agents WHERE runner_id = ?", id); got != 0 {
		t.Errorf("agents after an empty register = %d, want 0", got)
	}

	// An agent without adapter, harness or model is refused, naming it.
	for name, bad := range map[string]AgentAdvert{
		"no adapter": {Harness: "claude", Model: "sonnet"},
		"no harness": {Adapter: "claude", Model: "sonnet"},
		"no model":   {Adapter: "claude", Harness: "claude"},
	} {
		if _, err := e.eng.Register(e.ctx, "runner-a", 0, []AgentAdvert{bad}); err == nil {
			t.Errorf("%s: accepted, want refusal", name)
		}
	}
	// The same (adapter, model) twice in one call is refused: the key
	// would be ambiguous.
	dup := []AgentAdvert{
		{Adapter: "claude", Harness: "claude", Model: "sonnet"},
		{Adapter: "claude", Harness: "claude", Model: "sonnet", Account: "other"},
	}
	if _, err := e.eng.Register(e.ctx, "runner-a", 0, dup); err == nil {
		t.Error("a duplicate agent key was accepted, want refusal")
	}
}

// TestRegisterStoresTheRunnerCap covers the runner layer's own half
// (§8.1): a runner advertises its max_parallel when it registers, a
// re-register replaces it, 0 means unset again, and a negative value is
// the caller's mistake.
func TestRegisterStoresTheRunnerCap(t *testing.T) {
	e := newEnv(t)
	id, err := e.eng.Register(e.ctx, "capped", 4, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := e.int("SELECT max_parallel FROM runners WHERE id = ?", id); got != 4 {
		t.Errorf("max_parallel = %d, want 4", got)
	}
	for _, want := range []int64{2, 0} {
		if _, err := e.eng.Register(e.ctx, "capped", int(want), nil); err != nil {
			t.Fatalf("re-register with %d: %v", want, err)
		}
		if got := e.int("SELECT max_parallel FROM runners WHERE id = ?", id); got != want {
			t.Errorf("max_parallel after re-register = %d, want %d", got, want)
		}
	}
	if _, err := e.eng.Register(e.ctx, "capped", -1, nil); !errors.Is(err, ErrInvalidAgent) {
		t.Errorf("register with -1 = %v, want an invalid-advertisement refusal", err)
	}
}

// TestLeaseGrantCarriesTaskFields covers what the runner's workspace is
// made of (§5.1, §5.5): the repo URL — the task's own, else the
// project's — the base ref, the check command, the harness constraint,
// and the attempt number a re-dispatch's branch name needs (§6.4).
func TestLeaseGrantCarriesTaskFields(t *testing.T) {
	e := newEnv(t)
	// The runner offers the project's foreman pair, which the last lease
	// below needs, beside the worker classes the first ones use.
	e.seedAgent(e.runner1, "claude", "claude", "sonnet", "claude")
	p := e.work()
	p.Harness = "claude"
	p.Labels = []string{"linux"}
	e.submit(p)
	g := e.lease(e.runner1, []string{"claude"}, []string{"linux"})
	if g.RepoURL != "https://example.invalid/brigada.git" {
		t.Errorf("repo URL = %q, want the project's", g.RepoURL)
	}
	if g.BaseRef != "main" || g.CheckCmd != "go test ./..." {
		t.Errorf("base ref = %q, check = %q, want the task's", g.BaseRef, g.CheckCmd)
	}
	if g.Harness != "claude" {
		t.Errorf("harness = %q, want the job's constraint", g.Harness)
	}
	if g.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", g.Attempt)
	}
	if g.ResumeSessionID != "" {
		t.Errorf("resume session id = %q, want none for a fresh job", g.ResumeSessionID)
	}

	// A lost lease bumps the attempt the next one sees (§6.4).
	e.expireLease()
	g2 := e.lease(e.runner1, []string{"claude"}, []string{"linux"})
	if g2.Attempt != 2 {
		t.Errorf("attempt after a lost lease = %d, want 2", g2.Attempt)
	}
	// A session-mode job resumes the id its key's last run ended on
	// (§4.2); this one never ran, so there is nothing to resume.
	if g2.Mode == ModeSession && g2.ResumeSessionID != "" {
		t.Errorf("resume session id = %q without a prior run", g2.ResumeSessionID)
	}
	// A completion that parks the task (here: the worker asking its
	// question, §3) frees the key without queuing anything behind it, so
	// the session round below has the key to itself.
	e.complete(g2, Completion{Result: `{"status":"needs_help","question":"which file?"}`, SessionID: "sess-1"})

	// A task's own repo URL wins over the project's (§6.1).
	e.exec("UPDATE tasks SET repo_url = 'https://example.invalid/other.git' WHERE id = ?", e.task)
	next := e.work()
	next.Mode = ModeSession
	e.submit(next)
	g3 := e.lease(e.runner1, nil, nil)
	if g3.RepoURL != "https://example.invalid/other.git" {
		t.Errorf("repo URL = %q, want the task's own", g3.RepoURL)
	}
	if g3.ResumeSessionID != "sess-1" {
		t.Errorf("resume session id = %q, want the id the last run wrote back", g3.ResumeSessionID)
	}
	e.complete(g3, done())

	// A foreman turn carries the project repo/default branch for reading,
	// with no task or check. The needs_help park above
	// already woke one (§4.2) — it waits for the project's foreman
	// harness, so the lease asks for it by name.
	gf := e.lease(e.runner1, []string{"claude"}, nil)
	if gf == nil || gf.TaskID != 0 || gf.RepoURL != "https://example.invalid/brigada.git" || gf.BaseRef != "main" || gf.CheckCmd != "" {
		t.Errorf("foreman grant = %+v, want project checkout metadata and no task/check", gf)
	}
}

// TestHeartbeatRecordsProgress covers §5.6 on the engine side: an ok
// heartbeat writes the worker's progress timestamps onto the job.
func TestHeartbeatRecordsProgress(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	progress := time.Date(2026, 10, 12, 12, 5, 0, 0, time.UTC)
	ans, _, err := e.eng.Heartbeat(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, Progress{
		LastProgressAt: progress,
		Events:         []string{"tool_start Edit", "message thinking"},
	})
	if err != nil || ans != AnsOK {
		t.Fatalf("heartbeat = %s, %v; want ok", ans, err)
	}
	got := e.str("SELECT last_progress_at FROM jobs WHERE id = ?", g.JobID)
	if got != fmtTime(progress) {
		t.Errorf("last_progress_at = %q, want %q", got, fmtTime(progress))
	}
	if e.str("SELECT COALESCE(last_activity_at, '') FROM jobs WHERE id = ?", g.JobID) == "" {
		t.Error("last_activity_at was not recorded")
	}
	// A heartbeat from a non-holder records nothing.
	before := e.str("SELECT last_progress_at FROM jobs WHERE id = ?", g.JobID)
	if ans, _, err := e.eng.Heartbeat(e.ctx, e.runner2, g.JobID, g.LeaseID, Progress{LastProgressAt: progress.Add(time.Hour)}); err != nil || ans != AnsLost {
		t.Fatalf("non-holder heartbeat = %s, %v; want lost", ans, err)
	}
	if after := e.str("SELECT last_progress_at FROM jobs WHERE id = ?", g.JobID); after != before {
		t.Errorf("a lost heartbeat changed last_progress_at: %q -> %q", before, after)
	}
}

func TestHeartbeatAndCompleteRequireTheLeaseHolder(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	// The lease id alone is not enough: a runner the job was never
	// leased to is lost, same as a wrong lease id (§12.3).
	if ans, _, err := e.eng.Heartbeat(e.ctx, e.runner2, g.JobID, g.LeaseID, Progress{}); err != nil || ans != AnsLost {
		t.Errorf("heartbeat by a non-holder = %s, %v; want lost", ans, err)
	}
	if ans, err := e.eng.Complete(e.ctx, e.runner2, g.JobID, g.LeaseID, done()); err != nil || ans != AnsLost {
		t.Errorf("complete by a non-holder = %s, %v; want lost", ans, err)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", g.JobID); status != "leased" {
		t.Errorf("status after the non-holder calls = %q, want leased untouched", status)
	}

	// The holder still can.
	if ans := e.heartbeat(g); ans != AnsOK {
		t.Errorf("holder heartbeat = %s, want ok", ans)
	}
	if ans := e.complete(g, done()); ans != AnsOK {
		t.Errorf("holder complete = %s, want ok", ans)
	}
}

func TestJobReadsTheCurrentWord(t *testing.T) {
	e := newEnv(t)

	if info, err := e.eng.Job(e.ctx, 99999); err != nil || info != nil {
		t.Fatalf("Job(99999) = %v, %v; want nil, nil", info, err)
	}

	res := e.submit(e.work())
	info, err := e.eng.Job(e.ctx, res.JobID)
	if err != nil {
		t.Fatalf("read the submitted job: %v", err)
	}
	if info.Status != "queued" || info.Key != e.work().Key || info.Kind != KindWork {
		t.Errorf("queued job read back as %+v", info)
	}
	if info.CreatedAt == "" || info.UpdatedAt == "" {
		t.Errorf("timestamps missing from the read: %+v", info)
	}
	if len(info.Labels) != 0 {
		t.Errorf("labels = %v, want the empty list", info.Labels)
	}

	// An expired lease shows as queued, not leased: Job sweeps first.
	g := e.leaseAny(e.runner1)
	e.expireLease()
	info, err = e.eng.Job(e.ctx, g.JobID)
	if err != nil {
		t.Fatalf("read the expired job: %v", err)
	}
	if info.Status != "queued" {
		t.Errorf("status after the lease expired = %q, want queued", info.Status)
	}

	e.complete(g, done())
	info, err = e.eng.Job(e.ctx, g.JobID)
	if err != nil {
		t.Fatalf("read the finished job: %v", err)
	}
	if info.Status != "done" || string(info.Result) == "" {
		t.Errorf("finished job read back as %+v", info)
	}
	if info.LeaseExpiresAt != "" {
		t.Errorf("lease_expires_at = %q, want empty once the lease ended", info.LeaseExpiresAt)
	}
}
