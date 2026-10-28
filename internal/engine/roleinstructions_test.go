package engine

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/defaults"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// setRole sets one role's text, failing the test on a refusal.
func (e *env) setRole(role, text string) *RoleInstructions {
	e.t.Helper()
	ri, err := e.eng.SetRoleInstructions(e.ctx, envRepo, role, text)
	if err != nil {
		e.t.Fatalf("set %s instructions: %v", role, err)
	}
	return ri
}

// roleText reads one role's record, failing the test on an error.
func (e *env) roleText(role string) *RoleInstructions {
	e.t.Helper()
	ri, err := e.eng.RoleInstructions(e.ctx, envRepo, role)
	if err != nil {
		e.t.Fatalf("read %s instructions: %v", role, err)
	}
	return ri
}

// TestRoleInstructionsThreeStates: a role moves between not configured
// (no row, the live default; or the upgrade's seed of it), file (a text
// the user set) and empty by choice (an empty text the user set).
func TestRoleInstructionsThreeStates(t *testing.T) {
	e := newEnv(t)

	// Not configured: no row, so the live embedded default applies.
	live, err := defaults.LiveRoleText(RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	ri := e.roleText(RoleWorker)
	if ri.State != RoleTextNotConfigured || ri.Text != live || ri.Source != SourceDefault ||
		ri.Version != defaults.Version || ri.Digest != defaults.Digest(live) || ri.Bytes != len(live) {
		t.Fatalf("unconfigured role = %+v, want the live default with its provenance", ri)
	}

	// File: a text the user set, with its digest and bytes recorded.
	const text = "Name the test you moved."
	ri = e.setRole(RoleWorker, text)
	if ri.State != RoleTextFile || ri.Text != text || ri.Source != SourceLocal || ri.Version != "" ||
		ri.Digest != instructionsDigest(text) || ri.Bytes != len(text) {
		t.Fatalf("after the set = %+v, want the file state with digest and bytes", ri)
	}

	// Empty by choice: an empty text is a state of its own, not unset.
	ri = e.setRole(RoleWorker, "")
	if ri.State != RoleTextEmptyByChoice || ri.Text != "" || ri.Bytes != 0 {
		t.Fatalf("after the empty set = %+v, want empty by choice", ri)
	}

	// The upgrade's seed reads as not configured, with the seeded text.
	seeded, err := defaults.LiveRoleText(RoleForeman)
	if err != nil {
		t.Fatal(err)
	}
	source := store.RoleSourceSeededPrefix + defaults.Version + ", by upgrade"
	e.exec(`INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, ?, ?, ?)`,
		e.project, RoleForeman, seeded,
		`{"source":`+quoteJSON(source)+`,"version":`+quoteJSON(defaults.Version)+
			`,"digest":"`+defaults.Digest(seeded)+`","bytes":`+strconv.Itoa(len(seeded))+`}`)
	ri = e.roleText(RoleForeman)
	if ri.State != RoleTextNotConfigured || ri.Text != seeded || ri.Source != source || ri.Version != defaults.Version {
		t.Fatalf("seeded role = %+v, want not configured with the seeded text", ri)
	}

	// Only the four roles exist.
	if _, err := e.eng.RoleInstructions(e.ctx, envRepo, "operator"); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("unknown role on read: err = %v, want ErrInvalidSetting", err)
	}
	if _, err := e.eng.SetRoleInstructions(e.ctx, envRepo, "operator", "x"); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("unknown role on set: err = %v, want ErrInvalidSetting", err)
	}
	if ri, err := e.eng.RoleInstructions(e.ctx, "https://elsewhere.invalid/x.git", RoleWorker); err != nil || ri != nil {
		t.Errorf("a repo with no project: ri = %+v, err = %v; want nil, nil", ri, err)
	}
}

// TestRoleInstructionsBounds: the text is bounded at 8192 bytes and
// refused above it or when not UTF-8 or holding a NUL, never cut, with
// the role, the size and the limit named; the previous value stays.
func TestRoleInstructionsBounds(t *testing.T) {
	e := newEnv(t)
	atLimit := strings.Repeat("a", MaxRoleTextBytes)
	if ri := e.setRole(RoleReviewer, atLimit); ri.State != RoleTextFile || ri.Bytes != MaxRoleTextBytes {
		t.Fatalf("at the limit = %+v, want the file state at 8192 bytes", ri)
	}
	for name, bad := range map[string]string{
		"over the limit": atLimit + "b",
		"nul":            "a\x00b",
		"not utf-8":      "a\xffb",
	} {
		_, err := e.eng.SetRoleInstructions(e.ctx, envRepo, RoleReviewer, bad)
		if !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s: err = %v, want ErrInvalidSetting", name, err)
		}
		if !strings.Contains(err.Error(), RoleReviewer) {
			t.Errorf("%s: err = %v, want the role named", name, err)
		}
		if name == "over the limit" && (!strings.Contains(err.Error(), "8193") || !strings.Contains(err.Error(), "8192")) {
			t.Errorf("%s: err = %v, want the size and the limit", name, err)
		}
	}
	if ri := e.roleText(RoleReviewer); ri.State != RoleTextFile || ri.Text != atLimit {
		t.Errorf("after the refusals = %+v, want the previous value in effect", ri)
	}
}

// quoteJSON and itoa keep the seeded meta literal in the tests short.
func quoteJSON(s string) string { return `"` + s + `"` }

// importRole imports one role's text, failing the test on a refusal.
func (e *env) importRole(role, text string) (*RoleInstructions, error) {
	e.t.Helper()
	return e.eng.ImportRoleInstructions(e.ctx, envRepo, role, text)
}

// TestRoleInstructionsImport: an imported role text is validated like a
// set and records the imported source and the import's hash as its
// settings version; a changed import over a still-imported value
// applies; a refusal keeps the previous value; a local override wins
// and is returned kept; an empty import unsets the role.
func TestRoleInstructionsImport(t *testing.T) {
	e := newEnv(t)

	// The first import applies over the not-configured role.
	ri, err := e.importRole(RolePlanReview, "v1 rules")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if ri.State != RoleTextFile || ri.Text != "v1 rules" || ri.Source != SourceImported ||
		ri.Version != roleImportVersion(RolePlanReview, "v1 rules") ||
		ri.Digest != instructionsDigest("v1 rules") || ri.Bytes != len("v1 rules") {
		t.Fatalf("after the import = %+v, want the file state with imported provenance and version", ri)
	}

	// A changed import over a still-imported value applies.
	ri, err = e.importRole(RolePlanReview, "v2 rules")
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if ri.Text != "v2 rules" || ri.Source != SourceImported || ri.Version != roleImportVersion(RolePlanReview, "v2 rules") {
		t.Fatalf("after the re-import = %+v, want the new text with its own version", ri)
	}

	// A refused import — over the bound, a NUL, not UTF-8 — names the
	// role and the numbers and keeps the previous value in effect.
	atLimit := strings.Repeat("b", MaxRoleTextBytes)
	if _, err := e.importRole(RolePlanReview, atLimit); err != nil {
		t.Fatalf("import at the limit: %v", err)
	}
	for name, bad := range map[string]string{
		"over the limit": atLimit + "c",
		"nul":            "a\x00b",
		"not utf-8":      "a\xffb",
	} {
		_, err := e.importRole(RolePlanReview, bad)
		if !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s: err = %v, want ErrInvalidSetting", name, err)
		}
		if !strings.Contains(err.Error(), RolePlanReview) {
			t.Errorf("%s: err = %v, want the role named", name, err)
		}
		if name == "over the limit" && (!strings.Contains(err.Error(), "8193") || !strings.Contains(err.Error(), "8192")) {
			t.Errorf("%s: err = %v, want the size and the limit", name, err)
		}
	}
	if ri := e.roleText(RolePlanReview); ri.Text != atLimit || ri.Source != SourceImported {
		t.Errorf("after the refusals = %+v, want the previous imported value", ri)
	}

	// A local override wins over the file: the import is refused with
	// the kept local record beside it.
	local := e.setRole(RolePlanReview, "my local rules")
	ri, err = e.importRole(RolePlanReview, "v3 rules")
	if !errors.Is(err, ErrRoleTextKept) {
		t.Fatalf("import over a local override: err = %v, want ErrRoleTextKept", err)
	}
	if ri == nil || ri.State != RoleTextFile || ri.Text != local.Text || ri.Source != SourceLocal {
		t.Errorf("kept record = %+v, want the local one", ri)
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("err = %v, want it to name the local override", err)
	}
	if got := e.roleText(RolePlanReview); got.Text != "my local rules" {
		t.Errorf("after the conflict = %+v, want the local override kept", got)
	}

	// An empty import unsets the role: back to not configured.
	ri, err = e.importRole(RoleReviewer, "")
	if err != nil {
		t.Fatalf("empty import over an unconfigured role: %v", err)
	}
	if ri.State != RoleTextNotConfigured {
		t.Fatalf("after the empty import = %+v, want not configured", ri)
	}
	if _, err := e.importRole(RoleWorker, "w1 rules"); err != nil {
		t.Fatalf("import the worker text: %v", err)
	}
	if ri, err = e.importRole(RoleWorker, ""); err != nil || ri.State != RoleTextNotConfigured {
		t.Fatalf("empty import over an imported value = %+v, %v; want not configured", ri, err)
	}
	e.setRole(RoleReviewer, "kept?")
	if ri, err = e.importRole(RoleReviewer, ""); !errors.Is(err, ErrRoleTextKept) {
		t.Fatalf("empty import over a local override: err = %v, want ErrRoleTextKept", err)
	} else if ri.Text != "kept?" {
		t.Errorf("kept record = %+v, want the local text", ri)
	}

	// The seeded provenance is no override: an import replaces it.
	seeded, err := defaults.LiveRoleText(RoleForeman)
	if err != nil {
		t.Fatal(err)
	}
	source := store.RoleSourceSeededPrefix + defaults.Version + ", by upgrade"
	e.exec(`INSERT INTO role_instructions (project_id, role, text, meta) VALUES (?, ?, ?, ?)`,
		e.project, RoleForeman, seeded,
		`{"source":`+quoteJSON(source)+`,"version":`+quoteJSON(defaults.Version)+
			`,"digest":"`+defaults.Digest(seeded)+`","bytes":`+strconv.Itoa(len(seeded))+`}`)
	if ri, err = e.importRole(RoleForeman, "file rules"); err != nil || ri.Text != "file rules" || ri.Source != SourceImported {
		t.Fatalf("import over the seed = %+v, %v; want the imported text", ri, err)
	}

	// Only the four roles exist on the import path too.
	if _, err := e.importRole("operator", "x"); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("unknown role on import: err = %v, want ErrInvalidSetting", err)
	}
}
