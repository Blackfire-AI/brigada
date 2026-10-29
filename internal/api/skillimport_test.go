package api

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestSkillImportEndToEnd: the client's parsed skills ride the import
// request to the engine, the resolver finds them, a re-import replaces the
// set without touching a created task, a refusal names the skill and keeps
// the set, and status flags a skill whose file changed after import while
// the stored snapshot stays until the next import.
func TestSkillImportEndToEnd(t *testing.T) {
	e := newEnv(t)
	handler := e.api.Handler()
	client := func(secret string) *Client {
		return &Client{token: secret, http: &http.Client{Transport: funcTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w.Result(), nil
		})}}
	}
	user, admin := client("alice-secret-1"), client("root-secret-1")
	const repo = "https://example.invalid/skills.git"
	canon, err := engine.Canonicalize(repo)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("schema: 1\nrepo: " + canon + "\nskills_dir: .brigada/skills\n")
	skill := func(name, text string) engine.Skill {
		return engine.Skill{Name: name, Description: "d " + name, Text: text, For: "both", TreeDigest: "tree-" + name}
	}
	do := func(skills []engine.Skill) error {
		_, err := user.ImportProjectConfigFull(e.ctx, repo, content, nil, nil, &skills, true)
		return err
	}

	if err := do([]engine.Skill{skill("alpha", "---\nname: alpha\n---\nv1"), skill("beta", "---\nname: beta\n---\nb1")}); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := e.eng.Skills(e.ctx, repo)
	if err != nil || len(got) != 2 {
		t.Fatalf("skills after import = %v, %v", got, err)
	}
	task, err := admin.SubmitTask(e.ctx, TaskSubmit{RepoURL: repo, BaseRef: "main", Spec: "work", Skills: []string{"alpha"}})
	if err != nil {
		t.Fatalf("a task naming an imported skill: %v", err)
	}
	taskSkills := func() string {
		var raw string
		if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT skills FROM tasks WHERE id = ?", task.TaskID).Scan(&raw)
		}); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := taskSkills()

	// Re-import: alpha edited, beta removed.
	if err := do([]engine.Skill{skill("alpha", "---\nname: alpha\n---\nv2")}); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	got, _ = e.eng.Skills(e.ctx, repo)
	if len(got) != 1 || got[0].Name != "alpha" || !strings.HasSuffix(got[0].Text, "v2") {
		t.Fatalf("skills after re-import = %+v", got)
	}
	if after := taskSkills(); after != before {
		t.Errorf("a created task's skills changed on re-import: %q -> %q", before, after)
	}
	if _, err := admin.SubmitTask(e.ctx, TaskSubmit{RepoURL: repo, BaseRef: "main", Spec: "work", Skills: []string{"beta"}}); err == nil {
		t.Errorf("a removed skill still resolved")
	}

	// A refusal names the skill and leaves the set.
	err = do([]engine.Skill{skill("alpha", "v3"), skill("huge", strings.Repeat("x", engine.MaxSkillBytes+1))})
	if err == nil || !strings.Contains(err.Error(), "huge") || !strings.Contains(err.Error(), "4096") {
		t.Errorf("oversized skill: err = %v, want it named with the bound", err)
	}
	if got, _ = e.eng.Skills(e.ctx, repo); len(got) != 1 || !strings.HasSuffix(got[0].Text, "v2") {
		t.Errorf("a refused import changed the set: %+v", got)
	}

	// Status: the client's current digest against the imported one.
	ps, err := user.ProjectSettings(e.ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	stored := got[0].Digest
	changed := func(d engine.SkillFileDigest) bool {
		t.Helper()
		st, err := user.StatusWithFileDigests(e.ctx, ps.ProjectID, FileDigests{Skills: map[string]engine.SkillFileDigest{"alpha": d}})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range st.Skills {
			if s.Name == "alpha" {
				if s.ChangedSinceImport == nil {
					t.Fatalf("no comparison for alpha")
				}
				if s.Digest != stored {
					t.Errorf("the stored digest moved to %s", s.Digest)
				}
				return *s.ChangedSinceImport
			}
		}
		t.Fatalf("alpha is not in status")
		return false
	}
	if changed(engine.SkillFileDigest{Digest: stored, Tree: "tree-alpha"}) {
		t.Errorf("an unedited skill was flagged")
	}
	// A supporting-file edit moves the tree digest alone.
	if !changed(engine.SkillFileDigest{Digest: stored, Tree: "tree-edited"}) {
		t.Errorf("a tree-only edit was not flagged")
	}
	if !changed(engine.SkillFileDigest{Digest: "edited"}) {
		t.Errorf("an edited skill was not flagged")
	}
}
