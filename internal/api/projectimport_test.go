package api

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestImportAllOrNothing: one import through the API is one transaction
// (§13.1), not a config commit followed by separate role and skills
// imports. A failure injected into the skills write — the last step,
// after the config keys, the skills_dir and the role texts were already
// written in the same transaction — answers 500 and leaves every
// previous value in effect; the same import then succeeds whole and
// changes all four kinds of value together.
func TestImportAllOrNothing(t *testing.T) {
	e := newEnv(t)
	handler := e.api.Handler()
	user := &Client{token: "alice-secret-1", http: &http.Client{Transport: funcTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}}
	const repo = "https://example.invalid/allornothing.git"
	canon, err := engine.Canonicalize(repo)
	if err != nil {
		t.Fatal(err)
	}
	file := func(body string) []byte {
		return []byte("schema: 1\nrepo: " + canon + "\n" + body)
	}
	skill := func(name, text string) engine.Skill {
		return engine.Skill{Name: name, Description: "d " + name, Text: text}
	}
	importAs := func(content []byte, foreman, worker, skills string) error {
		beta := []engine.Skill{skill(skills, skills+" v1")}
		_, err := user.ImportProjectConfigFull(e.ctx, repo, content, nil,
			map[string]string{"foreman": foreman, "worker": worker}, &beta, true)
		return err
	}

	// One import seeds previous values of all four kinds.
	if err := importAs(file("approval_max_tasks: 2\nskills_dir: .brigada/skills\ninstructions:\n  foreman: f.md\n  worker: w.md\n"),
		"foreman one", "worker one", "alpha"); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// The skills write fails after the config keys, the skills_dir and
	// the role texts were already written in the same transaction.
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TRIGGER fail_skills BEFORE INSERT ON skills BEGIN SELECT RAISE(ABORT, 'injected skills failure'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	newFile := file("approval_max_tasks: 5\nskills_dir: tools/skills\ninstructions:\n  foreman: f.md\n  worker: w.md\n")
	err = importAs(newFile, "foreman two", "worker two", "beta")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v, want a 500 from the injected failure", err)
	}

	// Every previous value stands: the config keys, the skills_dir, the
	// role texts and the skills.
	ps, err := user.ProjectSettings(e.ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	value := func(key string) engine.SettingValue {
		t.Helper()
		for _, s := range ps.Settings {
			if s.Key == key {
				return s
			}
		}
		t.Fatalf("no %s in the settings", key)
		return engine.SettingValue{}
	}
	if sv := value("approval_max_tasks"); sv.Value != "2" {
		t.Errorf("approval_max_tasks = %+v, want the previous 2", sv)
	}
	if sv := value("skills_dir"); sv.Value != ".brigada/skills" || sv.Source != engine.SourceImported {
		t.Errorf("skills_dir = %+v, want the previous import's", sv)
	}
	for role, want := range map[string]string{engine.RoleForeman: "foreman one", engine.RoleWorker: "worker one"} {
		ri, err := e.eng.RoleInstruction(e.ctx, repo, role)
		if err != nil || ri == nil {
			t.Fatalf("read the %s instructions: %v", role, err)
		}
		if ri.Source != engine.SourceImported || ri.Text != want {
			t.Errorf("%s = %+v, want the previous %q", role, ri, want)
		}
	}
	if got, err := e.eng.Skills(e.ctx, repo); err != nil || len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("skills = %+v, %v; want the previous alpha alone", got, err)
	}

	// The same import succeeds whole: the config keys, the skills_dir,
	// the role texts and the skills move together.
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("DROP TRIGGER fail_skills")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := importAs(newFile, "foreman two", "worker two", "beta"); err != nil {
		t.Fatalf("import after the failure was lifted: %v", err)
	}
	ps, err = user.ProjectSettings(e.ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if sv := value("approval_max_tasks"); sv.Value != "5" {
		t.Errorf("approval_max_tasks = %+v, want 5", sv)
	}
	if sv := value("skills_dir"); sv.Value != "tools/skills" {
		t.Errorf("skills_dir = %+v, want tools/skills", sv)
	}
	for role, want := range map[string]string{engine.RoleForeman: "foreman two", engine.RoleWorker: "worker two"} {
		ri, err := e.eng.RoleInstruction(e.ctx, repo, role)
		if err != nil || ri == nil {
			t.Fatalf("read the %s instructions: %v", role, err)
		}
		if ri.Text != want {
			t.Errorf("%s = %+v, want the file's %q", role, ri, want)
		}
	}
	if got, err := e.eng.Skills(e.ctx, repo); err != nil || len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("skills = %+v, %v; want beta alone", got, err)
	}
}
