package engine

import "testing"

func (e *env) taskCheck(id int64) string {
	e.t.Helper()
	return e.str("SELECT COALESCE(check_cmd, '') FROM tasks WHERE id = ?", id)
}

func TestDefaultCheckSetting(t *testing.T) {
	e := newEnv(t)
	find := func(ps ProjectSettings) SettingValue {
		for _, s := range ps.Settings {
			if s.Key == SettingDefaultCheck {
				return s
			}
		}
		t.Fatalf("no %s in %+v", SettingDefaultCheck, ps.Settings)
		return SettingValue{}
	}
	if sv := find(e.set(map[string]string{SettingDefaultCheck: "go test ./..."})); sv.Value != "go test ./..." || sv.Default {
		t.Errorf("default_check = %+v, want the set command, not default", sv)
	}
	if sv := find(e.set(map[string]string{SettingDefaultCheck: ""})); sv.Value != "none" || !sv.Default {
		t.Errorf("after reset default_check = %+v, want none/default", sv)
	}
}

func TestDefaultCheckInheritance(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingApprovalMaxTasks: "-1"})
	none := e.createPlan([]PlanTask{{Spec: "a"}})
	if got := e.taskCheck(none.Tasks[0].TaskID); got != "" {
		t.Errorf("no default: check = %q, want none", got)
	}
	e.set(map[string]string{SettingDefaultCheck: "make ci"})
	res := e.createPlan([]PlanTask{{Spec: "b"}, {Spec: "c", Check: "own"}})
	if got := e.taskCheck(res.Tasks[0].TaskID); got != "make ci" {
		t.Errorf("omitted check = %q, want the default", got)
	}
	if got := e.taskCheck(res.Tasks[1].TaskID); got != "own" {
		t.Errorf("own check = %q, want kept", got)
	}
	e.set(map[string]string{SettingDefaultCheck: "make other"})
	if got := e.taskCheck(res.Tasks[0].TaskID); got != "make ci" {
		t.Errorf("after default change check = %q, want unchanged", got)
	}
	am := e.amend(res.PlanID, PlanAmendment{Add: []PlanTask{{Spec: "d"}, {Spec: "e", Check: "own2"}}})
	if got := e.taskCheck(am.Added[0].TaskID); got != "make other" {
		t.Errorf("amend-added check = %q, want the default", got)
	}
	if got := e.taskCheck(am.Added[1].TaskID); got != "own2" {
		t.Errorf("amend-added own check = %q, want kept", got)
	}
}
