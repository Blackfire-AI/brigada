package engine

import (
	"errors"
	"strings"
	"testing"
)

const envRepo = "https://example.invalid/brigada.git"

// set applies key=value pairs the way the CLI sends them.
func (e *env) set(values map[string]string) ProjectSettings {
	e.t.Helper()
	got, err := e.eng.SetProjectSettings(e.ctx, envRepo, values)
	if err != nil {
		e.t.Fatalf("set settings %v: %v", values, err)
	}
	return got
}

// The approval limit reads what SetProjectSettings wrote: every value
// the CLI's parser accepts is one the limit understands.
func TestApprovalReadsWrittenSettings(t *testing.T) {
	t.Run("zero waits every plan", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalMaxTasks: "0"})
		e.assertAwaits(e.createPlan(nTasks(1)), "every plan needs approval")
	})
	t.Run("negative switches the limit off", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalMaxTasks: "-1"})
		e.assertRuns(e.createPlan(nTasks(9)), 9)
	})
	t.Run("a raised limit", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalMaxTasks: "8"})
		e.assertRuns(e.createPlan(nTasks(8)), 8)
	})
	t.Run("classes", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalClasses: "review+answer"})
		e.assertAwaits(e.createPlan(nTasks(1)), "class implement outside review,answer")
	})
	t.Run("protected paths", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalProtectedPaths: "infra/**+db/*.sql"})
		e.assertAwaits(e.createPlan([]PlanTask{{Spec: "edit infra/main.tf"}}), "protected paths: infra/**")
	})
	t.Run("reset restores the default", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalMaxTasks: "0"})
		e.set(map[string]string{SettingApprovalMaxTasks: ""})
		e.assertRuns(e.createPlan(nTasks(5)), 5)
	})
}

func TestProjectSettingsView(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_max_tasks": 3, "other": "kept"}`)
	got := e.set(map[string]string{SettingApprovalProtectedPaths: "infra/**"})
	want := []SettingValue{
		{Key: SettingApprovalMaxTasks, Value: "3", Source: SourceLocal},
		{Key: SettingApprovalClasses, Value: "implement+review+answer", Default: true, Source: SourceDefault},
		{Key: SettingApprovalProtectedPaths, Value: "infra/**", Source: SourceLocal},
		{Key: SettingDefaultCheck, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingWorkerTools, Value: "agent default", Default: true, Source: SourceDefault},
		{Key: SettingSetup, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingSetupCacheKeyFiles, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingSetupTimeout, Value: "15", Default: true, Source: SourceDefault},
		{Key: SettingHoldIdleMinutes, Value: "15", Default: true, Source: SourceDefault},
		{Key: SettingHoldMaxMinutes, Value: "240", Default: true, Source: SourceDefault},
		{Key: SettingInstructions, Value: "none", Default: true, Source: SourceDefault},
	}
	if len(got.Settings) != len(want) {
		t.Fatalf("settings = %+v, want %+v", got.Settings, want)
	}
	for i := range want {
		if got.Settings[i] != want[i] {
			t.Errorf("setting %d = %+v, want %+v", i, got.Settings[i], want[i])
		}
	}
	if raw := e.str("SELECT settings FROM projects WHERE id = ?", e.project); !strings.Contains(raw, `"other":"kept"`) {
		t.Errorf("stored settings %s lost an unrelated key", raw)
	}
	read, err := e.eng.ProjectSettings(e.ctx, envRepo)
	if err != nil || read == nil || len(read.Settings) != len(SettingKeys) || read.Settings[0].Value != "3" {
		t.Fatalf("read = %+v, %v", read, err)
	}
	if none, err := e.eng.ProjectSettings(e.ctx, "https://example.invalid/none.git"); err != nil || none != nil {
		t.Fatalf("read of an unknown repo = %+v, %v; want nil", none, err)
	}
}

func TestSetProjectSettingsRefusals(t *testing.T) {
	e := newEnv(t)
	for _, values := range []map[string]string{
		{"nope": "1"},
		{SettingApprovalMaxTasks: "many"},
		{SettingApprovalMaxTasks: "1.5"},
		{SettingApprovalClasses: "a++b"},
		{SettingApprovalClasses: "has space"},
		{SettingApprovalProtectedPaths: "bad[glob"},
		{SettingApprovalMaxTasks: "2", "nope": "1"},
	} {
		_, err := e.eng.SetProjectSettings(e.ctx, envRepo, values)
		if !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("set %v: err = %v, want ErrInvalidSetting", values, err)
		} else if !strings.Contains(err.Error(), "valid keys: approval_max_tasks, approval_classes, approval_protected_paths") {
			t.Errorf("set %v: err = %v, want it to name the valid keys", values, err)
		}
	}
	if raw := e.str("SELECT settings FROM projects WHERE id = ?", e.project); raw != "{}" {
		t.Errorf("settings = %s after refusals, want {}", raw)
	}
}
