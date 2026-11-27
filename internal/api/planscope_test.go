package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
)

// TestStatusAndHistoryPlanScope covers the plan parameter of
// /v1/status and /v1/history (§10, §9): the plan's own status and
// timeline, a 404 for an unknown plan and a 400 for a malformed one.
func TestStatusAndHistoryPlanScope(t *testing.T) {
	e := newEnv(t)
	e.submitWork()
	e.exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
		VALUES ('plan_done', ?, NULL, NULL, ?, ?)`, e.project1, fmt.Sprintf(`{"plan":%d}`, e.plan), seedTS)

	code, _, data := e.do(http.MethodGet, fmt.Sprintf("/v1/status?plan=%d", e.plan), "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("plan status: %d %s", code, data)
	}
	st := decode[Status](t, data)
	if len(st.Plans) != 1 || st.Plans[0].ID != e.plan || st.PlanScope == nil || st.PlanScope.PlanID != e.plan {
		t.Errorf("plan status = %+v", st)
	}
	if len(st.Jobs) != 0 || len(st.Skills) != 0 {
		t.Errorf("global sections in the plan status: %+v", st)
	}
	if code, _, _ := e.do(http.MethodGet, "/v1/status?plan=9999", "alice", "", nil); code != http.StatusNotFound {
		t.Errorf("unknown plan status: %d, want 404", code)
	}
	if code, _, _ := e.do(http.MethodGet, "/v1/status?plan=x", "alice", "", nil); code != http.StatusBadRequest {
		t.Errorf("malformed plan: %d, want 400", code)
	}

	code, _, data = e.do(http.MethodGet, fmt.Sprintf("/v1/history?plan=%d", e.plan), "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("plan history: %d %s", code, data)
	}
	page := decode[history.Page](t, data)
	var done bool
	for _, it := range page.Items {
		done = done || it.Kind == "plan_done"
	}
	if !done || len(page.Items) < 2 {
		t.Errorf("plan history = %+v", page.Items)
	}
	if code, _, _ := e.do(http.MethodGet, "/v1/history?plan=x", "alice", "", nil); code != http.StatusBadRequest {
		t.Errorf("malformed history plan: %d, want 400", code)
	}
}

// TestPlanStatusSections renders the compact summary: counts, blockers,
// land head, spend and recent transitions, with none of the global
// sections.
func TestPlanStatusSections(t *testing.T) {
	st := Status{
		Jobs: map[string]int{"queued": 3},
		Plans: []engine.StatusPlan{{ID: 7, State: "active", Title: "ship it", Tasks: []engine.StatusTask{
			{ID: 1, State: "done", Class: "implement", Landed: true, LandedCommit: "abc1234"},
			{ID: 2, State: "needs_foreman", LandReady: "held by rule land_ready: no"},
			{ID: 3, State: "needs_foreman"},
		}}},
		NeedsYou: []engine.NeedsYouItem{{Kind: "task", ID: 2, State: "needs_foreman", Reason: "which API?"}},
		Skills:   []engine.StatusSkill{{Name: "cli-flag"}},
		PlanScope: &engine.PlanScope{PlanID: 7, Tokens: 1200, CostUSD: 0.5, LandHead: "abc1234",
			Recent: []history.Item{{Type: history.TypeEvent, Kind: "plan_done", CreatedAt: "2026-11-09T10:00:00Z"}}},
	}
	out := PlanStatusSections(st, time.Now())
	for _, want := range []string{"plan #7 active ship it", "tasks (3): done 1, needs_foreman 2", "task #2 needs_foreman: which API?",
		"gate: task #2 held by rule land_ready: no", "land head: abc1234", "spend: 1200 tokens, $0.50", "recent:"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"jobs:", "cli-flag", "foreman report"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("summary holds the global %q:\n%s", unwanted, out)
		}
	}
}
