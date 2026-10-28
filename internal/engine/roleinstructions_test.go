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
