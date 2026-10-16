package engine

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/planfile"
)

// The plan file carries a task's extra repositories (§5.8): the canonical
// identity and the ref, never the URL the runner fetches from.
func TestPlanFileCarriesExtraRepos(t *testing.T) {
	m := newMirrorEnv(t)
	// The mirror env points the project at a file remote, so the default is
	// written to the row directly, in the shape the setting stores.
	m.setSettings(`{"approval_max_tasks":-1,"extra_repos":[{"name":"schema","repo":"example.invalid/org/schema","url":"https://example.invalid/org/schema.git","ref":"v1"}]}`)
	res, err := m.eng.CreatePlan(m.ctx, m.project, "mirror extras", []PlanTask{
		{Name: "inherits", Spec: "a"},
		{Name: "own", Spec: "b", ExtraRepos: []ExtraRepo{{Name: "tools", Repo: "https://token:s3cret@example.invalid/org/tools.git", Ref: commitSHA}}},
		{Name: "none", Spec: "c", ExtraRepos: []ExtraRepo{}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	p := m.mustFile("brigada/plans", res.PlanID)
	want := map[string][]planfile.ExtraRepo{
		"inherits": {{Name: "schema", Repo: "example.invalid/org/schema", Ref: "v1"}},
		"own":      {{Name: "tools", Repo: "example.invalid/org/tools", Ref: commitSHA}},
		"none":     nil,
	}
	for name, w := range want {
		got := taskByName(t, p, name).ExtraRepos
		if len(got) != len(w) || (len(w) > 0 && got[0] != w[0]) {
			t.Errorf("task %s extra repos = %+v, want %+v", name, got, w)
		}
	}
	out, err := exec.Command("git", "-C", m.remote, "show", "brigada/plans:"+planfile.Path(res.PlanID)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "s3cret") || strings.Contains(string(out), "token") {
		t.Errorf("the plan file holds a credential:\n%s", out)
	}
	if w := m.warnings(); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
}
