package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestForemanMutationNeedsNoHoldGuard: a foreman
// mutation is authorised by the foreman token alone — no hold, no fence
// header — and the interactive hold's routes are gone.
func TestForemanMutationNeedsNoHoldGuard(t *testing.T) {
	e := newEnv(t)
	var holds int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM attach_holds WHERE state = 'live'").Scan(&holds)
	}); err != nil || holds != 0 {
		t.Fatalf("live holds = %d (%v), want none", holds, err)
	}
	body := `{"title":"no hold","tasks":[{"spec":"the spec"}]}`
	code, _, data := e.do(http.MethodPost, "/v1/plans", "foreman1", "no-hold-plan", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("foreman creates a plan without a hold: %d %s", code, data)
	}
	if res := decode[planCreateBody](t, data); res.Answer != "created" || res.PlanID == 0 {
		t.Fatalf("plan create = %+v, want created", res)
	}
	for _, path := range []string{"/v1/foreman/hold/acquire", "/v1/foreman/hold/yield", "/v1/foreman/decision-note"} {
		if code, _, _ := e.do(http.MethodPost, path, "foreman1", "", []byte(`{}`)); code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404: the route is retired", path, code)
		}
	}
}

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
	// The task's check is the project's default, so the plan gate passes it.
	if err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE projects SET settings = '{"default_check": "go test ./..."}' WHERE id = ?`, e.project1)
		return err
	}); err != nil {
		t.Fatalf("set the default check: %v", err)
	}
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

// A respec round-trips through the codecs: the spec of a pending task is
// appended to and replaced, and a task that has started is refused.
func TestPlanAmendRespecRoundTrip(t *testing.T) {
	ctx := t.Context()
	st := openFreshStore(t, filepath.Join(t.TempDir(), "store.db"))
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, engine.Config{PlanReview: &planReviewOff})
	if _, err := eng.SetProjectForeman(ctx, "https://example.invalid/respec.git", "claude", "sonnet"); err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte(tokenFileBody), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(ctx, st, eng, nil, tokens)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClientWithHTTPClient(&http.Client{Transport: funcTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w.Result(), nil
	})}, "foreman-secret-1")
	plan, err := c.CreatePlan(ctx, PlanCreate{Title: "respec", Tasks: []PlanTask{
		{Name: "build", Spec: "build it"},
		{Name: "ship", Spec: "ship it", Deps: []string{"build"}},
	}}, "respec-plan")
	if err != nil {
		t.Fatal(err)
	}
	spec := func(name string) string {
		var s string
		if err := st.View(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT spec FROM tasks WHERE name = ?", name).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	am := PlanAmend{Respec: []PlanRespec{{Task: "ship", Spec: "and tag it", Mode: "append"}}}
	res, err := c.AmendPlan(ctx, plan.PlanID, am, "respec-call")
	if err != nil || len(res.Respecced) != 1 {
		t.Fatalf("amend = %+v (%v)", res, err)
	}
	again, err := c.AmendPlan(ctx, plan.PlanID, am, "respec-call")
	if err != nil || !reflect.DeepEqual(again, res) {
		t.Fatalf("idempotent respec = %+v (%v)", again, err)
	}
	if got := spec("ship"); got != "ship it\n\nand tag it" {
		t.Errorf("appended spec = %q", got)
	}
	if _, err := c.AmendPlan(ctx, plan.PlanID, PlanAmend{Respec: []PlanRespec{{Task: "ship", Spec: "new"}}}, "respec-replace"); err != nil {
		t.Fatal(err)
	}
	if got := spec("ship"); got != "new" {
		t.Errorf("replaced spec = %q", got)
	}
	_, err = c.AmendPlan(ctx, plan.PlanID, PlanAmend{Respec: []PlanRespec{{Task: "build", Spec: "x"}}}, "respec-started")
	if err == nil || !strings.Contains(err.Error(), "follow-up task") {
		t.Errorf("started task err = %v, want the follow-up advice", err)
	}
}

// Exercise the real request and response codecs with authenticated routes.
func TestPlanAmendRetierRoundTrip(t *testing.T) {
	ctx := t.Context()
	st := openFreshStore(t, filepath.Join(t.TempDir(), "store.db"))
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, engine.Config{PlanReview: &planReviewOff})
	repo := "https://example.invalid/retier.git"
	if _, err := eng.SetProjectForeman(ctx, repo, "claude", "sonnet"); err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte(tokenFileBody), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(ctx, st, eng, nil, tokens)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClientWithHTTPClient(&http.Client{Transport: funcTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w.Result(), nil
	})}, "foreman-secret-1")
	plan, err := c.CreatePlan(ctx, PlanCreate{Title: "retier", Tasks: []PlanTask{{Name: "build", Spec: "build it", Tier: "standard", Effort: "high"}}}, "retier-plan")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE tasks SET state = 'needs_foreman', effective_tier = 'heavy' WHERE id = ?", plan.TaskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	am := PlanAmend{Retier: []PlanRetier{{Task: "build", Tier: "light", Effort: "low"}}}
	res, err := c.AmendPlan(ctx, plan.PlanID, am, "retier-call")
	if err != nil || !reflect.DeepEqual(res.Retiered, []int64{plan.TaskID}) {
		t.Fatalf("amend = %+v (%v)", res, err)
	}
	again, err := c.AmendPlan(ctx, plan.PlanID, am, "retier-call")
	if err != nil || !reflect.DeepEqual(again, res) {
		t.Fatalf("idempotent retier = %+v (%v)", again, err)
	}
	var settings string
	if err := st.View(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT tier || '|' || effective_tier || '|' || effort || '|' || state FROM tasks WHERE id = ?", plan.TaskID).Scan(&settings)
	}); err != nil {
		t.Fatal(err)
	}
	if settings != "light|light|low|needs_foreman" {
		t.Errorf("retier = %s", settings)
	}
	_, err = c.AmendPlan(ctx, plan.PlanID, PlanAmend{Retier: []PlanRetier{{Task: strconv.FormatInt(plan.TaskID, 10), Effort: "ultra"}}}, "bad-retier")
	if err == nil || !strings.Contains(err.Error(), "effort") {
		t.Errorf("invalid effort accepted: %v", err)
	}
}

// TestPlanSkillsRoundTrip: the skills field of a task rides plan_create,
// plan_amend's respec and retier, and the task submit through the API to
// the engine's resolver — the ordered names recorded on the task, an
// absent field keeping the recorded skills, [] clearing them — and an
// unknown name refuses the call naming the task and the known skills.
func TestPlanSkillsRoundTrip(t *testing.T) {
	e := newEnv(t)
	const repo = "https://example.invalid/one.git"
	if _, err := e.eng.ImportSkills(e.ctx, repo, []engine.Skill{
		{Name: "cli-flag", Text: "register the flag"},
		{Name: "docs-change", Text: "update the docs"},
	}); err != nil {
		t.Fatalf("import skills: %v", err)
	}
	skillNamesOf := func(task int64) []string {
		t.Helper()
		var raw string
		if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT skills FROM tasks WHERE id = ?", task).Scan(&raw)
		}); err != nil {
			t.Fatalf("read the skills of task %d: %v", task, err)
		}
		var s []struct {
			Name string `json:"name"`
		}
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &s); err != nil {
				t.Fatalf("parse the skills %q: %v", raw, err)
			}
		}
		out := make([]string, len(s))
		for i, x := range s {
			out[i] = x.Name
		}
		return out
	}

	body := `{"title":"skilled","tasks":[{"name":"build","spec":"build it","skills":["docs-change","cli-flag"]},{"name":"ship","spec":"ship it","deps":["build"],"skills":["cli-flag"]}]}`
	code, _, data := e.do(http.MethodPost, "/v1/plans", "foreman1", "skills-1", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("plan_create: %d %s", code, data)
	}
	res := decode[planCreateBody](t, data)
	if got := skillNamesOf(res.TaskID); len(got) != 2 || got[0] != "docs-change" || got[1] != "cli-flag" {
		t.Errorf("first task skills = %v, want the order given", got)
	}
	var ship int64
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT id FROM tasks WHERE plan_id = ? AND name = 'ship'", res.PlanID).Scan(&ship)
	}); err != nil {
		t.Fatalf("read ship: %v", err)
	}

	amend := func(idem, body string) (int, []byte) {
		t.Helper()
		path := "/v1/plans/" + strconv.FormatInt(res.PlanID, 10) + "/amend"
		code, _, data := e.do(http.MethodPost, path, "foreman1", idem, []byte(body))
		return code, data
	}
	if code, data := amend("skills-2", `{"respec":[{"task":"ship","spec":"ship more","skills":["docs-change"]}]}`); code != http.StatusOK {
		t.Fatalf("respec with skills: %d %s", code, data)
	}
	if got := skillNamesOf(ship); len(got) != 1 || got[0] != "docs-change" {
		t.Errorf("after a respec with skills = %v, want docs-change", got)
	}
	if code, data := amend("skills-3", `{"respec":[{"task":"ship","spec":"ship more"}]}`); code != http.StatusOK {
		t.Fatalf("respec without skills: %d %s", code, data)
	}
	if got := skillNamesOf(ship); len(got) != 1 || got[0] != "docs-change" {
		t.Errorf("after a respec without skills = %v, want the recorded skill kept", got)
	}
	if code, data := amend("skills-4", `{"retier":[{"task":"ship","tier":"light","skills":[]}]}`); code != http.StatusOK {
		t.Fatalf("retier with skills: %d %s", code, data)
	}
	if got := skillNamesOf(ship); len(got) != 0 {
		t.Errorf("after a retier with [] = %v, want none", got)
	}
	if code, data := amend("skills-5", `{"respec":[{"task":"ship","spec":"ship more","skills":["nope"]}]}`); code != http.StatusBadRequest ||
		!strings.Contains(string(data), "nope") || !strings.Contains(string(data), "cli-flag, docs-change") {
		t.Errorf("an unknown skill: %d %s, want a 400 naming it and the known skills", code, data)
	}

	sub := `{"repo_url":"` + repo + `","base_ref":"main","spec":"direct","skills":["cli-flag"]}`
	code, _, data = e.do(http.MethodPost, "/v1/tasks", "root", "skills-6", []byte(sub))
	if code != http.StatusOK {
		t.Fatalf("task submit: %d %s", code, data)
	}
	direct := decode[struct {
		TaskID int64 `json:"task_id"`
	}](t, data)
	if got := skillNamesOf(direct.TaskID); len(got) != 1 || got[0] != "cli-flag" {
		t.Errorf("a direct submission's skills = %v, want cli-flag", got)
	}
	if got, meta := e.str("SELECT instructions FROM tasks WHERE id = ?", direct.TaskID), e.str("SELECT instructions_meta FROM tasks WHERE id = ?", direct.TaskID); got != "" || !strings.Contains(meta, `"source":"default"`) {
		t.Errorf("a direct submission recorded worker text %q with meta %q, want none with the not-configured source", got, meta)
	}
}
