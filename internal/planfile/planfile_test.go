package planfile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func goldenPlan() Plan {
	return Plan{
		ID:      7,
		Title:   "Ship the frobnicator",
		Repo:    "https://example.invalid/brigada.git",
		State:   "active",
		Created: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
		Updated: time.Date(2026, 10, 12, 12, 30, 0, 500_000_000, time.UTC),
		Tasks: []Task{
			{ID: 11, Name: "schema", Title: "Add the schema", Class: "implement", State: "done", Branch: "brigada/task-11"},
			{ID: 12, Name: "api", Title: "Expose it", Class: "implement", Deps: []string{"schema"}, Base: "schema", Continue: "schema", State: "needs_foreman",
				Branch: "brigada/task-12", Rounds: 2, Question: "Which \"auth\" scheme?\nbearer or basic?"},
			{ID: 13, Name: "docs", Title: "Write the docs", Class: "answer", Deps: []string{"schema", "api"}, Base: "api", State: "pending"},
		},
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	got, err := Render(goldenPlan())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "plan.golden"))
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the rendering:\n%s\nwant the golden:\n%s", got, want)
	}
}

func TestParseInvertsRender(t *testing.T) {
	plans := map[string]Plan{
		"golden":          goldenPlan(),
		"caps":            {ID: 9, State: "active", Created: time.Unix(1, 0).UTC(), Updated: time.Unix(2, 0).UTC(), Tasks: []Task{{ID: 1, State: "ready", Caps: map[string]int{"restart": 4, "fix_rounds": 0}}, {ID: 2, State: "pending"}}},
		"tier and effort": {ID: 8, State: "active", Created: time.Unix(1, 0).UTC(), Updated: time.Unix(2, 0).UTC(), Tasks: []Task{{ID: 1, Tier: "heavy", Effort: "high", State: "ready"}, {ID: 2, Tier: "light", State: "pending"}}},
		"no tasks": {ID: 1, Title: "", Repo: "r", State: "draft",
			Created: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), Updated: time.Date(2026, 1, 2, 3, 4, 5, 7, time.UTC)},
		"awkward text": {ID: 2, Title: "tab\there, \"quotes\", back\\slash, ünïcode, [brackets], # hash", Repo: "file:///a b/c.git",
			State: "active", Created: time.Unix(1, 0).UTC(), Updated: time.Unix(2, 0).UTC(),
			Tasks: []Task{{ID: 1, Title: "t: x", Class: "implement", State: "ready", Branch: "a, b"},
				{ID: 2, Name: "x, \"y\"", Title: "u", Class: "c", Deps: []string{"a, b", "c\"d"}, Base: "c\"d", State: "pending"}}},
	}
	for name, p := range plans {
		data, err := Render(p)
		if err != nil {
			t.Fatalf("%s: render: %v", name, err)
		}
		got, err := Parse(data)
		if err != nil {
			t.Fatalf("%s: parse: %v\n%s", name, err, data)
		}
		if !reflect.DeepEqual(got, p) {
			t.Errorf("%s: parse(render) =\n%+v\nwant\n%+v", name, got, p)
		}
		again, _ := Render(got)
		if string(again) != string(data) {
			t.Errorf("%s: render is not stable across a parse", name)
		}
	}
}

func TestRenderRefusesAStateTheReaderCouldNotRead(t *testing.T) {
	p := goldenPlan()
	p.State = "Not Ok"
	if _, err := Render(p); err == nil {
		t.Error("render took a state that is not a lowercase word")
	}
	p = goldenPlan()
	p.Tasks[0].State = ""
	if _, err := Render(p); err == nil {
		t.Error("render took an empty task state")
	}
}

func TestParseIsStrict(t *testing.T) {
	good, err := Render(goldenPlan())
	if err != nil {
		t.Fatal(err)
	}
	text := string(good)
	cases := []struct {
		name, from, to, want string
	}{
		{"keys out of order", "title: \"Ship the frobnicator\"\nrepo: \"https://example.invalid/brigada.git\"\n",
			"repo: \"https://example.invalid/brigada.git\"\ntitle: \"Ship the frobnicator\"\n", "title"},
		{"unquoted title", "title: \"Ship the frobnicator\"", "title: Ship the frobnicator", "title"},
		{"single-quoted title", "title: \"Ship the frobnicator\"", "title: 'Ship the frobnicator'", "title"},
		{"trailing text after a string", "repo: \"https://example.invalid/brigada.git\"", "repo: \"https://example.invalid/brigada.git\" # x", "repo"},
		{"a bad plan id", "plan: 7", "plan: seven", "plan"},
		{"a padded plan id", "plan: 7", "plan: 07", "plan"},
		{"a non-UTC time", "created: 2026-10-12T12:00:00Z", "created: 2026-10-12T14:00:00+02:00", "created"},
		{"a capitalised state", "state: active", "state: Active", "state"},
		{"a deps list without the space", "deps: [\"schema\", \"api\"]", "deps: [\"schema\",\"api\"]", "deps"},
		{"deps as a bare list", "deps: [\"schema\"]", "deps: [schema]", "deps"},
		{"a missing task key", "    branch: \"brigada/task-11\"\n", "", "branch"},
		{"bad tier", "    state: done", "    tier: big\n    state: done", "light, standard, heavy"},
		{"bad effort", "    state: done", "    effort: ultra\n    state: done", "low, medium, high, max"},
		{"tier and effort out of order", "    state: done", "    effort: high\n    tier: heavy\n    state: done", "state"},
		{"a negative rounds", "rounds: 2", "rounds: -2", "rounds"},
		{"an unknown key", "    rounds: 2\n", "    rounds: 2\n    extra: 1\n", "extra"},
		{"a task at the wrong indent", "  - id: 13", " - id: 13", "13"},
		{"an empty question", "    question: \"Which \\\"auth\\\" scheme?\\nbearer or basic?\"", "    question: \"\"", "question"},
		{"junk after the tasks", "    rounds: 0\n", "    rounds: 0\nstray: 1\n", "stray"},
	}
	for _, c := range cases {
		if !strings.Contains(text, c.from) {
			t.Fatalf("%s: the golden has no %q", c.name, c.from)
		}
		_, err := Parse([]byte(strings.Replace(text, c.from, c.to, 1)))
		if err == nil {
			t.Errorf("%s: parse took a file the writer would not write", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "line") {
			t.Errorf("%s: error %q should name the line and %q", c.name, err, c.want)
		}
	}
	if _, err := Parse(nil); err == nil {
		t.Error("parse took an empty file")
	}
	if _, err := Parse([]byte(text[:strings.Index(text, "created:")])); err == nil {
		t.Error("parse took a truncated file")
	}
}

func TestParseIgnoresCommentsAndBlankLines(t *testing.T) {
	good, _ := Render(goldenPlan())
	text := "\n# hand note\n" + strings.Replace(string(good), "tasks:\n", "\ntasks:\n  # between\n", 1)
	got, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got, goldenPlan()) {
		t.Errorf("parse = %+v", got)
	}
}

func TestDescribe(t *testing.T) {
	p := goldenPlan()
	if got := Describe(nil, p); got != "created: active, 3 tasks" {
		t.Errorf("first push = %q", got)
	}
	next := goldenPlan()
	next.Tasks[1].State = "running"
	next.Tasks[2].Branch = "brigada/task-13"
	next.State = "done"
	want := "state active -> done; task api needs_foreman -> running; task docs branch brigada/task-13"
	if got := Describe(&p, next); got != want {
		t.Errorf("changes = %q, want %q", got, want)
	}
	next = goldenPlan()
	next.Tasks[1].Rounds = 3
	if got := Describe(&p, next); got != "updated" {
		t.Errorf("rounds only = %q, want updated", got)
	}
	next = goldenPlan()
	next.State = "done"
	next.Tasks[0].State = "pending"
	for i := 0; i < 3; i++ {
		next.Tasks = append(next.Tasks, Task{ID: int64(100 + i), State: "pending"})
	}
	if got := Describe(&p, next); !strings.Contains(got, "and 1 more") {
		t.Errorf("many changes = %q, want the tail folded", got)
	}
}
