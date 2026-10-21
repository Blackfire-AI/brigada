package runnerstate_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

func TestSavedAgents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runner")
	if specs, err := runnerstate.LoadAgents(dir); specs != nil || err != nil {
		t.Fatalf("missing choices: %v, %v", specs, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("reading choices created directory: %v", err)
	}
	want := []string{
		"harness=claude,model=opus,effort=high,tools=none",
		"harness=kimi,model=kimi-k2,classes=review",
	}
	for _, specs := range [][]string{want, want[1:]} {
		if err := runnerstate.SaveAgents(dir, specs); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "agents")
		data, err := os.ReadFile(file)
		if err != nil || string(data) != strings.Join(specs, "\n")+"\n" {
			t.Fatalf("saved contents: %q, %v", data, err)
		}
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("saved mode: %v, %v", info, err)
		}
		got, err := runnerstate.LoadAgents(dir)
		if err != nil || !reflect.DeepEqual(got, specs) {
			t.Fatalf("loaded choices: %v, %v", got, err)
		}
		if err := os.Chmod(file, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Neither stopping/cleaning stale metadata nor starting a runner consumes
	// the saved choices.
	release, err := runnerstate.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runnerstate.Publish(dir, nil, "live-spec"); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	if _, _, err := runnerstate.ClearStale(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := runnerstate.LoadAgents(dir); err != nil ||
		!reflect.DeepEqual(got, want[1:]) {
		t.Fatalf("cleanup changed saved choices: %v, %v", got, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".agents-") {
			t.Errorf("temporary file left behind: %s", file.Name())
		}
	}
}

func TestAppendAgentKeepsEveryLine(t *testing.T) {
	dir := t.TempDir()
	kept := []string{"# mine", "harness=claude,model=sonnet", "", "not an agent line"}
	if err := runnerstate.SaveAgents(dir, kept); err != nil {
		t.Fatal(err)
	}
	if err := runnerstate.AppendAgent(dir, "harness=codex,model=gpt-5"); err != nil {
		t.Fatal(err)
	}
	got, err := runnerstate.LoadAgents(dir)
	want := append(append([]string{}, kept...), "harness=codex,model=gpt-5")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, %v; want %q", got, err, want)
	}
}
