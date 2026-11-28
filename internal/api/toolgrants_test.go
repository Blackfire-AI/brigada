package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestToolGrantsRoundTrip: a grant list inside the allowlist round-trips
// through plan_create, plan_amend's add and respec and the debug submit,
// read back on the task; a pattern outside it is refused at each entry
// point, named, and a task without the field grants none.
func TestToolGrantsRoundTrip(t *testing.T) {
	e := newEnv(t)
	// The whole allowlist pre-approved (§6.3), so the plan runs and the
	// round trip reads its tasks rather than an approval hold.
	allow, err := json.Marshal(map[string][]string{engine.SettingApprovalToolGrants: engine.ToolGrantAllowlist})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE projects SET settings = ? WHERE id = ?`, string(allow), e.project1)
	grants := []string{"Bash(git merge:*)", "Bash(git restore:*)"}
	read := func(task int64) []string {
		t.Helper()
		code, _, data := e.do(http.MethodGet, "/v1/tasks/"+strconv.FormatInt(task, 10), "alice", "", nil)
		if code != http.StatusOK {
			t.Fatalf("read task %d: %d %s", task, code, data)
		}
		return decode[engine.TaskInfo](t, data).ToolGrants
	}

	body := `{"title":"grants","tasks":[{"name":"build","spec":"the spec","tool_grants":["Bash(git merge:*)","Bash(git restore:*)"]},{"name":"ship","spec":"ship it","deps":["build"]}]}`
	code, _, data := e.do(http.MethodPost, "/v1/plans", "foreman1", "grants-1", []byte(body))
	if code != http.StatusOK {
		t.Fatalf("create with grants: %d %s", code, data)
	}
	res := decode[planCreateBody](t, data)
	if got := read(res.TaskID); !slices.Equal(got, grants) {
		t.Errorf("created tool_grants = %v, want %v", got, grants)
	}
	ship, err := strconv.ParseInt(e.str("SELECT id FROM tasks WHERE plan_id = ? AND name = 'ship'", res.PlanID), 10, 64)
	if err != nil {
		t.Fatalf("ship's id: %v", err)
	}
	if got := e.str("SELECT tool_grants FROM tasks WHERE id = ?", ship); got != "" {
		t.Errorf("grantless tool_grants = %q, want ''", got)
	}

	amend := func(idem, body string) (int, []byte) {
		t.Helper()
		path := "/v1/plans/" + strconv.FormatInt(res.PlanID, 10) + "/amend"
		code, _, data := e.do(http.MethodPost, path, "foreman1", idem, []byte(body))
		return code, data
	}
	// amend-add carries the added task's grants.
	code, data = amend("grants-2", `{"add":[{"name":"extra","spec":"extra work","deps":["build"],"tool_grants":["Bash(git checkout:*)"]}]}`)
	if code != http.StatusOK {
		t.Fatalf("amend add with grants: %d %s", code, data)
	}
	added := decode[struct {
		Added []struct {
			TaskID int64 `json:"task_id"`
		} `json:"added"`
	}](t, data).Added[0].TaskID
	if got := read(added); !slices.Equal(got, []string{"Bash(git checkout:*)"}) {
		t.Errorf("added tool_grants = %v, want the checkout grant", got)
	}
	// amend-respec replaces the grants; absent keeps them; [] clears.
	if code, data := amend("grants-3", `{"respec":[{"task":"ship","spec":"ship more","tool_grants":["Bash(git cherry-pick:*)"]}]}`); code != http.StatusOK {
		t.Fatalf("respec with grants: %d %s", code, data)
	}
	if got := read(ship); !slices.Equal(got, []string{"Bash(git cherry-pick:*)"}) {
		t.Errorf("respecced tool_grants = %v, want the cherry-pick grant", got)
	}
	if code, data := amend("grants-4", `{"respec":[{"task":"ship","spec":"ship more still"}]}`); code != http.StatusOK {
		t.Fatalf("respec without grants: %d %s", code, data)
	}
	if got := read(ship); !slices.Equal(got, []string{"Bash(git cherry-pick:*)"}) {
		t.Errorf("respec without tool_grants = %v, want the recorded grants kept", got)
	}
	if code, data := amend("grants-5", `{"respec":[{"task":"ship","spec":"ship grantless","tool_grants":[]}]}`); code != http.StatusOK {
		t.Fatalf("respec with []: %d %s", code, data)
	}
	if got := read(ship); len(got) != 0 {
		t.Errorf("respec with [] = %v, want the grants cleared", got)
	}

	// The debug submit carries its grants.
	sub := `{"repo_url":"https://example.invalid/grants.git","base_ref":"main","spec":"direct","tool_grants":["Bash(git merge:*)"]}`
	code, _, data = e.do(http.MethodPost, "/v1/tasks", "root", "grants-6", []byte(sub))
	if code != http.StatusOK {
		t.Fatalf("submit with grants: %d %s", code, data)
	}
	direct := decode[struct {
		TaskID int64 `json:"task_id"`
	}](t, data).TaskID
	if got := read(direct); !slices.Equal(got, []string{"Bash(git merge:*)"}) {
		t.Errorf("submitted tool_grants = %v, want the merge grant", got)
	}

	// A pattern outside the allowlist is refused at each entry point,
	// the refusal naming it.
	const bad = "Bash(git push:*)"
	for name, call := range map[string]struct {
		path, token, idem, body string
	}{
		"create": {"/v1/plans", "foreman1", "grants-bad-1", `{"title":"bad","tasks":[{"spec":"s","tool_grants":["` + bad + `"]}]}`},
		"add":    {"/v1/plans/" + strconv.FormatInt(res.PlanID, 10) + "/amend", "foreman1", "grants-bad-2", `{"add":[{"name":"bad","spec":"s","tool_grants":["` + bad + `"]}]}`},
		"respec": {"/v1/plans/" + strconv.FormatInt(res.PlanID, 10) + "/amend", "foreman1", "grants-bad-3", `{"respec":[{"task":"ship","spec":"s","tool_grants":["` + bad + `"]}]}`},
		"submit": {"/v1/tasks", "root", "grants-bad-4", `{"repo_url":"https://example.invalid/grants.git","base_ref":"main","spec":"s","tool_grants":["` + bad + `"]}`},
	} {
		code, _, data := e.do(http.MethodPost, call.path, call.token, call.idem, []byte(call.body))
		if code != http.StatusBadRequest || !strings.Contains(string(data), bad) {
			t.Errorf("%s with an off-allowlist pattern: %d %s, want a 400 naming %s", name, code, data, bad)
		}
	}
}
