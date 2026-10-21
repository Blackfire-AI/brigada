package engine

import (
	"errors"
	"strconv"
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
	t.Run("the plan gate holds every plan", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingGatePlan: GateAsk})
		e.assertAwaits(e.createPlan(nTasks(1)), "gate_plan is ask")
	})
	t.Run("the plan gate holds with the limit off", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalMaxTasks: "-1", SettingGatePlan: GateAsk})
		e.assertAwaits(e.createPlan(nTasks(1)), "gate_plan is ask")
	})
	t.Run("the plan gate reset", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingGatePlan: GateAsk})
		e.set(map[string]string{SettingGatePlan: ""})
		e.assertRuns(e.createPlan(nTasks(1)), 1)
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
		{Key: SettingGatePlan, Value: "auto", Default: true, Source: SourceDefault},
		{Key: SettingGateLand, Value: "auto", Default: true, Source: SourceDefault},
		{Key: SettingGatePush, Value: "ask", Default: true, Source: SourceDefault},
		{Key: SettingApprovalTiers, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingApprovalRestarted, Value: "false", Default: true, Source: SourceDefault},
		{Key: SettingLandTarget, Value: "main", Default: true, Source: SourceDefault},
		{Key: SettingAfterLand, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingDefaultCheck, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingWorkerTools, Value: "agent default", Default: true, Source: SourceDefault},
		{Key: SettingSetup, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingSetupCacheKeyFiles, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingSetupTimeout, Value: "15", Default: true, Source: SourceDefault},
		{Key: SettingExtraRepos, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingForemanRotateTokens, Value: "400000", Default: true, Source: SourceDefault},
		{Key: SettingForemanWakesPerDay, Value: "20", Default: true, Source: SourceDefault},
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
		{SettingGatePlan: "maybe"},
		{SettingGateLand: "never"},
		{SettingGatePush: "always"},
		{SettingApprovalTiers: "huge"},
		{SettingApprovalTiers: "light+premium"},
		{SettingApprovalRestarted: "maybe"},
		{SettingLandTarget: "bad..name"},
		{SettingLandTarget: "feature/x.lock"},
		{SettingForemanWakesPerDay: "0"},
		{SettingForemanWakesPerDay: strconv.Itoa(MaxForemanWakesPerDay + 1)},
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

// land_target's default (§6.3) is the project's default branch, whatever
// that branch is named.
func TestLandTargetDefaultBranch(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE projects SET default_branch = 'trunk' WHERE id = ?", e.project)
	got, err := e.eng.ProjectSettings(e.ctx, envRepo)
	if err != nil || got == nil {
		t.Fatalf("read: %+v %v", got, err)
	}
	if s := settingOf(t, *got, SettingLandTarget); s.Value != "trunk" || !s.Default {
		t.Errorf("land_target = %+v, want trunk at the default", s)
	}
}

// The gate and land keys (§6.3) round-trip: what SetProjectSettings
// accepts is what the view shows and the settings row stores, in the
// shape approvalSettings reads back.
func TestGateLandSettingsRoundTrip(t *testing.T) {
	e := newEnv(t)
	values := map[string]string{
		SettingGatePlan:          GateAsk,
		SettingGateLand:          GateAsk,
		SettingGatePush:          GateNever,
		SettingApprovalTiers:     "light+heavy",
		SettingApprovalRestarted: "true",
		SettingLandTarget:        "brigada/landed",
		SettingAfterLand:         "make announce",
	}
	got := e.set(values)
	for k, v := range values {
		if s := settingOf(t, got, k); s.Value != v || s.Default || s.Source != SourceLocal {
			t.Errorf("%s = %+v, want %q local", k, s, v)
		}
	}
	raw := e.str("SELECT settings FROM projects WHERE id = ?", e.project)
	for _, want := range []string{
		`"gate_plan":"ask"`, `"gate_land":"ask"`, `"gate_push":"never"`,
		`"approval_tiers":["light","heavy"]`, `"approval_restarted":true`,
		`"land_target":"brigada/landed"`, `"after_land":"make announce"`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("stored settings %s lacks %s", raw, want)
		}
	}
}
