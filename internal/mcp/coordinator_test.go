package mcp

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// startCoordinator runs the coordinator bridge (§12.2, `brigada mcp
// --coordinator`) over pipes, bearing the env's client token.
func startCoordinator(t *testing.T, e *env) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := &Server{Client: e.client, Tools: CoordinatorTools}
	go func() { _ = srv.Serve(context.Background(), inR, outW) }()
	t.Cleanup(func() {
		inW.Close()
		outW.Close()
	})
	return &session{t: t, w: inW, r: bufio.NewReader(outR)}
}

const coordRepo = "https://example.invalid/x.git"

// waitingPlan creates a plan in the seeded project that the approval
// limit holds (a class outside the allowed set) and returns its id.
func waitingPlan(t *testing.T, e *env, key string) int64 {
	t.Helper()
	res, err := e.client.CreatePlan(t.Context(), api.PlanCreate{
		RepoURL: coordRepo,
		Title:   "ship it " + key,
		Tasks:   []api.PlanTask{{Title: "deploy", Spec: "deploy it", Class: "deploy"}},
	}, "create-"+key)
	if err != nil {
		t.Fatalf("create the plan: %v", err)
	}
	if res.State != "pending_approval" {
		t.Fatalf("plan %d = %s, want pending_approval", res.PlanID, res.State)
	}
	return res.PlanID
}

func amendWaiting(t *testing.T, e *env, plan int64, key string) {
	t.Helper()
	_, err := e.client.AmendPlan(t.Context(), plan, api.PlanAmend{
		Add: []api.PlanTask{{Title: "extra", Spec: "one more", Class: "deploy"}},
	}, "amend-"+key)
	if err != nil {
		t.Fatalf("amend plan %d: %v", plan, err)
	}
}

func planStateOf(t *testing.T, e *env, plan int64) string {
	t.Helper()
	var state string
	err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT state FROM plans WHERE id = ?", plan).Scan(&state)
	})
	if err != nil {
		t.Fatalf("read plan %d: %v", plan, err)
	}
	return state
}

// TestCoordinatorBridgeOffer: the coordinator offers its six tools and
// nothing that plans or edits (§4.6, §12.2): no plan_create, no
// plan_amend, no task mutation, no foreman answer, no hold tool. A call
// to any of them is the protocol's unknown-tool refusal.
func TestCoordinatorBridgeOffer(t *testing.T) {
	s := startCoordinator(t, newEnv(t))
	a := s.call(1, "tools/list", nil)
	var res struct {
		Tools []toolInfo `json:"tools"`
	}
	if a.Error != nil || json.Unmarshal(a.Result, &res) != nil {
		t.Fatalf("tools/list = %+v", a)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if got, want := strings.Join(names, ","), "list_projects,ask_foreman,status,history,plan_approve,plan_reject"; got != want {
		t.Fatalf("coordinator tools = %s, want %s", got, want)
	}
	if got := strings.Join(CoordinatorToolNames(), ","); got != strings.Join(names, ",") {
		t.Errorf("CoordinatorToolNames() = %s, want the offer %s", got, strings.Join(names, ","))
	}
	for i, name := range []string{"plan_create", "plan_amend", "task_restart", "task_cancel", "land_retry", "answer", "decision_note", "hold_acquire", "hold_yield", "events", "capacity", "ask_user"} {
		a = s.call(10+i, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}})
		if a.Error == nil || a.Error.Code != codeInvalidParams || !strings.Contains(a.Error.Message, "unknown tool") {
			t.Errorf("%s on the coordinator bridge = %+v, want an unknown tool", name, a.Error)
		}
	}
	// The foreman's own offer still has the plan tools: the coordinator
	// set is a separate list, not an edit of that one.
	if !strings.Contains(strings.Join(ToolNames(), ","), "plan_create") {
		t.Errorf("the foreman offer lost plan_create: %v", ToolNames())
	}
	for _, name := range ToolNames() {
		if name == "plan_approve" || name == "plan_reject" {
			t.Errorf("the foreman offer carries %s: a foreman credential never approves", name)
		}
	}
}

// TestCoordinatorReadsAndAsks: list_projects names the project by its
// URL, status and history take a project or none, and ask_foreman needs
// the URL.
func TestCoordinatorReadsAndAsks(t *testing.T) {
	s := startCoordinator(t, newEnv(t))
	call := func(id int, name string, args map[string]any) (string, bool) {
		t.Helper()
		return toolText(t, s.call(id, "tools/call", map[string]any{"name": name, "arguments": args}))
	}
	if text, isErr := call(1, "list_projects", map[string]any{}); isErr || !strings.Contains(text, coordRepo+": foreman claude/sonnet") {
		t.Errorf("list_projects = %q (error %v)", text, isErr)
	}
	if text, isErr := call(2, "status", map[string]any{}); isErr || !strings.Contains(text, "engine pid") {
		t.Errorf("status = %q (error %v)", text, isErr)
	}
	if text, isErr := call(3, "status", map[string]any{"repo_url": "example.invalid/x"}); isErr || !strings.Contains(text, "engine pid") {
		t.Errorf("status for one project by its canonical URL = %q (error %v)", text, isErr)
	}
	if text, isErr := call(4, "status", map[string]any{"repo_url": "https://example.invalid/other.git"}); !isErr || !strings.Contains(text, "no project for repo") {
		t.Errorf("status for an unknown project = %q (error %v)", text, isErr)
	}
	if text, isErr := call(5, "history", map[string]any{}); isErr || !strings.Contains(text, "answered") {
		t.Errorf("history = %q (error %v)", text, isErr)
	}
	if text, isErr := call(6, "history", map[string]any{"repo_url": coordRepo}); isErr || !strings.Contains(text, "answered") {
		t.Errorf("history of one project = %q (error %v)", text, isErr)
	}
	if text, isErr := call(7, "ask_foreman", map[string]any{"text": "plan the deploy"}); !isErr || !strings.Contains(text, "repo_url is required") {
		t.Errorf("ask_foreman without a repo_url = %q (error %v)", text, isErr)
	}
	if text, isErr := call(8, "ask_foreman", map[string]any{"repo_url": coordRepo, "text": "plan the deploy"}); isErr || !strings.Contains(text, "waking up") {
		t.Errorf("ask_foreman = %q (error %v)", text, isErr)
	}
}

// TestCoordinatorApproveRefusals: a plan_approve or plan_reject missing
// its project, plan or version is the tool's own error; a version the
// plan has moved past is refused whole and applies nothing; a plan of
// another project, or none, is refused.
func TestCoordinatorApproveRefusals(t *testing.T) {
	e := newEnv(t)
	s := startCoordinator(t, e)
	call := func(id int, name string, args map[string]any) (string, bool) {
		t.Helper()
		return toolText(t, s.call(id, "tools/call", map[string]any{"name": name, "arguments": args}))
	}
	plan := waitingPlan(t, e, "a")

	// The status the coordinator shows the user lists the waiting plan
	// once: its tasks in the plans section, its version and why it
	// waits in the approval block.
	text, isErr := call(1, "status", map[string]any{})
	if isErr || !strings.Contains(text, "plans awaiting approval") || !strings.Contains(text, "version 1 of https://example.invalid/x.git") ||
		!strings.Contains(text, "pending_approval ship it a") || !strings.Contains(text, " pending deploy") {
		t.Fatalf("status = %q (error %v), want the waiting plan with its version, and its tasks in the plans section", text, isErr)
	}

	for i, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"plan": plan, "version": 1}, "repo_url is required"},
		{map[string]any{"repo_url": coordRepo, "version": 1}, "plan is required"},
		{map[string]any{"repo_url": coordRepo, "plan": plan}, "version is required"},
		{map[string]any{"repo_url": "https://example.invalid/other.git", "plan": plan, "version": 1}, "no project for repo"},
		{map[string]any{"repo_url": coordRepo, "plan": plan + 100, "version": 1}, "no such plan"},
	} {
		for j, name := range []string{"plan_approve", "plan_reject"} {
			if got, isErr := call(10+10*i+j, name, tc.args); !isErr || !strings.Contains(got, tc.want) {
				t.Errorf("%s %v = %q (error %v), want an error with %q", name, tc.args, got, isErr, tc.want)
			}
		}
	}
	if got := planStateOf(t, e, plan); got != "pending_approval" {
		t.Fatalf("a refused call changed plan %d to %s", plan, got)
	}

	// An amendment after the user looked invalidates the version they saw.
	amendWaiting(t, e, plan, "a")
	for i, name := range []string{"plan_approve", "plan_reject"} {
		got, isErr := call(100+i, name, map[string]any{"repo_url": coordRepo, "plan": plan, "version": 1})
		if !isErr || !strings.Contains(got, "stale_plan") || !strings.Contains(got, "version 2") {
			t.Errorf("%s on a stale version = %q (error %v), want stale_plan naming version 2", name, got, isErr)
		}
	}
	if got := planStateOf(t, e, plan); got != "pending_approval" {
		t.Fatalf("a stale call changed plan %d to %s", plan, got)
	}
	text, _ = call(102, "status", map[string]any{"repo_url": coordRepo})
	if !strings.Contains(text, "plan "+strconv.FormatInt(plan, 10)+" version 2") {
		t.Errorf("status after the amendment = %q, want version 2", text)
	}

	// The version the user was shown last goes through.
	got, isErr := call(103, "plan_approve", map[string]any{"repo_url": coordRepo, "plan": plan, "version": 2})
	if isErr || !strings.Contains(got, "approved") {
		t.Fatalf("plan_approve at the current version = %q (error %v)", got, isErr)
	}
	if st := planStateOf(t, e, plan); st != "active" {
		t.Errorf("approved plan %d = %s, want active", plan, st)
	}
	// An approved plan is no longer awaiting approval.
	if got, isErr := call(104, "plan_approve", map[string]any{"repo_url": coordRepo, "plan": plan, "version": 2}); !isErr || !strings.Contains(got, "not awaiting approval") {
		t.Errorf("a second approval = %q (error %v), want a refusal", got, isErr)
	}

	// Reject at the current version cancels the plan.
	other := waitingPlan(t, e, "b")
	got, isErr = call(105, "plan_reject", map[string]any{"repo_url": coordRepo, "plan": other, "version": 1})
	if isErr || !strings.Contains(got, "rejected") {
		t.Fatalf("plan_reject at the current version = %q (error %v)", got, isErr)
	}
	if st := planStateOf(t, e, other); st != "cancelled" {
		t.Errorf("rejected plan %d = %s, want cancelled", other, st)
	}

	// The coordinator's status trims finished plans like the foreman's
	// (§10): with four finished, the default counts the oldest behind
	// one summary line; all=true lists every finished plan.
	for i := 0; i < 3; i++ {
		if err := e.st.Update(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
				VALUES (1, ?, 'done', ?, ?)`, fmt.Sprintf("older %d", i), seedTS, seedTS)
			return err
		}); err != nil {
			t.Fatalf("seed the finished plans: %v", err)
		}
	}
	text, _ = call(106, "status", map[string]any{})
	if !strings.Contains(text, "1 older finished plans hidden; pass all=true") {
		t.Errorf("status with four finished plans = %q, want the hidden count line", text)
	}
	text, _ = call(107, "status", map[string]any{"all": true})
	if strings.Contains(text, "hidden") || !strings.Contains(text, " done older 0") {
		t.Errorf("status all = %q, want every finished plan listed and no hidden line", text)
	}
}

// TestPlanVersionBumpsOnAmend: the version starts at 1 and every
// amendment, the cancel-only kind too, moves it.
func TestPlanVersionBumpsOnAmend(t *testing.T) {
	e := newEnv(t)
	plan := waitingPlan(t, e, "v")
	version := func() int64 {
		var v int64
		err := e.st.View(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT version FROM plans WHERE id = ?", plan).Scan(&v)
		})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := version(); v != 1 {
		t.Fatalf("new plan version = %d, want 1", v)
	}
	amendWaiting(t, e, plan, "v1")
	if v := version(); v != 2 {
		t.Fatalf("version after an added task = %d, want 2", v)
	}
	var first int64
	err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT MIN(id) FROM tasks WHERE plan_id = ?", plan).Scan(&first)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.AmendPlan(t.Context(), plan, api.PlanAmend{Cancel: []string{strconv.FormatInt(first, 10)}}, "amend-v2"); err != nil {
		t.Fatalf("cancel task %d: %v", first, err)
	}
	if v := version(); v != 3 {
		t.Fatalf("version after a cancel-only amendment = %d, want 3", v)
	}
}

// TestCoordinatorAskForemanAnswersAndRestarts: the two foreman replies a
// user sends through ask_foreman reach the project's foreman as the
// user's words, unaltered. The first call submits the foreman's turn
// (nobody is attached), and that turn's prompt carries its message. The
// second call finds the turn running and waits in the inbox for the next
// turn, which carries it. Each reply is one line; the foreman's report
// comes back later, through status and history, not in the reply.
func TestCoordinatorAskForemanAnswersAndRestarts(t *testing.T) {
	e := newEnv(t)
	s := startCoordinator(t, e)
	call := func(id int, args map[string]any) (string, bool) {
		t.Helper()
		return toolText(t, s.call(id, "tools/call", map[string]any{"name": "ask_foreman", "arguments": args}))
	}
	texts := []string{
		"answer task 3: the staging database is the one in the fixtures",
		"restart task 3 with this note: the fixture moved to testdata/v2",
	}
	for i, text := range texts {
		reply, isErr := call(20+i, map[string]any{"repo_url": coordRepo, "text": text})
		if i == 0 && (isErr || !strings.Contains(reply, "waking up")) {
			t.Errorf("ask_foreman %q = %q (error %v), want the foreman woken", text, reply, isErr)
		}
		if i == 1 && (isErr || !strings.Contains(reply, "a foreman turn is running") || !strings.Contains(reply, "next turn")) {
			t.Errorf("ask_foreman %q = %q (error %v), want it queued for the next turn", text, reply, isErr)
		}
	}
	// The woken turn is the newest foreman job (the seeded one is older):
	// its prompt must carry the first message, so a turn submitted without
	// it fails here.
	var prompt string
	err := e.st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT prompt FROM jobs WHERE kind = ? ORDER BY id DESC LIMIT 1", engine.KindForeman).Scan(&prompt)
	})
	if err != nil {
		t.Fatalf("read the foreman's turn: %v", err)
	}
	if !strings.Contains(prompt, texts[0]) {
		t.Errorf("the foreman's turn prompt lacks the user's message %q:\n%s", texts[0], prompt)
	}
	history, isErr := toolText(t, s.call(30, "tools/call", map[string]any{"name": "history", "arguments": map[string]any{"repo_url": coordRepo}}))
	if isErr {
		t.Fatalf("history = %q", history)
	}
	for _, text := range texts {
		if !strings.Contains(history, text) {
			t.Errorf("the foreman's history lacks the user's message %q:\n%s", text, history)
		}
	}
}
