package planfile

import (
	"reflect"
	"strings"
	"testing"
)

func extraPlan() Plan {
	p := goldenPlan()
	p.Tasks[1].ExtraRepos = []ExtraRepo{
		{Name: "schema", Repo: "example.invalid/org/schema", Ref: "v2.3.0"},
		{Name: "tools", Repo: "file:/srv/git/my tools"},
	}
	return p
}

// A task's extra repositories (§5.8) are written only when it has some, so
// the files of tasks without them stay as they were, and read back whole.
func TestExtraReposRoundTrip(t *testing.T) {
	got, err := Render(extraPlan())
	if err != nil {
		t.Fatal(err)
	}
	want := `    extra_repos: [["schema", "example.invalid/org/schema", "v2.3.0"], ["tools", "file:/srv/git/my tools", ""]]` + "\n"
	if !strings.Contains(string(got), want) {
		t.Errorf("the rendering does not hold %q:\n%s", want, got)
	}
	if n := strings.Count(string(got), "extra_repos:"); n != 1 {
		t.Errorf("extra_repos written %d times, want once: only the task that has some", n)
	}
	back, err := Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(back, extraPlan()) {
		t.Errorf("round trip = %+v, want %+v", back, extraPlan())
	}
	plain, err := Render(goldenPlan())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "extra_repos") {
		t.Error("a plan with no extra repositories mentions the field")
	}
}

func TestParseRefusesAMalformedExtraRepos(t *testing.T) {
	good, err := Render(extraPlan())
	if err != nil {
		t.Fatal(err)
	}
	line := `[["schema", "example.invalid/org/schema", "v2.3.0"], ["tools", "file:/srv/git/my tools", ""]]`
	if !strings.Contains(string(good), line) {
		t.Fatalf("the rendering does not hold %s", line)
	}
	for name, bad := range map[string]string{
		"empty list":     `[]`,
		"two parts":      `[["schema", "example.invalid/org/schema"]]`,
		"unquoted":       `[[schema, repo, ""]]`,
		"no name":        `[["", "example.invalid/org/schema", ""]]`,
		"no repo":        `[["a", "", ""]]`,
		"missing comma":  `[["a", "b", ""]["c", "d", ""]]`,
		"not a list":     `"a b"`,
		"trailing stuff": `[["a", "b", ""]] x`,
	} {
		if _, err := Parse([]byte(strings.Replace(string(good), line, bad, 1))); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
