package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/defaults"
)

func roleTextViews(t *testing.T, e *env) map[string]RoleTextView {
	t.Helper()
	list, err := e.eng.ProjectRoleTexts(e.ctx, envRepo)
	if err != nil || len(list) != len(RoleTexts) {
		t.Fatalf("role texts = %+v, %v", list, err)
	}
	out := map[string]RoleTextView{}
	for _, v := range list {
		out[v.Role] = v
	}
	return out
}

// TestRoleTextBounds: a role text is bounded at 8192 bytes of valid
// UTF-8 with no NUL, never cut; a refusal names the role, the size and
// the limit, and the previous value stays in effect.
func TestRoleTextBounds(t *testing.T) {
	e := newEnv(t)
	atLimit := strings.Repeat("a", MaxInstructionsBytes)
	if _, err := e.eng.SetRoleText(e.ctx, envRepo, RoleReviewer, atLimit, "", ""); err != nil {
		t.Fatalf("set at the limit: %v", err)
	}
	for name, tc := range map[string]struct {
		role, text string
	}{
		"over the limit": {RoleReviewer, atLimit + "b"},
		"nul":            {RoleForeman, "a\x00b"},
		"not utf-8":      {RoleWorker, "a\xffb"},
		"unknown role":   {"chef", "abc"},
	} {
		_, err := e.eng.SetRoleText(e.ctx, envRepo, tc.role, tc.text, "", "")
		if !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s: err = %v, want ErrInvalidSetting", name, err)
		}
		if name == "over the limit" && (!strings.Contains(err.Error(), "reviewer") || !strings.Contains(err.Error(), "8193") || !strings.Contains(err.Error(), "8192")) {
			t.Errorf("%s: err = %v, want the role, the size and the limit", name, err)
		}
		if name == "unknown role" && !strings.Contains(err.Error(), "chef") {
			t.Errorf("%s: err = %v, want the role named", name, err)
		}
	}
	v := roleTextViews(t, e)[RoleReviewer]
	if v.Text != atLimit || v.State != RoleTextFile || v.Digest != defaults.Digest(atLimit) || v.Bytes != MaxInstructionsBytes {
		t.Errorf("after refusals reviewer = %+v, want the previous value", v)
	}
	if err := ValidateRoleText(RolePlanReview, atLimit); err != nil {
		t.Errorf("validate at the limit: %v", err)
	}
}

// TestRoleTextStates: the three states — not configured, empty by
// choice, file — and the provenance each records.
func TestRoleTextStates(t *testing.T) {
	e := newEnv(t)
	for _, v := range roleTextViews(t, e) {
		if v.State != RoleTextNotConfigured {
			t.Errorf("fresh project %s = %+v, want not configured", v.Role, v)
		}
	}
	if _, err := e.eng.SetRoleText(e.ctx, envRepo, RoleForeman, "", "", ""); err != nil {
		t.Fatalf("empty by choice: %v", err)
	}
	fileSource := "file .brigada/plan.md, imported now"
	if _, err := e.eng.SetRoleText(e.ctx, envRepo, RolePlanReview, "plan rules", fileSource, "importhash"); err != nil {
		t.Fatalf("file: %v", err)
	}
	views := roleTextViews(t, e)
	if v := views[RoleForeman]; v.State != RoleTextEmptyByChoice || v.Text != "" || v.Source != SourceLocal {
		t.Errorf("foreman = %+v, want empty by choice", v)
	}
	if v := views[RolePlanReview]; v.State != RoleTextFile || v.Text != "plan rules" || v.Source != fileSource || v.Version != "importhash" || v.Digest != defaults.Digest("plan rules") {
		t.Errorf("plan_review = %+v, want the file state with provenance", v)
	}
	if err := e.eng.ResetRoleText(e.ctx, envRepo, RoleForeman); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if v := roleTextViews(t, e)[RoleForeman]; v.State != RoleTextNotConfigured || v.Text != "" {
		t.Errorf("after reset foreman = %+v, want not configured", v)
	}
	if v := roleTextViews(t, e)[RolePlanReview]; v.State != RoleTextFile {
		t.Errorf("reset touched plan_review = %+v", v)
	}
	if err := e.eng.ResetRoleText(e.ctx, envRepo, "chef"); err == nil {
		t.Errorf("reset of an unknown role was accepted")
	}
}

// TestWorkerRoleTextSynchronized: the worker's role text is the
// instructions setting — a set or an import through either surface
// moves the setting and the stored row together, so the task
// resolution and the role views agree.
func TestWorkerRoleTextSynchronized(t *testing.T) {
	e := newEnv(t)
	// Through the setting: the row materializes it with the local source.
	e.set(map[string]string{SettingInstructions: "use tabs"})
	if v := roleTextViews(t, e)[RoleWorker]; v.State != RoleTextFile || v.Text != "use tabs" || v.Source != SourceLocal || v.Digest != defaults.Digest("use tabs") {
		t.Errorf("after the setting worker = %+v, want the setting materialized", v)
	}
	// Through SetRoleText: the setting moves with the row.
	if _, err := e.eng.SetRoleText(e.ctx, envRepo, RoleWorker, "use spaces", "", ""); err != nil {
		t.Fatalf("set the worker text: %v", err)
	}
	sv := settingOf(t, *mustSettings(t, e), SettingInstructions)
	if sv.Source != SourceLocal || !strings.HasPrefix(sv.Value, "10 bytes") {
		t.Errorf("after SetRoleText the setting = %+v, want the local override", sv)
	}
	if v := roleTextViews(t, e)[RoleWorker]; v.Text != "use spaces" || v.Source != SourceLocal {
		t.Errorf("after SetRoleText worker = %+v, want one value", v)
	}
	// Through the import: the row carries the import's source and hash.
	res, err := e.importWithText("instructions_file: .brigada/instructions.md\n", ptr("v1 rules"), true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if v := roleTextViews(t, e)[RoleWorker]; v.Text != "v1 rules" || v.Source != SourceImported || v.Version != res.Hash {
		t.Errorf("after the import worker = %+v, want the import's provenance (hash %s)", v, res.Hash)
	}
	// Empty by choice: the setting is unset, the row keeps the state.
	if _, err := e.eng.SetRoleText(e.ctx, envRepo, RoleWorker, "", "", ""); err != nil {
		t.Fatalf("empty by choice: %v", err)
	}
	if sv := settingOf(t, *mustSettings(t, e), SettingInstructions); !sv.Default || sv.Value != "none" {
		t.Errorf("after the empty set the setting = %+v, want none/default", sv)
	}
	if v := roleTextViews(t, e)[RoleWorker]; v.State != RoleTextEmptyByChoice || v.Text != "" {
		t.Errorf("after the empty set worker = %+v, want empty by choice", v)
	}
	// A setting set over the marker replaces it.
	e.set(map[string]string{SettingInstructions: "back"})
	if v := roleTextViews(t, e)[RoleWorker]; v.State != RoleTextFile || v.Text != "back" {
		t.Errorf("after the re-set worker = %+v, want the file state", v)
	}
	// Reset: both gone.
	if err := e.eng.ResetRoleText(e.ctx, envRepo, RoleWorker); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if sv := settingOf(t, *mustSettings(t, e), SettingInstructions); !sv.Default || sv.Value != "none" {
		t.Errorf("after the reset the setting = %+v, want none/default", sv)
	}
	if v := roleTextViews(t, e)[RoleWorker]; v.State != RoleTextNotConfigured || v.Text != "" {
		t.Errorf("after the reset worker = %+v, want not configured", v)
	}
}

// TestWorkerRoleTextImportProvenance: a worker import through the
// role-text API keeps its source and settings version in the row while
// the setting stays synchronized as the text's one source of truth.
func TestWorkerRoleTextImportProvenance(t *testing.T) {
	e := newEnv(t)
	const fileSource = "file .brigada/instructions.md, imported now"
	v, err := e.eng.SetRoleText(e.ctx, envRepo, RoleWorker, "shared rules", fileSource, "imp0rthash")
	if err != nil {
		t.Fatalf("import the worker text: %v", err)
	}
	if v.State != RoleTextFile || v.Text != "shared rules" || v.Source != fileSource || v.Version != "imp0rthash" || v.Digest != defaults.Digest("shared rules") {
		t.Errorf("worker import = %+v, want the provenance kept", v)
	}
	// The row is where the provenance lives.
	if got := e.str("SELECT text FROM role_instructions WHERE project_id = ? AND role = 'worker'", e.project); got != "shared rules" {
		t.Errorf("worker row text = %q, want the imported text", got)
	}
	if got := e.str("SELECT source FROM role_instructions WHERE project_id = ? AND role = 'worker'", e.project); got != fileSource {
		t.Errorf("worker row source = %q, want the import's", got)
	}
	if got := e.str("SELECT settings_version FROM role_instructions WHERE project_id = ? AND role = 'worker'", e.project); got != "imp0rthash" {
		t.Errorf("worker row version = %q, want the import's", got)
	}
	// The setting holds the same text, as the effective resolution reads it.
	sv := settingOf(t, *mustSettings(t, e), SettingInstructions)
	if sv.Source != SourceLocal || !strings.HasPrefix(sv.Value, "12 bytes") {
		t.Errorf("the setting after the import = %+v, want the synchronized local override", sv)
	}
}

// TestWorkerEmptyChoiceSurvivesImports: a worker text set empty by
// choice keeps its state and precedence — an unrelated import leaves
// it, an instruction-bearing import asks before replacing it, and an
// accepted one replaces it.
func TestWorkerEmptyChoiceSurvivesImports(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.SetRoleText(e.ctx, envRepo, RoleWorker, "", "", ""); err != nil {
		t.Fatalf("empty by choice: %v", err)
	}
	// An unrelated import: nothing about the worker text changes.
	if _, err := e.eng.ImportProjectConfig(e.ctx, envRepo, cfgFile("approval_max_tasks: -1\n"), false); err != nil {
		t.Fatalf("unrelated import: %v", err)
	}
	if v := roleTextViews(t, e)[RoleWorker]; v.State != RoleTextEmptyByChoice || v.Text != "" {
		t.Errorf("after the unrelated import worker = %+v, want empty by choice kept", v)
	}
	// An instruction-bearing import: the empty choice holds the local
	// slot, so the import conflicts — both values named, nothing slid
	// under the choice — instead of replacing it.
	res, err := e.importWithText("instructions_file: .brigada/instructions.md\n", ptr("v1 rules"), false)
	if err != nil || len(res.Conflicts) != 1 || res.Conflicts[0].Key != SettingInstructions || res.Conflicts[0].New != "v1 rules" {
		t.Fatalf("import over the empty choice = %+v, %v; want one instructions conflict", res, err)
	}
	if v := roleTextViews(t, e)[RoleWorker]; v.State != RoleTextEmptyByChoice || v.Text != "" {
		t.Errorf("after the conflicting import worker = %+v, want empty by choice kept", v)
	}
	// A deliberate change replaces the choice; the import's text then
	// reads through the setting like any set.
	if _, err := e.eng.SetRoleText(e.ctx, envRepo, RoleWorker, "v1 rules", "", ""); err != nil {
		t.Fatalf("deliberate change: %v", err)
	}
	if v := roleTextViews(t, e)[RoleWorker]; v.State != RoleTextFile || v.Text != "v1 rules" || v.Source != SourceLocal {
		t.Errorf("after the deliberate change worker = %+v, want the set text", v)
	}
	if sv := settingOf(t, *mustSettings(t, e), SettingInstructions); sv.Source != SourceLocal || !strings.HasPrefix(sv.Value, "8 bytes") {
		t.Errorf("after the deliberate change the setting = %+v, want the local override", sv)
	}
}

// TestRoleTextSeededDefaultIsNotConfigured: a row an upgrade generated
// reads as not configured, the embedded default it holds.
func TestRoleTextSeededDefaultIsNotConfigured(t *testing.T) {
	e := newEnv(t)
	generated := defaults.GeneratedProvenance(defaults.Version)
	text, err := defaults.RoleText(RoleForeman)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO role_instructions (project_id, role, text, source, digest, updated_at)
		VALUES (?, ?, ?, ?, ?, '2026-10-29T00:00:00Z')`, e.project, RoleForeman, text, generated, defaults.Digest(text))
	v := roleTextViews(t, e)[RoleForeman]
	if v.State != RoleTextNotConfigured || v.Text != text || v.Source != generated {
		t.Errorf("seeded foreman = %+v, want not configured holding the default", v)
	}
}
