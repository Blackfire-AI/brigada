package runnerstate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

func TestParallel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runner")
	if n, err := runnerstate.LoadParallel(dir); n != 0 || err != nil {
		t.Fatalf("missing: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("load created directory: %v", err)
	}
	for _, n := range []int{2, 64, 1} {
		if err := runnerstate.SaveParallel(dir, n); err != nil {
			t.Fatal(err)
		}
		if got, err := runnerstate.LoadParallel(dir); got != n || err != nil {
			t.Fatalf("got=%d want=%d err=%v", got, n, err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "parallel"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private saved choice: info=%v err=%v", info, err)
	}
	for _, n := range []int{0, -1, 65} {
		if err := runnerstate.SaveParallel(dir, n); err == nil {
			t.Errorf("saved invalid choice %d", n)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "parallel"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runnerstate.LoadParallel(dir); err == nil {
		t.Fatal("accepted broken saved choice")
	}
}

func TestPublishParallel(t *testing.T) {
	dir := t.TempDir()
	release, err := runnerstate.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := runnerstate.PublishParallel(dir, nil, 4, "saved-spec"); err != nil {
		t.Fatal(err)
	}
	rec, running, err := runnerstate.Running(dir)
	if err != nil || !running || rec.MaxParallel != 4 ||
		len(rec.Specs) != 1 || rec.Specs[0] != "saved-spec" {
		t.Fatalf("record=%+v running=%v err=%v", rec, running, err)
	}
}
