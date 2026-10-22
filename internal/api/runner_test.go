package api

import (
	"database/sql"
	"encoding/json"

	"fmt"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"net/http"
	"testing"
	"time"
)

func TestRunnerFlow(t *testing.T) {
	e := newEnv(t)

	// A runner that has not registered may not lease (§5.1): its row is
	// what a lease references.
	code, _, data := e.do(http.MethodPost, "/v1/runner/lease", "r2", "", []byte(`{"wait":0}`))
	if code != http.StatusForbidden {
		t.Fatalf("lease before register: %d %s, want 403", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeUnknownRunner {
		t.Errorf("lease before register code = %q, want %q", err.Code, codeUnknownRunner)
	}

	// Register is an upsert under the token's name.
	first := e.register("r1")
	if again := e.register("r1"); again != first {
		t.Errorf("re-register id = %d, want %d", again, first)
	}
	second := e.register("r2")
	if second == first {
		t.Errorf("second runner id = %d, want a fresh row", second)
	}

	// register → lease → heartbeat → complete (§12.3).
	jobID := e.submitWork()
	_, data = e.lease("r1", 0)
	grant := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data)
	if grant.Job == nil {
		t.Fatalf("lease answered no job: %s", data)
	}
	g := grant.Job
	if g.JobID != jobID || g.LeaseID == "" || g.Kind != "work" || g.Key != fmt.Sprintf("task:%d", e.task) {
		t.Errorf("grant = %+v, want the submitted job with a lease", g)
	}
	// The session generation rides the grant (§5.1): the runner names
	// the session-stable worktree path from it.
	if g.SessionGen != 0 {
		t.Errorf("grant session_gen = %d, want 0 for the task's first attempt", g.SessionGen)
	}

	code, _, data = e.do(http.MethodPost, "/v1/runner/heartbeat", "r1", "",
		[]byte(fmt.Sprintf(`{"job_id":%d,"lease_id":%q}`, g.JobID, g.LeaseID)))
	if code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", code, data)
	}
	hb := decode[struct {
		Answer    string     `json:"answer"`
		ExpiresAt *time.Time `json:"expires_at"`
	}](t, data)
	if hb.Answer != "ok" || hb.ExpiresAt == nil {
		t.Fatalf("heartbeat answered %+v, want ok with an expiry", hb)
	}

	// Someone else's lease id is lost, not an error.
	code, _, data = e.do(http.MethodPost, "/v1/runner/heartbeat", "r1", "",
		[]byte(fmt.Sprintf(`{"job_id":%d,"lease_id":"not-the-lease"}`, g.JobID)))
	if code != http.StatusOK {
		t.Fatalf("heartbeat with a wrong lease: %d %s, want 200", code, data)
	}
	if got := decode[answerBody](t, data).Answer; got != "lost" {
		t.Errorf("wrong-lease heartbeat answered %q, want lost", got)
	}

	// Another runner's token holds no lease here either: identity comes
	// from the token (§12.3), so r2 cannot heartbeat or complete r1's
	// job even knowing the lease id.
	for _, path := range []string{"heartbeat", "complete"} {
		body := fmt.Sprintf(`{"job_id":%d,"lease_id":%q}`, g.JobID, g.LeaseID)
		if path == "complete" {
			body = fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"result":"{\"status\":\"done\"}"}`, g.JobID, g.LeaseID)
		}
		code, _, data = e.do(http.MethodPost, "/v1/runner/"+path, "r2", "", []byte(body))
		if code != http.StatusOK {
			t.Fatalf("%s by a non-holder: %d %s, want 200", path, code, data)
		}
		if got := decode[answerBody](t, data).Answer; got != "lost" {
			t.Errorf("%s by a non-holder answered %q, want lost", path, got)
		}
	}

	complete := fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"result":"{\"status\":\"done\",\"summary\":\"finished\"}","session_id":"sess-1","artifacts":[{"kind":"transcript","path":"/transcripts/1","size":12,"sha256":"ab"}]}`,
		g.JobID, g.LeaseID)
	code, _, data = e.do(http.MethodPost, "/v1/runner/complete", "r1", "", []byte(complete))
	if code != http.StatusOK {
		t.Fatalf("complete: %d %s", code, data)
	}
	if got := decode[answerBody](t, data).Answer; got != "ok" {
		t.Fatalf("complete answered %q, want ok", got)
	}
	// Complete is idempotent per lease id: the repeat is already.
	code, _, data = e.do(http.MethodPost, "/v1/runner/complete", "r1", "", []byte(complete))
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "already" {
		t.Errorf("repeat complete = %d %s, want already", code, data)
	}
	// A completion needs exactly one of result or error.
	code, _, data = e.do(http.MethodPost, "/v1/runner/complete", "r1", "",
		[]byte(fmt.Sprintf(`{"job_id":%d,"lease_id":%q}`, g.JobID, g.LeaseID)))
	if code != http.StatusBadRequest {
		t.Errorf("complete with neither result nor error = %d, want 400", code)
	}
	// An outcome must be one of the runner's stop outcomes (§5.6) and
	// ride with a result, never an error.
	for _, body := range []string{
		fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"result":"r","outcome":"finished"}`, g.JobID, g.LeaseID),
		fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"error":"e","outcome":"stalled"}`, g.JobID, g.LeaseID),
	} {
		code, _, data = e.do(http.MethodPost, "/v1/runner/complete", "r1", "", []byte(body))
		if code != http.StatusBadRequest {
			t.Errorf("complete with a bad outcome = %d %s, want 400", code, data)
		}
	}

	// The client sees the finished job.
	code, _, data = e.do(http.MethodGet, fmt.Sprintf("/v1/jobs/%d", jobID), "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("client reads the finished job: %d %s", code, data)
	}
	job := decode[struct {
		Status string `json:"status"`
		Result struct {
			Result    string `json:"result"`
			SessionID string `json:"session_id"`
		} `json:"result"`
		Attempts int64 `json:"attempts"`
	}](t, data)
	if job.Status != "done" || job.Result.Result == "" || job.Result.SessionID != "sess-1" || job.Attempts != 1 {
		t.Errorf("finished job = %s, want done with the result, the written-back session and one attempt", data)
	}

	// The completion queued the task's review job (§7) on the same key;
	// r1 takes it and delivers a verdict, so the key is free for the
	// turn below and the review's own flow is exercised end to end.
	_, data = e.lease("r1", 0)
	review := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job
	if review == nil || review.Kind != "review" {
		t.Fatalf("lease of the review job = %+v, want the queued review", review)
	}
	verdictResult, err := json.Marshal(map[string]string{
		"verdict":  "approve",
		"findings": strings.Repeat("looks right: the work matches the spec. ", 3),
	})
	if err != nil {
		t.Fatalf("encode the verdict: %v", err)
	}
	verdict := fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"result":%q}`,
		review.JobID, review.LeaseID, string(verdictResult))
	code, _, data = e.do(http.MethodPost, "/v1/runner/complete", "r1", "", []byte(verdict))
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "ok" {
		t.Fatalf("complete the review: %d %s, want ok", code, data)
	}

	// Release: the job goes back to queued, the releaser does not see it
	// again until the exclusion lapses, another runner takes it (§12.3).
	// The verdict finished the plan, so the engine's own wake queued this
	// turn already (§4.2) — the submit above coalesced behind it — and it
	// waits for the project's foreman harness like any turn.
	// Both runners offer the project's foreman pair (claude/sonnet): a turn
	// is leased only to an agent offering exactly its pair.
	e.registerForeman("r1")
	e.registerForeman("r2")
	turn := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"wake up"}`,
		e.project1, e.project1)
	code, _, data = e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(turn))
	if code != http.StatusOK {
		t.Fatalf("submit the foreman turn: %d %s, want ok", code, data)
	}
	code, _, data = e.do(http.MethodPost, "/v1/runner/lease", "r1", "",
		[]byte(`{"wait":0,"harnesses":["claude"]}`))
	if code != http.StatusOK {
		t.Fatalf("lease the foreman turn: %d %s, want ok", code, data)
	}
	g2 := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job
	if g2 == nil {
		t.Fatalf("lease of the foreman turn answered no job: %s", data)
	}
	code, _, data = e.do(http.MethodPost, "/v1/runner/release", "r1", "",
		[]byte(fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"reason":"wrong repo"}`, g2.JobID, g2.LeaseID)))
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "ok" {
		t.Fatalf("release = %d %s, want ok", code, data)
	}
	_, data = e.lease("r1", 0)
	if job := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job; job != nil {
		t.Errorf("the releaser leased its own released job back: %+v", job)
	}
	// The turn is session-mode and its session sits on r1 (§8), so r2
	// cannot take it either; once the exclusion lapses, r1 can.
	code, _, data = e.do(http.MethodPost, "/v1/runner/lease", "r2", "",
		[]byte(`{"wait":0,"harnesses":["claude"]}`))
	if code != http.StatusOK {
		t.Fatalf("lease by r2: %d %s, want ok", code, data)
	}
	if job := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job; job != nil {
		t.Errorf("another runner leased a session pinned elsewhere: %+v", job)
	}
	e.clock.Advance(2*time.Minute + time.Second)
	code, _, data = e.do(http.MethodPost, "/v1/runner/lease", "r1", "",
		[]byte(`{"wait":0,"harnesses":["claude"]}`))
	if code != http.StatusOK {
		t.Fatalf("lease by r1 after the exclusion: %d %s, want ok", code, data)
	}
	g3 := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job
	if g3 == nil || g3.JobID != g2.JobID || g3.LeaseID == g2.LeaseID {
		t.Errorf("the session's runner after the exclusion = %+v, want the same job under a fresh lease", g3)
	}
}

func TestLeaseLongPoll(t *testing.T) {
	e := newEnv(t)
	e.register("r1")

	// With nothing to lease, the wait runs out and the answer is an
	// explicit no-job, not an error.
	start := time.Now()
	code, _, data := e.do(http.MethodPost, "/v1/runner/lease", "r1", "", []byte(`{"wait":1}`))
	if code != http.StatusOK {
		t.Fatalf("empty lease: %d %s", code, data)
	}
	if job := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job; job != nil {
		t.Fatalf("empty lease answered a job: %s", data)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("empty lease returned after %v, want it to wait the given second", elapsed)
	}

	// A job arriving mid-wait ends the poll at once.
	type reply struct {
		code int
		data []byte
	}
	answered := make(chan reply, 1)
	go func() {
		code, _, data, err := e.call(http.MethodPost, "/v1/runner/lease", e.bearer("r1"), "", []byte(`{"wait":30}`))
		if err != nil {
			code, data = 0, []byte(err.Error())
		}
		answered <- reply{code, data}
	}()
	time.Sleep(50 * time.Millisecond)
	e.submitWork()
	select {
	case r := <-answered:
		if r.code != http.StatusOK {
			t.Fatalf("mid-wait lease: %d %s", r.code, r.data)
		}
		if job := decode[struct {
			Job *jobGrant `json:"job"`
		}](t, r.data).Job; job == nil {
			t.Fatalf("mid-wait lease answered no job: %s", r.data)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("mid-wait lease returned after %v, want it to notice the job at once", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the mid-wait lease never answered")
	}
}

func TestStaleEpochThroughAPI(t *testing.T) {
	e := newEnv(t)
	e.register("r1")
	jobID := e.submitWork()
	_, data := e.lease("r1", 0)
	g := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job
	if g == nil {
		t.Fatalf("lease answered no job: %s", data)
	}

	// A recover fences the pre-bump leases out (§6.4).
	code, _, data := e.do(http.MethodPost, "/v1/admin/fence-epoch", "root", "", nil)
	if code != http.StatusOK {
		t.Fatalf("bump fence epoch: %d %s", code, data)
	}
	if got := decode[struct {
		Epoch int `json:"epoch"`
	}](t, data).Epoch; got != 2 {
		t.Fatalf("epoch after the bump = %d, want 2", got)
	}
	for _, call := range []struct {
		path string
		body string
	}{
		{"heartbeat", fmt.Sprintf(`{"job_id":%d,"lease_id":%q}`, g.JobID, g.LeaseID)},
		{"complete", fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"result":"done"}`, g.JobID, g.LeaseID)},
		{"release", fmt.Sprintf(`{"job_id":%d,"lease_id":%q}`, g.JobID, g.LeaseID)},
	} {
		code, _, data := e.do(http.MethodPost, "/v1/runner/"+call.path, "r1", "", []byte(call.body))
		if code != http.StatusOK {
			t.Errorf("%s after the bump: %d %s, want 200", call.path, code, data)
			continue
		}
		if got := decode[answerBody](t, data).Answer; got != "stale_epoch" {
			t.Errorf("%s after the bump answered %q, want stale_epoch", call.path, got)
		}
	}
	// The job itself is untouched: the runner was fenced out, not fired.
	code, _, data = e.do(http.MethodGet, fmt.Sprintf("/v1/jobs/%d", jobID), "alice", "", nil)
	if code != http.StatusOK || decode[struct {
		Status string `json:"status"`
	}](t, data).Status != "leased" {
		t.Errorf("the leased job after the bump = %d %s, want leased still", code, data)
	}
}

// TestRegisterPersistsAgents covers §5.1 through the API: the agents a
// register carries are upserted into the agents table keyed by runner,
// adapter and model, and a re-register replaces the runner's set.
func TestRegisterPersistsAgents(t *testing.T) {
	e := newEnv(t)
	body := `{"agents":[
		{"adapter":"claude","harness":"claude","model":"sonnet","family":"anthropic","account":"work","classes":["implement","review"]},
		{"adapter":"kimi","harness":"kimi","model":"k2","family":"moonshot","account":"work","classes":["implement"]}
	]}`
	code, _, data := e.do(http.MethodPost, "/v1/runner/register", "r1", "", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("register with agents: %d %s", code, data)
	}
	if got := decode[struct {
		Agents int `json:"agents"`
	}](t, data).Agents; got != 2 {
		t.Errorf("register echoed %d agents, want 2", got)
	}
	var runnerID int64
	var classes string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT a.runner_id, a.classes FROM agents a JOIN runners r ON r.id = a.runner_id
			WHERE r.name = 'r1' AND a.adapter = 'claude' AND a.model = 'sonnet'`).Scan(&runnerID, &classes)
	}); err != nil {
		t.Fatalf("the advertisement was not persisted: %v", err)
	}
	if classes != `["implement","review"]` {
		t.Errorf("classes = %s, want the advertised ones", classes)
	}

	// A re-register replaces the set: kimi is gone, codex arrives.
	replace := `{"agents":[{"adapter":"codex","harness":"codex","model":"gpt","family":"openai","account":"work","classes":["review"]}]}`
	code, _, data = e.do(http.MethodPost, "/v1/runner/register", "r1", "", []byte(replace))
	if code != http.StatusOK {
		t.Fatalf("re-register: %d %s", code, data)
	}
	var n, kimi int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow("SELECT COUNT(*) FROM agents WHERE runner_id = ?", runnerID).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow("SELECT COUNT(*) FROM agents WHERE runner_id = ? AND adapter = 'kimi'", runnerID).Scan(&kimi)
	}); err != nil {
		t.Fatalf("read agents: %v", err)
	}
	if n != 1 || kimi != 0 {
		t.Errorf("agents after re-register = %d (kimi %d), want the set replaced", n, kimi)
	}

	// An agent missing its key fields is the caller's mistake: 400.
	bad := `{"agents":[{"adapter":"claude","model":"sonnet"}]}`
	code, _, data = e.do(http.MethodPost, "/v1/runner/register", "r2", "", []byte(bad))
	if code != http.StatusBadRequest {
		t.Errorf("register an agent without a harness: %d %s, want 400", code, data)
	}
}

// TestClientRunnerProtocol runs the client side of §12.3 — the same
// code the runner uses — against the server over the socket: register,
// lease, heartbeat with progress, release.
func TestClientRunnerProtocol(t *testing.T) {
	e := newEnv(t)
	c := NewClient(e.srv.Listener.Addr().String(), e.secrets["r1"])
	if err := c.Register(e.ctx, []Agent{{
		Adapter: "fake", Harness: "fake", Model: "fake-1",
		Family: "testfam", Account: "acct-1", Classes: []string{"implement"},
	}}, 0); err != nil {
		t.Fatalf("client register: %v", err)
	}
	jobID := e.submitWork()

	grant, err := c.Lease(e.ctx, 0, []string{"claude"}, []string{"linux"})
	if err != nil {
		t.Fatalf("client lease: %v", err)
	}
	if grant == nil || grant.JobID != jobID {
		t.Fatalf("client lease = %+v, want the submitted job", grant)
	}
	// The grant carries the task's workspace fields (§5.1).
	if grant.RepoURL == "" || grant.BaseRef == "" || grant.CheckCmd == "" || grant.Attempt != 1 {
		t.Errorf("grant fields = %+v, want the task's repo, base ref, check and attempt", grant)
	}

	ans, expires, err := c.Heartbeat(e.ctx, grant.JobID, grant.LeaseID, Progress{
		LastProgressAt: time.Now(),
		Events:         []string{"tool_start Edit"},
	})
	if err != nil || ans != engine.AnsOK || expires.IsZero() {
		t.Fatalf("client heartbeat = %s, %v, %v; want ok with an expiry", ans, expires, err)
	}
	ans, err = c.Release(e.ctx, grant.JobID, grant.LeaseID, "done testing")
	if err != nil || ans != engine.AnsOK {
		t.Fatalf("client release = %s, %v; want ok", ans, err)
	}
	// The releaser holds nothing anymore: a late complete for the same
	// lease is lost (§12.3).
	ans, err = c.Complete(e.ctx, grant.JobID, grant.LeaseID, Completion{Result: `{"status":"done"}`})
	if err != nil || ans != engine.AnsLost {
		t.Fatalf("complete after release = %s, %v; want lost", ans, err)
	}
	// A wrong token is an error, not an answer.
	if err := NewClient(e.srv.Listener.Addr().String(), "not-the-secret").Register(e.ctx, nil, 0); err == nil {
		t.Error("an unknown token was accepted, want an error")
	}
}
