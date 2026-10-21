package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

const setupFile = "setup: npm ci --cache \"$BRIGADA_CACHE_DIR\"\n" +
	"setup_cache_key_files: [package-lock.json, .nvmrc]\n" +
	"setup_timeout_minutes: 20\n"

func TestSetupKeysParse(t *testing.T) {
	cfg, err := ParseProjectConfig(cfgFile(setupFile))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		SettingSetup:              `"npm ci --cache \"$BRIGADA_CACHE_DIR\""`,
		SettingSetupCacheKeyFiles: `["package-lock.json",".nvmrc"]`,
		SettingSetupTimeout:       `20`,
	}
	for k, w := range want {
		if got := string(cfg.Values[k]); got != w {
			t.Errorf("%s = %s, want %s", k, got, w)
		}
	}
	for name, body := range map[string]string{
		"setup list":            "setup: [a, b]\n",
		"setup empty":           "setup:\n",
		"timeout zero":          "setup_timeout_minutes: 0\n",
		"timeout quoted":        "setup_timeout_minutes: \"5\"\n",
		"timeout word":          "setup_timeout_minutes: soon\n",
		"key file absolute":     "setup_cache_key_files: [/etc/passwd]\n",
		"key file home":         "setup_cache_key_files: [\"~/x\"]\n",
		"key file out of repo":  "setup_cache_key_files: [../x]\n",
		"key files empty list":  "setup_cache_key_files: []\n",
		"nested setup (design)": "setup:\n  command: x\n",
	} {
		if _, err := ParseProjectConfig(cfgFile(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSetupImportShowAndProvenance(t *testing.T) {
	e := newEnv(t)
	res := e.importCfg(cfgFile(setupFile), false)
	if res.NeedsConfirm || len(res.Applied) != 3 {
		t.Fatalf("result = %+v, want the three setup keys applied", res)
	}
	got, err := e.eng.ProjectSettings(e.ctx, envRepo)
	if err != nil || got == nil {
		t.Fatalf("read: %+v %v", got, err)
	}
	hash := ConfigHash(cfgFile(setupFile))
	for k, v := range map[string]string{
		SettingSetup:              `npm ci --cache "$BRIGADA_CACHE_DIR"`,
		SettingSetupCacheKeyFiles: "package-lock.json+.nvmrc",
		SettingSetupTimeout:       "20",
	} {
		s := settingOf(t, *got, k)
		if s.Value != v || s.Source != SourceImported || s.Hash != hash || s.Default {
			t.Errorf("%s = %+v, want %q imported from %s", k, s, v, hash)
		}
	}

	// A local override of the command wins over a later file and keeps its
	// provenance apart from the keys still imported.
	e.set(map[string]string{SettingSetup: "make deps"})
	res = e.importCfg(cfgFile(setupFile+"default_check: make test\n"), false)
	if len(res.Conflicts) != 1 || res.Conflicts[0].Key != SettingSetup || res.Conflicts[0].Old != "make deps" {
		t.Fatalf("conflicts = %+v, want the local setup kept", res.Conflicts)
	}
	got, _ = e.eng.ProjectSettings(e.ctx, envRepo)
	if s := settingOf(t, *got, SettingSetup); s.Source != SourceLocal || s.Value != "make deps" {
		t.Errorf("setup = %+v, want the local override", s)
	}
	if s := settingOf(t, *got, SettingSetupTimeout); s.Source != SourceImported {
		t.Errorf("setup_timeout_minutes = %+v, want still imported", s)
	}

	// A file that drops a key the import set returns it to the default.
	e.importCfg(cfgFile("setup_cache_key_files: [package-lock.json, .nvmrc]\n"), false)
	got, _ = e.eng.ProjectSettings(e.ctx, envRepo)
	if s := settingOf(t, *got, SettingSetupTimeout); !s.Default || s.Value != "15" {
		t.Errorf("setup_timeout_minutes = %+v, want the default 15", s)
	}

	// project set refuses what the file would.
	for _, bad := range []map[string]string{
		{SettingSetupTimeout: "0"},
		{SettingSetupCacheKeyFiles: "../lock"},
		{SettingSetupCacheKeyFiles: "/abs"},
	} {
		if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, bad); err == nil {
			t.Errorf("project set %v accepted", bad)
		}
	}
}

func TestTaskRecordsTheSetupAtCreation(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalMaxTasks: "-1"})
	none := e.createPlan([]PlanTask{{Spec: "a"}})
	if got := e.str("SELECT COALESCE(setup, '') FROM tasks WHERE id = ?", none.Tasks[0].TaskID); got != "" {
		t.Errorf("no setting: task setup = %q, want none", got)
	}
	e.importCfg(cfgFile(setupFile), false)
	res := e.createPlan([]PlanTask{{Spec: "b"}})
	rec := e.str("SELECT COALESCE(setup, '') FROM tasks WHERE id = ?", res.Tasks[0].TaskID)
	var spec SetupSpec
	if err := json.Unmarshal([]byte(rec), &spec); err != nil {
		t.Fatalf("setup record %q: %v", rec, err)
	}
	if spec.Command != `npm ci --cache "$BRIGADA_CACHE_DIR"` || spec.TimeoutMinutes != 20 ||
		strings.Join(spec.CacheKeyFiles, ",") != "package-lock.json,.nvmrc" ||
		spec.Source != SourceImported || spec.Hash != ConfigHash(cfgFile(setupFile)) {
		t.Errorf("recorded setup = %+v, want the imported setting with its source and hash", spec)
	}

	// A later change leaves the recorded task alone; a task made after it
	// records the new value, with the default timeout.
	e.set(map[string]string{SettingSetup: "make deps", SettingSetupTimeout: ""})
	if got := e.str("SELECT setup FROM tasks WHERE id = ?", res.Tasks[0].TaskID); got != rec {
		t.Errorf("task setup after a setting change = %q, want %q", got, rec)
	}
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Spec: "c"}}})
	var later SetupSpec
	if err := json.Unmarshal([]byte(e.str("SELECT setup FROM tasks WHERE id = ?", am.Added[0].TaskID)), &later); err != nil {
		t.Fatal(err)
	}
	if later.Command != "make deps" || later.TimeoutMinutes != defaultSetupTimeoutMinutes || later.Source != SourceLocal {
		t.Errorf("later task setup = %+v, want the local command and the default timeout", later)
	}

	// The lease grant carries the record, not the project's setting.
	r := e.agented("author", "fam", "implement")
	e.cancel(e.int("SELECT id FROM jobs WHERE task_id = ?", none.Tasks[0].TaskID))
	g := e.leaseAny(r)
	if g == nil || g.TaskID != res.Tasks[0].TaskID || g.Setup == nil || g.Setup.Command != spec.Command {
		t.Fatalf("grant = %+v, want the recorded setup of task %d", g, res.Tasks[0].TaskID)
	}
}

func TestSetupFailureIsRetriedAsInfrastructure(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "fam", "implement")
	e.submit(e.work())
	fail := func() *LeaseGrant {
		g := e.leaseAny(author)
		if g == nil {
			t.Fatal("nothing to lease")
		}
		if ans := e.complete(g, Completion{Error: "setup failed: exit code 1", ErrorKind: ErrorKindSetup}); ans != AnsOK {
			t.Fatalf("complete = %s", ans)
		}
		return g
	}
	for i := 1; i <= setupRetries; i++ {
		fail()
		if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskReady {
			t.Fatalf("after setup failure %d: task state = %s, want ready (retried)", i, state)
		}
		// The retry waits out the growing delay before it may be leased.
		e.clock.Advance(retryDelayFor(i))
	}
	if n := e.countEvents(evTaskFailed); n != 0 {
		t.Errorf("task_failed events = %d, want none while retries remain", n)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", e.task); n != setupRetries {
		t.Errorf("retry jobs = %d, want %d", n, setupRetries)
	}
	// The retries are spent: the foreman judges it.
	fail()
	if state := e.str("SELECT state FROM tasks WHERE id = ?", e.task); state != taskNeedsForeman {
		t.Errorf("task state = %s, want needs_foreman once the retries are out", state)
	}
	if n := e.countEvents(evTaskFailed); n != 1 {
		t.Errorf("task_failed events = %d, want one", n)
	}
	// Neither agent health nor a fix round is charged: three failed jobs
	// on one agent are past the failure streak.
	if h := e.str("SELECT health FROM agents WHERE id = ?", e.agentOf(author)); h != "ready" {
		t.Errorf("agent health = %s, want ready", h)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND kind = 'fix'", e.task); n != 0 {
		t.Errorf("fix jobs = %d, want none", n)
	}
}
