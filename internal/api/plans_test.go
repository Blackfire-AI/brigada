package api

import (
	"database/sql"
	"net/http"
	"strconv"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestPlanCreateEndpoint covers /v1/plans (§6.2, §12.1): the foreman
// creates a plan in its own project without naming it, a repo URL that
// names another project is a 403, a client or admin must name the
// project's repo URL, and an idempotent repeat returns the first
// result.
func TestPlanCreateEndpoint(t *testing.T) {
	e := newEnv(t)

	// The role check: a runner token may not create plans.
	if code, _, _ := e.do(http.MethodPost, "/v1/plans", "r1", "", []byte(`{}`)); code != http.StatusForbidden {
		t.Fatalf("runner creates a plan: %d, want 403", code)
	}

	// The foreman creates in its own project, naming no repo (§12.1:
	// a foreman token's project is the only one it may plan in).
	body := `{"title":"the plan","tasks":[{"title":"t","spec":"the spec","check":"go test ./...","tier":"heavy","effort":"max"}]}`
	code, _, data := e.do(http.MethodPost, "/v1/plans", "foreman1", "plan-1", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("foreman creates a plan: %d %s", code, data)
	}
	res := decode[planCreateBody](t, data)
	if res.Answer != "created" || res.PlanID == 0 || res.TaskID == 0 || res.JobID == 0 {
		t.Fatalf("plan create = %+v, want created with plan, task and job", res)
	}
	planInProject(t, e, res.PlanID, e.project1)
	code, _, data = e.do(http.MethodGet, "/v1/tasks/"+strconv.FormatInt(res.TaskID, 10), "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("read task: %d %s", code, data)
	}
	taskInfo := decode[engine.TaskInfo](t, data)
	if taskInfo.Tier != "heavy" || taskInfo.EffectiveTier != "heavy" || taskInfo.Effort != "max" {
		t.Errorf("round trip = %+v", taskInfo)
	}

	// A repo URL that names another project is a mismatch: 403, the
	// foreman's scope named.
	other := `{"repo_url":"https://example.invalid/two.git","title":"x","tasks":[{"spec":"s"}]}`
	if code, _, data = e.do(http.MethodPost, "/v1/plans", "foreman1", "", []byte(other)); code != http.StatusForbidden {
		t.Fatalf("foreman plans another project: %d %s, want 403", code, data)
	}
	if err := decode[errBody](t, data); err.Code != codeForbidden {
		t.Errorf("scope error code = %q, want %q", err.Code, codeForbidden)
	}
	// A repo no project carries is a mismatch the same way.
	unknown := `{"repo_url":"https://example.invalid/nope.git","title":"x","tasks":[{"spec":"s"}]}`
	if code, _, _ = e.do(http.MethodPost, "/v1/plans", "foreman1", "", []byte(unknown)); code != http.StatusForbidden {
		t.Errorf("foreman plans an unknown repo: %d, want 403", code)
	}

	// A client must name the project's repo URL (§12.1).
	if code, _, data = e.do(http.MethodPost, "/v1/plans", "alice", "", []byte(body)); code != http.StatusBadRequest {
		t.Fatalf("client creates without a repo: %d %s, want 400", code, data)
	}
	if code, _, data = e.do(http.MethodPost, "/v1/plans", "alice", "", []byte(unknown)); code != http.StatusNotFound {
		t.Fatalf("client names an unknown repo: %d %s, want 404", code, data)
	}
	named := `{"repo_url":"https://example.invalid/one.git","title":"by client","tasks":[{"spec":"s"}]}`
	if code, _, data = e.do(http.MethodPost, "/v1/plans", "alice", "", []byte(named)); code != http.StatusOK {
		t.Fatalf("client creates a plan: %d %s", code, data)
	}
	clientRes := decode[planCreateBody](t, data)
	planInProject(t, e, clientRes.PlanID, e.project1)

	// An idempotent repeat returns the first result, plan and all.
	code, _, repeat := e.do(http.MethodPost, "/v1/plans", "foreman1", "plan-1", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("idempotent repeat: %d %s, want 200", code, repeat)
	}
	if rep := decode[planCreateBody](t, repeat); rep != res {
		t.Errorf("repeat = %+v, want the first answer %+v", rep, res)
	}
	// One seeded plan, the foreman's and the client's — no more.
	if got := planCount(t, e); got != 3 {
		t.Errorf("plans after the repeat = %d, want 3: the repeat created nothing", got)
	}
	// The same key with another body is a 409 (§12.1).
	if code, _, _ = e.do(http.MethodPost, "/v1/plans", "foreman1", "plan-1", []byte(named)); code != http.StatusConflict {
		t.Errorf("same key, another body: %d, want 409", code)
	}

	// The engine's refusals ride as 400s naming the rule (§12.1).
	for name, bad := range map[string]string{
		"no title":   `{"tasks":[{"spec":"s"}]}`,
		"no tasks":   `{"title":"x","tasks":[]}`,
		"a cycle":    `{"title":"x","tasks":[{"name":"a","spec":"one","deps":["b"]},{"name":"b","spec":"two","deps":["a"]}]}`,
		"outside":    `{"title":"x","tasks":[{"name":"a","spec":"one","deps":["zzz"]}]}`,
		"no spec":    `{"title":"x","tasks":[{"title":"t"}]}`,
		"bad tier":   `{"title":"x","tasks":[{"spec":"s","tier":"big"}]}`,
		"bad effort": `{"title":"x","tasks":[{"spec":"s","effort":"ultra"}]}`,
		"bad encode": `{"title":42}`,
	} {
		if code, _, data = e.do(http.MethodPost, "/v1/plans", "foreman1", "", []byte(bad)); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s, want 400", name, code, data)
		}
	}
	if got := planCount(t, e); got != 3 {
		t.Errorf("plans after the refusals = %d, want 3", got)
	}
}

// planInProject reads one plan's project and state from the store.
func planInProject(t *testing.T, e *env, plan, project int64) {
	t.Helper()
	var state string
	var got int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT state, project_id FROM plans WHERE id = ?", plan).Scan(&state, &got)
	}); err != nil {
		t.Fatalf("read plan %d: %v", plan, err)
	}
	if state != "active" || got != project {
		t.Errorf("plan %d = %s in project %d, want active in project %d", plan, state, got, project)
	}
}

func planCount(t *testing.T, e *env) int64 {
	t.Helper()
	var n int64
	if err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM plans").Scan(&n)
	}); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	return n
}

// planCreateBody is the endpoint's answer as the tests decode it.
type planCreateBody struct {
	Answer string `json:"answer"`
	PlanID int64  `json:"plan_id"`
	TaskID int64  `json:"task_id"`
	JobID  int64  `json:"job_id"`
}
