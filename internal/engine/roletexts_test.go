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
