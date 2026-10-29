package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestRoleImportEndToEnd: a role key dropped from project.yaml's
// instructions map makes that role not configured at the next import —
// its imported row is removed (§5.9) — while a local override under the
// dropped key keeps its text and the import reports the conflict. The
// worker rides the config import itself: no instructions_file and no map
// key remove its imported row, and a local worker text stands.
func TestRoleImportEndToEnd(t *testing.T) {
	e := newEnv(t)
	handler := e.api.Handler()
	client := func(secret string) *Client {
		return &Client{token: secret, http: &http.Client{Transport: funcTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w.Result(), nil
		})}}
	}
	user := client("alice-secret-1")
	strPtr := func(s string) *string { return &s }
	const repo = "https://example.invalid/roles.git"
	canon, err := engine.Canonicalize(repo)
	if err != nil {
		t.Fatal(err)
	}
	file := func(body string) []byte {
		return []byte("schema: 1\nrepo: " + canon + "\n" + body)
	}
	state := func(role string) *engine.RoleInstruction {
		t.Helper()
		ri, err := e.eng.RoleInstruction(e.ctx, repo, role)
		if err != nil || ri == nil {
			t.Fatalf("read %s instructions: %v, %+v", role, err, ri)
		}
		return ri
	}

	// A map naming foreman, reviewer and worker; then foreman's key is
	// dropped from the file.
	withForeman := file("instructions:\n  foreman: f.md\n  reviewer: r.md\n  worker: w.md\n")
	res, err := user.ImportProjectConfigFull(e.ctx, repo, withForeman, nil,
		map[string]string{"foreman": "foreman file text", "reviewer": "reviewer file text", "worker": "worker file text"}, nil, true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for role, want := range map[string]string{engine.RoleForeman: "foreman file text", engine.RoleReviewer: "reviewer file text", engine.RoleWorker: "worker file text"} {
		if ri := state(role); ri.State != engine.RoleStateFile || ri.Source != engine.SourceImported || ri.Text != want {
			t.Fatalf("%s after the import = %+v, want the file's text", role, ri)
		}
	}
	withoutForeman := file("instructions:\n  reviewer: r.md\n  worker: w.md\n")
	res, err = user.ImportProjectConfigFull(e.ctx, repo, withoutForeman, nil,
		map[string]string{"reviewer": "reviewer file text", "worker": "worker file text"}, nil, true)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	var applied bool
	for _, c := range res.Applied {
		if c.Key == "instructions.foreman" {
			applied = true
			if c.New != engine.RoleStateNotConfigured || !strings.Contains(c.Old, "bytes") {
				t.Errorf("foreman removal = %+v, want a summary moving to not configured", c)
			}
		}
	}
	if !applied {
		t.Fatalf("the dropped foreman key was not reported:\n%+v", res.Applied)
	}
	if ri := state(engine.RoleForeman); ri.State != engine.RoleStateNotConfigured || ri.Text != "" {
		t.Errorf("foreman after its key was dropped = %+v, want not configured", ri)
	}
	if ri := state(engine.RoleReviewer); ri.Source != engine.SourceImported || ri.Text != "reviewer file text" {
		t.Errorf("reviewer, still named by the file = %+v, want its import kept", ri)
	}

	// A local override under a dropped key keeps its text; the import
	// reports the conflict, both texts shown as ErrRoleTextKept shows
	// them (§5.9).
	if err := user.SetRoleInstructions(e.ctx, repo, map[string]string{engine.RoleReviewer: "my local reviewer text"}); err != nil {
		t.Fatalf("set the local override: %v", err)
	}
	withoutReviewer := file("instructions:\n  worker: w.md\n")
	res, err = user.ImportProjectConfigFull(e.ctx, repo, withoutReviewer, nil,
		map[string]string{"worker": "worker file text"}, nil, true)
	if err != nil {
		t.Fatalf("re-import over the override: %v", err)
	}
	var conflict string
	for _, c := range res.Conflicts {
		if c.Key == "instructions.reviewer" {
			conflict = c.Old
		}
	}
	if conflict == "" || !strings.Contains(conflict, "my local reviewer text") {
		t.Fatalf("the kept override was not reported as a conflict: %+v", res.Conflicts)
	}
	if ri := state(engine.RoleReviewer); ri.Source != engine.SourceLocal || ri.Text != "my local reviewer text" {
		t.Errorf("reviewer under the dropped key = %+v, want the local text kept", ri)
	}

	// The worker: no map key and no instructions_file remove its imported
	// row through the config import itself; a local worker text stands.
	plain := file("approval_max_tasks: 3\n")
	if _, err = user.ImportProjectConfigFull(e.ctx, repo, plain, nil, nil, nil, true); err != nil {
		t.Fatalf("import without the worker key: %v", err)
	}
	if ri := state(engine.RoleWorker); ri.State != engine.RoleStateNotConfigured || ri.Text != "" {
		t.Errorf("worker after its key was dropped = %+v, want not configured", ri)
	}
	if _, err = user.ImportProjectConfigFull(e.ctx, repo, file("instructions_file: .brigada/instructions.md\n"), strPtr("worker file text"), nil, nil, true); err != nil {
		t.Fatalf("import the alias: %v", err)
	}
	if ri := state(engine.RoleWorker); ri.Source != engine.SourceImported || ri.Text != "worker file text" {
		t.Fatalf("worker on its alias = %+v, want the file's text", ri)
	}
	if err := user.SetRoleInstructions(e.ctx, repo, map[string]string{engine.RoleWorker: "my local worker text"}); err != nil {
		t.Fatalf("set the local worker text: %v", err)
	}
	if _, err = user.ImportProjectConfigFull(e.ctx, repo, plain, nil, nil, nil, true); err != nil {
		t.Fatalf("re-import without the worker key: %v", err)
	}
	if ri := state(engine.RoleWorker); ri.Source != engine.SourceLocal || ri.Text != "my local worker text" {
		t.Errorf("local worker text over the dropped key = %+v, want it kept", ri)
	}
}
