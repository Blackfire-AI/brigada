package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	schemaSSH  = "git@example.invalid:org/schema.git"
	schemaCan  = "example.invalid/org/schema"
	toolsHTTPS = "https://example.invalid/org/tools.git"
	toolsCan   = "example.invalid/org/tools"
	commitSHA  = "0123456789abcdef0123456789abcdef01234567"
)

// extraRecord reads the extra repositories a task recorded.
func (e *env) extraRecord(task int64) *ExtraReposSpec {
	e.t.Helper()
	return parseExtraReposRecord(e.str("SELECT COALESCE(extra_repos, '') FROM tasks WHERE id = ?", task))
}

func extraNames(spec *ExtraReposSpec) string {
	if spec == nil {
		return ""
	}
	var names []string
	for _, x := range spec.Repos {
		names = append(names, x.Name)
	}
	return strings.Join(names, ",")
}

func TestExtraReposAreCanonicalizedAtCreation(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{{Spec: "a", ExtraRepos: []ExtraRepo{
		{Name: "schema", Repo: schemaSSH, Ref: "v2.3.0"},
		{Name: "tools", Repo: "https://user:tok3n@Example.INVALID/org/tools.git/"},
	}}})
	spec := e.extraRecord(res.Tasks[0].TaskID)
	if spec == nil || spec.Source != ExtraSourceTask || len(spec.Repos) != 2 {
		t.Fatalf("record = %+v, want two entries from the task", spec)
	}
	if x := spec.Repos[0]; x.Name != "schema" || x.Repo != schemaCan || x.Ref != "v2.3.0" || x.URL == "" {
		t.Errorf("schema = %+v, want the canonical identity, the ref and a fetch URL", x)
	}
	if x := spec.Repos[1]; x.Repo != toolsCan || x.Ref != "" {
		t.Errorf("tools = %+v, want the canonical identity and the default branch", x)
	}
	// SSH and HTTPS forms of one repository are one identity, and a token
	// in a URL never reaches the record.
	raw := e.str("SELECT extra_repos FROM tasks WHERE id = ?", res.Tasks[0].TaskID)
	if strings.Contains(raw, "tok3n") || strings.Contains(raw, "user:") {
		t.Errorf("the record holds a credential: %s", raw)
	}
	other := e.createPlan([]PlanTask{{Spec: "b", ExtraRepos: []ExtraRepo{{Name: "s", Repo: "https://example.invalid/org/schema"}}}})
	if got := e.extraRecord(other.Tasks[0].TaskID).Repos[0].Repo; got != schemaCan {
		t.Errorf("https form = %q, want the same identity %q", got, schemaCan)
	}
	// A fetch URL a caller sends is ignored: the runner fetches what the
	// repo field names.
	forged := e.createPlan([]PlanTask{{Spec: "c", ExtraRepos: []ExtraRepo{{Name: "s", Repo: schemaSSH, URL: "https://evil.invalid/x.git"}}}})
	if got := e.extraRecord(forged.Tasks[0].TaskID).Repos[0].URL; strings.Contains(got, "evil") {
		t.Errorf("a caller's own URL was recorded: %q", got)
	}
}

func TestExtraReposRefuseWhatCannotBeRun(t *testing.T) {
	cases := map[string][]ExtraRepo{
		"two names, one variable": {{Name: "a-b", Repo: schemaSSH}, {Name: "a_b", Repo: toolsHTTPS}},
		"one name twice":          {{Name: "a", Repo: schemaSSH}, {Name: "a", Repo: toolsHTTPS}},
		"capital":                 {{Name: "Schema", Repo: schemaSSH}},
		"leading digit":           {{Name: "1x", Repo: schemaSSH}},
		"path in the name":        {{Name: "../x", Repo: schemaSSH}},
		"empty name":              {{Name: "", Repo: schemaSSH}},
		"no repo":                 {{Name: "a"}},
		"bare host and path":      {{Name: "a", Repo: "example.invalid/org/schema"}},
		"ref that is an option":   {{Name: "a", Repo: schemaSSH, Ref: "--upload-pack=x"}},
		"ref with a space":        {{Name: "a", Repo: schemaSSH, Ref: "v1 v2"}},
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			plans := e.int("SELECT COUNT(*) FROM plans")
			_, err := e.eng.CreatePlan(e.ctx, e.project, "p", []PlanTask{{Spec: "a", ExtraRepos: list}})
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "tok") {
				t.Errorf("error quotes a URL: %v", err)
			}
			if n := e.int("SELECT COUNT(*) FROM plans"); n != plans {
				t.Errorf("plans = %d after a refusal, want the %d there were", n, plans)
			}
		})
	}
	e := newEnv(t)
	_, err := e.eng.CreatePlan(e.ctx, e.project, "p", []PlanTask{{Spec: "a", ExtraRepos: cases["two names, one variable"]}})
	if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), "BRIGADA_REPO_A_B") {
		t.Errorf("clash error = %v, want it to name the variable", err)
	}
}

func TestExtraRepoEnvVar(t *testing.T) {
	for name, want := range map[string]string{"schema": "BRIGADA_REPO_SCHEMA", "a-b": "BRIGADA_REPO_A_B", "gen-client_2": "BRIGADA_REPO_GEN_CLIENT_2"} {
		if got := (ExtraRepo{Name: name}).EnvVar(); got != want {
			t.Errorf("EnvVar(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestExtraReposProjectDefault(t *testing.T) {
	e := newEnv(t)
	find := func(ps ProjectSettings) SettingValue { return settingOf(t, ps, SettingExtraRepos) }
	if sv := find(e.set(map[string]string{SettingExtraRepos: "schema " + schemaSSH + " v1+tools " + toolsHTTPS})); sv.Default || sv.Source != SourceLocal ||
		!strings.Contains(sv.Value, "schema") || !strings.Contains(sv.Value, "tools") {
		t.Errorf("extra_repos = %+v, want the two local entries", sv)
	}
	if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingExtraRepos: "only-a-name"}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("an item with no URL: %v, want ErrInvalidSetting", err)
	}
	if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingExtraRepos: "a-b " + schemaSSH + "+a_b " + toolsHTTPS}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("a clashing default: %v, want ErrInvalidSetting", err)
	}

	// Omitted: the task inherits the default, with its source; the plan
	// runs without asking.
	res := e.createPlan([]PlanTask{{Spec: "a"}})
	e.assertRuns(res, 1)
	spec := e.extraRecord(res.Tasks[0].TaskID)
	if spec == nil || spec.Source != ExtraSourceDefault || extraNames(spec) != "schema,tools" || spec.Repos[0].Ref != "v1" {
		t.Fatalf("inherited record = %+v", spec)
	}
	before := e.str("SELECT extra_repos FROM tasks WHERE id = ?", res.Tasks[0].TaskID)

	// A later change to the default leaves a recorded task alone.
	e.set(map[string]string{SettingExtraRepos: "only " + toolsHTTPS})
	if got := e.str("SELECT extra_repos FROM tasks WHERE id = ?", res.Tasks[0].TaskID); got != before {
		t.Errorf("record after a default change = %s, want %s", got, before)
	}
	later := e.createPlan([]PlanTask{{Spec: "b"}})
	if got := extraNames(e.extraRecord(later.Tasks[0].TaskID)); got != "only" {
		t.Errorf("task made after the change records %q, want the new default", got)
	}

	// A given list, even [], replaces the default entirely.
	empty := e.createPlan([]PlanTask{{Spec: "c", ExtraRepos: []ExtraRepo{}}})
	if spec := e.extraRecord(empty.Tasks[0].TaskID); spec != nil {
		t.Errorf("[] recorded %+v, want no extra repositories", spec)
	}
	// An entry with a default's name wins; the rest of the default is gone.
	e.set(map[string]string{SettingExtraRepos: "schema " + schemaSSH + "+tools " + toolsHTTPS})
	clash := e.createPlan([]PlanTask{{Spec: "d", ExtraRepos: []ExtraRepo{{Name: "schema", Repo: toolsHTTPS, Ref: commitSHA}}}})
	spec = e.extraRecord(clash.Tasks[0].TaskID)
	if spec == nil || spec.Source != ExtraSourceTask || len(spec.Repos) != 1 || spec.Repos[0].Repo != toolsCan || spec.Repos[0].Ref != commitSHA {
		t.Errorf("clash record = %+v, want the task's own entry alone", spec)
	}

	// Reset: back to none.
	if sv := find(e.set(map[string]string{SettingExtraRepos: ""})); sv.Value != "none" || !sv.Default {
		t.Errorf("after reset = %+v", sv)
	}
}

func TestExtraReposDefaultFromProjectFile(t *testing.T) {
	file := "extra_repos:\n  - schema " + schemaSSH + " v2.3.0\n  - \"tools " + toolsHTTPS + "\"\n"
	cfg, err := ParseProjectConfig(cfgFile(file))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var list []ExtraRepo
	if err := json.Unmarshal(cfg.Values[SettingExtraRepos], &list); err != nil || len(list) != 2 ||
		list[0].Repo != schemaCan || list[0].Ref != "v2.3.0" || list[1].Repo != toolsCan || list[1].URL == "" {
		t.Fatalf("parsed = %s (%v)", cfg.Values[SettingExtraRepos], err)
	}
	if _, err := ParseProjectConfig(cfgFile("extra_repos: [\"a " + schemaSSH + "\"]\n")); err != nil {
		t.Errorf("flow list: %v", err)
	}
	for name, body := range map[string]string{
		"no url":         "extra_repos: [schema]\n",
		"four parts":     "extra_repos: [\"a " + schemaSSH + " v1 extra\"]\n",
		"bad name":       "extra_repos: [\"Schema " + schemaSSH + "\"]\n",
		"clash":          "extra_repos: [\"a-b " + schemaSSH + "\", \"a_b " + toolsHTTPS + "\"]\n",
		"empty list":     "extra_repos: []\n",
		"nested (maps)":  "extra_repos:\n  - name: schema\n    repo: x\n",
		"bare host/path": "extra_repos: [\"a example.invalid/org/schema\"]\n",
	} {
		if _, err := ParseProjectConfig(cfgFile(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	e := newEnv(t)
	res := e.importCfg(cfgFile(file), false)
	if res.NeedsConfirm || len(res.Applied) != 1 || res.Applied[0].Key != SettingExtraRepos {
		t.Fatalf("import = %+v, want extra_repos applied", res)
	}
	got, err := e.eng.ProjectSettings(e.ctx, envRepo)
	if err != nil || got == nil {
		t.Fatalf("show: %+v %v", got, err)
	}
	sv := settingOf(t, *got, SettingExtraRepos)
	if sv.Source != SourceImported || sv.Hash != ConfigHash(cfgFile(file)) || !strings.Contains(sv.Value, "schema") || !strings.Contains(sv.Value, "v2.3.0") {
		t.Errorf("extra_repos = %+v, want the imported list with its provenance", sv)
	}
	// A task made after the import records where the list came from.
	res2 := e.createPlan([]PlanTask{{Spec: "a"}})
	spec := e.extraRecord(res2.Tasks[0].TaskID)
	if spec == nil || spec.Source != ExtraSourceDefault || spec.Hash != ConfigHash(cfgFile(file)) {
		t.Errorf("record = %+v, want project-default with the imported file's hash", spec)
	}
	// A local override stands against the file, as for any key.
	e.set(map[string]string{SettingExtraRepos: "mine " + toolsHTTPS})
	if again := e.importCfg(cfgFile(file), false); len(again.Conflicts) != 1 {
		t.Errorf("re-import over a local override = %+v, want the conflict shown", again)
	}
}

func TestExtraReposApproval(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingExtraRepos: "schema " + schemaSSH})

	// Omitted, or only what the default already names: no reason to ask.
	e.assertRuns(e.createPlan([]PlanTask{{Spec: "a"}}), 1)
	e = newEnv(t)
	e.set(map[string]string{SettingExtraRepos: "schema " + schemaSSH})
	e.assertRuns(e.createPlan([]PlanTask{{Spec: "a", ExtraRepos: []ExtraRepo{{Name: "again", Repo: "https://example.invalid/org/schema", Ref: "v9"}}}}), 1)
	e = newEnv(t)
	e.assertRuns(e.createPlan([]PlanTask{{Spec: "a", ExtraRepos: []ExtraRepo{}}}), 1)

	// A repository outside the project default asks, and says which, with
	// its ref and whether the ref is a commit.
	e = newEnv(t)
	e.set(map[string]string{SettingExtraRepos: "schema " + schemaSSH})
	res := e.createPlan([]PlanTask{{Spec: "a", ExtraRepos: []ExtraRepo{
		{Name: "tools", Repo: toolsHTTPS, Ref: "main"},
		{Name: "pinned", Repo: "https://example.invalid/org/pinned.git", Ref: commitSHA},
		{Name: "floating", Repo: "https://example.invalid/org/floating.git"},
	}}})
	e.assertAwaits(res, "extra repositories outside the project default")
	why := strings.Join(res.Reasons, "; ")
	for _, want := range []string{"tools (" + toolsCan + ", ref main, not a commit)", "pinned (example.invalid/org/pinned, commit " + commitSHA + ")", "floating (example.invalid/org/floating, default branch, not a commit)"} {
		if !strings.Contains(why, want) {
			t.Errorf("reasons %q miss %q", why, want)
		}
	}
	if !strings.Contains(e.str("SELECT payload FROM events WHERE kind = ?", evPlanAwaitingApproval), "tools") {
		t.Error("the plan_awaiting_approval event does not name the repository")
	}

	// The limit being off (a negative max) is the user's choice to run
	// everything.
	e = newEnv(t)
	e.set(map[string]string{SettingApprovalMaxTasks: "-1"})
	e.assertRuns(e.createPlan([]PlanTask{{Spec: "a", ExtraRepos: []ExtraRepo{{Name: "tools", Repo: toolsHTTPS}}}}), 1)
}

func TestExtraReposAmendment(t *testing.T) {
	e := newEnv(t)
	res := e.createPlan([]PlanTask{{Name: "a", Spec: "a"}})
	e.assertRuns(res, 1)

	// An added task inheriting the default adds nothing to ask about; one
	// that names a repository outside it holds the plan.
	e.set(map[string]string{SettingExtraRepos: "schema " + schemaSSH})
	plain := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Spec: "b"}}})
	if plain.State != "active" {
		t.Fatalf("state = %s, want active", plain.State)
	}
	if spec := e.extraRecord(plain.Added[0].TaskID); spec == nil || spec.Source != ExtraSourceDefault {
		t.Errorf("amend-added record = %+v, want the project default", spec)
	}
	held := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Spec: "c", ExtraRepos: []ExtraRepo{{Name: "tools", Repo: toolsHTTPS, Ref: "v1"}}}}})
	if held.State != planAwaitingApproval || !strings.Contains(strings.Join(held.Reasons, ";"), "tools ("+toolsCan+", ref v1, not a commit)") {
		t.Fatalf("held = %s %v, want the plan held for the repository outside the default", held.State, held.Reasons)
	}
	if spec := e.extraRecord(held.Added[0].TaskID); spec == nil || spec.Source != ExtraSourceTask || spec.Repos[0].Repo != toolsCan {
		t.Errorf("amend-added record = %+v", spec)
	}
	// A refused list refuses the whole amendment.
	_, err := e.eng.AmendPlan(e.ctx, e.project, res.PlanID, PlanAmendment{Add: []PlanTask{{Spec: "d", ExtraRepos: []ExtraRepo{{Name: "x", Repo: "nohost"}}}}})
	if err == nil {
		t.Error("an amendment with an unusable repository was accepted")
	}
}

func TestExtraReposReachTheLeaseGrant(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalMaxTasks: "-1", SettingExtraRepos: "schema " + schemaSSH + " v1"})
	author := e.agented("author", "fam", "implement")
	own := e.createPlan([]PlanTask{{Spec: "own", ExtraRepos: []ExtraRepo{{Name: "tools", Repo: toolsHTTPS}}}})
	g := e.leaseAny(author)
	if g == nil || g.TaskID != own.Tasks[0].TaskID || len(g.ExtraRepos) != 1 || g.ExtraRepos[0].Name != "tools" ||
		g.ExtraRepos[0].URL == "" || g.ExtraRepos[0].Repo != toolsCan {
		t.Fatalf("grant = %+v, want the task's own list with its fetch URL", g)
	}
}
