package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/defaults"
)

func mustRole(t *testing.T, e *env, role string) *RoleInstruction {
	t.Helper()
	ri, err := e.eng.RoleInstruction(e.ctx, envRepo, role)
	if err != nil || ri == nil {
		t.Fatalf("read %s instructions: %v, %+v", role, err, ri)
	}
	return ri
}

// mustSetRole sets a role text, failing the test on a refusal.
func mustSetRole(t *testing.T, e *env, role, text string) *RoleInstruction {
	t.Helper()
	ri, err := e.eng.SetRoleInstruction(e.ctx, envRepo, role, text)
	if err != nil {
		t.Fatalf("set %s instructions: %v", role, err)
	}
	return ri
}

// TestRoleInstructionStates: a role starts not configured, with no text
// of its own — at run time the engine never falls back to a text in the
// binary (§5.9); a set makes it a file text with local provenance; an
// empty set is an empty-by-choice mark that suppresses the default; the
// worker role mirrors the instructions setting.
func TestRoleInstructionStates(t *testing.T) {
	e := newEnv(t)
	ri := mustRole(t, e, RoleForeman)
	if ri.State != RoleStateNotConfigured || ri.Source != SourceDefault || ri.Text != "" || ri.Bytes != 0 || ri.Digest != instructionsDigest("") || ri.Version != "" {
		t.Errorf("foreman before any set = %+v, want not configured with no text", ri)
	}

	ri = mustSetRole(t, e, RoleForeman, "Be terse.")
	if ri.State != RoleStateFile || ri.Source != SourceLocal || ri.Text != "Be terse." || ri.Bytes != 9 || ri.Digest != defaults.Digest("Be terse.") || ri.Version != "" || ri.Provenance != SourceLocal {
		t.Errorf("foreman after a set = %+v, want a local file text", ri)
	}
	if got := mustRole(t, e, RoleForeman); got.Text != "Be terse." {
		t.Errorf("foreman reread = %q, want the set text", got.Text)
	}

	ri = mustSetRole(t, e, RoleReviewer, "")
	if ri.State != RoleStateEmptyByChoice || ri.Source != SourceLocal || ri.Text != "" || ri.Bytes != 0 {
		t.Errorf("reviewer after an empty set = %+v, want empty by choice", ri)
	}
	if got := mustRole(t, e, RoleWorker).Text; got != "" {
		t.Errorf("worker after the reviewer's set = %q, want no text of an unconfigured role", got)
	}

	// The worker role is the instructions setting: both change together.
	mustSetRole(t, e, RoleWorker, "Use tabs.")
	if sv := settingOf(t, *mustSettings(t, e), SettingInstructions); sv.Default || !strings.HasPrefix(sv.Value, "9 bytes") {
		t.Errorf("instructions setting after a worker set = %+v, want the same text", sv)
	}
	mustSetRole(t, e, RoleWorker, "")
	if ri := mustRole(t, e, RoleWorker); ri.State != RoleStateEmptyByChoice || ri.Text != "" {
		t.Errorf("worker after an empty set = %+v, want empty by choice", ri)
	}
	if sv := settingOf(t, *mustSettings(t, e), SettingInstructions); !sv.Default || sv.Value != "none" {
		t.Errorf("instructions setting after an empty worker set = %+v, want none/default", sv)
	}

	if _, err := e.eng.RoleInstruction(e.ctx, envRepo, "boss"); err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Errorf("unknown role err = %v, want unknown role", err)
	}
	if _, err := e.eng.RoleInstruction(e.ctx, envRepo, RoleForeman); err != nil {
		t.Fatalf("read role: %v", err)
	}
	missing, err := e.eng.RoleInstruction(e.ctx, "https://example.invalid/none.git", RoleForeman)
	if err != nil || missing != nil {
		t.Errorf("role of a repo without a project = %+v, %v; want nil, nil", missing, err)
	}
}

// TestRoleInstructionBounds: every role's text is bounded at 8192 bytes,
// must be valid UTF-8 with no NUL, and is never truncated; a refusal
// names the role, the size and the limit, and the previous value stays.
func TestRoleInstructionBounds(t *testing.T) {
	e := newEnv(t)
	for _, role := range []string{RoleWorker, RoleReviewer, RoleForeman, RolePlanReview} {
		prev := "keep " + role
		mustSetRole(t, e, role, prev)
		over := strings.Repeat("a", MaxRoleInstructionBytes) + "b"
		_, err := e.eng.SetRoleInstruction(e.ctx, envRepo, role, over)
		if !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s over the limit: err = %v, want ErrInvalidSetting", role, err)
			continue
		}
		for _, want := range []string{role, "8193", "8192", "nothing is truncated"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s over the limit: err = %v, want it to name %q", role, err, want)
			}
		}
		for name, bad := range map[string]string{
			"nul":       "a\x00b",
			"not utf-8": "a\xffb",
		} {
			_, err := e.eng.SetRoleInstruction(e.ctx, envRepo, role, bad)
			if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), role) {
				t.Errorf("%s %s: err = %v, want a refusal naming the role", role, name, err)
			}
		}
		if _, err := e.eng.SetRoleInstruction(e.ctx, envRepo, "boss", "x"); err == nil || !strings.Contains(err.Error(), "unknown role") {
			t.Errorf("unknown role: err = %v, want unknown role", err)
		}
		if ri := mustRole(t, e, role); ri.Text != prev {
			t.Errorf("%s after the refusals = %q, want the previous value", role, ri.Text)
		}
		atLimit := strings.Repeat("a", MaxRoleInstructionBytes)
		if ri := mustSetRole(t, e, role, atLimit); ri.Bytes != MaxRoleInstructionBytes || ri.Text != atLimit {
			t.Errorf("%s at the limit = %d bytes, want the whole text", role, ri.Bytes)
		}
	}
}

// TestSettingsSyncWorkerRoleRow: a `project set instructions=` mirrors
// into the worker role row, and a reset returns the role to not
// configured, where the embedded default applies.
func TestSettingsSyncWorkerRoleRow(t *testing.T) {
	e := newEnv(t)
	e.set(map[string]string{SettingInstructions: "from the setting"})
	if ri := mustRole(t, e, RoleWorker); ri.State != RoleStateFile || ri.Source != SourceLocal || ri.Text != "from the setting" {
		t.Errorf("worker after `project set instructions` = %+v, want a local file text", ri)
	}
	e.set(map[string]string{SettingInstructions: ""})
	if ri := mustRole(t, e, RoleWorker); ri.State != RoleStateNotConfigured || ri.Source != SourceDefault {
		t.Errorf("worker after the reset = %+v, want not configured", ri)
	}
}

// TestUnimportRoleFiles: an import whose request no longer configures a
// role moves it to not configured — its imported row is removed (§5.9
// 'Three states, no hidden fallback'), so dropping a key from
// project.yaml's instructions map is a choice that sticks. A local
// override under the dropped key keeps its text and comes back as Kept,
// the conflict ErrRoleTextKept reports for a file the map still names.
// The upgrade's seed stands (no import owns the role), the worker never
// moves here — its text imports with project.yaml itself — and a repo
// without a project has nothing to remove.
func TestUnimportRoleFiles(t *testing.T) {
	e := newEnv(t)
	if _, err := e.eng.ImportRoleFile(e.ctx, envRepo, RoleForeman, "file foreman rules"); err != nil {
		t.Fatalf("import foreman: %v", err)
	}
	if _, err := e.eng.ImportRoleFile(e.ctx, envRepo, RoleReviewer, "file reviewer rules"); err != nil {
		t.Fatalf("import reviewer: %v", err)
	}
	mustSetRole(t, e, RolePlanReview, "my local plan review")
	seed := func(role, text string) {
		t.Helper()
		if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO role_instructions (project_id, role, text, source, settings_version, digest, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, e.project, role, text, SourceGenerated, defaults.Version, instructionsDigest(text), seedTS)
			return err
		}); err != nil {
			t.Fatalf("seed %s: %v", role, err)
		}
	}
	seed(RoleWorker, "seeded worker")

	removals, err := e.eng.UnimportRoleFiles(e.ctx, envRepo, map[string]bool{RoleReviewer: true})
	if err != nil {
		t.Fatalf("unimport: %v", err)
	}
	if len(removals) != 2 {
		t.Fatalf("removals = %+v, want the imported foreman removed and the local plan_review kept", removals)
	}
	byRole := map[string]RoleFileRemoval{}
	for _, rm := range removals {
		byRole[rm.Role] = rm
	}
	rm := byRole[RoleForeman]
	if rm.Removed == nil || rm.Kept != nil || rm.Removed.Text != "file foreman rules" {
		t.Errorf("foreman removal = %+v, want the imported record removed", rm)
	}
	if c := rm.Change(); c.Key != "instructions.foreman" || c.New != RoleStateNotConfigured || !strings.Contains(c.Old, "18 bytes") {
		t.Errorf("foreman removal change = %+v, want a summary moving to not configured", c)
	}
	rm = byRole[RolePlanReview]
	if rm.Kept == nil || rm.Removed != nil || rm.Kept.Text != "my local plan review" || rm.Kept.Source != SourceLocal {
		t.Errorf("plan_review removal = %+v, want the local override kept", rm)
	}
	if c := rm.Change(); !strings.Contains(c.Old, "my local plan review") || c.New != RoleStateNotConfigured {
		t.Errorf("plan_review removal change = %+v, want the kept text against not configured", c)
	}

	if ri := mustRole(t, e, RoleForeman); ri.State != RoleStateNotConfigured || ri.Source != SourceDefault || ri.Text != "" {
		t.Errorf("foreman after the removal = %+v, want not configured", ri)
	}
	if ri := mustRole(t, e, RolePlanReview); ri.State != RoleStateFile || ri.Text != "my local plan review" {
		t.Errorf("plan_review after the removal = %+v, want the local text", ri)
	}
	if ri := mustRole(t, e, RoleReviewer); ri.Source != SourceImported || ri.Text != "file reviewer rules" {
		t.Errorf("reviewer, still configured by the request = %+v, want the import kept", ri)
	}
	if ri := mustRole(t, e, RoleWorker); ri.Source != SourceGenerated || ri.Text != "seeded worker" {
		t.Errorf("worker after the removal = %+v, want the seed untouched: the worker rides the config import", ri)
	}

	// An empty configured set removes every non-worker imported row; the
	// seed and the local row still stand, the local one as a conflict.
	removals, err = e.eng.UnimportRoleFiles(e.ctx, envRepo, nil)
	if err != nil || len(removals) != 2 {
		t.Fatalf("unimport all = %+v, %v; want the reviewer removed and the local plan_review kept", removals, err)
	}
	for _, rm := range removals {
		switch rm.Role {
		case RoleReviewer:
			if rm.Removed == nil {
				t.Errorf("reviewer removal = %+v, want the imported record removed", rm)
			}
		case RolePlanReview:
			if rm.Kept == nil || rm.Kept.Text != "my local plan review" {
				t.Errorf("plan_review removal = %+v, want the local override kept", rm)
			}
		default:
			t.Errorf("unexpected removal %+v", rm)
		}
	}
	if ri := mustRole(t, e, RoleReviewer); ri.State != RoleStateNotConfigured {
		t.Errorf("reviewer after the second removal = %+v, want not configured", ri)
	}

	if got, err := e.eng.UnimportRoleFiles(e.ctx, "https://example.invalid/none.git", nil); err != nil || got != nil {
		t.Errorf("a repo without a project = %+v, %v; want nil, nil", got, err)
	}
	if _, err := e.eng.UnimportRoleFiles(e.ctx, "", nil); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("no repo: err = %v, want ErrInvalidSetting", err)
	}
}

// TestImportSyncsWorkerRoleInstruction: an import writes the worker role
// row with its text and provenance, and a re-import that changes only
// other keys still moves the row's settings version to the new hash
// while the text stays; an empty-by-choice mark is preserved by the
// merge itself — the file's text conflicts, the settings keep no
// instructions — unless the import explicitly accepts its replacement,
// which both views then take with the file's provenance; the same
// acceptance replaces a local text; a file that stops setting
// instructions drops an imported row, not a local one; a whitespace-only
// file over the bound is refused like any oversized text, with the
// worker role, the size and the limit named.
func TestImportSyncsWorkerRoleInstruction(t *testing.T) {
	e := newEnv(t)
	const file = "instructions_file: .brigada/instructions.md\n"

	res, err := e.importWithText(file+"approval_max_tasks: 3\n", ptr("v1 rules"), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	ri := mustRole(t, e, RoleWorker)
	if ri.State != RoleStateFile || ri.Source != SourceImported || ri.Text != "v1 rules" || ri.Version != res.Hash || ri.Digest != defaults.Digest("v1 rules") {
		t.Errorf("worker after the import = %+v, want imported with the import hash", ri)
	}

	// The same text, another key changed: the hash moves, the text
	// stays, and the row's settings version follows the new hash.
	res2, err := e.importWithText(file+"approval_max_tasks: 5\n", ptr("v1 rules"), false)
	if err != nil || len(res2.Applied) != 1 {
		t.Fatalf("re-import = %+v, %v, want the other key applied", res2, err)
	}
	if res2.Hash == res.Hash {
		t.Fatalf("the re-import kept the old hash %s", res.Hash)
	}
	if ri := mustRole(t, e, RoleWorker); ri.Text != "v1 rules" || ri.Source != SourceImported || ri.Version != res2.Hash {
		t.Errorf("worker after the re-import = %+v, want the same text dated with the new hash %s", ri, res2.Hash)
	}

	// An empty-by-choice mark is preserved by the merge itself: the
	// file's text conflicts, the settings keep no instructions, and the
	// role keeps the marker — no reader can use the file's text.
	mustSetRole(t, e, RoleWorker, "")
	res3, err := e.importWithText(file+"approval_max_tasks: 5\n", ptr("v3 rules"), false)
	if err != nil || len(res3.Conflicts) != 1 || len(res3.Applied) != 0 {
		t.Fatalf("import over the empty choice = %+v, %v, want one conflict and nothing applied", res3, err)
	}
	if c := res3.Conflicts[0]; c.Key != SettingInstructions || c.Old != "" || c.New != "v3 rules" {
		t.Errorf("the empty-choice conflict = %+v, want both texts on instructions", c)
	}
	if ri := mustRole(t, e, RoleWorker); ri.State != RoleStateEmptyByChoice || ri.Source != SourceLocal || ri.Text != "" {
		t.Errorf("worker after the conflicted import = %+v, want the marker kept", ri)
	}
	if sv := settingOf(t, *mustSettings(t, e), SettingInstructions); !sv.Default || sv.Value != "none" {
		t.Errorf("instructions setting after the conflicted import = %+v, want none/default", sv)
	}

	// An accepted first import replaces a local text: both views and
	// their provenance agree on the file's text afterwards.
	e2 := newEnv(t)
	mustSetRole(t, e2, RoleWorker, "my local rules")
	res4, err := e2.importWithText(file, ptr("v1 rules"), true)
	if err != nil || res4.NeedsConfirm || len(res4.Diffs) != 1 || len(res4.Applied) != 1 {
		t.Fatalf("accepted import over a local text = %+v, %v, want the diff accepted", res4, err)
	}
	if ri := mustRole(t, e2, RoleWorker); ri.State != RoleStateFile || ri.Source != SourceImported || ri.Text != "v1 rules" || ri.Version != res4.Hash {
		t.Errorf("worker after the accepted replacement = %+v, want the file's text with its hash", ri)
	}
	if sv := settingOf(t, *mustSettings(t, e2), SettingInstructions); sv.Source != SourceImported || sv.Hash != res4.Hash || !strings.HasPrefix(sv.Value, "8 bytes") {
		t.Errorf("instructions setting after the accepted replacement = %+v, want the same text and hash as the role", sv)
	}

	// The same acceptance replaces an empty-by-choice mark.
	e3 := newEnv(t)
	mustSetRole(t, e3, RoleWorker, "")
	res5, err := e3.importWithText(file, ptr("w1 rules"), true)
	if err != nil || res5.NeedsConfirm || len(res5.Diffs) != 1 || res5.Diffs[0].Key != SettingInstructions || res5.Diffs[0].Old != "none" {
		t.Fatalf("accepted import over the empty choice = %+v, %v, want the marker's diff accepted", res5, err)
	}
	if ri := mustRole(t, e3, RoleWorker); ri.State != RoleStateFile || ri.Source != SourceImported || ri.Text != "w1 rules" || ri.Version != res5.Hash {
		t.Errorf("worker after the accepted marker replacement = %+v, want the file's text", ri)
	}
	if sv := settingOf(t, *mustSettings(t, e3), SettingInstructions); sv.Source != SourceImported || sv.Hash != res5.Hash {
		t.Errorf("instructions setting after the accepted marker replacement = %+v, want the file's text", sv)
	}

	// A local text conflicts with the file's and keeps its value.
	e4 := newEnv(t)
	if _, err := e4.importWithText(file, ptr("first rules"), false); err != nil {
		t.Fatalf("import: %v", err)
	}
	mustSetRole(t, e4, RoleWorker, "my local rules")
	if res, err := e4.importWithText(file, ptr("v4 rules"), false); err != nil || len(res.Conflicts) != 1 {
		t.Fatalf("import over a local text = %+v, %v, want one conflict", res, err)
	}
	if ri := mustRole(t, e4, RoleWorker); ri.Source != SourceLocal || ri.Text != "my local rules" {
		t.Errorf("worker after the conflicted import = %+v, want the local text kept", ri)
	}

	// A file that stops setting instructions drops an imported row, not
	// a local one.
	e5 := newEnv(t)
	if _, err := e5.importWithText(file, ptr("file rules"), false); err != nil {
		t.Fatalf("import: %v", err)
	}
	if ri := mustRole(t, e5, RoleWorker); ri.Source != SourceImported || ri.Text != "file rules" {
		t.Fatalf("worker before the drop = %+v, want imported", ri)
	}
	if _, err := e5.eng.ImportProjectConfig(e5.ctx, envRepo, cfgFile("approval_max_tasks: 3\n"), false); err != nil {
		t.Fatalf("import without instructions: %v", err)
	}
	if ri := mustRole(t, e5, RoleWorker); ri.State != RoleStateNotConfigured || ri.Source != SourceDefault {
		t.Errorf("worker after the drop = %+v, want not configured", ri)
	}

	// A whitespace-only file over the bound is refused like any
	// oversized text — never read as an unset value that clears the
	// import — and the previous value stays in effect.
	e6 := newEnv(t)
	res6, err := e6.importWithText(file, ptr("v1 rules"), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	_, err = e6.importWithText(file, ptr(strings.Repeat(" ", MaxRoleInstructionBytes+1)), false)
	if err == nil || !errors.Is(err, ErrInvalidSetting) {
		t.Fatalf("oversize whitespace-only import err = %v, want ErrInvalidSetting", err)
	}
	for _, want := range []string{RoleWorker, "8193", "8192"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("oversize whitespace-only import err = %v, want it to name %q", err, want)
		}
	}
	if ri := mustRole(t, e6, RoleWorker); ri.Source != SourceImported || ri.Text != "v1 rules" || ri.Version != res6.Hash {
		t.Errorf("worker after the refused import = %+v, want the previous value", ri)
	}
	if sv := settingOf(t, *mustSettings(t, e6), SettingInstructions); sv.Source != SourceImported || sv.Hash != res6.Hash {
		t.Errorf("instructions setting after the refused import = %+v, want the previous value", sv)
	}
}
