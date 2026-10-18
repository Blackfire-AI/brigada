package engine

import "testing"

// Removal's preflight must never mistake another project's live work for
// work in the repository being removed.
func TestStatusJobCountsRespectProjectScope(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	other := e.addProject("https://example.invalid/other.git")
	all, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if all.Jobs[statusQueued] == 0 {
		t.Fatal("fixture must contain live work")
	}
	own, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if own.Jobs[statusQueued] != all.Jobs[statusQueued] {
		t.Fatalf("own=%v all=%v", own.Jobs, all.Jobs)
	}
	empty, err := e.eng.Status(e.ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	for state, count := range empty.Jobs {
		if count != 0 {
			t.Errorf("other project's %s count=%d, want zero", state, count)
		}
	}
}

func TestStatusJobCountsExcludeArchivedProjectInEveryScope(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	other := e.addProject("https://example.invalid/other.git")
	if _, err := e.eng.ArchiveProject(e.ctx, archiveRepo, true); err != nil {
		t.Fatal(err)
	}
	for _, project := range []int64{0, e.project, other} {
		st, err := e.eng.Status(e.ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range JobStatuses {
			count, ok := st.Jobs[state]
			if !ok || count != 0 {
				t.Errorf("scope %d: %s count = %d (present %v), want zero",
					project, state, count, ok)
			}
		}
	}
}
