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
	t.Run("zero is off, the default", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalMaxTasks: "0"})
		e.assertRuns(e.createPlan(nTasks(9)), 9)
	})
	t.Run("a budget holds by spend", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalBudget: "1000000"})
		e.assertRuns(e.createPlan(nTasks(1)), 1) // a new plan has spent nothing
	})
	t.Run("negative switches the rules off", func(t *testing.T) {
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
		e.set(map[string]string{SettingApprovalMaxTasks: "1"})
		e.set(map[string]string{SettingApprovalMaxTasks: ""})
		e.assertRuns(e.createPlan(nTasks(5)), 5)
	})
	t.Run("a budget reset", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingApprovalBudget: "$5+2000000"})
		e.set(map[string]string{SettingApprovalBudget: ""})
		e.assertRuns(e.createPlan(nTasks(1)), 1)
	})
}

func TestProjectSettingsView(t *testing.T) {
	e := newEnv(t)
	e.setSettings(`{"approval_max_tasks": 3, "other": "kept"}`)
	got := e.set(map[string]string{SettingApprovalProtectedPaths: "infra/**"})
	want := []SettingValue{
		{Key: SettingApprovalMaxTasks, Value: "3", Source: SourceLocal},
		{Key: SettingApprovalBudget, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingApprovalClasses, Value: "implement+review+answer", Default: true, Source: SourceDefault},
		{Key: SettingApprovalProtectedPaths, Value: "infra/**", Source: SourceLocal},
		{Key: SettingGatePlan, Value: "auto", Default: true, Source: SourceDefault},
		{Key: SettingGateLand, Value: "auto", Default: true, Source: SourceDefault},
		{Key: SettingGatePush, Value: "auto", Default: true, Source: SourceDefault},
		{Key: SettingPlanReview, Value: "true", Default: true, Source: SourceDefault},
		{Key: SettingPlanReviewTier, Value: "standard", Default: true, Source: SourceDefault},
		{Key: SettingPlanReviewEffort, Value: "medium", Default: true, Source: SourceDefault},
		{Key: SettingApprovalTiers, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingApprovalRestarted, Value: "false", Default: true, Source: SourceDefault},
		{Key: SettingLandTarget, Value: "main", Default: true, Source: SourceDefault},
		{Key: SettingAfterLand, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingLandReady, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingLandBatchMax, Value: "5", Default: true, Source: SourceDefault},
		{Key: SettingDefaultCheck, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingWorkerTools, Value: "agent default", Default: true, Source: SourceDefault},
		{Key: SettingSetup, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingSetupCacheKeyFiles, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingSetupTimeout, Value: "15", Default: true, Source: SourceDefault},
		{Key: SettingExtraRepos, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingForemanRotateTokens, Value: "400000", Default: true, Source: SourceDefault},
		{Key: SettingForemanWakesPerDay, Value: "20", Default: true, Source: SourceDefault},
		{Key: SettingPlanFiles, Value: "on", Default: true, Source: SourceDefault},
		{Key: SettingPlanFileBranch, Value: defaultPlanFileBranch, Default: true, Source: SourceDefault},
		{Key: SettingBranchRetentionDays, Value: "7", Default: true, Source: SourceDefault},
		{Key: SettingTracker, Value: "auto", Default: true, Source: SourceDefault},
		{Key: SettingTrackerURL, Value: "none", Default: true, Source: SourceDefault},
		{Key: SettingInstructions, Value: "none", Default: true, Source: SourceDefault},
		{Key: configKeySkillsDir, Value: DefaultSkillsDir, Default: true, Source: SourceDefault},
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
	if err != nil || read == nil || len(read.Settings) != len(SettingKeys)+1 || read.Settings[0].Value != "3" {
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
		{SettingApprovalBudget: "0"},
		{SettingApprovalBudget: "-3"},
		{SettingApprovalBudget: "$0"},
		{SettingApprovalBudget: "many"},
		{SettingApprovalBudget: "1000+2000"},
		{SettingApprovalBudget: "$5+$6"},
		{SettingApprovalBudget: "1+$2+$3"},
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
		{SettingBranchRetentionDays: "-1"},
		{SettingBranchRetentionDays: "soon"},
		{SettingLandBatchMax: "0"},
		{SettingLandBatchMax: "21"},
		{SettingLandBatchMax: "-1"},
		{SettingLandBatchMax: "2.5"},
		{SettingLandBatchMax: "many"},
		{SettingTracker: "gitlab"},
		{SettingTracker: "auto"},
		{SettingTrackerURL: "ssh://git.example.invalid"},
		{SettingTrackerURL: "git.example.invalid"},
		{SettingTrackerURL: "https://user:pw@git.example.invalid"},
	} {
		_, err := e.eng.SetProjectSettings(e.ctx, envRepo, values)
		if !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("set %v: err = %v, want ErrInvalidSetting", values, err)
		} else if !strings.Contains(err.Error(), "valid keys: approval_max_tasks, approval_budget, approval_classes") {
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
		SettingLandReady:         ".brigada/land-ready.sh",
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
		`"land_ready":".brigada/land-ready.sh"`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("stored settings %s lacks %s", raw, want)
		}
	}
	// Unset returns land_ready to its default.
	got = e.set(map[string]string{SettingLandReady: ""})
	if s := settingOf(t, got, SettingLandReady); s.Value != "none" || !s.Default {
		t.Errorf("land_ready after unset = %+v, want none at the default", s)
	}
}

// branch_retention_days (§13.2) round-trips like the other numeric
// settings: 0 is a value of its own (delete at once), not the default,
// and a reset returns the 7 the design note records.
func TestBranchRetentionDaysRoundTrip(t *testing.T) {
	e := newEnv(t)
	got := e.set(map[string]string{SettingBranchRetentionDays: "0"})
	if s := settingOf(t, got, SettingBranchRetentionDays); s.Value != "0" || s.Default || s.Source != SourceLocal {
		t.Errorf("branch_retention_days 0 = %+v, want a local 0", s)
	}
	got = e.set(map[string]string{SettingBranchRetentionDays: "30"})
	if s := settingOf(t, got, SettingBranchRetentionDays); s.Value != "30" {
		t.Errorf("branch_retention_days 30 = %+v, want 30", s)
	}
	if raw := e.str("SELECT settings FROM projects WHERE id = ?", e.project); !strings.Contains(raw, `"branch_retention_days":30`) {
		t.Errorf("stored settings %s lack branch_retention_days 30", raw)
	}
	got = e.set(map[string]string{SettingBranchRetentionDays: ""})
	if s := settingOf(t, got, SettingBranchRetentionDays); s.Value != "7" || !s.Default {
		t.Errorf("branch_retention_days after reset = %+v, want the 7 default", s)
	}
}

// land_batch_max takes 1 to 20, defaults to 5, and a refused value names
// the key and leaves the prior setting in place.
func TestLandBatchMaxValidation(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"1", "20", "7"} {
		got := e.set(map[string]string{SettingLandBatchMax: v})
		if s := settingOf(t, got, SettingLandBatchMax); s.Value != v || s.Default || s.Source != SourceLocal {
			t.Errorf("land_batch_max %s = %+v, want a local %s", v, s, v)
		}
	}
	for _, v := range []string{"0", "21", "-1", "2.5", "many"} {
		_, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingLandBatchMax: v})
		if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), SettingLandBatchMax) {
			t.Errorf("land_batch_max %s: err = %v, want ErrInvalidSetting naming the key", v, err)
		}
	}
	if raw := e.str("SELECT settings FROM projects WHERE id = ?", e.project); !strings.Contains(raw, `"land_batch_max":7`) {
		t.Errorf("stored settings %s lost the prior land_batch_max 7", raw)
	}
	got := e.set(map[string]string{SettingLandBatchMax: ""})
	if s := settingOf(t, got, SettingLandBatchMax); s.Value != "5" || !s.Default {
		t.Errorf("land_batch_max after reset = %+v, want the 5 default", s)
	}
}

// land_ready is a repo-relative path: an absolute path or one that
// escapes the repository is refused.
func TestLandReadyValidation(t *testing.T) {
	for _, v := range []string{"/bin/ready", "../ready.sh", "a/../../ready.sh", "..", "~/land-ready.sh", `.brigada\ready.sh`} {
		if _, err := parseSettingValue(SettingLandReady, v); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("land_ready %q: err = %v, want ErrInvalidSetting", v, err)
		}
	}
	raw, err := parseSettingValue(SettingLandReady, " .brigada/land-ready.sh ")
	if err != nil || string(raw) != `".brigada/land-ready.sh"` {
		t.Errorf("land_ready valid = %s, %v", raw, err)
	}
}
