package engine

import (
	"errors"
	"strings"
	"testing"
)

const cfgRepo = "example.invalid/brigada"

// cfgFile is a schema-1 file naming the fixture's repo, plus body.
func cfgFile(body string) []byte {
	return []byte("schema: 1\nrepo: " + cfgRepo + "\n" + body)
}

func TestParseProjectConfigTable(t *testing.T) {
	const all = `default_check: go vet ./... && go test ./...  # the gate
worker_tools: Bash(go test:*)+Bash(go vet:*)
approval_max_tasks: 3
approval_classes: [implement, "review"]
approval_protected_paths:
  - infra/**
  - 'db/*.sql'
`
	cases := []struct {
		name    string
		content string
		wantErr string // "" = accepted
	}{
		{"minimal", "schema: 1\nrepo: " + cfgRepo + "\n", ""},
		{"leading document marker and comments", "---\n# shared\nschema: 1\nrepo: " + cfgRepo + " # origin\n", ""},
		{"every key", string(cfgFile(all)), ""},
		{"file remote identity", "schema: 1\nrepo: file:/srv/git/brigada\n", ""},
		{"none tool scope", string(cfgFile("worker_tools: none\n")), ""},
		{"empty file", "", "schema is required"},
		{"no schema", "repo: " + cfgRepo + "\n", "schema is required"},
		{"unknown schema version", "schema: 2\nrepo: " + cfgRepo + "\nfuture_key: x\n", "unknown schema version 2"},
		{"quoted schema", "schema: \"1\"\nrepo: " + cfgRepo + "\n", "schema must be the integer 1"},
		{"no repo", "schema: 1\n", "repo is required"},
		{"unknown key", string(cfgFile("agents: [claude]\n")), `unknown key "agents"`},
		{"secret-bearing key", string(cfgFile("api_token: abc\n")), `unknown key "api_token"`},
		{"local path key", string(cfgFile("state_dir: /home/me/.state\n")), `unknown key "state_dir"`},
		// A key of the retired mode A (hold bounds, quiet window) in an
		// existing file is reported as unknown, never a crash.
		{"retired mode A key", string(cfgFile("hold_idle_minutes: 20\n")), `unknown key "hold_idle_minutes"`},
		{"retired quiet key", string(cfgFile("interactive_quiet_minutes: 5\n")), `unknown key "interactive_quiet_minutes"`},
		{"local-only wake budget", string(cfgFile("foreman_wakes_per_day: 30\n")), `foreman_wakes_per_day (line 3) is a local-only setting`},
		{"duplicate key", string(cfgFile("default_check: a\ndefault_check: b\n")), `key "default_check" appears twice`},
		{"repo with a scheme", "schema: 1\nrepo: https://example.invalid/brigada.git\n", "canonical form"},
		{"repo with a credential", "schema: 1\nrepo: https://tok:secret@example.invalid/brigada\n", "canonical form"},
		{"repo relative path", "schema: 1\nrepo: ../brigada\n", "repo:"},
		{"repo list", "schema: 1\nrepo: [a]\n", "canonical repo URL string"},
		{"max tasks not an integer", string(cfgFile("approval_max_tasks: many\n")), "approval_max_tasks takes an integer"},
		{"max tasks quoted", string(cfgFile("approval_max_tasks: \"3\"\n")), "approval_max_tasks (line 3) must be an integer"},
		{"max tasks list", string(cfgFile("approval_max_tasks: [3]\n")), "must be an integer"},
		{"default check list", string(cfgFile("default_check: [a, b]\n")), "must be a string"},
		{"default check empty", string(cfgFile("default_check:\n")), "has no value"},
		{"worker tools empty scope", string(cfgFile("worker_tools: \"+\"\n")), "worker_tools"},
		{"classes with a bad word", string(cfgFile("approval_classes: [\"has space\"]\n")), "not a class name"},
		{"classes empty list", string(cfgFile("approval_classes: []\n")), "empty list"},
		{"list item holding the separator", string(cfgFile("approval_protected_paths: [\"a+b\"]\n")), "separates list items"},
		{"protected path absolute", string(cfgFile("approval_protected_paths: [/etc/passwd]\n")), "repo-relative"},
		{"protected path home", string(cfgFile("approval_protected_paths: [\"~/x\"]\n")), "repo-relative"},
		{"protected path bad glob", string(cfgFile("approval_protected_paths: [\"bad[glob\"]\n")), "not a valid glob"},
		{"nested map", string(cfgFile("default_check:\n  cmd: x\n")), "nested structures"},
		{"list under a scalar", string(cfgFile("default_check: x\n  - y\n")), "already has a value"},
		{"second document", string(cfgFile("---\n")), "one document per file"},
		{"tab", string(cfgFile("default_check:\tx\n")), "tab"},
		{"anchor", string(cfgFile("default_check: &a x\n")), "quote the value"},
		{"mapping in a plain value", string(cfgFile("default_check: a: b\n")), "quote it"},
		{"not key value", string(cfgFile("just words\n")), "want `key: value`"},
		{"oversized", string(cfgFile("default_check: " + strings.Repeat("x", 70<<10) + "\n")), "byte limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseProjectConfig([]byte(tc.content))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				if cfg.Repo == "" {
					t.Fatalf("no repo in %+v", cfg)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parse err = %v, want it to contain %q", err, tc.wantErr)
			}
			if !errors.Is(err, ErrInvalidSetting) {
				t.Errorf("err = %v, want ErrInvalidSetting", err)
			}
		})
	}
	t.Run("values take the stored shape", func(t *testing.T) {
		cfg, err := ParseProjectConfig(cfgFile(all))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			SettingDefaultCheck:           "\"go vet ./... \\u0026\\u0026 go test ./...\"",
			SettingWorkerTools:            `"Bash(go test:*)+Bash(go vet:*)"`,
			SettingApprovalMaxTasks:       `3`,
			SettingApprovalClasses:        `["implement","review"]`,
			SettingApprovalProtectedPaths: `["infra/**","db/*.sql"]`,
		}
		for k, w := range want {
			if got := string(cfg.Values[k]); got != w {
				t.Errorf("%s = %s, want %s", k, got, w)
			}
		}
	})
}

func settingOf(t *testing.T, ps ProjectSettings, key string) SettingValue {
	t.Helper()
	for _, s := range ps.Settings {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no setting %s in %+v", key, ps.Settings)
	return SettingValue{}
}

func (e *env) importCfg(content []byte, accept bool) ImportResult {
	e.t.Helper()
	res, err := e.eng.ImportProjectConfig(e.ctx, envRepo, content, accept)
	if err != nil {
		e.t.Fatalf("import: %v", err)
	}
	return res
}

func TestImportApplies(t *testing.T) {
	e := newEnv(t)
	content := cfgFile("default_check: make test\napproval_max_tasks: 2\nworker_tools: Bash(make:*)\n")
	res := e.importCfg(content, false)
	if res.NeedsConfirm || len(res.Applied) != 3 || len(res.Conflicts) != 0 {
		t.Fatalf("result = %+v, want 3 applied", res)
	}
	hash := ConfigHash(content)
	if res.Hash != hash || res.Settings == nil || res.Settings.ImportHash != hash {
		t.Fatalf("hash = %s / %+v, want %s", res.Hash, res.Settings, hash)
	}
	got, err := e.eng.ProjectSettings(e.ctx, envRepo)
	if err != nil || got == nil {
		t.Fatalf("read: %+v %v", got, err)
	}
	for k, v := range map[string]string{SettingDefaultCheck: "make test", SettingApprovalMaxTasks: "2", SettingWorkerTools: "Bash(make:*)"} {
		s := settingOf(t, *got, k)
		if s.Value != v || s.Source != SourceImported || s.Hash != hash || s.Default {
			t.Errorf("%s = %+v, want %q imported from %s", k, s, v, hash)
		}
	}
	if s := settingOf(t, *got, SettingApprovalClasses); s.Source != SourceDefault || !s.Default {
		t.Errorf("approval_classes = %+v, want the default", s)
	}
	// The imported limit is the one the approval gate reads.
	e.assertAwaits(e.createPlan(nTasks(3)), "3 tasks, over the limit of 2")
}

func TestImportLocalOverrideWinsWithConflict(t *testing.T) {
	e := newEnv(t)
	e.importCfg(cfgFile("default_check: make test\napproval_max_tasks: 2\n"), false)
	e.set(map[string]string{SettingDefaultCheck: "go test ./..."})

	res := e.importCfg(cfgFile("default_check: make check\napproval_max_tasks: 4\n"), false)
	if len(res.Conflicts) != 1 || res.Conflicts[0] != (ConfigChange{SettingDefaultCheck, "go test ./...", "make check"}) {
		t.Fatalf("conflicts = %+v", res.Conflicts)
	}
	if len(res.Applied) != 1 || res.Applied[0].Key != SettingApprovalMaxTasks {
		t.Fatalf("applied = %+v, want only approval_max_tasks", res.Applied)
	}
	if s := settingOf(t, *res.Settings, SettingDefaultCheck); s.Value != "go test ./..." || s.Source != SourceLocal {
		t.Errorf("default_check = %+v, want the local override kept", s)
	}
	if s := settingOf(t, *res.Settings, SettingApprovalMaxTasks); s.Value != "4" || s.Source != SourceImported {
		t.Errorf("approval_max_tasks = %+v, want 4 imported", s)
	}

	// The same file again is the same conflict, still no silent overwrite.
	if res = e.importCfg(cfgFile("default_check: make check\napproval_max_tasks: 4\n"), true); len(res.Conflicts) != 1 {
		t.Fatalf("repeat conflicts = %+v", res.Conflicts)
	}

	// Resetting the override hands the key back to the file.
	e.set(map[string]string{SettingDefaultCheck: ""})
	res = e.importCfg(cfgFile("default_check: make check\napproval_max_tasks: 4\n"), false)
	if s := settingOf(t, *res.Settings, SettingDefaultCheck); s.Value != "make check" || s.Source != SourceImported || len(res.Conflicts) != 0 {
		t.Errorf("default_check = %+v (conflicts %+v), want make check imported", s, res.Conflicts)
	}

	// A set to the very value the import holds is still the user's.
	e.set(map[string]string{SettingApprovalMaxTasks: "4"})
	if got, _ := e.eng.ProjectSettings(e.ctx, envRepo); settingOf(t, *got, SettingApprovalMaxTasks).Source != SourceLocal {
		t.Errorf("a set after the import should make the key local: %+v", got.Settings)
	}
}

func TestReimportAfterFileChange(t *testing.T) {
	e := newEnv(t)
	v1 := cfgFile("default_check: make test\napproval_classes: [implement]\n")
	e.importCfg(v1, false)

	// Same content: nothing to do.
	if res := e.importCfg(v1, false); len(res.Applied) != 0 || len(res.Conflicts) != 0 {
		t.Fatalf("re-import of the same file = %+v", res)
	}

	// A change over a value that is still the previous import's applies;
	// a key the file dropped returns to its default.
	v2 := cfgFile("default_check: make check\n")
	res := e.importCfg(v2, false)
	want := []ConfigChange{
		{SettingApprovalClasses, "implement", "default"},
		{SettingDefaultCheck, "make test", "make check"},
	}
	if len(res.Applied) != 2 || res.Applied[0] != want[0] || res.Applied[1] != want[1] {
		t.Fatalf("applied = %+v, want %+v", res.Applied, want)
	}
	if s := settingOf(t, *res.Settings, SettingDefaultCheck); s.Hash != ConfigHash(v2) {
		t.Errorf("default_check hash = %s, want the new import's %s", s.Hash, ConfigHash(v2))
	}
	if s := settingOf(t, *res.Settings, SettingApprovalClasses); !s.Default {
		t.Errorf("approval_classes = %+v, want the default", s)
	}
	if res.Settings.ImportHash != ConfigHash(v2) {
		t.Errorf("import hash = %s", res.Settings.ImportHash)
	}
}

func TestFirstImportOverHandSetValuesAsksFirst(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingDefaultCheck: "hand check", SettingApprovalMaxTasks: "7"})
	before := e.str("SELECT settings || '|' || settings_prov FROM projects WHERE id = ?", e.project)

	content := cfgFile("default_check: file check\napproval_max_tasks: 7\nworker_tools: none\n")
	res := e.importCfg(content, false)
	if !res.NeedsConfirm || len(res.Diffs) != 1 || res.Diffs[0] != (ConfigChange{SettingDefaultCheck, "hand check", "file check"}) {
		t.Fatalf("result = %+v, want one diff and NeedsConfirm", res)
	}
	if res.Settings != nil || len(res.Applied) != 0 {
		t.Fatalf("a pending import reports nothing applied: %+v", res)
	}
	if after := e.str("SELECT settings || '|' || settings_prov FROM projects WHERE id = ?", e.project); after != before {
		t.Fatalf("store changed before acceptance: %s -> %s", before, after)
	}

	res = e.importCfg(content, true)
	if res.NeedsConfirm || res.Settings == nil {
		t.Fatalf("accepted result = %+v", res)
	}
	if s := settingOf(t, *res.Settings, SettingDefaultCheck); s.Value != "file check" || s.Source != SourceImported {
		t.Errorf("default_check = %+v, want the file's, imported", s)
	}
	// The hand-set value the file agrees with stays the user's own.
	if s := settingOf(t, *res.Settings, SettingApprovalMaxTasks); s.Value != "7" || s.Source != SourceLocal {
		t.Errorf("approval_max_tasks = %+v, want 7 still local", s)
	}
	if s := settingOf(t, *res.Settings, SettingWorkerTools); s.Value != "none" || s.Source != SourceImported {
		t.Errorf("worker_tools = %+v, want none imported", s)
	}

	// After a first import the next one is a conflict, not a question.
	e.set(map[string]string{SettingDefaultCheck: "mine again"})
	res = e.importCfg(cfgFile("default_check: file v2\n"), false)
	if res.NeedsConfirm || len(res.Conflicts) != 1 {
		t.Fatalf("second import = %+v, want a conflict and no question", res)
	}
}

func TestImportRefusals(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingDefaultCheck: "hand check"})
	before := e.str("SELECT settings || '|' || settings_prov FROM projects WHERE id = ?", e.project)

	t.Run("repo mismatch", func(t *testing.T) {
		other := []byte("schema: 1\nrepo: example.invalid/other\ndefault_check: x\n")
		_, err := e.eng.ImportProjectConfig(e.ctx, envRepo, other, true)
		if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), "not this repository's origin") {
			t.Fatalf("err = %v, want a repo mismatch", err)
		}
	})
	t.Run("the origin spelling does not matter", func(t *testing.T) {
		for _, origin := range []string{"git@example.invalid:brigada.git", "ssh://git@example.invalid/brigada", "https://user:pw@example.invalid/brigada.git"} {
			if _, err := e.eng.ImportProjectConfig(e.ctx, origin, []byte("schema: 1\nrepo: example.invalid/other\n"), true); err == nil || strings.Contains(err.Error(), "pw") {
				t.Errorf("origin %q: err = %v, want a mismatch with no credential in it", origin, err)
			}
		}
	})
	t.Run("invalid file", func(t *testing.T) {
		_, err := e.eng.ImportProjectConfig(e.ctx, envRepo, cfgFile("token: s3cret\n"), true)
		if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), `unknown key "token"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a refused import changes nothing", func(t *testing.T) {
		if after := e.str("SELECT settings || '|' || settings_prov FROM projects WHERE id = ?", e.project); after != before {
			t.Fatalf("store changed: %s -> %s", before, after)
		}
	})
	t.Run("a file remote matches its canonical identity", func(t *testing.T) {
		res, err := e.eng.ImportProjectConfig(e.ctx, "file:///srv/git/brigada", []byte("schema: 1\nrepo: file:/srv/git/brigada\ndefault_check: x\n"), false)
		if err != nil || res.Settings == nil || settingOf(t, *res.Settings, SettingDefaultCheck).Source != SourceImported {
			t.Fatalf("file remote import = %+v, %v", res, err)
		}
	})
}
