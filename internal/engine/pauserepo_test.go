package engine

import (
	"database/sql"
	"errors"
	"testing"
)

// PauseProject names a project by its repo URL for a pause (§8.3) and
// refuses a URL no project has; LivePauseOrigin reads the scope's
// standing pause.
func TestPauseProjectAndLiveOrigin(t *testing.T) {
	e := newEnv(t)
	var repo string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT repo_url FROM projects WHERE id = ?", e.project).Scan(&repo)
	}); err != nil {
		t.Fatal(err)
	}
	id, got, err := e.eng.PauseProject(e.ctx, repo)
	if err != nil || id != e.project || got != repo {
		t.Fatalf("PauseProject(%q) = %d, %q, %v; want project %d", repo, id, got, err, e.project)
	}
	if _, _, err := e.eng.PauseProject(e.ctx, "https://example.invalid/none.git"); !errors.Is(err, ErrNoProject) {
		t.Fatalf("unknown repo: %v, want ErrNoProject", err)
	}
	if origin, err := e.eng.LivePauseOrigin(e.ctx, 0); err != nil || origin != "" {
		t.Fatalf("origin before a pause = %q, %v", origin, err)
	}
	e.pauseEngine(PauseOriginDrain, "test")
	if origin, err := e.eng.LivePauseOrigin(e.ctx, 0); err != nil || origin != PauseOriginDrain {
		t.Fatalf("origin = %q, %v; want drain", origin, err)
	}
}
