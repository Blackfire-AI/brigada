package api

import (
	"fmt"
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

	// Release: the job goes back to queued, the releaser does not see it
	// again until the exclusion lapses, another runner takes it (§12.3).
	turn := fmt.Sprintf(`{"key":"foreman:%d","project_id":%d,"kind":"foreman","mode":"session","prompt":"wake up"}`,
		e.project1, e.project1)
	code, _, data = e.do(http.MethodPost, "/v1/jobs", "root", "", []byte(turn))
	if code != http.StatusOK {
		t.Fatalf("submit the foreman turn: %d %s", code, data)
	}
	_, data = e.lease("r1", 0)
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
	_, data = e.lease("r2", 0)
	if job := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job; job != nil {
		t.Errorf("another runner leased a session pinned elsewhere: %+v", job)
	}
	e.clock.Advance(2*time.Minute + time.Second)
	_, data = e.lease("r1", 0)
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
	time.Sleep(200 * time.Millisecond)
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
