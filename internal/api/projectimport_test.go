package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestProjectImportAllKindsTogether: one PUT /v1/projects/import moves
// the four kinds of value together — the config keys, the skills_dir,
// the role texts and the skills — and a refusal the handler catches up
// front changes nothing (§13.1: an import is one step; the rollback
// behind a late failure is covered in the engine's tests).
func TestProjectImportAllKindsTogether(t *testing.T) {
	e := newEnv(t)
	handler := e.api.Handler()
	user := &Client{token: "alice-secret-1", http: &http.Client{Transport: funcTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}}
	const repo = "https://example.invalid/all.git"
	canon, err := engine.Canonicalize(repo)
	if err != nil {
		t.Fatal(err)
	}
	file := func(check, dir string) []byte {
		return []byte("schema: 1\nrepo: " + canon + "\ndefault_check: " + check + "\nskills_dir: " + dir +
			"\ninstructions:\n  foreman: .brigada/instructions/foreman.md\n  worker: .brigada/instructions/worker.md\n  reviewer: .brigada/instructions/reviewer.md\n")
	}
	skill := func(name, text string) engine.Skill {
		return engine.Skill{Name: name, Description: "d " + name, Text: text, TreeDigest: "tree-" + name}
	}
	importAll := func(content []byte, roles map[string]string, skills []engine.Skill) (engine.ImportResult, error) {
		return user.ImportProjectConfigFull(e.ctx, repo, content, nil, roles, &skills, true)
	}

	if _, err := importAll(file("make test", "tools/skills"),
		map[string]string{engine.RoleForeman: "f1", engine.RoleWorker: "w1", engine.RoleReviewer: "r1"},
		[]engine.Skill{skill("alpha", "a1")}); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// A role text over the bound is refused before anything runs.
	_, err = importAll(file("go test ./...", "other/skills"),
		map[string]string{engine.RoleForeman: "f2", engine.RoleWorker: "w2", engine.RoleReviewer: strings.Repeat("r", engine.MaxRoleInstructionBytes+1)},
		[]engine.Skill{skill("beta", "b2")})
	if err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("oversized role: err = %v, want the reviewer named", err)
	}

	res, err := importAll(file("go test ./...", "other/skills"),
		map[string]string{engine.RoleForeman: "f2", engine.RoleWorker: "w2", engine.RoleReviewer: "r2"},
		[]engine.Skill{skill("beta", "b2")})
	if err != nil || res.NeedsConfirm || len(res.Conflicts) != 0 {
		t.Fatalf("second import = %+v, %v, want a clean apply", res, err)
	}
	ps, err := e.eng.ProjectSettings(e.ctx, repo)
	if err != nil || ps == nil {
		t.Fatalf("read settings: %+v, %v", ps, err)
	}
	setting := func(key string) engine.SettingValue {
		for _, s := range ps.Settings {
			if s.Key == key {
				return s
			}
		}
		t.Fatalf("no setting %s", key)
		return engine.SettingValue{}
	}
	if s := setting(engine.SettingDefaultCheck); s.Value != "go test ./..." || s.Source != engine.SourceImported {
		t.Errorf("default_check = %+v, want the file's, imported", s)
	}
	if s := setting("skills_dir"); s.Value != "other/skills" || s.Source != engine.SourceImported {
		t.Errorf("skills_dir = %+v, want the file's, imported", s)
	}
	for role, text := range map[string]string{engine.RoleForeman: "f2", engine.RoleWorker: "w2", engine.RoleReviewer: "r2"} {
		ri, err := e.eng.RoleInstruction(e.ctx, repo, role)
		if err != nil || ri == nil {
			t.Fatalf("read %s instructions: %v, %+v", role, err, ri)
		}
		if ri.Text != text || ri.Source != engine.SourceImported {
			t.Errorf("%s = %+v, want %q imported", role, ri, text)
		}
	}
	got, err := e.eng.Skills(e.ctx, repo)
	if err != nil || len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("skills after the import = %+v, %v, want beta alone", got, err)
	}
}
