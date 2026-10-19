package runnerstate_test

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

func TestSavedForeman(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runner")
	if got, err := runnerstate.LoadForeman(dir); got != "" || err != nil {
		t.Fatalf("missing foreman: %q, %v", got, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("reading the foreman created directory: %v", err)
	}
	for _, want := range []string{"claude/sonnet@high", "codex/default"} {
		if err := runnerstate.SaveForeman(dir, want); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, "foreman-choice"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("saved mode: %v, %v", info, err)
		}
		if got, err := runnerstate.LoadForeman(dir); got != want || err != nil {
			t.Fatalf("loaded foreman: %q, %v; want %q", got, err, want)
		}
	}
}

// The foreman's scratch directories live at <dir>/foreman/<project>
// (runner.ForemanScratchDir): the saved choice must not need that path.
func TestSavedForemanBesideScratchDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runner")
	if err := os.MkdirAll(filepath.Join(dir, "foreman", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := runnerstate.LoadForeman(dir); got != "" || err != nil {
		t.Fatalf("missing foreman beside scratch dir: %q, %v", got, err)
	}
	if err := runnerstate.SaveForeman(dir, "claude/opus@high"); err != nil {
		t.Fatal(err)
	}
	if got, err := runnerstate.LoadForeman(dir); got != "claude/opus@high" || err != nil {
		t.Fatalf("loaded foreman: %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "foreman", "1")); err != nil || !info.IsDir() {
		t.Fatalf("scratch dir lost: %v, %v", info, err)
	}
}

// A choice saved under the old name, the file "foreman", is moved to the
// new one and frees the path; a choice saved under the new name wins.
func TestSavedForemanMigrates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runner")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "foreman")
	if err := os.WriteFile(old, []byte("codex/default@high\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := runnerstate.LoadForeman(dir); got != "codex/default@high" || err != nil {
		t.Fatalf("old foreman: %q, %v", got, err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("old file left behind: %v", err)
	}

	if err := os.WriteFile(old, []byte("kimi/default\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runnerstate.MigrateForeman(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := runnerstate.LoadForeman(dir); got != "codex/default@high" || err != nil {
		t.Fatalf("newer foreman lost: %q, %v", got, err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("stale old file left behind: %v", err)
	}
}

// Foreman turns and attaches migrate at once and make their scratch
// directories in the freed path: every call succeeds and the choice
// survives.
func TestSavedForemanMigratesConcurrently(t *testing.T) {
	for round := range 50 {
		dir := filepath.Join(t.TempDir(), "runner")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "foreman"), []byte("claude/opus@high\n"), 0600); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for project := range 8 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				if err := runnerstate.MigrateForeman(dir); err != nil {
					errs <- err
					return
				}
				// What runner.ForemanScratchDir does after migrating.
				errs <- os.MkdirAll(filepath.Join(dir, "foreman", strconv.Itoa(project)), 0o755)
			}()
			go func() {
				defer wg.Done()
				_, err := runnerstate.LoadForeman(dir)
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if got, err := runnerstate.LoadForeman(dir); got != "claude/opus@high" || err != nil {
			t.Fatalf("round %d: foreman after the race: %q, %v", round, got, err)
		}
		for project := range 8 {
			if info, err := os.Stat(filepath.Join(dir, "foreman", strconv.Itoa(project))); err != nil || !info.IsDir() {
				t.Fatalf("round %d: scratch dir %d: %v, %v", round, project, info, err)
			}
		}
	}
}
