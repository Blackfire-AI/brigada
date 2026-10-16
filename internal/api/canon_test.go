package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A repo URL whose password starts with digits and a '/' is refused at
// every public entry, and its secret is in no answer.
func TestSentinelRepoURLIsRefusedWithoutLeaking(t *testing.T) {
	const secret = "s3cr3t-sentinel"
	bad := "user.name:1234/" + secret + "/unsafe@example.invalid:team/repo"
	quoted, _ := json.Marshal(bad)
	e := newEnv(t)

	for _, tc := range []struct {
		name, method, path, who string
		body                    string
	}{
		{"task submit", http.MethodPost, "/v1/tasks", "root", `{"repo_url":` + string(quoted) + `,"base_ref":"main","spec":"go"}`},
		{"foreman set", http.MethodPut, "/v1/projects/foreman", "alice", `{"repo_url":` + string(quoted) + `,"harness":"claude","model":"opus"}`},
		{"settings set", http.MethodPut, "/v1/projects/settings", "alice", `{"repo_url":` + string(quoted) + `,"values":{"approval.max_tasks":"3"}}`},
		{"foreman get", http.MethodGet, "/v1/projects/foreman?repo=" + url.QueryEscape(bad), "alice", ""},
		{"settings get", http.MethodGet, "/v1/projects/settings?repo=" + url.QueryEscape(bad), "alice", ""},
		{"ask", http.MethodPost, "/v1/foreman/messages", "alice", `{"repo_url":` + string(quoted) + `,"text":"hi"}`},
		{"attach", http.MethodPost, "/v1/foreman/attach", "alice", `{"repo_url":` + string(quoted) + `}`},
		{"plan create", http.MethodPost, "/v1/plans", "alice", `{"repo_url":` + string(quoted) + `,"title":"p","tasks":[{"title":"t","spec":"s"}]}`},
	} {
		var body []byte
		if tc.body != "" {
			body = []byte(tc.body)
		}
		key := ""
		if tc.method != http.MethodGet {
			key = "key-" + tc.name
		}
		code, _, data := e.do(tc.method, tc.path, tc.who, key, body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", tc.name, code, data)
		}
		if strings.Contains(string(data), secret) {
			t.Errorf("%s: the answer leaks the secret: %s", tc.name, data)
		}
	}

	code, _, data := e.do(http.MethodGet, "/v1/status", "alice", "", nil)
	if code != http.StatusOK || strings.Contains(string(data), secret) {
		t.Errorf("status = %d %s, want 200 without the secret", code, data)
	}
	code, _, data = e.do(http.MethodGet, "/v1/projects", "alice", "", nil)
	if code != http.StatusOK || strings.Contains(string(data), secret) {
		t.Errorf("project list = %d %s, want 200 without the secret", code, data)
	}
}

// A project made by one URL form is found by another on the wire.
func TestProjectIsFoundByAnyFormOfItsURL(t *testing.T) {
	e := newEnv(t)
	code, _, data := e.do(http.MethodPut, "/v1/projects/foreman", "alice", "k1",
		[]byte(`{"repo_url":"https://example.invalid/team/forms.git","harness":"claude","model":"opus"}`))
	if code != http.StatusOK {
		t.Fatalf("choose the foreman: %d %s", code, data)
	}
	code, _, data = e.do(http.MethodGet, "/v1/projects/foreman?repo="+url.QueryEscape("git@example.invalid:team/forms"), "alice", "", nil)
	if code != http.StatusOK || !strings.Contains(string(data), `"repo_url":"example.invalid/team/forms"`) {
		t.Errorf("read by the scp form = %d %s, want the project by its canonical URL", code, data)
	}
}
